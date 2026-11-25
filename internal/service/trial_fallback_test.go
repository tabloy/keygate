package service

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// mailCatcher is an SMTP server that accepts every message and keeps
// its recipient and subject, so the tests can see which email a sweep
// sent. It speaks just enough SMTP for EmailService: no STARTTLS, no
// AUTH.
type mailCatcher struct {
	addr string
	mu   sync.Mutex
	got  []caughtMail
}

type caughtMail struct{ to, subject, body string }

func newMailCatcher(t *testing.T) *mailCatcher {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mailCatcher{addr: l.Addr().String()}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go m.serve(conn)
		}
	}()
	return m
}

func (m *mailCatcher) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	say("220 catcher")
	var mail caughtMail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 catcher")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			mail.to = strings.Trim(strings.TrimSpace(line)[len("RCPT TO:"):], "<> ")
			say("250 ok")
		case cmd == "DATA":
			say("354 go on")
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				if s, ok := strings.CutPrefix(l, "Subject: "); ok && mail.subject == "" {
					mail.subject = strings.TrimSpace(s)
				}
				mail.body += l
			}
			m.mu.Lock()
			m.got = append(m.got, mail)
			m.mu.Unlock()
			mail = caughtMail{}
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// body returns the raw message sent to addr, headers included.
func (m *mailCatcher) body(addr string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.got {
		if g.to == addr {
			return g.body
		}
	}
	return ""
}

// to waits briefly for the mail sent to addr (the service sends from a
// goroutine) and returns the subjects that arrived.
func (m *mailCatcher) to(addr string, want int) []string {
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		var out []string
		for _, g := range m.got {
			if g.to == addr {
				out = append(out, g.subject)
			}
		}
		m.mu.Unlock()
		if len(out) >= want || time.Now().After(deadline) {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type fallbackFixture struct {
	t        *testing.T
	s        *store.Store
	c        *ExpiryChecker
	mail     *mailCatcher
	prod     *model.Product
	trial    *model.Plan // names community as its fallback
	bare     *model.Plan // a trial plan with no fallback
	free     *model.Plan // "Community": free subscription plan
	webhook  *model.Webhook
	suffix   string
	ctx      context.Context
	sequence int
}

func newFallbackFixture(t *testing.T) *fallbackFixture {
	t.Helper()
	s := openNotifyStore(t)
	setNotify(t, s, "plan_changed", "")
	setNotify(t, s, "trial_expired", "")
	ctx := context.Background()
	f := &fallbackFixture{t: t, s: s, ctx: ctx, suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")}
	f.prod = &model.Product{Name: "Fallback App", Slug: "fallback-" + f.suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, f.prod); err != nil {
		t.Fatal(err)
	}
	f.free = f.plan("Community", "subscription", "")
	f.trial = f.plan("Premium Trial", "trial", f.free.ID)
	f.bare = f.plan("Plain Trial", "trial", "")

	// A webhook so the dispatches are recorded as deliveries. Its URL
	// refuses connections; only the rows matter here.
	f.webhook = &model.Webhook{ProductID: f.prod.ID, URL: "http://127.0.0.1:1/hook", Secret: "s",
		Events: []string{model.EventPlanChanged, model.EventLicenseExpired, "license.deactivated"}, Active: true}
	if err := s.CreateWebhook(ctx, f.webhook); err != nil {
		t.Fatal(err)
	}
	f.mail = newMailCatcher(t)
	host, port, _ := net.SplitHostPort(f.mail.addr)
	wh := NewWebhookService(s, slog.Default(), time.Second, 0, true)
	f.c = NewExpiryChecker(s, NewEmailService(host, port, "", "", "keygate@test.local", slog.Default(), s), wh, slog.Default())
	return f
}

func (f *fallbackFixture) plan(name, licenseType, fallback string) *model.Plan {
	f.t.Helper()
	f.sequence++
	p := &model.Plan{ProductID: f.prod.ID, Name: name, Slug: "p" + strconv.Itoa(f.sequence) + "-" + f.suffix,
		LicenseType: licenseType, LicenseModel: "standard", MaxActivations: 1, TrialDays: 14, Active: true,
		TrialFallbackPlanID: fallback}
	if licenseType == "subscription" {
		p.BillingInterval = "month"
	}
	if err := f.s.CreatePlan(f.ctx, p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// license is a trial on plan that ended a minute ago, with one
// activation.
func (f *fallbackFixture) license(plan *model.Plan) *model.License {
	f.t.Helper()
	f.sequence++
	ended := time.Now().Add(-time.Minute).Truncate(time.Second)
	id := strconv.Itoa(f.sequence) + "-" + f.suffix
	lic := &model.License{ProductID: f.prod.ID, PlanID: plan.ID, Email: "fallback-" + id + "@example.com",
		LicenseKey: "KEY-FALLBACK-" + id, Status: model.StatusTrialing, ValidUntil: &ended}
	if err := f.s.CreateLicense(f.ctx, lic); err != nil {
		f.t.Fatal(err)
	}
	if err := store.SyncLicenseSubscriptionIn(f.ctx, f.s.DB, lic.ID, plan, lic.Status, lic.ValidUntil); err != nil {
		f.t.Fatal(err)
	}
	act := &model.Activation{LicenseID: lic.ID, Identifier: "host-" + id, IdentifierType: "device"}
	if err := f.s.CreateActivation(f.ctx, act); err != nil {
		f.t.Fatal(err)
	}
	return f.load(lic)
}

// load reads lic as the sweep does: with its product and plan.
func (f *fallbackFixture) load(lic *model.License) *model.License {
	f.t.Helper()
	fresh := new(model.License)
	if err := f.s.DB.NewSelect().Model(fresh).Relation("Product").Relation("Plan").
		Where("license.id = ?", lic.ID).Scan(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return fresh
}

func (f *fallbackFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.DB.NewRaw(query, args...).Scan(f.ctx, &n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// eventually is count for rows written in the background (the audit
// log is): it waits briefly for want, then returns what is there.
func (f *fallbackFixture) eventually(want int, query string, args ...any) int {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := f.count(query, args...)
		if n >= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fallbackFixture) deliveries(lic *model.License, event string) int {
	return f.count("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ? AND payload->'data'->>'license_id' = ?",
		f.webhook.ID, event, lic.ID)
}

// A trial whose plan names a fallback moves onto it when it ends: active,
// no expiry, same key and activations, and the customer is told their
// trial ended and which plan they carry on with.
func TestTrialFallback_MovesEndedTrial(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.trial)

	f.c.endTrial(f.ctx, lic)

	got := f.load(lic)
	if got.Status != model.StatusActive || got.PlanID != f.free.ID {
		t.Fatalf("licence is %s on %s, want active on the fallback plan", got.Status, got.PlanID)
	}
	if got.ValidUntil != nil || got.UpdatesUntil != nil {
		t.Fatalf("valid_until %v, updates_until %v; want both cleared", got.ValidUntil, got.UpdatesUntil)
	}
	if got.KeyHash != lic.KeyHash || got.LicenseKey != lic.LicenseKey {
		t.Fatal("the licence key changed")
	}
	if n := f.count("SELECT count(*) FROM activations WHERE license_id = ?", lic.ID); n != 1 {
		t.Fatalf("%d activations, want the one it had", n)
	}
	sub, err := f.s.FindSubscriptionByLicense(f.ctx, lic.ID)
	if err != nil || sub.PlanID != f.free.ID || sub.Status != model.StatusActive || sub.TrialEnd != nil {
		t.Fatalf("subscription row %+v (%v); want it on the fallback plan, active, trial cleared", sub, err)
	}
	if n := f.eventually(1, "SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'plan_changed' AND changes->>'reason' = 'trial_ended' AND changes->>'new_plan_id' = ?",
		lic.ID, f.free.ID); n != 1 {
		t.Fatalf("%d plan_changed audit entries, want 1", n)
	}
	if f.deliveries(lic, model.EventPlanChanged) != 1 || f.deliveries(lic, model.EventLicenseExpired) != 0 {
		t.Fatalf("webhooks: %d plan.changed, %d license.expired; want 1 and 0",
			f.deliveries(lic, model.EventPlanChanged), f.deliveries(lic, model.EventLicenseExpired))
	}
	if n := f.count("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ? AND payload->'data'->>'license_id' = ? "+
		"AND payload->'data'->>'reason' = 'trial_ended' AND payload->'data'->>'old_plan_id' = ? AND payload->'data'->>'new_plan_id' = ? "+
		"AND payload->'data'->>'email' = ?",
		f.webhook.ID, model.EventPlanChanged, lic.ID, f.trial.ID, f.free.ID, lic.Email); n != 1 {
		t.Fatal("plan.changed payload lacks reason trial_ended, the old and new plan, or the email")
	}
	if subjects := f.mail.to(lic.Email, 1); len(subjects) != 1 || subjects[0] != "Fallback App trial has ended" {
		t.Fatalf("mail sent: %q; want only the trial ended email", subjects)
	}
	if body := f.mail.body(lic.Email); !strings.Contains(body, "carries on with the <strong>Community</strong> plan") {
		t.Fatalf("the trial ended email does not name the fallback plan:\n%s", body)
	}
}

// Without a fallback a trial ends exactly as before.
func TestTrialFallback_NoFallbackExpires(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.bare)

	f.c.endTrial(f.ctx, lic)

	got := f.load(lic)
	if got.Status != model.StatusExpired || got.PlanID != f.bare.ID {
		t.Fatalf("licence is %s on %s, want expired on its trial plan", got.Status, got.PlanID)
	}
	if f.deliveries(lic, model.EventLicenseExpired) != 1 || f.deliveries(lic, model.EventPlanChanged) != 0 {
		t.Fatal("want license.expired and no plan.changed")
	}
	if sub, err := f.s.FindSubscriptionByLicense(f.ctx, lic.ID); err != nil || sub.Status != model.StatusExpired {
		t.Fatalf("subscription row %+v (%v); want it expired with the licence", sub, err)
	}
	if subjects := f.mail.to(lic.Email, 1); len(subjects) != 1 || subjects[0] != "Fallback App trial has ended" {
		t.Fatalf("mail sent: %q; want only the trial expired email", subjects)
	}
}

// Deleting the fallback plan clears the setting, and the trials of the
// plan that named it expire as before.
func TestTrialFallback_DeletedFallbackExpires(t *testing.T) {
	f := newFallbackFixture(t)
	gone := f.plan("Gone", "subscription", "")
	trial := f.plan("Short Trial", "trial", gone.ID)
	lic := f.license(trial)
	if err := f.s.DeletePlan(f.ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, trial.ID); p.TrialFallbackPlanID != "" {
		t.Fatalf("fallback still %q after its plan was deleted", p.TrialFallbackPlanID)
	}

	f.c.endTrial(f.ctx, lic) // a copy read before the delete

	if got := f.load(lic); got.Status != model.StatusExpired {
		t.Fatalf("licence is %s, want expired", got.Status)
	}
}

// A fallback that stopped qualifying after it was named (here: it was
// given a price) is never handed out; the trial expires instead.
func TestTrialFallback_RepricedFallbackExpires(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.trial)
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("stripe_price_id = ?", "price_fallback_"+f.suffix).
		Where("id = ?", f.free.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}

	// The store's answer, on a second licence: expired, with the reason.
	other := f.license(f.trial)
	res, err := f.s.MoveEndedTrialToFallback(f.ctx, other.ID, time.Now())
	if err != nil || res.Moved || !res.Expire || !strings.Contains(res.Refusal, "free") {
		t.Fatalf("got %+v, %v; want expire with a refusal naming the price", res, err)
	}
	if got := f.load(other); got.Status != model.StatusExpired {
		t.Fatalf("the store answered expire but left the licence %s", got.Status)
	}

	f.c.endTrial(f.ctx, lic)
	if got := f.load(lic); got.Status != model.StatusExpired || got.PlanID != f.trial.ID {
		t.Fatalf("licence is %s on %s, want expired on its trial plan", got.Status, got.PlanID)
	}
	// Support finds why it expired rather than moved.
	if n := f.eventually(1, "SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'expired' AND changes->>'fallback_refused' LIKE '%free%'",
		lic.ID); n != 1 {
		t.Fatalf("%d expired audits naming the refusal, want 1", n)
	}
}

// A trial suspended after the sweep read it stays suspended: the move
// re-reads the licence and takes only a trial still trialing, so it
// neither moves it nor lifts the suspension.
func TestTrialFallback_SuspendedSinceReadIsLeft(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.trial)
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).Set("status = ?", model.StatusSuspended).
		Where("id = ?", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.c.endTrial(f.ctx, lic) // its copy still says trialing
	if got := f.load(lic); got.Status != model.StatusSuspended || got.PlanID != f.trial.ID {
		t.Fatalf("licence is %s on %s, want it suspended on its trial plan", got.Status, got.PlanID)
	}
	if f.deliveries(lic, model.EventPlanChanged) != 0 || f.deliveries(lic, model.EventLicenseExpired) != 0 {
		t.Fatal("a suspended licence was announced as moved or expired")
	}
}

// Replicas sweep at once: the trial moves once, with one announcement.
func TestTrialFallback_ConcurrentSweepsMoveOnce(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.trial)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.c.endTrial(f.ctx, lic) // each with the same copy, as each replica read it
		}()
	}
	wg.Wait()
	if got := f.load(lic); got.Status != model.StatusActive || got.PlanID != f.free.ID {
		t.Fatalf("licence is %s on %s, want active on the fallback", got.Status, got.PlanID)
	}
	if n := f.deliveries(lic, model.EventPlanChanged); n != 1 {
		t.Fatalf("%d plan.changed deliveries, want 1", n)
	}
	if n := f.deliveries(lic, model.EventLicenseExpired); n != 0 {
		t.Fatalf("%d license.expired deliveries, want 0", n)
	}
}

// Devices already activated keep working after the move, as on any
// downgrade: the fallback's limit holds new activations back instead.
// Nothing is removed or reported as deactivated.
func TestTrialFallback_KeepsDevicesOverTheLimit(t *testing.T) {
	f := newFallbackFixture(t)
	roomy := f.plan("Roomy Trial", "trial", f.free.ID)
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("max_activations = 5").Where("id = ?", roomy.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	lic := f.license(roomy) // with one device, room for five on the trial
	for _, name := range []string{"second-pc", "third-pc"} {
		if err := f.s.CreateActivation(f.ctx, &model.Activation{LicenseID: lic.ID, Identifier: name + "-" + lic.ID[:8], IdentifierType: "device"}); err != nil {
			t.Fatal(err)
		}
	}

	f.c.endTrial(f.ctx, f.load(lic))

	if got := f.load(lic); got.PlanID != f.free.ID {
		t.Fatalf("licence on %s, want the fallback", got.PlanID)
	}
	if n := f.count("SELECT count(*) FROM activations WHERE license_id = ?", lic.ID); n != 3 {
		t.Fatalf("%d devices after the move, want all 3 kept", n)
	}
	if n := f.deliveries(lic, "license.deactivated"); n != 0 {
		t.Fatalf("%d license.deactivated deliveries, want none", n)
	}
	extra := &model.Activation{LicenseID: lic.ID, Identifier: "fourth-pc-" + lic.ID[:8], IdentifierType: "device"}
	// The limit comes from the plan the licence is on now, read under its
	// lock: the trial's five would let a fourth in.
	if err := f.s.ActivateWithinLimit(f.ctx, extra, 100); err == nil {
		t.Fatal("a new device was activated beyond the fallback's limit")
	}
}

// The sweep's copy may be stale: a trial extended since it was read is
// left alone, and a trial already moved is not moved, mailed or
// announced again.
func TestTrialFallback_StaleCopyChangesNothing(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.trial)
	later := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).Set("valid_until = ?", later).
		Where("id = ?", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.c.endTrial(f.ctx, lic) // still says the trial ended a minute ago
	if got := f.load(lic); got.Status != model.StatusTrialing || got.PlanID != f.trial.ID || !got.ValidUntil.Equal(later) {
		t.Fatalf("extended trial changed: %s on %s until %v", got.Status, got.PlanID, got.ValidUntil)
	}

	moved := f.license(f.trial)
	f.c.endTrial(f.ctx, moved)
	f.c.endTrial(f.ctx, moved)         // a second pass, or a second replica, with the same copy
	f.c.endTrial(f.ctx, f.load(moved)) // and a fresh read of it
	if f.deliveries(moved, model.EventPlanChanged) != 1 {
		t.Fatalf("%d plan.changed deliveries, want 1", f.deliveries(moved, model.EventPlanChanged))
	}
	if got := f.load(moved); got.Status != model.StatusActive || got.PlanID != f.free.ID {
		t.Fatalf("licence is %s on %s after the second pass", got.Status, got.PlanID)
	}
	f.mail.to(moved.Email, 1)
	time.Sleep(300 * time.Millisecond) // room for a stray second mail to arrive, if one was sent
	if subjects := f.mail.to(moved.Email, 0); len(subjects) != 1 {
		t.Fatalf("mail sent: %q; want one", subjects)
	}
}

// Stripe owns the plan of a licence it bills, so such a trial is not
// moved here; it expires as before.
func TestTrialFallback_StripeBilledTrialExpires(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.trial)
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).
		Set("stripe_subscription_id = ?", "sub_fallback_"+f.suffix).Set("payment_provider = 'stripe'").
		Where("id = ?", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.c.endTrial(f.ctx, f.load(lic))
	if got := f.load(lic); got.Status != model.StatusExpired || got.PlanID != f.trial.ID {
		t.Fatalf("licence is %s on %s, want expired on its trial plan", got.Status, got.PlanID)
	}
	if f.deliveries(lic, model.EventPlanChanged) != 0 || f.deliveries(lic, model.EventLicenseExpired) != 1 {
		t.Fatal("want license.expired and no plan.changed")
	}
	if subjects := f.mail.to(lic.Email, 1); len(subjects) != 1 || subjects[0] != "Fallback App trial has ended" {
		t.Fatalf("mail sent: %q; want only the trial expired email", subjects)
	}
	// The reminder makes no promise to it either.
	if got := f.c.trialFallbackName(f.ctx, f.load(lic), true); got != "" {
		t.Fatalf("reminder names %q for a Stripe-billed trial", got)
	}
}

// The trial reminder says where the licence goes when it names a
// fallback, and reads as before when it doesn't.
func TestTrialFallback_ReminderWording(t *testing.T) {
	f := newFallbackFixture(t)
	withFallback, without := f.license(f.trial), f.license(f.bare)
	if got := f.c.trialFallbackName(f.ctx, withFallback, true); got != "Community" {
		t.Fatalf("fallback name %q, want Community", got)
	}
	if got := f.c.trialFallbackName(f.ctx, without, true); got != "" {
		t.Fatalf("fallback name %q for a plan without one", got)
	}
	if got := f.c.trialFallbackName(f.ctx, withFallback, false); got != "" {
		t.Fatalf("fallback name %q for a licence that is not trialing", got)
	}
	// A fallback that no longer qualifies (priced by hand) is not promised.
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("stripe_price_id = ?", "price_rem_"+f.suffix).
		Where("id = ?", f.free.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.c.trialFallbackName(f.ctx, withFallback, true); got != "" {
		t.Fatalf("fallback name %q for a fallback that no longer qualifies", got)
	}

	e := newTemplateTestService()
	_, moving := e.RenderLicenseExpiring("Acme", "KG-1", "2026-10-08", 3, true, "Community")
	_, ending := e.RenderLicenseExpiring("Acme", "KG-1", "2026-10-08", 3, true, "")
	if !strings.Contains(moving, "moves to the <strong>Community</strong> plan") || strings.Contains(moving, "to keep using it") {
		t.Fatalf("fallback wording missing:\n%s", moving)
	}
	if strings.Contains(ending, "moves to") || !strings.Contains(ending, "Choose a plan before then to keep using it.") {
		t.Fatalf("plain trial wording changed:\n%s", ending)
	}
	if err := ValidateTemplate("license_expiring", `<p>{{if .FallbackPlan}}Then {{.FallbackPlan}}{{end}}</p>`); err != nil {
		t.Fatalf("a custom template using FallbackPlan was refused: %v", err)
	}
}

// A move that fails leaves the trial as it was, trialing, for the next
// sweep to try again: it is not expired in its place, and nothing is
// announced.
func TestTrialFallback_FailedMoveIsRetriedNotExpired(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.trial)
	name := "block_move_" + f.suffix
	if _, err := f.s.DB.NewRaw(`CREATE FUNCTION ` + name + `() RETURNS trigger AS $$
		BEGIN IF NEW.id = '` + lic.ID + `' AND NEW.plan_id IS DISTINCT FROM OLD.plan_id THEN RAISE EXCEPTION 'blocked for the test'; END IF; RETURN NEW; END
		$$ LANGUAGE plpgsql`).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DROP TRIGGER IF EXISTS " + name + " ON licenses").Exec(context.Background())
		_, _ = f.s.DB.NewRaw("DROP FUNCTION IF EXISTS " + name + "()").Exec(context.Background())
	})
	if _, err := f.s.DB.NewRaw("CREATE TRIGGER " + name + " BEFORE UPDATE ON licenses FOR EACH ROW EXECUTE FUNCTION " + name + "()").Exec(f.ctx); err != nil {
		t.Fatal(err)
	}

	f.c.endTrial(f.ctx, lic)

	if got := f.load(lic); got.Status != model.StatusTrialing || got.PlanID != f.trial.ID {
		t.Fatalf("licence is %s on %s after a failed move, want trialing on its trial plan", got.Status, got.PlanID)
	}
	if f.deliveries(lic, model.EventPlanChanged) != 0 || f.deliveries(lic, model.EventLicenseExpired) != 0 {
		t.Fatal("a failed move was announced")
	}
}

// A trial that will move to a fallback is not read as expired before the
// sweep moves it, even on a plan with no grace: it keeps a day. One with
// no fallback ends at its end date; one billed by Stripe keeps the plan's
// own grace.
func TestTrialFallback_UsableUntilTheSweepMovesIt(t *testing.T) {
	f := newFallbackFixture(t)
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("grace_days = 0").
		Where("id IN (?, ?)", f.trial.ID, f.bare.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	moving, ending := f.load(f.license(f.trial)), f.load(f.license(f.bare)) // ended a minute ago
	if err := LicenseUsable(moving); err != nil || !IsLicenseUsable(moving) {
		t.Fatalf("a trial about to move to its fallback: %v, usable %v; want usable", err, IsLicenseUsable(moving))
	}
	if err := LicenseUsable(ending); err == nil || IsLicenseUsable(ending) {
		t.Fatal("a trial with no fallback and no grace is still usable")
	}
	// A day, not more: past it the trial reads as expired after all.
	dayAgo := time.Now().Add(-25 * time.Hour)
	late := *moving
	late.ValidUntil = &dayAgo
	if err := LicenseUsable(&late); err == nil {
		t.Fatal("a trial a day and more past its end is still usable")
	}
	// Only a trial gets it: once moved (active), the plan's own grace.
	active := *moving
	active.Status = model.StatusActive
	if active.GraceDays() != 0 {
		t.Fatalf("an active licence on the plan gets %d days of grace, want the plan's 0", active.GraceDays())
	}
	moving.StripeSubscriptionID = "sub_test"
	if err := LicenseUsable(moving); err == nil {
		t.Fatal("a Stripe-billed trial got the fallback's day of grace")
	}
}

// A fallback named after the sweep read the plan is taken: the move is
// decided under the locks, not on the sweep's copy.
func TestTrialFallback_NamedSinceTheReadIsTaken(t *testing.T) {
	f := newFallbackFixture(t)
	lic := f.license(f.bare) // its copy: a plan with no fallback
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("trial_fallback_plan_id = ?", f.free.ID).
		Where("id = ?", f.bare.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.c.endTrial(f.ctx, lic)
	if got := f.load(lic); got.Status != model.StatusActive || got.PlanID != f.free.ID {
		t.Fatalf("licence is %s on %s, want it moved to the fallback named since", got.Status, got.PlanID)
	}
}

// A trial Stripe doesn't bill, on a plan with no fallback, gets no grace
// days from verify or in its token: the sweep ends it at its end date.
func TestTrialNotBilledByStripe_EndsAtItsEndDate(t *testing.T) {
	f := newFallbackFixture(t)
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("grace_days = 7").
		Where("id = ?", f.bare.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	lic := f.license(f.bare)
	in := VerifyInput{LicenseKey: lic.LicenseKey}
	if err := f.s.DB.NewRaw("SELECT identifier FROM activations WHERE license_id = ?", lic.ID).Scan(f.ctx, &in.Identifier); err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	svc := NewLicenseService(f.s, priv, slog.Default(), nil, nil)

	// A day left: the token stops at the trial's end.
	end := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).Set("valid_until = ?", end).
		Where("id = ?", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Verify(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := license.Verify(res.Token, license.PublicKey(priv))
	if err != nil {
		t.Fatal(err)
	}
	if exp := time.Unix(tok.ExpiresAt, 0); !exp.Equal(end) || tok.GraceDays != 0 || res.GraceDays != 0 {
		t.Errorf("grace_days %d, token grc %d and good offline %s past the trial's end; want none",
			res.GraceDays, tok.GraceDays, exp.Sub(end))
	}

	// Back to its end a minute ago, before the sweep has run.
	if _, err := f.s.DB.NewUpdate().Model((*model.License)(nil)).Set("valid_until = ?", lic.ValidUntil).
		Where("id = ?", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(f.ctx, in); err == nil {
		t.Error("verify answered a minute after the trial ended")
	}
}
