// Package rawstore keeps the original files downloaded from public sources.
// Files are never overwritten with different content.
package rawstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// File describes a stored raw file.
type File struct {
	Key, SHA256 string
	Size        int64
}

// Store holds raw files by key: raw/<source>/<YYYY-MM-DD>/<file>.
type Store interface {
	// Put streams r into key and returns its SHA-256. Putting the same
	// content again is a no-op; different content under an existing key is an error.
	Put(ctx context.Context, key string, r io.Reader) (File, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Exists(ctx context.Context, key string) (bool, error)
	// Stat describes the file under key; ok is false when there is none.
	Stat(ctx context.Context, key string) (f File, ok bool, err error)
}

// ErrConflict is returned by Put when the key already holds different content.
var ErrConflict = errors.New("rawstore: key already holds different content")

const (
	shaMeta  = "sha256"
	partSize = 16 << 20 // also the threshold above which uploads are multipart
	maxParts = 9999
)

type s3Store struct {
	c        *minio.Client
	bucket   string
	spoolDir string
}

// NewS3 connects to an S3-compatible server and creates the bucket if missing.
// endpoint is host:port; TLS is used only when it starts with https://.
// Uploads are spooled to RAW_SPOOL_DIR (default: the OS temp dir) while being
// hashed, so that very large files never sit in memory.
func NewS3(endpoint, access, secret, bucket string) (Store, error) {
	secure := strings.HasPrefix(endpoint, "https://")
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" || bucket == "" {
		return nil, errors.New("rawstore: endpoint and bucket are required")
	}
	c, err := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: secure})
	if err != nil {
		return nil, fmt.Errorf("rawstore: client: %w", err)
	}
	ctx := context.Background()
	ok, err := c.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("rawstore: check bucket: %w", err)
	}
	if !ok {
		if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			// Another process may have created it in the meantime.
			if again, e := c.BucketExists(ctx, bucket); e != nil || !again {
				return nil, fmt.Errorf("rawstore: create bucket: %w", err)
			}
		}
	}
	return &s3Store{c: c, bucket: bucket, spoolDir: os.Getenv("RAW_SPOOL_DIR")}, nil
}

// Put spools r to a local file while hashing it, then writes the object in a
// single request that only succeeds if the key does not exist yet
// (If-None-Match: *), so concurrent writers can never overwrite each other.
func (s *s3Store) Put(ctx context.Context, key string, r io.Reader) (File, error) {
	if key == "" {
		return File{}, errors.New("rawstore: empty key")
	}
	spool, err := os.CreateTemp(s.spoolDir, SpoolPattern)
	if err != nil {
		return File{}, fmt.Errorf("rawstore: spool: %w", err)
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()
	h := sha256.New()
	sw := &recordingWriter{w: spool}
	size, err := io.Copy(io.MultiWriter(sw, h), r)
	if err != nil {
		if sw.err != nil {
			// The disk, not the source: spool full or unwritable.
			return File{}, fmt.Errorf("rawstore: write spool for %s: %w", key, err)
		}
		return File{}, fmt.Errorf("rawstore: read source for %s: %w", key, err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return File{}, fmt.Errorf("rawstore: spool: %w", err)
	}
	f := File{Key: key, SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}

	if size > partSize {
		err = s.putMultipart(ctx, key, spool, size, f.SHA256)
	} else {
		opts := minio.PutObjectOptions{UserMetadata: map[string]string{shaMeta: f.SHA256}}
		opts.SetMatchETagExcept("*")
		_, err = s.c.PutObject(ctx, s.bucket, key, spool, size, opts)
	}
	if err == nil {
		return f, nil
	}
	if minio.ToErrorResponse(err).Code != "PreconditionFailed" {
		return File{}, fmt.Errorf("rawstore: store %s: %w", key, err)
	}
	// The key already exists: fine only if it holds the same bytes.
	info, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return File{}, fmt.Errorf("rawstore: stat %s: %w", key, err)
	}
	have, err := s.storedSHA(ctx, key, info)
	if err != nil {
		return File{}, err
	}
	if have != f.SHA256 {
		return File{}, fmt.Errorf("%w: %s (have %s, got %s)", ErrConflict, key, have, f.SHA256)
	}
	return f, nil
}

// putMultipart uploads the spooled file in parts. minio-go's PutObject drops
// the If-None-Match precondition on the final CompleteMultipartUpload call
// (where S3 and MinIO evaluate it), so the low-level API is used to send it there.
func (s *s3Store) putMultipart(ctx context.Context, key string, spool *os.File, size int64, sha string) error {
	core := minio.Core{Client: s.c}
	uploadID, err := core.NewMultipartUpload(ctx, s.bucket, key, minio.PutObjectOptions{UserMetadata: map[string]string{shaMeta: sha}})
	if err != nil {
		return err
	}
	size1 := int64(partSize)
	if need := (size + maxParts - 1) / maxParts; need > size1 {
		size1 = need
	}
	var parts []minio.CompletePart
	for off, n := int64(0), 1; off < size; off, n = off+size1, n+1 {
		length := min(size1, size-off)
		p, err := core.PutObjectPart(ctx, s.bucket, key, uploadID, n, io.NewSectionReader(spool, off, length), length, minio.PutObjectPartOptions{})
		if err != nil {
			s.abort(key, uploadID)
			return err
		}
		parts = append(parts, minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag})
	}
	final := minio.PutObjectOptions{}
	final.SetMatchETagExcept("*")
	if _, err := core.CompleteMultipartUpload(ctx, s.bucket, key, uploadID, parts, final); err != nil {
		s.abort(key, uploadID)
		return err
	}
	return nil
}

// abortTimeout bounds the clean-up of a failed multipart upload, so a
// store that stopped answering cannot hang the caller.
const abortTimeout = 30 * time.Second

func (s *s3Store) abort(key, uploadID string) {
	// Own context: the caller's may be cancelled already.
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()
	_ = minio.Core{Client: s.c}.AbortMultipartUpload(ctx, s.bucket, key, uploadID)
}

// recordingWriter remembers the error of its writer, so Put can tell a
// failed spool write from a failed read of the source.
type recordingWriter struct {
	w   io.Writer
	err error
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	n, err := r.w.Write(p)
	if err != nil {
		r.err = err
	}
	return n, err
}

// SpoolPattern is the name pattern of Put's spool files.
const SpoolPattern = "rawstore-*"

// SweepSpool removes spool files of Put (SpoolPattern) left in dir (the OS
// temp dir when empty) by a process that died, i.e. not modified for at
// least olderThan: a live Put writes its file while downloading and uploads
// it right after. It returns how many files it removed; files it cannot
// remove are reported in the error and the others are still removed.
func SweepSpool(dir string, olderThan time.Duration) (int, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	matches, err := filepath.Glob(filepath.Join(dir, SpoolPattern))
	if err != nil {
		return 0, fmt.Errorf("rawstore: sweep spool: %w", err)
	}
	removed := 0
	var errs []error
	for _, m := range matches {
		info, err := os.Lstat(m)
		if err != nil || !info.Mode().IsRegular() || time.Since(info.ModTime()) < olderThan {
			continue
		}
		if err := os.Remove(m); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("rawstore: sweep spool: %w", errors.Join(errs...))
	}
	return removed, nil
}

// storedSHA returns the checksum recorded for key, computing it from the
// content when the object has none.
func (s *s3Store) storedSHA(ctx context.Context, key string, info minio.ObjectInfo) (string, error) {
	for k, v := range info.UserMetadata {
		if strings.EqualFold(k, shaMeta) && v != "" {
			return v, nil
		}
	}
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return "", fmt.Errorf("rawstore: read %s: %w", key, err)
	}
	defer obj.Close()
	h := sha256.New()
	if _, err := io.Copy(h, obj); err != nil {
		return "", fmt.Errorf("rawstore: hash %s: %w", key, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *s3Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("rawstore: open %s: %w", key, err)
	}
	// GetObject is lazy; surface a missing key now.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, fmt.Errorf("rawstore: open %s: %w", key, err)
	}
	return obj, nil
}

func (s *s3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("rawstore: stat %s: %w", key, err)
}

func (s *s3Store) Stat(ctx context.Context, key string) (File, bool, error) {
	info, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if isNotFound(err) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, fmt.Errorf("rawstore: stat %s: %w", key, err)
	}
	sha, err := s.storedSHA(ctx, key, info)
	if err != nil {
		return File{}, false, err
	}
	return File{Key: key, SHA256: sha, Size: info.Size}, true, nil
}

func isNotFound(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchKey"
}
