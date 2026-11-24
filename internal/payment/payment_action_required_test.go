package payment

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// The 3DS/SCA notice is the only mail that carries the link a
// customer needs to complete payment authentication. It must be
// queued (durable, retried on an SMTP hiccup) rather than sent
// directly from the webhook handler, the way the dunning ladder
// already is - see RenderPaymentFailed.
func TestPaymentActionRequiredIsQueuedNotSentDirectly(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "actionreq", "subscription")
	suffix := time.Now().Format("150405.000000")
	subID := "sub_actionreq_" + suffix
	email := "actionreq-" + suffix + "@example.com"
	lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: email, LicenseKey: "KEY-actionreq-" + suffix,
		Status: model.StatusActive, StripeSubscriptionID: subID}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	// No SMTP listener at all: if the code tried to send directly, it
	// would either hang or fail; it must not depend on that here.
	h := &StripeHandler{Store: s, Email: service.NewEmailService("127.0.0.1", "1", "", "", "keygate@test.local", slog.Default(), s)}

	if err := h.onPaymentActionRequired(ctx, []byte(`{"subscription":"`+subID+`","hosted_invoice_url":"https://billing.example.com/i/abc"}`)); err != nil {
		t.Fatalf("onPaymentActionRequired: %v", err)
	}

	var subject, body string
	var count int
	if err := s.DB.NewRaw(
		"SELECT count(*), coalesce(max(subject), ''), coalesce(max(body), '') FROM email_queue WHERE to_addr = ?", email,
	).Scan(context.Background(), &count, &subject, &body); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("queued emails for %s = %d, want 1", email, count)
	}
	if !strings.Contains(body, "https://billing.example.com/i/abc") {
		t.Fatalf("queued mail body missing the hosted invoice URL: %s", body)
	}
}
