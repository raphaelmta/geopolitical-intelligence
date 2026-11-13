// Package queue wraps NATS JetStream: durable work-queue consumers with
// explicit acknowledgement.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream names. Subjects: ingest.request.<source>, jobs.run.<type>.
const StreamIngest, StreamJobs = "INGEST", "JOBS"

const (
	// A collection can run for hours; the handler is kept alive with
	// in-progress heartbeats, so AckWait only bounds how long a crashed
	// worker's message stays invisible.
	defaultAckWait   = 2 * time.Minute
	defaultHeartbeat = 30 * time.Second
	defaultNakDelay  = 30 * time.Second
	maxDeliver       = 5
)

// Client publishes to and consumes from JetStream.
type Client struct {
	nc *nats.Conn
	js jetstream.JetStream
	// prefix (like "t1a2b.") is prepended to subjects, and with dots turned
	// into underscores to stream names; empty in production, unique per test.
	prefix    string
	ackWait   time.Duration
	heartbeat time.Duration
	nakDelay  time.Duration
}

// Connect opens a connection and creates the INGEST and JOBS streams if missing.
func Connect(url string) (*Client, error) { return connect(url, "") }

// ConnectWithPrefix is Connect with every subject and stream name prefixed
// (like "t1a2b."), for tests that need streams of their own.
func ConnectWithPrefix(url, prefix string) (*Client, error) { return connect(url, prefix) }

func connect(url, prefix string) (*Client, error) {
	nc, err := nats.Connect(url, nats.Name("geoint"))
	if err != nil {
		return nil, fmt.Errorf("queue: connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("queue: jetstream: %w", err)
	}
	c := &Client{nc: nc, js: js, prefix: prefix,
		ackWait: defaultAckWait, heartbeat: defaultHeartbeat, nakDelay: defaultNakDelay}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, subjects := range map[string][]string{StreamIngest: {"ingest.>"}, StreamJobs: {"jobs.>"}} {
		cfg := jetstream.StreamConfig{
			Name:      c.streamName(name),
			Subjects:  []string{prefix + subjects[0]},
			Retention: jetstream.WorkQueuePolicy,
			Storage:   jetstream.FileStorage,
		}
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			nc.Close()
			return nil, fmt.Errorf("queue: stream %s: %w", cfg.Name, err)
		}
	}
	return c, nil
}

func (c *Client) streamName(name string) string { return StreamName(c.prefix, name) }

// StreamName is the JetStream name of the stream called name under prefix: the
// prefix with dots turned into underscores, then name.
func StreamName(prefix, name string) string {
	return strings.ReplaceAll(prefix, ".", "_") + name
}

// Close closes the connection.
func (c *Client) Close() { c.nc.Close() }

// Publish encodes v as JSON and stores it on subject; it returns once the
// server has persisted the message.
func (c *Client) Publish(ctx context.Context, subject string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("queue: encode: %w", err)
	}
	if _, err := c.js.Publish(ctx, c.prefix+subject, b); err != nil {
		return fmt.Errorf("queue: publish %s: %w", subject, err)
	}
	return nil
}

// Consume delivers messages matching subject to h, one at a time, until ctx
// is cancelled (then it returns nil). A message is acknowledged only after h
// returns nil; on error it is redelivered after a delay, at most maxDeliver
// times in total. While h runs, the message is kept in progress so a long
// handler is not redelivered. Several workers using the same consumer name
// share the work.
func (c *Client) Consume(ctx context.Context, stream, consumer, subject string, h func(context.Context, []byte) error) error {
	cons, err := c.js.CreateOrUpdateConsumer(ctx, c.streamName(stream), jetstream.ConsumerConfig{
		Durable:       consumer,
		FilterSubject: c.prefix + subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       c.ackWait,
		MaxDeliver:    maxDeliver,
	})
	if err != nil {
		return fmt.Errorf("queue: consumer %s: %w", consumer, err)
	}
	var inflight sync.WaitGroup
	cc, err := cons.Consume(func(m jetstream.Msg) {
		inflight.Add(1)
		defer inflight.Done()
		c.handle(ctx, m, h)
	}, jetstream.PullMaxMessages(1), // no prefetch: a buffered message would burn its AckWait
		jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
			slog.Warn("queue: consume error", "consumer", consumer, "error", err)
		}))
	if err != nil {
		return fmt.Errorf("queue: consume %s: %w", consumer, err)
	}
	<-ctx.Done()
	cc.Stop()
	inflight.Wait()
	return nil
}

func (c *Client) handle(ctx context.Context, m jetstream.Msg, h func(context.Context, []byte) error) {
	stop := make(chan struct{})
	var beat sync.WaitGroup
	beat.Add(1)
	go func() {
		defer beat.Done()
		t := time.NewTicker(c.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := m.InProgress(); err != nil {
					slog.Warn("queue: heartbeat failed", "subject", m.Subject(), "error", err)
				}
			}
		}
	}()
	err := safely(ctx, m, h)
	close(stop)
	beat.Wait()

	switch {
	case err == nil:
		if err := m.Ack(); err != nil {
			slog.Error("queue: ack failed", "subject", m.Subject(), "error", err)
		}
	case ctx.Err() != nil && lastDelivery(m):
		// Shutting down during the LAST delivery: a Nak would leave the
		// message in the work-queue stream forever (no delivery is left), so
		// it is terminated. What it started is recovered elsewhere: an ingest
		// run's collection stays running without its run lock and is failed
		// by the dead-run check (collection.FailDead) at the next worker
		// start; a job's row is failed by the jobs sweeper.
		slog.Warn("queue: shutdown during the last delivery, terminating the message", "subject", m.Subject())
		if err := m.Term(); err != nil {
			slog.Error("queue: term failed", "subject", m.Subject(), "error", err)
		}
	case ctx.Err() != nil:
		// Shutting down mid-handler: hand the message back right away.
		_ = m.Nak()
	case lastDelivery(m):
		// Redelivering would not happen anyway; Term so it does not sit
		// unacknowledged in the stream.
		slog.Error("queue: handler failed on the last delivery, dropping message", "subject", m.Subject(), "error", err)
		if err := m.Term(); err != nil {
			slog.Error("queue: term failed", "subject", m.Subject(), "error", err)
		}
	default:
		slog.Error("queue: handler failed", "subject", m.Subject(), "error", err)
		if err := m.NakWithDelay(c.nakDelay); err != nil {
			slog.Error("queue: nak failed", "subject", m.Subject(), "error", err)
		}
	}
}

// Attempt tells a handler which delivery of its message it is handling.
type Attempt struct {
	// N counts deliveries, starting at 1.
	N int
	// Last: no further delivery will follow if the handler fails.
	Last bool
}

type attemptKey struct{}

// AttemptFrom returns the delivery attempt of the message being handled, from
// the context Consume passes to the handler.
func AttemptFrom(ctx context.Context) (Attempt, bool) {
	a, ok := ctx.Value(attemptKey{}).(Attempt)
	return a, ok
}

// safely runs h, turning a panic into an error.
func safely(ctx context.Context, m jetstream.Msg, h func(context.Context, []byte) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("queue: handler panicked", "subject", m.Subject(), "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	if md, err := m.Metadata(); err == nil {
		ctx = context.WithValue(ctx, attemptKey{}, Attempt{N: int(md.NumDelivered), Last: md.NumDelivered >= maxDeliver})
	}
	return h(ctx, m.Data())
}

func lastDelivery(m jetstream.Msg) bool {
	md, err := m.Metadata()
	return err == nil && md.NumDelivered >= maxDeliver
}
