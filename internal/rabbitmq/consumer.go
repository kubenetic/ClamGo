package rabbitmq

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog/log"
)

type MessageHandler func(ctx context.Context, message amqp.Delivery) *ConsumeError

// Consumer is a placeholder for future consumer-related utilities
// built on top of ConnectionManager. It will manage consumer channels,
// subscriptions, and graceful shutdown semantics.
//
// Note: currently unused; reserved for upcoming features.
type Consumer struct {
	cm *ConnectionManager

	conCh *amqp.Channel
	cra   int // consumer reinit attempts
	conMu sync.Mutex

	pubCh *amqp.Channel
	pra   int // publisher reinit attempts
	pubMu sync.Mutex
}

func NewConsumer(cm *ConnectionManager) (*Consumer, error) {
	c := &Consumer{cm: cm}
	if err := c.initConsumerChannel(); err != nil {
		return nil, err
	}

	if err := c.initPublisherChannel(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Consumer) initConsumerChannel() error {
	ch, err := c.cm.getChannel()
	if err != nil {
		return err
	}

	if err := ch.Qos(1, 0, false); err != nil {
		_ = ch.Close()
		return err
	}

	c.conCh = ch
	c.cra = 1

	if c.cra > 1 {
		log.Debug().Msg("consumer channel reinitialized")
	} else {
		log.Debug().Msg("consumer channel initialized")
	}

	return nil
}

func (c *Consumer) initPublisherChannel() error {
	ch, err := c.cm.getChannel()
	if err != nil {
		return err
	}

	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return err
	}

	c.pubCh = ch
	c.pra = 1

	if c.pra > 1 {
		log.Debug().Msg("publisher channel reinitialized")
	} else {
		log.Debug().Msg("publisher channel initialized")
	}

	return nil
}

func (c *Consumer) Close() error {
	c.conMu.Lock()
	defer c.conMu.Unlock()

	c.pubMu.Lock()
	defer c.pubMu.Unlock()

	if c.conCh != nil && !c.conCh.IsClosed() {
		return c.conCh.Close()
	}

	if c.pubCh != nil && !c.pubCh.IsClosed() {
		return c.pubCh.Close()
	}

	return nil
}

// Subscribe initializes a consumer to a specified queue and processes messages using the provided MessageHandler
// callback.
//
// The provided context is used to signal the consumer to stop. Subscribe blocks until the context is canceled or
// the channel is closed.
func (c *Consumer) Subscribe(ctx context.Context, queue, consumer string, cb MessageHandler) error {
	backoff := 500 * time.Millisecond
	maxBackoff := 10 * time.Second

	for ctx.Err() == nil {
		// Lazy reinit if the channel is not ready with retry logic
		c.conMu.Lock()
		if c.conCh == nil || c.conCh.IsClosed() {
			if err := c.initConsumerChannel(); err != nil {
				c.conMu.Unlock()

				sleep := jitter(backoff)
				select {
				case <-time.After(sleep):
					backoff *= 2
					if backoff > maxBackoff {
						log.Error().Msg("backoff exceeded")
						return ErrChannelReinitBackoffExceed
					}
					continue

				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}

		// Channel is ready, proceed with consuming messages
		ch := c.conCh
		c.conMu.Unlock()

		backoff = 500 * time.Millisecond

		if consumer == "" {
			consumer = genConsumerTag()
		}

		messages, err := ch.ConsumeWithContext(
			ctx, queue, consumer, false, false, false, false, nil)
		if err != nil {
			log.Error().Err(err).Msg("error consuming messages")

			sleep := jitter(backoff)
			select {
			case <-time.After(sleep):
				backoff *= 2
				if backoff > maxBackoff {
					log.Error().Msg("backoff exceeded")
					return ctx.Err()
				}
				continue

			case <-ctx.Done():
				return ctx.Err()
			}
		}

		for {
			log.Debug().Msg("waiting for message...")

			select {
			case <-ctx.Done():
				_ = ch.Cancel(consumer, true)
				return ctx.Err()

			case message, ok := <-messages:
				if !ok {
					// in case of channel closure, cancel the consumer and force reinitialize the channel
					c.conMu.Lock()
					if c.conCh != nil {
						_ = c.conCh.Cancel(consumer, true)
						c.conCh = nil
					}
					c.conMu.Unlock()
					break
				}

				func() {
					cbCtx, cbCncl := context.WithTimeout(ctx, 30*time.Second)
					defer cbCncl()

					defer func() {
						if r := recover(); r != nil {
							log.Error().Msg("panic in message handler")
							_ = message.Nack(false, false)
						}
					}()

					err := cb(cbCtx, message)

					if err != nil && err.Republish {
						if err := c.republish(ctx, message); err != nil {
							log.Error().Err(err).Msg("error republishing message")
						}
					}
				}()
			}
		}
	}

	return ctx.Err()
}

func genConsumerTag() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	return fmt.Sprintf("c-%s-%d", hostname, os.Getpid())
}

func getMsgDestination(attempt int) (string, string) {
	switch attempt {
	case 1:
		return "scan.retry.x", "5s"
	case 2:
		return "scan.retry.x", "30s"
	case 3:
		return "scan.retry.x", "1m"
	default:
		return "scan.dead.x", "dead"
	}
}

func getAttempts(headers amqp.Table) int {
	attempt, ok := headers["x-attempts"]
	if !ok {
		return 1
	}
	return attempt.(int)
}

func cloneHeaders(h amqp.Table) amqp.Table {
	out := amqp.Table{}
	for k, v := range h {
		out[k] = v
	}
	return out
}

func (c *Consumer) republish(ctx context.Context, message amqp.Delivery) error {
	attempt := getAttempts(message.Headers)
	exchange, routingKey := getMsgDestination(attempt)

	if attempt > 3 {
		log.Error().Msg("message failed to process, publish to DLX")
	} else {
		log.Warn().
			Int("attempt", attempt).
			Str("exchange", exchange).
			Str("routingKey", routingKey).
			Msg("message failed to process, try to republish")
	}

	repubCtx, repubCncl := context.WithTimeout(ctx, 30*time.Second)
	defer repubCncl()

	headers := cloneHeaders(message.Headers)
	headers["x-attempts"] = attempt + 1

	c.pubMu.Lock()
	if c.pubCh == nil || c.pubCh.IsClosed() {
		_ = c.initPublisherChannel()
	}
	c.pubMu.Unlock()

	ch := c.pubCh

	const maxRetries = 3
	backoff := 500 * time.Millisecond
	for attempt := 0; attempt <= maxRetries; attempt++ {
		dc, err := ch.PublishWithDeferredConfirmWithContext(
			repubCtx, exchange, routingKey, false, false, amqp.Publishing{
				CorrelationId: message.CorrelationId,
				MessageId:     message.MessageId,
				ContentType:   message.ContentType,
				Headers:       headers,
				Body:          message.Body,
			})

		if err != nil {
			log.Error().Err(err).
				Str("exchange", exchange).
				Str("routingKey", routingKey).
				Msg("retry publish write failed")

			// force reinit so next attempt gets a fresh channel
			c.pubMu.Lock()
			if c.pubCh != nil {
				_ = c.pubCh.Close()
				c.pubCh = nil
			}
			c.pubMu.Unlock()

			return err
		} else {
			select {
			case <-dc.Done():
				if dc.Acked() {
					return nil
				}

				return ErrNacked

			case <-repubCtx.Done():
				log.Warn().Msg("publish cancelled by the caller")
				return repubCtx.Err()

			case <-time.After(30 * time.Second):
				log.Warn().Msg("publish timed out")
				return fmt.Errorf("confirmation timed out")
			}
		}

		select {
		case <-repubCtx.Done():
			log.Warn().Msg("publish cancelled by the caller")
			return repubCtx.Err()

		case <-time.After(jitter(backoff)):
			if backoff < 5*time.Second {
				backoff *= 2
			}
		}
	}
	return nil
}
