package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A release goes live only while the key it was signed with is active:
// rotated out or deactivated meanwhile, the publish is refused and the
// release stays a draft.
func TestPublishRelease_RefusedOnceTheKeyIsRetired(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	signedWith := func(artifactID string) string {
		t.Helper()
		art, err := s.FindArtifact(ctx, artifactID)
		if err != nil || art.SigningKeyID == "" {
			t.Fatalf("artifact = %+v, %v; want it signed", art, err)
		}
		return art.SigningKeyID
	}

	relID, artID := setupSignedDraft(t, s, ctx, true)
	keyID := signedWith(artID)
	if err := s.DeactivateSigningKey(ctx, keyID, "retired"); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishRelease(ctx, relID, true, keyID); !errors.Is(err, store.ErrReleaseSigningKeyRetired) {
		t.Fatalf("publish signed by a retired key = %v, want ErrReleaseSigningKeyRetired", err)
	}
	if rel, _ := s.FindReleaseByID(ctx, relID); rel.Status != model.ReleaseStatusDraft {
		t.Fatalf("release = %s, want still a draft", rel.Status)
	}

	// With the key active, the same publish goes through.
	relID, artID = setupSignedDraft(t, s, ctx, true)
	if err := s.PublishRelease(ctx, relID, true, signedWith(artID)); err != nil {
		t.Fatalf("publish signed by the active key = %v", err)
	}
}

// Of two calls removing or changing the same row, the second learns it
// is gone, so it neither reports a change nor hands out what opens
// nothing.
func TestSecondRemovalFindsItGone(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")

	prod := &model.Product{Name: "Gone " + suffix, Slug: "gone-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "gone-p-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 3, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "gone-" + suffix + "@example.com",
		LicenseKey: "KG-GONE-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	for name, remove := range map[string]func(context.Context, string) error{
		"DeleteActivation": s.DeleteActivation, "DeleteActivationByID": s.DeleteActivationByID,
	} {
		act := &model.Activation{LicenseID: lic.ID, Identifier: name + "-pc", IdentifierType: "device"}
		if err := s.CreateActivation(ctx, act); err != nil {
			t.Fatal(err)
		}
		if err := remove(ctx, act.ID); err != nil {
			t.Fatalf("%s = %v", name, err)
		}
		if err := remove(ctx, act.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s again = %v, want sql.ErrNoRows", name, err)
		}
	}

	whProd := &model.Product{Name: "Gone Hooks " + suffix, Slug: "gone-hooks-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, whProd); err != nil {
		t.Fatal(err)
	}
	wh := &model.Webhook{ProductID: whProd.ID, URL: "https://gone.example.com/hook", Secret: "s",
		Events: []string{"license.created"}, Active: true}
	if err := s.CreateWebhook(ctx, wh); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWebhook(ctx, wh.ID); err != nil {
		t.Fatal(err)
	}
	// Regression: a second delete of the same webhook (the loser of a
	// concurrent-delete race) used to report success with no error,
	// which let the handler write a second "deleted" audit log entry
	// for a delete that never happened.
	if err := s.DeleteWebhook(ctx, wh.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleting a webhook twice = %v, want sql.ErrNoRows", err)
	}

	raw := store.GenerateRawAPIKey()
	ak := &model.APIKey{Name: "gone-" + suffix, Prefix: raw[:12], Scopes: []string{"admin"}}
	if err := s.CreateAPIKey(ctx, ak, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAPIKey(ctx, ak.ID); err != nil {
		t.Fatal(err)
	}
	next := store.GenerateRawAPIKey()
	if err := s.RotateAPIKey(ctx, ak.ID, next, next[:12]); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rotating a deleted key = %v, want sql.ErrNoRows", err)
	}

	_, artID := setupSignedDraft(t, s, ctx, true)
	art, err := s.FindArtifact(ctx, artID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeactivateSigningKey(ctx, art.SigningKeyID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeactivateSigningKey(ctx, art.SigningKeyID, ""); !errors.Is(err, store.ErrSigningKeyInactive) {
		t.Fatalf("deactivating a key twice = %v, want ErrSigningKeyInactive", err)
	}
}

func signingKey(productID, tag string) *model.ReleaseSigningKey {
	blob := make([]byte, 64)
	for i := range blob {
		blob[i] = byte(i + 1)
	}
	return &model.ReleaseSigningKey{ProductID: productID, PublicKey: "pk-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-" + tag,
		PrivateKeyEncrypted: blob, Active: true}
}

// A key made while a rotation runs is one the rotation sees and
// retires, or one that finds the rotation's key active: never a
// rotation failing on it, and never two active keys.
func TestCreateAndRotateSigningKeyTakeTurns(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	for i := range 40 {
		suffix := time.Now().Format("150405.000000000")
		prod := &model.Product{Name: "Keys " + suffix, Slug: "keys-" + suffix, Type: "desktop"}
		if err := s.CreateProduct(ctx, prod); err != nil {
			t.Fatal(err)
		}
		errs := make(chan error, 2)
		go func() { errs <- s.CreateSigningKey(ctx, signingKey(prod.ID, "c"+suffix)) }()
		go func() { errs <- s.RotateSigningKey(ctx, signingKey(prod.ID, "r"+suffix), "") }()
		for range 2 {
			if err := <-errs; err != nil && !errors.Is(err, store.ErrSigningKeyAlreadyActive) {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		var active int
		if err := s.DB.NewRaw("SELECT count(*) FROM release_signing_keys WHERE product_id = ? AND active", prod.ID).Scan(ctx, &active); err != nil || active != 1 {
			t.Fatalf("round %d: active keys = %d, %v; want 1", i, active, err)
		}
		var rotated int
		if err := s.DB.NewRaw("SELECT count(*) FROM release_signing_keys WHERE product_id = ? AND public_key LIKE 'pk-%-r%'", prod.ID).Scan(ctx, &rotated); err != nil || rotated != 1 {
			t.Fatalf("round %d: the rotation's key was not made (%d, %v)", i, rotated, err)
		}
	}
}

// An artifact deleted while a publish signs it: the two take the
// release first and the artifact after, so they never deadlock, and the
// signer learns the artifact is gone instead of being told it signed it.
func TestDeleteArtifactWhileSigning(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	for i := range 60 {
		_, artID := setupSignedDraft(t, s, ctx, true)
		art, err := s.FindArtifact(ctx, artID)
		if err != nil {
			t.Fatal(err)
		}
		var delErr, signErr error
		done := make(chan struct{}, 2)
		go func() { _, delErr = s.DeleteArtifact(ctx, artID); done <- struct{}{} }()
		go func() {
			signErr = s.UpdateArtifactSignature(ctx, artID, "sig-again", art.SigningKeyID, "")
			done <- struct{}{}
		}()
		<-done
		<-done
		if delErr != nil {
			t.Fatalf("round %d: delete = %v", i, delErr)
		}
		if signErr != nil && !errors.Is(signErr, store.ErrArtifactNotFound) {
			t.Fatalf("round %d: signing = %v, want nil or ErrArtifactNotFound", i, signErr)
		}
		if _, err := s.FindArtifact(ctx, artID); err == nil {
			t.Fatalf("round %d: artifact still there after its delete", i)
		}
	}
}

// The signer waiting on the release lock while the artifact is deleted
// reads it gone once the delete commits: ErrArtifactNotFound, not a
// signature written to nothing.
func TestSignatureForAnArtifactDeletedWhileWaiting(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	relID, artID := setupSignedDraft(t, s, ctx, true)
	art, err := s.FindArtifact(ctx, artID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, "SELECT id FROM releases WHERE id = ? FOR UPDATE", relID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM release_artifacts WHERE id = ?", artID); err != nil {
		t.Fatal(err)
	}
	signed := make(chan error, 1)
	go func() { signed <- s.UpdateArtifactSignature(ctx, artID, "sig-late", art.SigningKeyID, "") }()
	time.Sleep(300 * time.Millisecond) // the signer is now waiting on the release
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-signed; !errors.Is(err, store.ErrArtifactNotFound) {
		t.Fatalf("signing an artifact deleted while it waited = %v, want ErrArtifactNotFound", err)
	}
}

// Two edits of one webhook, each from what its caller read, keep both:
// a receiver switched off is not switched back on by an edit of its URL.
func TestWebhookEditsKeepEachOther(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Hooks " + suffix, Slug: "hooks-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	w := &model.Webhook{ProductID: prod.ID, URL: "https://old.example.com/hook", Secret: "s", Events: []string{"license.created"}, Active: true}
	if err := s.CreateWebhook(ctx, w); err != nil {
		t.Fatal(err)
	}
	a, _ := s.FindWebhookByID(ctx, w.ID)
	b, _ := s.FindWebhookByID(ctx, w.ID)
	a.URL = "https://new.example.com/hook"
	b.Active = false
	if err := s.UpdateWebhook(ctx, a, "url"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWebhook(ctx, b, "active"); err != nil {
		t.Fatal(err)
	}
	if b.URL != "https://new.example.com/hook" {
		t.Errorf("the second edit read back url %q, want the first edit's", b.URL)
	}
	got, err := s.FindWebhookByID(ctx, w.ID)
	if err != nil || got.URL != "https://new.example.com/hook" || got.Active {
		t.Fatalf("webhook = %+v, %v; want the new URL and inactive", got, err)
	}
	if err := s.DeleteWebhook(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWebhook(ctx, a, "active"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("editing a deleted webhook = %v, want sql.ErrNoRows", err)
	}
}

// A signature is written only for the file that was signed: one for a
// file the artifact no longer points at is refused.
func TestSignatureForAReplacedArtifact(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, artID := setupSignedDraft(t, s, ctx, true)
	art, err := s.FindArtifact(ctx, artID)
	if err != nil {
		t.Fatal(err)
	}
	signed := *art
	if err := s.UpdateArtifactFile(ctx, artID, "releases/x/replaced.dmg", 7,
		"aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999", "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateArtifactSignatureOf(ctx, artID, signed.FileKey, signed.SHA256, "sig-old", art.SigningKeyID, ""); !errors.Is(err, store.ErrArtifactReplaced) {
		t.Fatalf("signature of the file replaced = %v, want ErrArtifactReplaced", err)
	}
	now, _ := s.FindArtifact(ctx, artID)
	if err := s.UpdateArtifactSignatureOf(ctx, artID, now.FileKey, now.SHA256, "sig-new", art.SigningKeyID, ""); err != nil {
		t.Fatalf("signature of the file it points at = %v", err)
	}
}

// A subscription row names its licence's plan, and follows it: when a
// downgrade moves the licence, and when the move is undone. A plan the
// licence left can then be deleted. Rows a downgrade left behind before
// this version are repaired when the plan they still name is deleted.
func TestSubscriptionRowFollowsItsLicencesPlan(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Sub " + suffix, Slug: "sub-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := func(slug string) *model.Plan {
		p := &model.Plan{ProductID: prod.ID, Name: slug, Slug: slug + "-" + suffix, LicenseType: "subscription",
			LicenseModel: "standard", Active: true}
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	big, small, other := plan("big"), plan("small"), plan("other")
	lic := &model.License{ProductID: prod.ID, PlanID: big.ID, Email: "sub-" + suffix + "@example.com",
		LicenseKey: "KG-SUB-" + suffix, Status: model.StatusActive, Quantity: 1}
	if err := s.CreateLicenseWithSubscription(ctx, lic, big); err != nil {
		t.Fatal(err)
	}
	subPlan := func() string {
		t.Helper()
		var id string
		if err := s.DB.NewRaw("SELECT plan_id FROM subscriptions WHERE license_id = ?", lic.ID).Scan(ctx, &id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	lowered := store.LicenseTerms{PlanID: small.ID, Quantity: 1}
	prev, err := s.LowerLicenseTerms(ctx, lic.ID, lowered, time.Minute, func(store.InUse) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := subPlan(); got != small.ID {
		t.Fatalf("subscription after the downgrade names %s, want the plan the licence moved to", got)
	}
	if err := s.RestoreLicenseTerms(ctx, lic.ID, lowered, prev); err != nil {
		t.Fatal(err)
	}
	if got := subPlan(); got != big.ID {
		t.Fatalf("subscription after the downgrade was undone names %s, want the plan it went back to", got)
	}

	// A row left behind by an older version: the licence moved, the row
	// did not. Deleting the plan it still names repairs it first.
	if _, err := s.DB.NewRaw("UPDATE licenses SET plan_id = ? WHERE id = ?", other.ID, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlanUnlessFallback(ctx, big.ID); err != nil {
		t.Fatalf("deleting a plan only a stale subscription row named = %v", err)
	}
	if got := subPlan(); got != other.ID {
		t.Fatalf("subscription after the repair names %s, want its licence's plan", got)
	}
}

// A publish that waits for a re-finalize of one of its artifacts judges
// the artifacts as that finalize left them: the signature it cleared is
// missing, and the release is refused rather than published unsigned
// with bytes nobody signed.
func TestPublishWaitingOnARefinalizeSeesItsChanges(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	relID, artID := setupSignedDraft(t, s, ctx, true)
	art, err := s.FindArtifact(ctx, artID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, "SELECT id FROM releases WHERE id = ? FOR UPDATE", relID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE release_artifacts SET file_key = 'releases/x/new.dmg',
		ed25519_sig = '', tauri_signature = '', signing_key_id = NULL WHERE id = ?`, artID); err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	go func() { published <- s.PublishRelease(ctx, relID, true, art.SigningKeyID) }()
	time.Sleep(300 * time.Millisecond) // the publish is now waiting on the release
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-published; !errors.Is(err, store.ErrReleaseArtifactsNotSigned) {
		t.Fatalf("publish past a re-finalize = %v, want ErrReleaseArtifactsNotSigned", err)
	}
	if rel, _ := s.FindReleaseByID(ctx, relID); rel.Status != model.ReleaseStatusDraft {
		t.Fatalf("release = %s, want still a draft", rel.Status)
	}
}

// A release published meanwhile is not publishable, whatever became of
// its key since: the caller is told that, not to retry for the key.
func TestPublishOfAPublishedReleaseSaysSo(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	relID, artID := setupSignedDraft(t, s, ctx, true)
	art, err := s.FindArtifact(ctx, artID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishRelease(ctx, relID, true, art.SigningKeyID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeactivateSigningKey(ctx, art.SigningKeyID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishRelease(ctx, relID, true, art.SigningKeyID); !errors.Is(err, store.ErrReleaseNotPublishable) {
		t.Fatalf("publishing a published release = %v, want ErrReleaseNotPublishable", err)
	}
}

// Each replacement reports the key it replaced, as stored when it took
// the release lock: of two finalizes, the second is told of the first's
// copy, not of the key both read before either wrote.
func TestReplaceArtifactFileReportsWhatItReplaced(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, artID := setupSignedDraft(t, s, ctx, false)
	before, err := s.FindArtifact(ctx, artID)
	if err != nil {
		t.Fatal(err)
	}
	sha := "aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999"
	first, err := s.ReplaceArtifactFile(ctx, artID, "releases/x/first.dmg", 1, sha, "application/octet-stream")
	if err != nil || first != before.FileKey {
		t.Fatalf("first replacement = %q, %v; want %q", first, err, before.FileKey)
	}
	second, err := s.ReplaceArtifactFile(ctx, artID, "releases/x/second.dmg", 1, sha, "application/octet-stream")
	if err != nil || second != "releases/x/first.dmg" {
		t.Fatalf("second replacement = %q, %v; want the first one's key", second, err)
	}
}
