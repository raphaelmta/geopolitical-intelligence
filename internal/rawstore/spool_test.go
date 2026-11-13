package rawstore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// A failed spool write is recorded, so Put labels it as the disk's fault
// and not the source's.
func TestRecordingWriterKeepsTheWriteError(t *testing.T) {
	w := &recordingWriter{w: failingWriter{}}
	_, err := io.Copy(io.MultiWriter(w, io.Discard), strings.NewReader("data"))
	if err == nil || w.err == nil {
		t.Fatalf("copy err %v, recorded %v; want both set", err, w.err)
	}
	ok := &recordingWriter{w: io.Discard}
	if _, err := io.Copy(ok, strings.NewReader("data")); err != nil || ok.err != nil {
		t.Fatalf("copy err %v, recorded %v; want none", err, ok.err)
	}
}

func TestSweepSpoolRemovesOnlyStaleSpoolFiles(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	write := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale1 := write("rawstore-111", old)
	stale2 := write("rawstore-222", old)
	fresh := write("rawstore-333", time.Now())
	other := write("receita-part.zip", old)
	if err := os.Mkdir(filepath.Join(dir, "rawstore-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := SweepSpool(dir, 15*time.Minute)
	if err != nil || n != 2 {
		t.Fatalf("SweepSpool = %d, %v; want 2, nil", n, err)
	}
	for _, p := range []string{stale1, stale2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists", p)
		}
	}
	for _, p := range []string{fresh, other, filepath.Join(dir, "rawstore-dir")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}
