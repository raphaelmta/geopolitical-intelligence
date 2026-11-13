package db

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The application roles created by migration 0021_roles.sql.
const (
	WorkerRole = "geoint_worker"
	APIRole    = "geoint_api"
)

// MinPasswordLength is the shortest role password SetRolePasswords accepts.
const MinPasswordLength = 16

// scramIterations is PostgreSQL's default scram_iterations.
const scramIterations = 4096

// SetRolePasswords gives each role of passwords LOGIN, the safe attributes
// (no superuser, CREATEDB, CREATEROLE, replication or RLS bypass) and its
// password. It runs at every `geoint migrate`, which is also how passwords
// are rotated.
//
// The password never reaches the server, its logs or pg_stat_activity in
// clear text: the SCRAM-SHA-256 verifier is computed here and stored as is
// (PostgreSQL keeps a verifier given in that form). ALTER ROLE takes no bind
// parameters, so the statement is built by the server with format('%I',
// '%L') from parameters; nothing from the environment is concatenated into
// SQL. Passwords must be printable ASCII (SCRAM's SASLprep leaves those
// unchanged, so the verifier matches what a client sends).
func SetRolePasswords(ctx context.Context, pool *pgxpool.Pool, passwords map[string]string) error {
	for role, pw := range passwords {
		if err := checkPassword(pw); err != nil {
			return fmt.Errorf("db: password of %s: %w", role, err)
		}
		verifier, err := scramVerifier(pw)
		if err != nil {
			return fmt.Errorf("db: password of %s: %w", role, err)
		}
		var stmt string
		if err := pool.QueryRow(ctx,
			`SELECT format('ALTER ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L', $1::text, $2::text)`,
			role, verifier).Scan(&stmt); err != nil {
			return fmt.Errorf("db: password of %s: %w", role, err)
		}
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("db: set password of %s: %w", role, err)
		}
	}
	return nil
}

func checkPassword(pw string) error {
	if len(pw) < MinPasswordLength {
		return fmt.Errorf("shorter than %d characters", MinPasswordLength)
	}
	for i := 0; i < len(pw); i++ {
		if pw[i] < 0x21 || pw[i] > 0x7e {
			return errors.New("only printable ASCII characters (no spaces) are accepted")
		}
	}
	return nil
}

// scramVerifier returns the SCRAM-SHA-256 verifier of password in the form
// PostgreSQL stores (RFC 5802/7677):
// SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return scramVerifierWithSalt(password, salt, scramIterations)
}

func scramVerifierWithSalt(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
