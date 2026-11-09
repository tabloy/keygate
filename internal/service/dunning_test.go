package service

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// dunningFixture is a subscription product whose past-due licences the
// dunning ladder walks, with the mail going to smtpAddr.
type dunningFixture struct {
	t      *testing.T
	s      *store.Store
	ctx    context.Context
	prod   *model.Product
	plan   *model.Plan
	suffix string
	n      int
}

func newDunningFixture(t *testing.T) *dunningFixture {
	t.Helper()
	s := openNotifyStore(t)
	ctx := context.Background()
	f := &dunningFixture{t: t, s: s, ctx: ctx, suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")}
	f.prod = &model.Product{Name: "Dunning App", Slug: "dunning-" + f.suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, f.prod); err != nil {
		t.Fatal(err)
	}
	f.plan = &model.Plan{ProductID: f.prod.ID, Name: "P", Slug: "dunning-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, f.plan); err != nil {
		t.Fatal(err)
	}
	return f
}

// pastDue is a licence that has been past due for days. It is expired
// when the test ends, so later dunning passes in the suite leave it be.
func (f *dunningFixture) pastDue(days int) *model.License {
	f.t.Helper()
	f.n++
	since := time.Now().Add(-time.Duration(days)*24*time.Hour - time.Hour)
	email := "dunning-" + f.suffix + "-" + strconv.Itoa(f.n) + "@example.com"
	lic := &model.License{ProductID: f.prod.ID, PlanID: f.plan.ID, Email: email,
		LicenseKey: "KEY-dunning-" + f.suffix + "-" + strconv.Itoa(f.n), Status: model.StatusPastDue, PastDueAt: &since}
	if err := f.s.CreateLicense(f.ctx, lic); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = f.s.DB.NewRaw("UPDATE licenses SET status = 'expired' WHERE id = ?", lic.ID).Exec(ctx)
		_, _ = f.s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", email).Exec(ctx)
	})
	return lic
}

func (f *dunningFixture) checker(smtpAddr string) *ExpiryChecker {
	host, port, _ := net.SplitHostPort(smtpAddr)
	return NewExpiryChecker(f.s, NewEmailService(host, port, "", "", "keygate@test.local", slog.Default(), f.s), nil, slog.Default())
}

// queued is the subjects of the mails waiting in the queue for to.
func (f *dunningFixture) queued(to string) []string {
	f.t.Helper()
	var subjects []string
	if err := f.s.DB.NewRaw("SELECT subject FROM email_queue WHERE to_addr = ? ORDER BY created_at", to).Scan(f.ctx, &subjects); err != nil {
		f.t.Fatal(err)
	}
	return subjects
}

// steps is the dunning tags recorded for lic, and whether each was sent.
func (f *dunningFixture) steps(lic *model.License) map[string]bool {
	f.t.Helper()
	var rows []struct {
		Tag  string `bun:"tag"`
		Sent bool   `bun:"sent"`
	}
	if err := f.s.DB.NewRaw("SELECT tag, sent_at IS NOT NULL AS sent FROM notifications WHERE license_id = ? AND tag LIKE 'dunning_%'",
		lic.ID).Scan(f.ctx, &rows); err != nil {
		f.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[strings.SplitN(r.Tag, ":", 2)[0]] = r.Sent
	}
	return out
}

// refusingSMTP is an address nothing listens on.
func refusingSMTP(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// Every replica runs the expiry loop. Two passes over the same
// past-due licences at the same moment must tell each customer once:
// a check-then-record lets both see the step as unsent and both mail
// it.
func TestDunningSendsOnceAcrossReplicas(t *testing.T) {
	f := newDunningFixture(t)
	mail := newMailCatcher(t)
	var lics []*model.License
	for range 10 {
		lics = append(lics, f.pastDue(15))
	}
	replicas := []*ExpiryChecker{f.checker(mail.addr), f.checker(mail.addr)}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c.SendPaymentFailureReminders(f.ctx)
		}()
	}
	close(start)
	wg.Wait()
	// A mail sent straight to SMTP goes out from a goroutine: give a
	// duplicate the time to arrive.
	time.Sleep(500 * time.Millisecond)

	for _, lic := range lics {
		// Sent directly or queued for sending: either way it reaches
		// the customer, and it must do so once.
		told := len(mail.to(lic.Email, 0)) + len(f.queued(lic.Email))
		if told != 1 {
			t.Errorf("%s was told %d times about one past-due step, want once", lic.Email, told)
		}
	}
}

// The step is queued, not sent from inside the expiry loop, and its
// claim is closed in the same transaction. An SMTP server that is down
// must neither hold the loop nor lose the notice: the queue retries it.
func TestDunningQueuesAndClosesTheClaim(t *testing.T) {
	f := newDunningFixture(t)
	lic := f.pastDue(15)
	c := f.checker(refusingSMTP(t))

	begun := time.Now()
	c.SendPaymentFailureReminders(f.ctx)
	if took := time.Since(begun); took > 5*time.Second {
		t.Fatalf("the dunning pass waited on SMTP: %s", took)
	}
	got := f.queued(lic.Email)
	if len(got) != 1 || !strings.Contains(got[0], "final payment notice") {
		t.Fatalf("queued %q, want the final notice once", got)
	}
	if steps := f.steps(lic); len(steps) != 1 || !steps["dunning_final"] {
		t.Fatalf("recorded steps %v, want dunning_final closed", steps)
	}
	c.SendPaymentFailureReminders(f.ctx)
	if got := f.queued(lic.Email); len(got) != 1 {
		t.Fatalf("a second pass queued the step again: %q", got)
	}
}

// Only the highest step reached is sent, once; a step already sent
// moves the ladder on rather than sending a lower one.
func TestDunningLadder(t *testing.T) {
	f := newDunningFixture(t)
	c := f.checker(refusingSMTP(t))

	week := f.pastDue(8)
	fortnight := f.pastDue(15)
	tag := "dunning_second:" + strconv.FormatInt(fortnight.PastDueAt.Unix(), 10)
	if _, err := f.s.DB.NewRaw("INSERT INTO notifications (id, license_id, tag) VALUES (?, ?, ?)",
		"dn-"+f.suffix, fortnight.ID, tag).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	c.SendPaymentFailureReminders(f.ctx)

	if got := f.queued(week.Email); len(got) != 1 || !strings.Contains(got[0], "payment still outstanding") {
		t.Fatalf("8 days past due: queued %q, want the second notice", got)
	}
	if steps := f.steps(week); len(steps) != 1 || !steps["dunning_second"] {
		t.Fatalf("8 days past due: recorded %v, want dunning_second alone", steps)
	}
	if got := f.queued(fortnight.Email); len(got) != 1 || !strings.Contains(got[0], "final payment notice") {
		t.Fatalf("15 days past due, second notice sent: queued %q, want the final notice", got)
	}
}

// A step an older version recorded — a row inserted already sent,
// before steps were claimed — is not sent again after the upgrade.
func TestDunningStepRecordedByAnOlderVersionIsNotResent(t *testing.T) {
	f := newDunningFixture(t)
	lic := f.pastDue(2)
	tag := "dunning_first:" + strconv.FormatInt(lic.PastDueAt.Unix(), 10)
	if _, err := f.s.DB.NewRaw("INSERT INTO notifications (id, license_id, tag) VALUES (?, ?, ?)",
		"do-"+f.suffix, lic.ID, tag).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.checker(refusingSMTP(t)).SendPaymentFailureReminders(f.ctx)
	if got := f.queued(lic.Email); len(got) != 0 {
		t.Fatalf("a step sent before the upgrade was queued again: %q", got)
	}
}
