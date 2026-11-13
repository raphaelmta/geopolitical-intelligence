package collection

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrFileConflict: the collection already recorded a file of that name with
// a different checksum.
var ErrFileConflict = errors.New("collection: file already recorded with a different checksum")

// ErrSourceURL: a file's source URL is missing or is not an absolute http(s) URL.
var ErrSourceURL = errors.New("collection: a raw file needs the absolute http(s) URL it was downloaded from")

// File is one raw file a collection loaded.
type File struct {
	// Name is the file part of its claims' record_ref.
	Name string
	// RawKey is where the preserved object is in the raw store.
	RawKey string
	// SHA256 is its checksum, 64 lowercase hex characters.
	SHA256 string
	Size   int64
	// SourceURL is the URL the file was downloaded from (for an API source,
	// the request URL). It is stored through SafeURL: never with credentials.
	SourceURL string
}

// RecordSourceFile records that the collection loaded f. The source URL is
// required and stored as SafeURL returns it. Recording the same file again is
// a no-op (the first row, URL included, is kept); the same name with another
// checksum is ErrFileConflict. Rows are never changed or deleted.
func RecordSourceFile(ctx context.Context, q Querier, collectionID int64, f File) error {
	u, err := SafeURL(f.SourceURL)
	if err != nil {
		return fmt.Errorf("collection: record file %s: %w", f.Name, err)
	}
	return record(ctx, q, collectionID, f.Name, f.RawKey, f.SHA256, f.Size, u)
}

func record(ctx context.Context, q Querier, collectionID int64, name, rawKey, sha256 string, size int64, sourceURL any) error {
	tag, err := q.Exec(ctx, `INSERT INTO collection_file (collection_id, name, raw_key, sha256, size_bytes, source_url)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (collection_id, name) DO NOTHING`,
		collectionID, name, rawKey, sha256, size, sourceURL)
	if err != nil {
		return fmt.Errorf("collection: record file %s: %w", name, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var have string
	if err := q.QueryRow(ctx, `SELECT sha256 FROM collection_file WHERE collection_id = $1 AND name = $2`,
		collectionID, name).Scan(&have); err != nil {
		return fmt.Errorf("collection: record file %s: %w", name, err)
	}
	if have != sha256 {
		return fmt.Errorf("%w: collection %d, %s (have %s, got %s)", ErrFileConflict, collectionID, name, have, sha256)
	}
	return nil
}

// secretParams are query parameter names (lower case) that may carry a
// credential; SafeURL drops them. Names starting with "x-amz-" (signed S3
// URLs) are dropped too.
var secretParams = map[string]bool{
	"token": true, "access_token": true, "auth": true, "authorization": true, "apikey": true, "api_key": true,
	"key": true, "secret": true, "password": true, "passwd": true, "pass": true, "pwd": true,
	"signature": true, "sig": true, "credential": true, "credentials": true, "session": true, "sessionid": true,
}

// SafeURL returns raw as an absolute http(s) URL fit to be stored and shown:
// without user information (a user, a password or a share token sent as
// Basic auth), without query parameters that may carry a credential
// (secretParams, x-amz-*) and without a fragment. Anything else is kept as
// is. A URL that is not absolute http(s) is ErrSourceURL.
func SafeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrSourceURL
	}
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	if u.RawQuery != "" {
		// Parameters are kept in their order; only secret ones are removed.
		var kept []string
		for _, kv := range strings.Split(u.RawQuery, "&") {
			k, _, _ := strings.Cut(kv, "=")
			if name, err := url.QueryUnescape(k); err == nil {
				k = name
			}
			lk := strings.ToLower(k)
			if kv == "" || secretParams[lk] || strings.HasPrefix(lk, "x-amz-") {
				continue
			}
			kept = append(kept, kv)
		}
		u.RawQuery = strings.Join(kept, "&")
	}
	u.ForceQuery = false
	return u.String(), nil
}
