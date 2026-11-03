package store_test

import (
	"context"
	"testing"
	"time"
)

// docs/api-contract.md and openapi.yaml: "Keys live 24h." The row's
// expires_at is only enforced by an hourly prune (whose ticker restarts
// with every process start), so a key whose 24 hours are over is still
// replayed, with its stale response, until the prune happens to run.
func TestIdempotencyClaim_ExpiredKeyIsNotReplayed(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	key := "expired-claim-" + time.Now().Format("150405.000000")
	const endpoint = "/api/v1/license/activate"
	const bodyHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	defer func() {
		_, _ = s.DB.NewRaw(`DELETE FROM idempotency_keys WHERE key = ?`, key).Exec(ctx)
	}()

	if row, err := s.IdempotencyClaim(ctx, key, endpoint, bodyHash); err != nil || row != nil {
		t.Fatalf("first claim = %v, %v; want nil, nil", row, err)
	}
	if err := s.IdempotencyComplete(ctx, key, endpoint, 200, `{"success":true,"data":{"token":"old"}}`); err != nil {
		t.Fatal(err)
	}
	// 25 hours later, the prune has not run yet.
	if _, err := s.DB.NewRaw(`UPDATE idempotency_keys SET created_at = now() - interval '25 hours',
		expires_at = now() - interval '1 hour' WHERE key = ? AND endpoint = ?`, key, endpoint).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	row, err := s.IdempotencyClaim(ctx, key, endpoint, bodyHash)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Errorf("a key 25h old was replayed (status %d, body %q); keys live 24h, so it should be claimed afresh", row.ResponseStatus, row.ResponseBody)
	}
}
