package rawstore_test

import (
	"bytes"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"geoint/internal/rawstore"
)

var ctx = context.Background()

func newStore(t *testing.T) (rawstore.Store, string) {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("TEST_S3_ENDPOINT is not set; run the tests through `make test`")
	}
	st, err := rawstore.NewS3(endpoint, os.Getenv("TEST_S3_ACCESS_KEY"), os.Getenv("TEST_S3_SECRET_KEY"), os.Getenv("TEST_S3_BUCKET"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	prefix := "raw/test-" + hex.EncodeToString(buf) + "/2026-10-01/"
	t.Cleanup(func() { removePrefix(t, endpoint, prefix) })
	return st, prefix
}

// removePrefix deletes the objects a test created, using a raw client.
func removePrefix(t *testing.T, endpoint, prefix string) {
	t.Helper()
	c, err := minio.New(strings.TrimPrefix(endpoint, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4(os.Getenv("TEST_S3_ACCESS_KEY"), os.Getenv("TEST_S3_SECRET_KEY"), "")})
	if err != nil {
		t.Errorf("cleanup client: %v", err)
		return
	}
	bucket := os.Getenv("TEST_S3_BUCKET")
	for o := range c.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			t.Errorf("cleanup list: %v", o.Err)
			return
		}
		if err := c.RemoveObject(context.Background(), bucket, o.Key, minio.RemoveObjectOptions{}); err != nil {
			t.Errorf("cleanup remove %s: %v", o.Key, err)
		}
	}
}

func TestPutStoresBytesAndReturnsSHA256(t *testing.T) {
	st, prefix := newStore(t)
	key := prefix + "a.txt"
	f, err := st.Put(ctx, key, strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if f.SHA256 != want || f.Size != 3 || f.Key != key {
		t.Fatalf("unexpected %+v", f)
	}
	rc, err := st.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != "abc" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestPutSameContentIsIdempotent(t *testing.T) {
	st, prefix := newStore(t)
	key := prefix + "a.txt"
	first, err := st.Put(ctx, key, strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.Put(ctx, key, strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("first %+v second %+v", first, second)
	}
}

func TestPutDifferentContentIsRejectedAndKeepsOriginal(t *testing.T) {
	st, prefix := newStore(t)
	key := prefix + "a.txt"
	if _, err := st.Put(ctx, key, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, key, strings.NewReader("different")); err == nil {
		t.Fatal("overwrite with different content accepted")
	}
	rc, err := st.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "abc" {
		t.Fatalf("original changed to %q", got)
	}
}

func TestExistsAndOpenMissing(t *testing.T) {
	st, prefix := newStore(t)
	key := prefix + "a.txt"
	ok, err := st.Exists(ctx, key)
	if err != nil || ok {
		t.Fatalf("before put: ok=%v err=%v", ok, err)
	}
	if _, err := st.Open(ctx, key); err == nil {
		t.Error("Open of a missing key succeeded")
	}
	if _, err := st.Put(ctx, key, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	ok, err = st.Exists(ctx, key)
	if err != nil || !ok {
		t.Fatalf("after put: ok=%v err=%v", ok, err)
	}
}

func TestPutLargeStreamsAcrossMultipartBoundary(t *testing.T) {
	st, prefix := newStore(t)
	key := prefix + "big.bin"
	const size = 40 << 20 // larger than one multipart part
	h := sha256.New()
	src := io.TeeReader(io.LimitReader(zeroReader{}, size), h)
	f, err := st.Put(ctx, key, src)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != size || f.SHA256 != hex.EncodeToString(h.Sum(nil)) {
		t.Fatalf("unexpected %+v", f)
	}
	rc, err := st.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if err != nil || n != size {
		t.Fatalf("read %d err=%v", n, err)
	}
	// Same bytes again: idempotent.
	again, err := st.Put(ctx, key, io.LimitReader(zeroReader{}, size))
	if err != nil || again != f {
		t.Fatalf("again %+v err=%v", again, err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestConcurrentPutsToSameKeyNeverOverwrite(t *testing.T) {
	// Small bodies exercise the single-request path, 20 MiB ones the multipart path.
	for name, size := range map[string]int{"small": 1 << 10, "multipart": 20 << 20} {
		t.Run(name, func(t *testing.T) {
			st, prefix := newStore(t)
			key := prefix + "race.bin"
			contents := [2][]byte{bytes.Repeat([]byte{'A'}, size), bytes.Repeat([]byte{'B'}, size)}
			const n = 8
			errs := make([]error, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, errs[i] = st.Put(ctx, key, bytes.NewReader(contents[i%2]))
				}()
			}
			close(start)
			wg.Wait()

			rc, err := st.Open(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			win := 0
			if got[0] == 'B' {
				win = 1
			}
			if !bytes.Equal(got, contents[win]) {
				t.Fatal("stored object is not one of the two contents")
			}
			for i, err := range errs {
				switch {
				case i%2 == win && err != nil:
					t.Errorf("put %d (winning content): %v", i, err)
				case i%2 != win && !errors.Is(err, rawstore.ErrConflict):
					t.Errorf("put %d (losing content): err = %v, want ErrConflict", i, err)
				}
			}
		})
	}
}

func TestStatReportsSizeAndChecksum(t *testing.T) {
	st, prefix := newStore(t)
	key := prefix + "s.txt"
	if _, ok, err := st.Stat(ctx, key); err != nil || ok {
		t.Fatalf("before put: ok=%v err=%v", ok, err)
	}
	put, err := st.Put(ctx, key, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.Stat(ctx, key)
	if err != nil || !ok {
		t.Fatalf("after put: ok=%v err=%v", ok, err)
	}
	if got != put || got.Size != 5 {
		t.Errorf("Stat = %+v, want %+v", got, put)
	}
}
