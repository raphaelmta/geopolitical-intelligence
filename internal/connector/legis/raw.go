package legis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/rawstore"
)

// ReadRaw reads a stored file, at most limit bytes, and checks its SHA-256
// against the one the raw store recorded.
func ReadRaw(ctx context.Context, raw rawstore.Store, f rawstore.File, limit int64) ([]byte, error) {
	rc, err := raw.Open(ctx, f.Key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Key, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: %w (%d bytes)", f.Key, ErrTooLarge, limit)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); f.SHA256 != "" && got != f.SHA256 {
		return nil, fmt.Errorf("%s: checksum %s, raw store recorded %s", f.Key, got, f.SHA256)
	}
	return b, nil
}

// Inputs are the raw files of one load, by base name, each read once,
// checked and recorded in collection_file (its name is the file part of the
// record_ref of every claim read from it).
type Inputs struct {
	files map[string]rawstore.File
	raw   rawstore.Store
	pool  *pgxpool.Pool
	col   int64
	limit int64
	urls  map[string]string
}

// NewInputs indexes files by base name; two files with one name are an
// error. urls maps raw keys to the request URL each file answers
// (Fetcher.URLs, kept by the connector between Fetch and Load).
func NewInputs(files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool, col int64, limit int64, urls map[string]string) (*Inputs, error) {
	in := &Inputs{files: map[string]rawstore.File{}, raw: raw, pool: pool, col: col, limit: limit, urls: urls}
	for _, f := range files {
		name := path.Base(f.Key)
		if _, dup := in.files[name]; dup {
			return nil, fmt.Errorf("two raw files named %s", name)
		}
		in.files[name] = f
	}
	return in, nil
}

// Names are the base names of the files.
func (in *Inputs) Names() []string {
	out := make([]string, 0, len(in.files))
	for n := range in.files {
		out = append(out, n)
	}
	return out
}

// Has reports whether a file of that name is among the inputs.
func (in *Inputs) Has(name string) bool { _, ok := in.files[name]; return ok }

// ErrMissingFile: a file the load needs is not among the fetched files.
var ErrMissingFile = errors.New("file missing from the fetched files")

// Read reads, checks and records the file named name.
func (in *Inputs) Read(ctx context.Context, name string) ([]byte, error) {
	f, ok := in.files[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrMissingFile)
	}
	b, err := ReadRaw(ctx, in.raw, f, in.limit)
	if err != nil {
		return nil, err
	}
	sum := f.SHA256
	if sum == "" {
		s := sha256.Sum256(b)
		sum = hex.EncodeToString(s[:])
	}
	u, ok := in.urls[f.Key]
	if !ok {
		return nil, fmt.Errorf("%s: no request URL recorded for it (Load runs after Fetch)", name)
	}
	if err := collection.RecordSourceFile(ctx, in.pool, in.col, collection.File{Name: name, RawKey: f.Key, SHA256: sum,
		Size: int64(len(b)), SourceURL: u}); err != nil {
		return nil, err
	}
	return b, nil
}

// Ref is a record_ref: the raw file name, ':' and a JSON pointer inside it.
// The API's provenance route takes the file name up to the last ':', so the
// pointer must have none.
func Ref(file, pointer string) string {
	return file + ":" + strings.ReplaceAll(pointer, ":", "_")
}

// ShapeError is a response whose structure is not the documented one: the
// collection fails rather than load something it does not understand.
type ShapeError struct {
	File, What string
}

func (e *ShapeError) Error() string {
	return fmt.Sprintf("%s: unexpected response shape: %s", e.File, e.What)
}

// Decode unmarshals b into v, reporting a failure as a ShapeError (without
// the content: responses hold personal data).
func Decode(file string, b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		switch {
		case errors.As(err, &se):
			return &ShapeError{File: file, What: fmt.Sprintf("invalid JSON at byte %d", se.Offset)}
		case errors.As(err, &te):
			return &ShapeError{File: file, What: fmt.Sprintf("field %q has a %s", te.Field, te.Value)}
		default:
			return &ShapeError{File: file, What: "cannot decode"}
		}
	}
	return nil
}

// LoadLock holds a session advisory lock for a whole load: two loads of one
// source never interleave their writes (first-seen person records, closing
// ended claims).
func LoadLock(ctx context.Context, pool *pgxpool.Pool, key int64, source string) (release func(), err error) {
	if n := pool.Config().MaxConns; n < 2 {
		return nil, fmt.Errorf("%s: the pool allows %d connection(s), the load needs at least 2 (pool_max_conns)", source, n)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: acquire lock connection: %w", source, err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("%s: load lock: %w", source, err)
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
			slog.Warn(source+": unlock failed, closing the connection", "error", err)
			_ = conn.Conn().Close(uctx)
		}
		conn.Release()
	}, nil
}
