package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/store"
)

// claimOwn takes the claim on this test's own mail.
//
// ClaimNextEmail hands back whichever mail is next in the queue, which
// in a package-wide run is very likely somebody else's: the reminder
// tests queue mail too. What is under test here is what the two
// finishing writes do to the row, so the claim is placed directly on
// the row this test queued.
func claimOwn(t *testing.T, s *store.Store, ctx context.Context, to string) (id, token string) {
	t.Helper()
	token = "test-claim-" + to
	if err := s.DB.NewRaw(
		"UPDATE email_queue SET claim_token = ? WHERE to_addr = ? RETURNING id", token, to,
	).Scan(ctx, &id); err != nil {
		t.Fatalf("claim own mail: %v", err)
	}
	return id, token
}

// A delivered mail must not keep its text.
//
// These bodies carry license keys: the fulfilment mail, the
// maintenance reminder, an admin resend. The queue is a delivery
// buffer, so once a mail is out the text has no further use, and
// keeping it would leave a credential in the database for as long as
// the row lives. Nothing prunes the table, so that is forever, and it
// outlives the license itself.
func TestMarkEmailSentClearsTheBody(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	to := "body-sent-" + time.Now().Format("150405.000000") + "@example.com"
	body := `<html><body>KG-AAAABBBB-CCCCDDDD-EEEEFFFF-GGGGHHHH</body></html>`
	if err := s.EnqueueEmail(ctx, to, "Your license", body); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	defer func() {
		_, _ = s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", to).Exec(ctx)
	}()

	var queued string
	if err := s.DB.NewRaw("SELECT body FROM email_queue WHERE to_addr = ?", to).Scan(ctx, &queued); err != nil {
		t.Fatalf("read queued body: %v", err)
	}
	if queued != body {
		t.Fatalf("the queue holds %q, want the body to send", queued)
	}
	id, token := claimOwn(t, s, ctx, to)
	s.MarkEmailSent(ctx, id, token)

	var stored, status, subject, addr string
	if err := s.DB.NewRaw(
		"SELECT body, status, subject, to_addr FROM email_queue WHERE id = ?", id,
	).Scan(ctx, &stored, &status, &subject, &addr); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "sent" {
		t.Errorf("status = %q, want sent", status)
	}
	if stored != "" {
		t.Errorf("a delivered mail still holds its body: %q", stored)
	}
	// What an operator needs afterwards stays.
	if subject == "" || addr != to {
		t.Errorf("subject/recipient were lost: subject=%q to=%q", subject, addr)
	}
}

// While a mail can still be retried its body has to survive, because
// the retry sends this same text.
func TestFailedEmailKeepsItsBodyWhileRetriesRemain(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	to := "body-retry-" + time.Now().Format("150405.000000") + "@example.com"
	body := `<html><body>KG-RETRYKEY-RETRYKEY-RETRYKEY-RETRYKEY</body></html>`
	if err := s.EnqueueEmail(ctx, to, "Your license", body); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	defer func() {
		_, _ = s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", to).Exec(ctx)
	}()

	id, token := claimOwn(t, s, ctx, to)
	s.MarkEmailFailed(ctx, id, token, "connection refused")

	var stored, status string
	var attempts, maxAttempts int
	if err := s.DB.NewRaw(
		"SELECT body, status, attempts, max_attempts FROM email_queue WHERE id = ?", id,
	).Scan(ctx, &stored, &status, &attempts, &maxAttempts); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q after one failure of %d, want pending", status, maxAttempts)
	}
	if stored != body {
		t.Errorf("a mail that will be retried lost its body: %q", stored)
	}
}

// Once it is given up on, nobody will send it again, so the key goes.
func TestGivenUpEmailDropsItsBody(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	to := "body-failed-" + time.Now().Format("150405.000000") + "@example.com"
	body := `<html><body>KG-DEADDEAD-DEADDEAD-DEADDEAD-DEADDEAD</body></html>`
	if err := s.EnqueueEmail(ctx, to, "Your license", body); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	defer func() {
		_, _ = s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", to).Exec(ctx)
	}()

	// Put it on its last attempt rather than looping through the
	// backoff, which is minutes long by design.
	if _, err := s.DB.NewRaw(
		"UPDATE email_queue SET attempts = max_attempts - 1 WHERE to_addr = ?", to,
	).Exec(ctx); err != nil {
		t.Fatalf("set up last attempt: %v", err)
	}
	id, token := claimOwn(t, s, ctx, to)
	s.MarkEmailFailed(ctx, id, token, "mailbox unavailable")

	var stored, status, errMsg string
	if err := s.DB.NewRaw(
		"SELECT body, status, error FROM email_queue WHERE id = ?", id,
	).Scan(ctx, &stored, &status, &errMsg); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if stored != "" {
		t.Errorf("a mail nobody will send again still holds its body: %q", stored)
	}
	if errMsg == "" {
		t.Error("the reason it failed was dropped along with the body")
	}
}
