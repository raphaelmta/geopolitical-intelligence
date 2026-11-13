package claims

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryableOnlyForSerializationFailureAndDeadlock(t *testing.T) {
	for code, want := range map[string]bool{"40001": true, "40P01": true, "23505": false, "57014": false} {
		err := fmt.Errorf("count relationship: %w", &pgconn.PgError{Code: code})
		if got := retryable(err); got != want {
			t.Errorf("code %s: retryable = %v, want %v", code, got, want)
		}
	}
	if retryable(errors.New("plain")) || retryable(ErrShrunkCollection) {
		t.Error("non-database error is retryable")
	}
}
