package handler

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
)

// docs/openapi.yaml: "Limits: 3 codes per address per 10 minutes (then 429
// RATE_LIMITED)". The limiter counts the address's recent codes and then,
// in a separate statement, stores the new one, so parallel requests all
// read a count under the limit and each mails (and stores) a code.
func TestOTPSend_RateLimitHoldsUnderParallelRequests(t *testing.T) {
	f := newSignupFixture(t, nil)

	const addresses, burst = 10, 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	for a := range addresses {
		email := "burst" + strconv.Itoa(a) + "-" + f.plan.CheckoutID + "@example.com"
		for range burst {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				f.call(f.h.SignupSend, http.MethodPost, map[string]any{"email": email})
			}()
		}
	}
	close(start)
	wg.Wait()

	for a := range addresses {
		email := "burst" + strconv.Itoa(a) + "-" + f.plan.CheckoutID + "@example.com"
		n, err := f.s.CountRecentOTPCodes(f.ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		if n > 3 {
			t.Errorf("%s: %d parallel code requests stored %d codes; the limit is 3 per 10 minutes", email, burst, n)
		}
	}
}
