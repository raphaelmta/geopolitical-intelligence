package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"geoint/internal/queue"
)

// Queue returns a NATS client whose subjects and streams carry a prefix
// unique to the test, so tests never collide with each other or with
// production names. Its streams are deleted on Cleanup.
func Queue(t *testing.T) *queue.Client {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Fatal("TEST_NATS_URL is not set; run the tests through `make test`")
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	c, err := queue.ConnectWithPrefix(url, "t"+hex.EncodeToString(buf)+".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deleteStreams(t, url, "t"+hex.EncodeToString(buf)+".")
		c.Close()
	})
	return c
}

// deleteStreams removes the prefixed streams through a connection of its own,
// so it works even when the test closed its client.
func deleteStreams(t *testing.T, url, prefix string) {
	nc, err := nats.Connect(url)
	if err != nil {
		t.Errorf("delete streams: %v", err)
		return
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Errorf("delete streams: %v", err)
		return
	}
	for _, name := range []string{queue.StreamIngest, queue.StreamJobs} {
		if err := js.DeleteStream(context.Background(), queue.StreamName(prefix, name)); err != nil {
			t.Errorf("delete stream %s: %v", name, err)
		}
	}
}
