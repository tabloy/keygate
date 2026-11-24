package service

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/store"
)

// A manual resend creates a new delivery row and a new
// X-Keygate-Delivery header, by design: retries and response codes
// are tracked separately. But the payload must carry its own stable
// "id" that survives the resend unchanged, or a receiver following
// the docs' advice to dedupe on X-Keygate-Delivery double-processes
// every resend of anything it already handled.
func TestRedispatchKeepsTheSamePayloadID(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	var deliveryHeaders []string
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		deliveryHeaders = append(deliveryHeaders, r.Header.Get("X-Keygate-Delivery"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer rcv.Close()

	f := newRetryFixture(t, http.StatusOK)
	// newRetryFixture already points f.wh at its own receiver; point
	// this test's webhook at ours instead so we can read the body.
	f.wh.URL = rcv.URL
	if err := f.s.UpdateWebhook(f.ctx, f.wh, "url"); err != nil {
		t.Fatal(err)
	}

	svc := NewWebhookService(f.s, slog.Default(), 2*time.Second, 5, true)
	if err := svc.DispatchWithLog(f.ctx, f.wh.ProductID, "license.created", map[string]any{"license_id": "lic_1"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := len(bodies)
		mu.Unlock()
		if got >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("original delivery never arrived")
		}
		time.Sleep(20 * time.Millisecond)
	}

	deliveries, _, err := f.s.ListWebhookDeliveries(f.ctx, store.WebhookDeliveryFilter{WebhookID: f.wh.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(deliveries))
	}

	if _, err := svc.Redispatch(f.ctx, deliveries[0].ID); err != nil {
		t.Fatalf("Redispatch: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := len(bodies)
		mu.Unlock()
		if got >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resent delivery never arrived")
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	id1, _ := bodies[0]["id"].(string)
	id2, _ := bodies[1]["id"].(string)
	if id1 == "" {
		t.Fatal("original delivery's payload has no \"id\"")
	}
	if id1 != id2 {
		t.Fatalf("payload id changed on resend: %q then %q, want the same event id so receivers can dedupe", id1, id2)
	}
	if deliveryHeaders[0] == deliveryHeaders[1] {
		t.Fatal("resend should get its own X-Keygate-Delivery, not reuse the original's")
	}
}
