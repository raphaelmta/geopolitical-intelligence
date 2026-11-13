// Package audit appends to the audit trail (table audit_log), which the
// database keeps append-only: rows can be neither updated nor deleted.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxDetailBytes bounds the JSON encoding of a detail map.
const MaxDetailBytes = 16 * 1024

// Log records that user (the OIDC subject) performed action, optionally on
// entityID, with structured detail (nil means {}).
//
// detail is stored verbatim and kept forever: callers must never put access
// tokens, credentials or free-text personal data in it (names, search strings
// typed by the user, CPF fragments). Use ids, enumerated values and counts.
func Log(ctx context.Context, p *pgxpool.Pool, user, action string, entityID *int64, detail map[string]any) error {
	// Blank counts as empty, as in the table checks (which trim ASCII
	// whitespace; TrimSpace is a superset).
	if strings.TrimSpace(user) == "" {
		return errors.New("audit: user is required")
	}
	if strings.TrimSpace(action) == "" {
		return errors.New("audit: action is required")
	}
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("audit: encode detail: %w", err)
	}
	if len(b) > MaxDetailBytes {
		return fmt.Errorf("audit: detail is %d bytes, limit is %d", len(b), MaxDetailBytes)
	}
	if _, err := p.Exec(ctx,
		`INSERT INTO audit_log (user_sub, action, entity_id, detail) VALUES ($1, $2, $3, $4::jsonb)`,
		user, action, entityID, string(b),
	); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}
