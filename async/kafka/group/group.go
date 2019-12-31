package group

import (
	"context"
	"fmt"
	"reflect"
	"runtime"

	"github.com/Shopify/sarama"
	"github.com/beatlabs/patron/async"
	"github.com/beatlabs/patron/async/kafka"
	"github.com/beatlabs/patron/errors"
	"github.com/beatlabs/patron/log"
	"github.com/opentracing/opentracing-go"
)

// Factory definition of a consumer factory.
type Factory struct {
	name    string
	group   string
	topic   string
	brokers []string
	oo      []kafka.OptionFunc
}

// New constructor.
func New(name, group, topic string, brokers []string, oo ...kafka.OptionFunc) (*Factory, error) {

	if name == "" {
		return nil, errors.New("name is required")
	}

	if group == "" {
		return nil, errors.New("group is required")
	}

	if len(brokers) == 0 {
		return nil, errors.New("provide at least one broker")
	}

	if topic == "" {
		return nil, errors.New("topic is required")
	}

	return &Factory{name: name, group: group, topic: topic, brokers: brokers, oo: oo}, nil
}

// Create a new consumer.
func (f *Factory) Create() (async.Consumer, error) {

	config, err := kafka.SaramaConfig(f.name)

	if err != nil {
		return nil, err
	}

	cc := kafka.ConsumerConfig{
		Brokers: f.brokers,
		Buffer:  0,
	}

	c := &consumer{
		topic:       f.topic,
		group:       f.group,
		traceTag:    opentracing.Tag{Key: "group", Value: f.group},
		saramaCnf:   config,
		consumerCnf: cc,
	}

	for _, o := range f.oo {
		err = o(c)
		if err != nil {
			return nil, fmt.Errorf("Could not apply OptionFunc '%v' to consumer : %v", runtime.FuncForPC(reflect.ValueOf(o).Pointer()).Name(), err)
		}
	}

	return c, nil
}

// consumer members can be injected or overwritten with the usage of OptionFunc arguments.
type consumer struct {
	kafka.Consumer
	topic       string
	group       string
	traceTag    opentracing.Tag
	cnl         context.CancelFunc
	cg          sarama.ConsumerGroup
	consumerCnf kafka.ConsumerConfig
	saramaCnf   *sarama.Config
}

func (c *consumer) consumerConfig() *kafka.ConsumerConfig { return &c.consumerCnf }
func (c *consumer) saramaConfig() *sarama.Config          { return c.saramaCnf }

// Close handles closing consumer.
func (c *consumer) Close() error {
	if c.cnl != nil {
		c.cnl()
	}

	return errors.Wrap(c.cg.Close(), "failed to close consumer")
}

// Consume starts consuming messages from a Kafka topic.
func (c *consumer) Consume(ctx context.Context) (<-chan async.Message, <-chan error, error) {
	ctx, cnl := context.WithCancel(ctx)
	c.cnl = cnl

	return consumeWithGroup(ctx, c)
}

func consumeWithGroup(ctx context.Context, c *consumer) (<-chan async.Message, <-chan error, error) {

	cg, err := sarama.NewConsumerGroup(c.consumerCnf.Brokers, c.group, c.saramaCnf)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create consumer")
	}
	c.cg = cg
	log.Infof("consuming messages from topic '%s' using group '%s'", c.topic, c.group)

	chMsg := make(chan async.Message, c.consumerCnf.Buffer)
	chErr := make(chan error, c.consumerCnf.Buffer)

	go func(consumer sarama.ConsumerGroup) {
		for {
			select {
			case <-ctx.Done():
				log.Info("canceling consuming messages requested")
				closeConsumer(consumer)
				return
			case consumerError := <-consumer.Errors():
				closeConsumer(consumer)
				chErr <- consumerError
				return
			}
		}
	}(c.cg)

	// Iterate over consumer sessions.
	go func(consumer sarama.ConsumerGroup) {
		hnd := handler{consumer: c, messages: chMsg}
		for {
			err := consumer.Consume(ctx, []string{c.topic}, hnd)
			if err != nil {
				chErr <- err
			}
		}
	}(c.cg)

	return chMsg, chErr, nil
}

func closeConsumer(cns sarama.ConsumerGroup) {
	if cns == nil {
		return
	}
	err := cns.Close()
	if err != nil {
		log.Errorf("failed to close partition consumer: %v", err)
	}
}

type handler struct {
	consumer *consumer
	messages chan async.Message
}

func (h handler) Setup(_ sarama.ConsumerGroupSession) error   { return nil }
func (h handler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }
func (h handler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	ctx := sess.Context()
	for msg := range claim.Messages() {
		kafka.TopicPartitionOffsetDiffGaugeSet(h.consumer.group, msg.Topic, msg.Partition, claim.HighWaterMarkOffset(), msg.Offset)
		m, err := kafka.ClaimMessage(ctx, h.consumer.consumerCnf.DecoderFunc, msg, sess)
		if err != nil {
			return err
		}
		h.messages <- m
	}
	return nil
}