package payment

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// The trial-ending mail names the day in UTC, like every other license
// date a customer is mailed. time.Unix answers in the server's zone, and
// the zone is fixed at process start, so the check runs in a child process
// started with TZ set.
func TestTrialEndingMailDateIsUTCWhateverTheServerZone(t *testing.T) {
	if os.Getenv("KEYGATE_TRIAL_DATE_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTrialEndingMailDateIsUTCWhateverTheServerZone$", "-test.v")
		cmd.Env = append(os.Environ(), "TZ=Pacific/Auckland", "KEYGATE_TRIAL_DATE_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		} else if strings.Contains(string(out), "--- SKIP") {
			t.Skip("skipping integration test: TEST_DATABASE_URL not set")
		}
		return
	}

	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "trialdate", "subscription")
	suffix := time.Now().Format("150405.000000")
	subID := "sub_trialdate_" + suffix
	email := "trialdate-" + suffix + "@example.com"
	lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: email, LicenseKey: "KEY-trialdate-" + suffix,
		Status: model.StatusTrialing, StripeSubscriptionID: subID}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	body := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		say("220 catcher")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.ToUpper(strings.TrimSpace(line)) == "DATA" {
				say("354 go on")
				var data strings.Builder
				for {
					d, err := r.ReadString('\n')
					if err != nil || d == ".\r\n" {
						break
					}
					data.WriteString(d)
				}
				body <- data.String()
				say("250 ok")
				continue
			}
			say("250 ok")
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	h := &StripeHandler{Store: s, Email: service.NewEmailService(host, port, "", "", "keygate@test.local", slog.Default(), s)}

	// 23:30 UTC on the 6th is already the 7th in Auckland.
	trialEnd := time.Date(2026, 11, 6, 23, 30, 0, 0, time.UTC).Unix()
	if err := h.onTrialWillEnd(ctx, []byte(fmt.Sprintf(`{"id":%q,"trial_end":%d}`, subID, trialEnd))); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-body:
		if !strings.Contains(got, "2026-11-06") || strings.Contains(got, "2026-11-07") {
			t.Fatalf("mail should name 2026-11-06 (UTC), not the server's day:\n%s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no mail arrived")
	}
}
