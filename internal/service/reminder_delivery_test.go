package service

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

func unconfiguredChecker(f *dunningFixture) *ExpiryChecker {
	return NewExpiryChecker(f.s, NewEmailService("", "", "", "", "", slog.Default(), f.s), nil, slog.Default())
}

func (f *dunningFixture) queueStatus(to string) []string {
	f.t.Helper()
	var statuses []string
	if err := f.s.DB.NewRaw("SELECT status FROM email_queue WHERE to_addr = ? ORDER BY created_at", to).Scan(f.ctx, &statuses); err != nil {
		f.t.Fatal(err)
	}
	return statuses
}

// Running without email is a supported mode, and the queue only drains
// once email is set up. A reminder queued meanwhile would go out months
// late: the whole ladder at once, to customers who have long since paid.
// Nothing is claimed either, so setting email up later sends the step the
// license is on now, once.
func TestDunningWaitsForEmailToBeConfigured(t *testing.T) {
	f := newDunningFixture(t)
	lic := f.pastDue(15)

	unconfiguredChecker(f).SendPaymentFailureReminders(f.ctx)
	if got := f.queued(lic.Email); len(got) != 0 {
		t.Fatalf("no email configured: queued %q, want nothing", got)
	}
	if steps := f.steps(lic); len(steps) != 0 {
		t.Fatalf("no email configured: recorded %v, want nothing", steps)
	}

	f.checker(refusingSMTP(t)).SendPaymentFailureReminders(f.ctx)
	if got := f.queued(lic.Email); len(got) != 1 || !strings.Contains(got[0], "final payment notice") {
		t.Fatalf("email configured later: queued %q, want the final notice once", got)
	}
}

func TestExpiryReminderWaitsForEmailToBeConfigured(t *testing.T) {
	f := newReminderFixture(t)
	lic := f.license(36 * time.Hour)
	c := NewExpiryChecker(f.s, NewEmailService("", "", "", "", "", slog.Default(), f.s), nil, slog.Default())

	c.SendExpiryReminders(context.Background())

	var n int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM email_queue WHERE to_addr = ?", lic.Email).Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no email configured: %d reminders queued, want none", n)
	}
	if err := f.s.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ?", lic.ID).Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no email configured: %d reminders recorded, want none", n)
	}
}

// A dunning mail can wait in the queue for days when the mail server is
// down. By the time it can go out the customer may have paid, and must not
// be told their access is about to be suspended.
func TestDunningMailIsDroppedOnceThePaymentRecovers(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	lic := f.pastDue(15)
	c := f.checker(mail.addr)
	c.SendPaymentFailureReminders(f.ctx)
	if got := f.queued(lic.Email); len(got) != 1 {
		t.Fatalf("queued %q, want one", got)
	}

	if _, err := f.s.DB.NewRaw("UPDATE licenses SET status = 'active', past_due_at = NULL WHERE id = ?", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	c.email.processQueue(f.ctx, f.s)

	if got := mail.to(lic.Email, 0); len(got) != 0 {
		t.Fatalf("customer has paid but was sent %q", got)
	}
	if got := f.queueStatus(lic.Email); len(got) != 1 || got[0] != "failed" {
		t.Fatalf("queue status %v, want the mail closed as failed", got)
	}
}

// The mail belongs to one past-due episode: a later episode has its own
// ladder and its own mail.
func TestDunningMailIsDroppedWhenAnotherEpisodeHasBegun(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	lic := f.pastDue(15)
	c := f.checker(mail.addr)
	c.SendPaymentFailureReminders(f.ctx)

	if _, err := f.s.DB.NewRaw("UPDATE licenses SET past_due_at = now() WHERE id = ?", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	c.email.processQueue(f.ctx, f.s)

	if got := mail.to(lic.Email, 0); len(got) != 0 {
		t.Fatalf("mail of an earlier episode was sent: %q", got)
	}
}

func TestDunningMailIsSentWhileStillPastDue(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	lic := f.pastDue(15)
	c := f.checker(mail.addr)
	c.SendPaymentFailureReminders(f.ctx)
	c.email.processQueue(f.ctx, f.s)

	if got := mail.to(lic.Email, 1); len(got) != 1 {
		t.Fatalf("delivered %q, want the notice once", got)
	}
	if got := f.queueStatus(lic.Email); len(got) != 1 || got[0] != "sent" {
		t.Fatalf("queue status %v, want sent", got)
	}
}

// Only dunning depends on the license still being past due.
func TestOtherReminderMailIsNotDroppedByTheDunningCheck(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	lic := f.pastDue(15)
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET status = ? WHERE id = ?", model.StatusActive, lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	token, err := f.s.ClaimNotification(f.ctx, lic.ID, "expiry_test:1")
	if err != nil || token == "" {
		t.Fatalf("claim: %q %v", token, err)
	}
	if err := f.s.EnqueueEmailAndCloseNotification(f.ctx, lic.Email, "your license expires", "body", token); err != nil {
		t.Fatal(err)
	}
	f.checker(mail.addr).email.processQueue(f.ctx, f.s)

	if got := mail.to(lic.Email, 1); len(got) != 1 {
		t.Fatalf("delivered %q, want the reminder once", got)
	}
}

// greylistingSMTP answers 452 to every recipient, as a full mailbox does.
func greylistingSMTP(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
				say("220 full")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
					case strings.HasPrefix(cmd, "RCPT TO:"):
						say("452 4.2.2 mailbox full")
					case cmd == "QUIT":
						say("221 bye")
						return
					default:
						say("250 ok")
					}
				}
			}()
		}
	}()
	return l.Addr().String()
}

// A recipient the queue has given up on must not start over at the next
// hourly sweep: that is five delivery attempts an hour until the license
// expires. The step is still tried again later, so a temporary problem
// does not lose the notice.
func TestGivenUpReminderIsHeldBeforeItIsQueuedAgain(t *testing.T) {
	f := newDunningFixture(t)
	lic := f.pastDue(15)
	c := f.checker(greylistingSMTP(t))
	c.SendPaymentFailureReminders(f.ctx)

	for range 5 {
		if _, err := f.s.DB.NewRaw("UPDATE email_queue SET next_retry = NULL WHERE to_addr = ? AND status = 'pending'", lic.Email).Exec(f.ctx); err != nil {
			t.Fatal(err)
		}
		c.email.processQueue(f.ctx, f.s)
	}
	if got := f.queueStatus(lic.Email); len(got) != 1 || got[0] != "failed" {
		t.Fatalf("queue status %v, want the mail given up on", got)
	}

	c.SendPaymentFailureReminders(f.ctx)
	if got := f.queueStatus(lic.Email); len(got) != 1 {
		t.Fatalf("the next sweep queued it again at once: %v", got)
	}

	if _, err := f.s.DB.NewRaw("UPDATE notifications SET claimed_at = now() - interval '11 minutes' WHERE license_id = ? AND sent_at IS NULL", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	c.SendPaymentFailureReminders(f.ctx)
	if got := f.queueStatus(lic.Email); len(got) != 2 || got[1] != "pending" {
		t.Fatalf("after the hold: queue status %v, want it queued again", got)
	}
}

// If the check cannot run, nobody knows whether the customer has paid, so
// the notice waits for the claim lease to lapse and is looked at again.
func TestDunningMailWaitsWhenItsCheckFails(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	lic := f.pastDue(15)
	c := f.checker(mail.addr)
	c.SendPaymentFailureReminders(f.ctx)

	// The check reads notifications; claiming the mail does not.
	if _, err := f.s.DB.NewRaw("ALTER TABLE notifications RENAME TO notifications_held").Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if _, err := f.s.DB.NewRaw("ALTER TABLE IF EXISTS notifications_held RENAME TO notifications").Exec(context.Background()); err != nil {
			t.Errorf("restore notifications: %v", err)
		}
	}
	t.Cleanup(restore)
	c.email.processQueue(f.ctx, f.s)
	restore()

	if got := mail.to(lic.Email, 0); len(got) != 0 {
		t.Fatalf("the check failed but the notice was sent: %q", got)
	}
	if got := f.queueStatus(lic.Email); len(got) != 1 || got[0] != "pending" {
		t.Fatalf("queue status %v, want the notice still pending", got)
	}

	if _, err := f.s.DB.NewRaw("UPDATE email_queue SET next_retry = NULL WHERE to_addr = ?", lic.Email).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	c.email.processQueue(f.ctx, f.s)
	if got := mail.to(lic.Email, 1); len(got) != 1 {
		t.Fatalf("after the check works again: delivered %q, want the notice once", got)
	}
}

// Mail held back by bad credentials stays queued while the ladder moves
// on. Once it can go out the customer gets the notice they are on now, not
// every notice they missed.
func TestLowerDunningStepIsDroppedOnceAHigherOneExists(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	lic := f.pastDue(2)
	c := f.checker(mail.addr)
	c.SendPaymentFailureReminders(f.ctx)

	tag := "dunning_final:" + strconv.FormatInt(lic.PastDueAt.Unix(), 10)
	token, err := f.s.ClaimNotification(f.ctx, lic.ID, tag)
	if err != nil || token == "" {
		t.Fatalf("claim: %q %v", token, err)
	}
	if err := f.s.EnqueueEmailAndCloseNotification(f.ctx, lic.Email, "final payment notice", "body", token); err != nil {
		t.Fatal(err)
	}
	c.email.processQueue(f.ctx, f.s)

	if got := mail.to(lic.Email, 1); len(got) != 1 {
		t.Fatalf("delivered %q, want only the final notice", got)
	}
	if got := f.queueStatus(lic.Email); len(got) != 2 || got[0] != "failed" || got[1] != "sent" {
		t.Fatalf("queue status %v, want [failed sent]", got)
	}
}

// A higher step that is only claimed may still fail to be queued. Dropping
// the lower notice then would leave the customer with nothing to receive.
func TestLowerDunningStepIsKeptWhileAHigherOneIsOnlyClaimed(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	lic := f.pastDue(2)
	c := f.checker(mail.addr)
	c.SendPaymentFailureReminders(f.ctx)

	tag := "dunning_final:" + strconv.FormatInt(lic.PastDueAt.Unix(), 10)
	token, err := f.s.ClaimNotification(f.ctx, lic.ID, tag)
	if err != nil || token == "" {
		t.Fatalf("claim: %q %v", token, err)
	}
	c.email.processQueue(f.ctx, f.s)

	if got := mail.to(lic.Email, 1); len(got) != 1 {
		t.Fatalf("delivered %q, want the lower notice", got)
	}
	if got := f.queueStatus(lic.Email); len(got) != 1 || got[0] != "sent" {
		t.Fatalf("queue status %v, want [sent]", got)
	}
}

// A higher step of an earlier episode says nothing about this one.
func TestDunningStepOfAnotherEpisodeDoesNotDropThisOne(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	lic := f.pastDue(2)
	c := f.checker(mail.addr)
	if _, err := f.s.DB.NewRaw("INSERT INTO notifications (id, license_id, tag) VALUES (?, ?, 'dunning_final:1')",
		"de-"+f.suffix, lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	c.SendPaymentFailureReminders(f.ctx)
	c.email.processQueue(f.ctx, f.s)

	if got := mail.to(lic.Email, 1); len(got) != 1 {
		t.Fatalf("delivered %q, want this episode's notice", got)
	}
}
