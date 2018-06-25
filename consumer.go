package cluster

import (
	"sync"
	"time"

	"github.com/Shopify/sarama"
)

// Consumer allows to consume topics as member of
// a consumer group cluster.
type Consumer interface {
	// Topics lists consumer subscriptions.
	Topics() []string
	// SetTopics sets the consumable topic(s) for this consumer.
	SetTopics(topics ...string)
	// Errors returns a read channel of errors that occurred during consuming, if
	// enabled. By default, errors are logged and not returned over this channel.
	// If you want to implement any custom error handling, set your config's
	// Consumer.Return.Errors setting to true, and read from this channel.
	Errors() <-chan error
	// Claims issues notifications about new claims. It should be consumed.
	Claims() <-chan *Claim
	// Close stops the consumer. It is required to call this method before a
	// consumer object passes out of scope, as it will otherwise leak memory.
	Close() error
}

type consumer struct {
	client    sarama.Client
	ownClient bool
	groupID   string
	config    *sarama.Config
	handler   Handler

	group sarama.ConsumerGroup

	topics   []string
	topicsMu sync.RWMutex

	claims    chan *Claim
	errors    chan error
	rebalance chan none

	closing   chan none
	closed    chan none
	closeOnce sync.Once
}

// NewConsumer starts a new consumer.
func NewConsumer(addrs []string, groupID string, topics []string, config *sarama.Config, handler Handler) (Consumer, error) {
	if config == nil {
		config = sarama.NewConfig()
		config.Version = sarama.V0_10_2_0
	}

	client, err := sarama.NewClient(addrs, config)
	if err != nil {
		return nil, err
	}
	csmr, err := NewConsumerFromClient(client, groupID, topics, handler)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	csmr.(*consumer).ownClient = true
	return csmr, nil
}

// NewConsumerFromClient starts a new consumer from an existing client.
//
// Please note that clients cannot be shared between consumers (due to Kafka internals),
// they can only be re-used which requires the user to call Close() on the first consumer
// before using this method again to initialize another one. Attempts to use a client with
// more than one consumer at a time will return errors.
func NewConsumerFromClient(client sarama.Client, groupID string, topics []string, handler Handler) (Consumer, error) {
	config := client.Config()
	if !config.Version.IsAtLeast(sarama.V0_10_2_0) {
		return nil, sarama.ConfigurationError("consumer groups require Version to be >= V0_10_2_0")
	}

	group, err := sarama.NewConsumerGroupFromClient(groupID, client)
	if err != nil {
		return nil, err
	}

	c := &consumer{
		groupID:   groupID,
		topics:    topics,
		client:    client,
		config:    config,
		group:     group,
		handler:   handler,
		claims:    make(chan *Claim, config.ChannelBufferSize),
		errors:    make(chan error, config.ChannelBufferSize),
		rebalance: make(chan none, 1),
		closing:   make(chan none, 1),
		closed:    make(chan none, 1),
	}
	go c.mainLoop()
	return c, nil
}

func (c *consumer) Errors() <-chan error {
	return c.errors
}

func (c *consumer) Claims() <-chan *Claim {
	return c.claims
}

func (c *consumer) Topics() []string {
	c.topicsMu.RLock()
	topics := c.topics
	c.topicsMu.RUnlock()

	return topics
}

func (c *consumer) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closing)
		<-c.closed

		// close consumer group
		err = c.group.Close()

		// close client if one was created by us
		if c.ownClient {
			if e := c.client.Close(); e != nil {
				err = e
			}
		}

		// close channels
		close(c.claims)
		close(c.errors)
	})
	return err
}

func (c *consumer) SetTopics(topics ...string) {
	c.topicsMu.Lock()
	c.topics = topics
	c.topicsMu.Unlock()

	// trigger a rebalance
	select {
	case c.rebalance <- none{}:
	default:
	}
}

func (c *consumer) mainLoop() {
	defer close(c.closed)

	for {
		// drain rebalance channel
		select {
		case <-c.rebalance:
		default:
		}

		// check if closing
		select {
		case <-c.closing:
			return
		default:
		}

		// obtain current topics
		topics := c.Topics()
		if len(topics) == 0 {
			c.backoff()
			continue
		}

		if err := c.nextSession(topics); err != nil {
			c.handleError(err)
			c.backoff()
		}
	}
}

func (c *consumer) nextSession(topics []string) error {
	// start a new session
	sess, err := c.group.Subscribe(topics)
	if err != nil {
		return err
	}
	defer sess.Close()

	// listen for rebalance, close the session
	go func() {
		defer sess.Cancel()

		select {
		case <-c.closing:
		case <-c.rebalance:
		}
	}()

	// handle errors
	go func() {
		for err := range sess.Errors() {
			c.handleError(err)
		}
	}()

	// issue rebalance notification about new claims
	select {
	case c.claims <- &Claim{Current: sess.Claims()}:
	default:
	}

	// start consumer loop, blocking
	sess.Consume(sarama.ConsumerGroupHandlerFunc(func(claim sarama.ConsumerGroupClaim) error {
		return c.handler.ProcessPartition(&partitionConsumer{
			ConsumerGroupClaim: claim,
			sess:               sess,
		})
	}))

	// close session explicitly
	return sess.Close()
}

func (c *consumer) handleError(err error) {
	if c.config.Consumer.Return.Errors {
		select {
		case c.errors <- err:
		case <-c.closing:
		}
	} else {
		sarama.Logger.Println(err)
	}
}

func (c *consumer) backoff() {
	backoff := time.NewTimer(c.config.Consumer.Retry.Backoff)
	defer backoff.Stop()

	select {
	case <-backoff.C:
	case <-c.closing:
	}
}
