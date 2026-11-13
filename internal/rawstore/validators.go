package rawstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Validators are what an HTTP server says about a file's identity (from a
// HEAD): its ETag, its Last-Modified date and its size. Connectors whose
// sources republish the same file names (TSE, CVM) key a raw file by them,
// so a file the server did not change is neither downloaded nor stored
// again, and a changed one gets a new key (the old object stays: raw files
// are never deleted). The risk is that of any HTTP cache: a file changed in
// place with identical validators is missed.
type Validators struct {
	ETag         string
	LastModified string
	Size         int64
}

// Known reports whether the server sent an ETag or a Last-Modified date. The
// size alone is not an identity: without either, callers keep one copy per
// collection.
func (v Validators) Known() bool {
	return strings.TrimSpace(v.ETag) != "" || strings.TrimSpace(v.LastModified) != ""
}

// ID is a short, stable name for the validators: the first 16 hex digits of
// the SHA-256 of ETag, Last-Modified and size (one per line).
func (v Validators) ID() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d", strings.TrimSpace(v.ETag), strings.TrimSpace(v.LastModified), v.Size)))
	return hex.EncodeToString(sum[:8])
}
