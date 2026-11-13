package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type msg struct{ N int }

// newClient connects with a unique prefix so streams and subjects never
// collide with production names or with other tests; its streams are removed
// on cleanup.
func newClient(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Fatal("TEST_NATS_URL is not set; run the tests through `make test`")
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	c, err := connect(url, "t"+hex.EncodeToString(buf)+".")
	if err != nil {
		t.Fatal(err)
	}
	c.nakDelay = 50 * time.Millisecond
	t.Cleanup(func() {
		for _, s := range []string{StreamIngest, StreamJobs} {
			_ = c.js.DeleteStream(context.Background(), c.streamName(s))
		}
		c.Close()
	})
	return c
}

func TestConsumeRedeliversWhenHandlerFails(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.Publish(ctx, "ingest.request.receita", msg{N: 7}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	second := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Consume(ctx, StreamIngest, "w", "ingest.request.*", func(_ context.Context, b []byte) error {
			if string(b) != `{"N":7}` {
				t.Errorf("payload %q", b)
			}
			if calls.Add(1) == 1 {
				return errors.New("boom")
			}
			close(second)
			return nil
		})
	}()
	select {
	case <-second:
	case <-time.After(15 * time.Second):
		t.Fatal("message was not redelivered")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Consume returned %v on clean cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Consume did not return after cancel")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("handler called %d times, want 2", n)
	}
}

func TestConsumeKeepsLongHandlerAliveWithHeartbeats(t *testing.T) {
	c := newClient(t)
	c.ackWait = 600 * time.Millisecond
	c.heartbeat = 150 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.Publish(ctx, "jobs.run.x", msg{N: 1}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	finished := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Consume(ctx, StreamJobs, "w", "jobs.run.*", func(hctx context.Context, _ []byte) error {
			calls.Add(1)
			select { // three times the ack wait
			case <-time.After(1800 * time.Millisecond):
			case <-hctx.Done():
				return hctx.Err()
			}
			close(finished)
			return nil
		})
	}()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("handler did not finish")
	}
	cancel()
	<-done
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler called %d times, want 1 (redelivered while running)", n)
	}
}

func TestConsumeStopsAfterMaxDeliveries(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Publish(ctx, "jobs.run.x", msg{N: 1}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- c.Consume(ctx, StreamJobs, "w", "jobs.run.*", func(context.Context, []byte) error {
			calls.Add(1)
			return errors.New("poison")
		})
	}()
	deadline := time.After(20 * time.Second)
	for calls.Load() < maxDeliver {
		select {
		case <-deadline:
			t.Fatalf("only %d deliveries", calls.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	time.Sleep(500 * time.Millisecond) // a sixth delivery would have arrived by now (nak delay 50ms)
	cancel()
	<-done
	if n := calls.Load(); n != maxDeliver {
		t.Fatalf("handler called %d times, want %d", n, maxDeliver)
	}
}

func TestConsumeSurvivesHandlerPanic(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Publish(ctx, "jobs.run.x", msg{N: 1}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	second := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Consume(ctx, StreamJobs, "w", "jobs.run.*", func(context.Context, []byte) error {
			if calls.Add(1) == 1 {
				panic("kaboom")
			}
			close(second)
			return nil
		})
	}()
	select {
	case <-second:
	case <-time.After(15 * time.Second):
		t.Fatal("message was not redelivered after a panic")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAttemptFromReportsDeliveryCount(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Publish(ctx, "ingest.request.receita", msg{N: 1}); err != nil {
		t.Fatal(err)
	}
	type seen struct {
		n    int
		last bool
	}
	got := make(chan seen, maxDeliver+2)
	go func() {
		_ = c.Consume(ctx, StreamIngest, "w", "ingest.request.*", func(ctx context.Context, _ []byte) error {
			a, ok := AttemptFrom(ctx)
			if !ok {
				t.Error("no attempt in context")
			}
			got <- seen{a.N, a.Last}
			return errors.New("again")
		})
	}()
	for want := 1; want <= maxDeliver; want++ {
		select {
		case s := <-got:
			if s.n != want || s.last != (want == maxDeliver) {
				t.Fatalf("delivery %d: got %+v", want, s)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("delivery %d never came", want)
		}
	}
	if _, ok := AttemptFrom(context.Background()); ok {
		t.Fatal("AttemptFrom on a plain context")
	}
}

// Shutdown during the last delivery terminates the message: it must not
// stay Nak'd in the work-queue stream with no delivery left.
func TestShutdownDuringTheLastDeliveryTermsTheMessage(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Publish(ctx, "jobs.run.x", msg{N: 1}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	inLast := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Consume(ctx, StreamJobs, "w", "jobs.run.*", func(hctx context.Context, _ []byte) error {
			if calls.Add(1) < maxDeliver {
				return errors.New("transient")
			}
			close(inLast)
			<-hctx.Done() // the worker is stopping during the last delivery
			return hctx.Err()
		})
	}()
	select {
	case <-inLast:
	case <-time.After(20 * time.Second):
		t.Fatalf("only %d deliveries", calls.Load())
	}
	cancel()
	<-done
	st, err := c.js.Stream(context.Background(), c.streamName(StreamJobs))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := st.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Msgs == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d message(s) left in the work-queue stream, want 0", info.State.Msgs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
