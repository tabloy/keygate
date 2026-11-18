package handler

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

// The admin's seat tools: each member's key by its hint and in full on
// request, a member's key reissued, a member removed or an invitation
// cancelled; each held to the licence named and to a bound API key's
// product, and audited without the key.
func TestAdminSeats(t *testing.T) {
	p := newPortalTeam(t)
	f, lic := p.f, p.f.licA
	wh := service.NewWebhookService(f.s, slog.Default(), time.Second, 1, true)
	h := &AdminHandler{Store: f.s, Webhook: wh, Seats: service.NewSeatService(f.s, wh, nil, slog.Default(), "")}
	hook := &model.Webhook{ProductID: f.prodA.ID, URL: "http://127.0.0.1:9/hook", Secret: "s",
		Events: []string{model.EventLicenseDeactivated, model.EventSeatRemoved}, Active: true}
	if err := f.s.CreateWebhook(f.ctx, hook); err != nil {
		t.Fatal(err)
	}
	sent := func(event string) int {
		t.Helper()
		var n int
		if err := f.s.DB.NewRaw("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ?", hook.ID, event).Scan(f.ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// bob has joined but never opened the portal: no key yet.
	plain, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	bob := &model.Seat{LicenseID: lic.ID, Email: "bob-as-" + f.suffix + "@example.com", Role: "member", InviteTokenHash: hash, InviteExpiresAt: &expires}
	if _, _, err := f.s.AddSeatWithinLimit(f.ctx, bob, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.AcceptSeatInvite(f.ctx, plain); err != nil {
		t.Fatal(err)
	}

	seatCall := func(fn gin.HandlerFunc, method, licenseID, seatID string, key *model.APIKey) (int, map[string]any) {
		t.Helper()
		return f.call(fn, method, "/admin/licenses/"+licenseID+"/seats/"+seatID, "", key,
			gin.Param{Key: "id", Value: licenseID}, gin.Param{Key: "seat_id", Value: seatID})
	}
	data := func(out map[string]any) map[string]any { d, _ := out["data"].(map[string]any); return d }

	// The list: each member's key by its hint, and their devices.
	code, out := f.call(h.ListLicenseSeats, http.MethodGet, "/admin/licenses/"+lic.ID+"/seats", "", nil, gin.Param{Key: "id", Value: lic.ID})
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, out)
	}
	rows, _ := data(out)["seats"].([]any)
	byEmail := map[string]map[string]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		byEmail[m["email"].(string)] = m
	}
	if a := byEmail[p.alice.Email]; a == nil || a["key_hint"] != p.aliceKey[len(p.aliceKey)-4:] || a["device_count"] != float64(1) {
		t.Fatalf("alice in the list = %v", a)
	}
	if pd := byEmail[p.pending.Email]; pd == nil || pd["key_hint"] != nil || pd["device_count"] != float64(0) {
		t.Fatalf("the pending invitation in the list = %v", pd)
	}
	if b := byEmail[bob.Email]; b == nil || b["key_hint"] != nil {
		t.Fatalf("bob, with no key yet, in the list = %v; listing must not make one", b)
	}
	if s, _ := f.s.FindSeatByID(f.ctx, bob.ID); s.MemberKeyHash != "" {
		t.Fatal("listing made bob a key")
	}

	// Reveal: alice's key in full; bob's made on the way, as the portal
	// would; nothing for an invitation or a seat on another licence.
	if code, out := seatCall(h.RevealMemberKey, http.MethodGet, lic.ID, p.alice.ID, nil); code != http.StatusOK || data(out)["license_key"] != p.aliceKey {
		t.Fatalf("reveal alice = %d %v", code, out)
	}
	code, out = seatCall(h.RevealMemberKey, http.MethodGet, lic.ID, bob.ID, nil)
	bobKey, _ := data(out)["license_key"].(string)
	if code != http.StatusOK || bobKey == "" {
		t.Fatalf("reveal bob = %d %v", code, out)
	}
	if again, _ := f.s.FindSeatByID(f.ctx, bob.ID); f.s.DecryptMemberKey(again) != bobKey {
		t.Fatal("bob's revealed key is not the one stored")
	}
	if code, out := seatCall(h.RevealMemberKey, http.MethodGet, lic.ID, p.pending.ID, nil); code != http.StatusConflict || errCode(out) != "SEAT_NOT_JOINED" {
		t.Fatalf("reveal a pending invitation = %d %v", code, out)
	}
	if code, out := seatCall(h.RevealMemberKey, http.MethodGet, f.licB.ID, p.alice.ID, nil); code != http.StatusNotFound || errCode(out) != "SEAT_NOT_FOUND" {
		t.Fatalf("reveal alice through another licence = %d %v", code, out)
	}
	other := &model.APIKey{ProductID: f.prodB.ID, Scopes: []string{model.ScopeLicensesWrite}}
	for name, fn := range map[string]gin.HandlerFunc{"reveal": h.RevealMemberKey, "reissue": h.RotateMemberKey, "remove": h.RemoveLicenseSeat} {
		if code, out := seatCall(fn, http.MethodPost, lic.ID, p.alice.ID, other); code != http.StatusForbidden || errCode(out) != "PRODUCT_SCOPE_MISMATCH" {
			t.Fatalf("%s with another product's key = %d %v", name, code, out)
		}
	}

	// Reissue alice's key: her old key and device go, the rest stay.
	code, out = seatCall(h.RotateMemberKey, http.MethodPost, lic.ID, p.alice.ID, nil)
	newKey, _ := data(out)["license_key"].(string)
	if code != http.StatusOK || newKey == "" || newKey == p.aliceKey || data(out)["devices_removed"] != float64(1) {
		t.Fatalf("reissue alice = %d %v", code, out)
	}
	if _, err := f.s.FindLicenseByKey(f.ctx, p.aliceKey); err == nil {
		t.Fatal("alice's old key still opens the licence")
	}
	for _, k := range []string{newKey, bobKey, lic.LicenseKey} {
		if _, err := f.s.FindLicenseByKey(f.ctx, k); err != nil {
			t.Fatalf("a key that should work does not: %v", err)
		}
	}
	if n := sent(model.EventLicenseDeactivated); n != 1 {
		t.Fatalf("%d license.deactivated after the reissue, want 1", n)
	}
	if code, out := seatCall(h.RotateMemberKey, http.MethodPost, lic.ID, p.pending.ID, nil); code != http.StatusConflict || errCode(out) != "SEAT_NOT_JOINED" {
		t.Fatalf("reissue for a pending invitation = %d %v", code, out)
	}

	// Remove bob and cancel the invitation; removing again answers the same.
	for _, seat := range []*model.Seat{bob, p.pending, bob} {
		if code, out := seatCall(h.RemoveLicenseSeat, http.MethodDelete, lic.ID, seat.ID, nil); code != http.StatusOK {
			t.Fatalf("remove %s = %d %v", seat.Email, code, out)
		}
	}
	if _, err := f.s.FindLicenseByKey(f.ctx, bobKey); err == nil {
		t.Fatal("bob's key still opens the licence after his removal")
	}
	if n := sent(model.EventSeatRemoved); n != 2 {
		t.Fatalf("%d seat.removed, want 2 (bob and the invitation, once each)", n)
	}

	// The audit says who did what, and never holds a key.
	var changes []string
	for range 40 {
		changes = nil
		_ = f.s.DB.NewRaw(`SELECT action || ' ' || changes::text FROM audit_logs WHERE actor_type = 'admin'
			AND action IN ('member_key_revealed', 'member_key_rotated', 'seat_removed')
			AND entity_id IN (?, ?, ?)`, p.alice.ID, bob.ID, p.pending.ID).Scan(f.ctx, &changes)
		if len(changes) >= 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(changes) != 5 {
		t.Fatalf("admin audit rows = %v, want 2 reveals, 1 reissue, 2 removals", changes)
	}
	var withoutIP int
	if err := f.s.DB.NewRaw(`SELECT count(*) FROM audit_logs WHERE actor_type = 'admin' AND coalesce(ip_address, '') = ''
		AND entity_id IN (?, ?, ?)`, p.alice.ID, bob.ID, p.pending.ID).Scan(f.ctx, &withoutIP); err != nil || withoutIP != 0 {
		t.Fatalf("%d admin audit rows without the admin's IP (%v)", withoutIP, err)
	}
	for _, c := range changes {
		for _, k := range []string{p.aliceKey, newKey, bobKey} {
			if strings.Contains(c, k) {
				t.Fatalf("the audit holds a key: %s", c)
			}
		}
	}
}

// A customer who is a member of someone else's licence sees it in their
// detail as a membership, with their own key's hint; and a member's
// address or own key finds the licence in the admin's search.
func TestAdminFindsMembers(t *testing.T) {
	p := newPortalTeam(t)
	f, lic := p.f, p.f.licA
	h := &AdminHandler{Store: f.s}

	alice, err := f.s.FindSeatByID(f.ctx, p.alice.ID)
	if err != nil || alice.UserID == "" {
		t.Fatalf("alice's account: %v", err)
	}
	code, out := f.call(h.GetUserDetail, http.MethodGet, "/admin/users/"+alice.UserID, "", nil, gin.Param{Key: "id", Value: alice.UserID})
	d, _ := out["data"].(map[string]any)
	if code != http.StatusOK {
		t.Fatalf("detail = %d %v", code, out)
	}
	ms, _ := d["memberships"].([]any)
	if len(ms) != 1 {
		t.Fatalf("alice's memberships = %v, want the one licence", ms)
	}
	m := ms[0].(map[string]any)
	ml, _ := m["license"].(map[string]any)
	hints, _ := d["member_key_hints"].(map[string]any)
	if ml["id"] != lic.ID || hints[alice.ID] != p.aliceKey[len(p.aliceKey)-4:] {
		t.Fatalf("membership = %v, hints %v", m, hints)
	}
	if owned, _ := d["licenses"].([]any); len(owned) != 0 {
		t.Fatalf("alice owns %v; a membership is not ownership", owned)
	}

	// Once the licence's address is alice's (a Stripe customer changing
	// theirs), she is its owner: it is hers, not a membership, though her
	// seat is still there.
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET email = ? WHERE id = ?", p.alice.Email, lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	_, out = f.call(h.GetUserDetail, http.MethodGet, "/admin/users/"+alice.UserID, "", nil, gin.Param{Key: "id", Value: alice.UserID})
	d, _ = out["data"].(map[string]any)
	owned, _ := d["licenses"].([]any)
	if d["memberships"] == nil {
		t.Fatal("memberships is null, want an empty list")
	}
	if ms, _ := d["memberships"].([]any); len(ms) != 0 || len(owned) != 1 {
		t.Fatalf("alice as the owner: licences %v, memberships %v; want it hers alone", owned, ms)
	}
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET email = ? WHERE id = ?", lic.Email, lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}

	for name, q := range map[string]string{
		"member's address":           strings.ToUpper(p.alice.Email),
		"an address with spaces":     "  " + p.alice.Email + " ",
		"member's whole key":         p.aliceKey,
		"member's key, in lowercase": strings.ToLower(p.aliceKey),
		"the whole licence key":      lic.LicenseKey,
	} {
		ls, _, err := f.s.ListLicenses(f.ctx, store.LicenseListFilter{Search: q, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, l := range ls {
			found = found || l.ID == lic.ID
		}
		if !found {
			t.Fatalf("searching by the %s did not find the licence", name)
		}
	}
	// A key is found only whole, never by a part that would let a key
	// be guessed a character at a time; and the term is literal: % and
	// _ match only themselves.
	for name, q := range map[string]string{
		"all but the last character of a member's key": p.aliceKey[:len(p.aliceKey)-1],
		"the start of the licence key":                 lic.LicenseKey[:12],
		"a lone _":                                     "_",
		"a lone %":                                     "%",
	} {
		ls, _, err := f.s.ListLicenses(f.ctx, store.LicenseListFilter{Search: q, Limit: 500})
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range ls {
			if l.ID == lic.ID {
				t.Fatalf("searching by %s found the licence", name)
			}
		}
	}

	// A removed member's address no longer does.
	ls, _, err := f.s.ListLicenses(f.ctx, store.LicenseListFilter{Search: p.gone.Email, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.ID == lic.ID && !strings.EqualFold(l.Email, p.gone.Email) {
			t.Fatal("a removed member's address still finds the licence")
		}
	}
}

// A customer's active seats are the memberships their detail lists: an
// invitation they have not accepted gives no access, and a seat at the
// licence's own address is the owner's, not a membership.
func TestCustomerActiveSeatsAreTheirMemberships(t *testing.T) {
	p := newPortalTeam(t)
	f, lic := p.f, p.f.licA
	h := &AdminHandler{Store: f.s}

	seats := func(email string) (float64, int) {
		t.Helper()
		u, err := f.s.FindUserByEmail(f.ctx, email)
		if err != nil {
			t.Fatalf("account for %s: %v", email, err)
		}
		code, out := f.call(h.GetUserDetail, http.MethodGet, "/admin/users/"+u.ID, "", nil, gin.Param{Key: "id", Value: u.ID})
		d, _ := out["data"].(map[string]any)
		if code != http.StatusOK {
			t.Fatalf("detail = %d %v", code, out)
		}
		n, _ := d["active_seats"].(float64)
		ms, _ := d["memberships"].([]any)
		return n, len(ms)
	}

	// The invitee already has an account, from a licence of their own.
	if err := f.s.UpsertUser(f.ctx, &model.User{Email: p.pending.Email, Name: "Pending"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who  string
		want float64
	}{{p.alice.Email, 1}, {p.pending.Email, 0}} {
		if n, ms := seats(c.who); n != c.want || ms != int(c.want) {
			t.Fatalf("%s: active seats %v, memberships %d; want %v of each", c.who, n, ms, c.want)
		}
	}

	// The licence's address becomes alice's: she owns it, and her seat
	// there is no membership.
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET email = ? WHERE id = ?", p.alice.Email, lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if n, ms := seats(p.alice.Email); n != 0 || ms != 0 {
		t.Fatalf("alice as the owner: active seats %v, memberships %d; want 0 of each", n, ms)
	}
}
