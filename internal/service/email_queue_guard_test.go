package service

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/store"
)

// A server with no SMTP configured must leave the queue alone.
//
// Send returns nil for a server it cannot reach, so a worker that
// drains the queue anyway claims each mail, skips it, and marks it
// delivered. Running without SMTP is a supported mode, which makes
// this the install where a license key queued by Stripe fulfilment
// disappears without anyone seeing an error. The backlog has to
// survive until there is something to send it with.
func TestQueueIsNotDrainedWithoutSMTP(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	db, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	to := "queue-guard-" + time.Now().Format("150405.000000") + "@example.com"
	body := `<html><body>KG-GUARDKEY-GUARDKEY-GUARDKEY-GUARD</body></html>`
	if err := db.EnqueueEmail(ctx, to, "Your license", body); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	defer func() {
		_, _ = db.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", to).Exec(ctx)
	}()

	// host and from empty is what "SMTP not configured" means.
	disabled := NewEmailService("", "", "", "", "", slog.New(slog.NewTextHandler(os.Stderr, nil)), db)
	if disabled.IsConfigured() {
		t.Fatal("a service built with no host or from reports itself configured")
	}
	disabled.processQueue(ctx, db)

	var status, stored string
	if err := db.DB.NewRaw(
		"SELECT status, body FROM email_queue WHERE to_addr = ?", to,
	).Scan(ctx, &status, &stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q after a pass with no SMTP, want pending", status)
	}
	if stored != body {
		t.Errorf("the mail lost its body while there was nothing to send it with: %q", stored)
	}
}
