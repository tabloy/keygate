package service

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// webhookReceiver counts the POSTs it gets per X-Keygate-Delivery id.
type webhookReceiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   map[string]int
	status int
}

func newWebhookReceiver(t *testing.T, status int) *webhookReceiver {
	t.Helper()
	r := &webhookReceiver{hits: map[string]int{}, status: status}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Slow enough that a second replica's tick lands while the
		// first one is still waiting on the receiver.
		time.Sleep(50 * time.Millisecond)
		r.mu.Lock()
		r.hits[req.Header.Get("X-Keygate-Delivery")]++
		r.mu.Unlock()
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *webhookReceiver) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[id]
}

// retryFixture is a webhook pointed at a receiver, and due deliveries
// for it that sort ahead of whatever else the shared database holds,
// so a retry pass takes this test's rows first.
type retryFixture struct {
	t   *testing.T
	s   *store.Store
	ctx context.Context
	wh  *model.Webhook
	rcv *webhookReceiver
}

func newRetryFixture(t *testing.T, status int) *retryFixture {
	t.Helper()
	s := openNotifyStore(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Retry App", Slug: "retry-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	f := &retryFixture{t: t, s: s, ctx: ctx, rcv: newWebhookReceiver(t, status)}
	f.wh = &model.Webhook{ProductID: prod.ID, URL: f.rcv.srv.URL + "/hook", Secret: "s",
		Events: []string{"license.created"}, Active: true}
	if err := s.CreateWebhook(ctx, f.wh); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.DB.NewRaw("DELETE FROM webhook_deliveries WHERE webhook_id = ?", f.wh.ID).Exec(ctx); err != nil {
			t.Errorf("clean up: %v", err)
		}
	})
	return f
}

// due writes a delivery the retry loop should pick up now.
func (f *retryFixture) due() *model.WebhookDelivery {
	f.t.Helper()
	past := time.Now().Add(-time.Minute)
	d := &model.WebhookDelivery{WebhookID: f.wh.ID, Event: "license.created",
		Payload: map[string]any{"event": "license.created"}, Status: "pending", Attempts: 1, NextRetry: &past}
	if err := f.s.CreateWebhookDelivery(f.ctx, d); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.s.DB.NewRaw("UPDATE webhook_deliveries SET created_at = '1970-01-01', next_retry = ? WHERE id = ?",
		past, d.ID).Exec(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *retryFixture) load(id string) *model.WebhookDelivery {
	f.t.Helper()
	d, err := f.s.FindWebhookDeliveryByID(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

// settled waits until none of ids is still pending, and then a little
// longer, so a duplicate send that was going to happen has happened.
func (f *retryFixture) settled(ids []string) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, id := range ids {
		for f.load(id).Status == "pending" {
			if time.Now().After(deadline) {
				f.t.Fatalf("delivery %s still pending", id)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	time.Sleep(300 * time.Millisecond)
}

// Every replica runs the retry loop. Two of them ticking together must
// not both send the same delivery: the receiver would see one delivery
// id twice, and the slower replica's write would land on a row the
// other has already closed.
func TestProcessRetriesSendsEachDeliveryOnceAcrossReplicas(t *testing.T) {
	f := newRetryFixture(t, http.StatusOK)
	var ids []string
	for range 10 {
		ids = append(ids, f.due().ID)
	}

	replicas := []*WebhookService{
		NewWebhookService(f.s, slog.Default(), 2*time.Second, 5, true),
		NewWebhookService(f.s, slog.Default(), 2*time.Second, 5, true),
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, r := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.ProcessRetries(f.ctx)
		}()
	}
	close(start)
	wg.Wait()
	f.settled(ids)

	for _, id := range ids {
		if n := f.rcv.count(id); n != 1 {
			t.Errorf("delivery %s was sent %d times, want once", id, n)
		}
		d := f.load(id)
		if d.Status != "delivered" || d.Attempts != 2 {
			t.Errorf("delivery %s: status %q attempts %d, want delivered after 2", id, d.Status, d.Attempts)
		}
	}
}

// A sender can wait for a sending slot past its lease, and by then
// another replica may have claimed the row. The stale sender does not
// send it, and its late result does not land on the row the new holder
// has closed.
func TestDeliveryTakenOverIsNotSentAgain(t *testing.T) {
	f := newRetryFixture(t, http.StatusOK)
	d := f.due()
	stale := *d

	taken, err := f.s.ClaimPendingDeliveries(f.ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 || taken[0].ID != d.ID {
		t.Fatalf("the other replica claimed %v, want %s", taken, d.ID)
	}
	if taken[0].ClaimToken == "" || taken[0].ClaimToken == stale.ClaimToken {
		t.Fatalf("the claim kept token %q", taken[0].ClaimToken)
	}

	svc := NewWebhookService(f.s, slog.Default(), 2*time.Second, 5, true)
	svc.deliver(f.ctx, f.wh, &stale)
	if n := f.rcv.count(d.ID); n != 0 {
		t.Fatalf("the stale sender sent it %d times", n)
	}

	svc.deliver(f.ctx, f.wh, taken[0])
	if n := f.rcv.count(d.ID); n != 1 {
		t.Fatalf("the holder sent it %d times, want once", n)
	}
	stale.Status = "failed"
	if held, err := f.s.FinishWebhookDelivery(f.ctx, &stale); err != nil || held {
		t.Fatalf("stale finish: held %v, %v; want it refused", held, err)
	}
	if got := f.load(d.ID); got.Status != "delivered" || got.ClaimToken != "" {
		t.Fatalf("after the holder finished: status %q token %q, want delivered and released", got.Status, got.ClaimToken)
	}
}

// A failed retry goes back in the queue with its backoff, released so
// that the next pass, on any replica, can claim it.
func TestFailedRetryIsReleasedWithBackoff(t *testing.T) {
	f := newRetryFixture(t, http.StatusInternalServerError)
	d := f.due()
	NewWebhookService(f.s, slog.Default(), 2*time.Second, 5, true).ProcessRetries(f.ctx)

	deadline := time.Now().Add(10 * time.Second)
	got := f.load(d.ID)
	for got.Attempts < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the retry never finished")
		}
		time.Sleep(20 * time.Millisecond)
		got = f.load(d.ID)
	}
	if got.Status != "pending" || got.ResponseCode != http.StatusInternalServerError || got.ClaimToken != "" {
		t.Fatalf("status %q code %d token %q, want pending, 500 and released", got.Status, got.ResponseCode, got.ClaimToken)
	}
	// Second attempt: 2^2 * 30s.
	if wait := time.Until(*got.NextRetry); wait < 110*time.Second || wait > 130*time.Second {
		t.Fatalf("next retry in %v, want about 2m", wait)
	}
	if n := f.rcv.count(d.ID); n != 1 {
		t.Fatalf("sent %d times, want once", n)
	}
}

// A delivery for a webhook that was switched off is failed outright,
// not retried forever: a disabled receiver is just as unreachable as a
// gone one. (A deleted webhook cascade-deletes its own pending
// deliveries, so that case can't leave one of these behind to check;
// disabling one is the reachable version of "nothing will ever accept
// this delivery.")
func TestRetryOfADisabledWebhookIsFailedOutright(t *testing.T) {
	f := newRetryFixture(t, http.StatusOK)
	d := f.due()
	f.wh.Active = false
	if err := f.s.UpdateWebhook(f.ctx, f.wh, "active"); err != nil {
		t.Fatal(err)
	}
	NewWebhookService(f.s, slog.Default(), 2*time.Second, 5, true).ProcessRetries(f.ctx)

	got := f.load(d.ID)
	if got.Status != "failed" || got.NextRetry != nil {
		t.Fatalf("status %q next_retry %v, want failed with no next retry", got.Status, got.NextRetry)
	}
	if n := f.rcv.count(d.ID); n != 0 {
		t.Fatalf("a disabled webhook's delivery was sent %d times, want never", n)
	}
}
