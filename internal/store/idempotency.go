package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// Idempotency sentinel errors. Surfaced by the middleware to map onto
// HTTP responses; the handler logic never sees them directly.
var (
	// ErrIdempotencyInFlight: same (key, endpoint) is currently being
	// processed by a concurrent request — the original hasn't yet
	// stored its response. The client should retry with backoff.
	ErrIdempotencyInFlight = errors.New("idempotency key in-flight")

	// ErrIdempotencyBodyMismatch: same key was used previously with a
	// different body. Reusing keys across different requests is a
	// programming error; we refuse rather than guess intent.
	ErrIdempotencyBodyMismatch = errors.New("idempotency key reused with a different request body")
)

// IdempotencyClaim attempts to register (key, endpoint, bodyHash) as
// the in-flight owner of this idempotency slot. The semantics:
//
//   - Returns (existing-row, nil) if a row already exists. The caller
//     inspects `ResponseComplete`:
//
//   - true  → replay the cached response, do not run the handler.
//
//   - false → another request is mid-flight, return 409 to the
//     client and let them retry.
//
//   - Returns (nil, nil) if the slot was successfully claimed. The caller
//     runs the handler, captures the response, then calls
//     IdempotencyComplete.
//
//   - Returns (nil, ErrIdempotencyBodyMismatch) if (key, endpoint)
//     already exists with a DIFFERENT bodyHash — the client reused the
//     key for a semantically different request, which is a bug.
//
// A row past its expires_at is claimed afresh, so the 24h TTL holds even
// when the periodic prune has not run yet.
//
// Concurrency safety: we use INSERT ... ON CONFLICT + a
// followup SELECT, both atomic at the SQL layer. A race between two
// concurrent claims of the same key is resolved by exactly ONE row's
// INSERT succeeding; the other sees the row via SELECT and returns
// in-flight.
func (s *Store) IdempotencyClaim(ctx context.Context, key, endpoint, bodyHash string) (*model.IdempotencyKey, error) {
	// Try to insert a fresh row.
	res, err := s.DB.NewRaw(`
		INSERT INTO idempotency_keys (key, endpoint, body_hash, response_status, response_body, response_complete, created_at, expires_at)
		VALUES (?, ?, ?, 0, '', false, now(), now() + interval '24 hours')
		ON CONFLICT (key, endpoint) DO UPDATE SET
			body_hash = EXCLUDED.body_hash, response_status = 0, response_body = '',
			response_complete = false, created_at = now(), expires_at = EXCLUDED.expires_at
		WHERE idempotency_keys.expires_at < now()
	`, key, endpoint, bodyHash).Exec(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := res.RowsAffected()
	if rows == 1 {
		// We claimed the slot. Caller runs the handler.
		return nil, nil
	}

	// Row already existed. Read it back.
	row := new(model.IdempotencyKey)
	err = s.DB.NewRaw(`
		SELECT key, endpoint, body_hash, response_status, response_body, response_complete, created_at, expires_at
		FROM idempotency_keys
		WHERE key = ? AND endpoint = ?
	`, key, endpoint).Scan(ctx, &row.Key, &row.Endpoint, &row.BodyHash, &row.ResponseStatus, &row.ResponseBody, &row.ResponseComplete, &row.CreatedAt, &row.ExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Extremely rare: the row vanished between INSERT failing
			// and SELECT — TTL cleanup ran in the gap. Treat as fresh.
			return nil, nil
		}
		return nil, err
	}
	if row.BodyHash != bodyHash {
		return nil, ErrIdempotencyBodyMismatch
	}
	return row, nil
}

// IdempotencyComplete writes the final response into the slot. Only the
// originator (the request that won IdempotencyClaim) should call this.
func (s *Store) IdempotencyComplete(ctx context.Context, key, endpoint string, status int, body string) error {
	// Cap body size: HTTP responses for SDK endpoints fit easily under
	// 64 KiB. Truncating defensively prevents a misbehaving handler
	// from filling the cache with megabytes.
	if len(body) > 65536 {
		body = body[:65536]
	}
	_, err := s.DB.NewRaw(`
		UPDATE idempotency_keys
		SET response_status = ?, response_body = ?, response_complete = true
		WHERE key = ? AND endpoint = ?
	`, status, body, key, endpoint).Exec(ctx)
	return err
}

// IdempotencyAbandon deletes a slot the originator failed to complete
// (panic, context cancel, server crash mid-handler). Called from a
// defer so the next retry can claim the slot fresh instead of being
// stuck in IN_FLIGHT until expiry.
func (s *Store) IdempotencyAbandon(ctx context.Context, key, endpoint string) {
	// Best effort: failures here are logged at the caller.
	_, _ = s.DB.NewRaw(`
		DELETE FROM idempotency_keys
		WHERE key = ? AND endpoint = ? AND response_complete = false
	`, key, endpoint).Exec(ctx)
}

// IdempotencyPruneExpired drops rows past their TTL. Safe to call on a
// timer; rowsAffected is informational only.
func (s *Store) IdempotencyPruneExpired(ctx context.Context) (int64, error) {
	res, err := s.DB.NewRaw(`
		DELETE FROM idempotency_keys WHERE expires_at < now()
	`).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ValidIdempotencyKey reports whether a header value is acceptable as
// an idempotency key. Reject empty, oversized, and control-char inputs
// so attackers can't poison the table by sending pathological strings.
func ValidIdempotencyKey(s string) bool {
	if len(s) < 1 || len(s) > 256 {
		return false
	}
	if strings.ContainsAny(s, "\x00\n\r") {
		return false
	}
	return true
}

// SecondsUntil returns seconds from now to t, capped non-negative.
// Used by the middleware to surface Retry-After hints.
func SecondsUntil(t time.Time) int {
	d := time.Until(t)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}
