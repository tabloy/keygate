package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"golang.org/x/mod/semver"

	"github.com/tabloy/keygate/internal/model"
)

// Release-related sentinel errors.
var (
	ErrReleaseNotFound       = errors.New("release not found")
	ErrReleaseAlreadyExists  = errors.New("release with same product/version already exists")
	ErrReleaseNotPublishable = errors.New("release is not in a publishable state")
	ErrReleaseNotYankable    = errors.New("release is not in a yankable state")
	// ErrReleaseNotDeletable: only DRAFT releases can be hard-deleted.
	// Published / yanked stay in the DB so audit trail is preserved.
	ErrReleaseNotDeletable = errors.New("only draft releases can be deleted")

	// Atomic-publish preconditions surfaced by PublishRelease when its
	// inline gate rejects the transition. The gate evaluates them in a
	// single SQL UPDATE, so these errors describe state observed AT the
	// moment of the update — concurrent AddArtifact / FinalizeArtifact
	// cannot create a stale "checked then violated" race.
	ErrReleaseNoArtifacts        = errors.New("release has no artifacts")
	ErrReleaseArtifactsNotReady  = errors.New("one or more artifacts have not finished uploading")
	ErrReleaseArtifactsNotSigned = errors.New("one or more artifacts are missing required signatures")
	// ErrReleaseSigningKeyRetired: the key the release was signed with
	// was rotated out or deactivated before the publish committed.
	ErrReleaseSigningKeyRetired = errors.New("the signing key was retired during publish")

	// Artifact-related.
	ErrArtifactNotFound = errors.New("release artifact not found")
	// ErrArtifactReplaced: a signature was for a file the artifact no
	// longer points at (finalized again while it was being signed).
	ErrArtifactReplaced      = errors.New("release artifact was replaced while it was being signed")
	ErrArtifactAlreadyExists = errors.New("release already has an artifact for this platform")
)

// ReleaseFilter narrows ListReleases queries.
type ReleaseFilter struct {
	Search    string
	Version   string
	ProductID string
	Channel   string
	Status    string
	Limit     int
	Offset    int
}

// MaxFeedListLimit caps how many published rows the store will return to
// the service for semver-max computation (see service.latestComputeWindow).
// 5000 covers any realistic single-product release backlog.
const MaxFeedListLimit = 5000

// ─── Release CRUD ───

// ProductHasEverPublishedRelease reports whether any release of the
// product has ever appeared in a feed — published, or yanked after
// being published. A product without one has never handed out a
// presigned artifact URL, so gating its feeds takes effect at once;
// with one, links from before the gate may still be usable.
func (s *Store) ProductHasEverPublishedRelease(ctx context.Context, productID string) (bool, error) {
	return ProductHasEverPublishedReleaseIn(ctx, s.DB, productID)
}

// FeedDrainLeftIn reports the full drain a newly gated product waits
// out and how much of it is left. What the public feed handed out
// keeps working after the gate goes up: a shared cache may serve the
// feed for its max-age, and every copy carries presigned artifact
// URLs valid for ttl — the lifetime this replica signs them with — so
// the wait is the longer of the two. A product that never published a
// release handed out nothing.
func FeedDrainLeftIn(ctx context.Context, db bun.IDB, prod *model.Product, ttl time.Duration) (drain, left time.Duration, err error) {
	if prod.FeedGatedAt == nil {
		return 0, 0, nil
	}
	published, err := ProductHasEverPublishedReleaseIn(ctx, db, prod.ID)
	if err != nil || !published {
		return 0, 0, err
	}
	// The bound is read on every check, not cached at start: a
	// replica configured with a shorter lifetime must not call links
	// another one signed for longer expired, and one that raises the
	// bound while this replica runs is picked up at once.
	recorded, err := DurationSettingIn(ctx, db, SettingFeedURLTTLBound, ttl)
	if err != nil {
		return 0, 0, err
	}
	if recorded > ttl {
		ttl = recorded
	}
	drain = max(ttl, model.FeedPublicMaxAge)
	// Elapsed time comes from the database clock too, for the same
	// reason the stamp does: replicas need not agree on the time, and
	// the one that decides is the one that wrote it.
	var elapsed float64
	if err := db.NewRaw("SELECT EXTRACT(EPOCH FROM (clock_timestamp() - ?))", *prod.FeedGatedAt).Scan(ctx, &elapsed); err != nil {
		return 0, 0, err
	}
	return drain, drain - time.Duration(elapsed*float64(time.Second)), nil
}

// FeedsCanEnforcePeriodsIn reports whether a finite update period
// written for this product would be enforceable now, and why not when
// it would not. Two conditions: the update feeds must require the
// license key (or the product serves no feeds), and what the public
// feed handed out before the gate went up must have expired — a copy
// of it, or a presigned link inside one, serves releases past any
// cutoff written while it is still usable.
//
// feedURLTTL is how long this replica signs those links for
// (STORAGE_FEED_URL_TTL); the wait is the longer of it and the bound
// the install has recorded. The 60-second cache window is only the
// floor — waiting that alone would call a link signed for a day
// expired after a minute.
//
// Callers hold the product's feed gating lock (FeedGatingLockIn) so
// the answer still holds when they write.
func FeedsCanEnforcePeriodsIn(ctx context.Context, db bun.IDB, productID string, feedURLTTL time.Duration) (bool, string, error) {
	prod, err := FindProductByIDIn(ctx, db, productID)
	if err != nil {
		return false, "", err
	}
	if !model.ProductSupports(prod.Type, model.CapReleases) {
		return true, "", nil
	}
	if !prod.FeedLicenseRequired {
		return false, "the product's update feeds do not require the license key", nil
	}
	_, left, err := FeedDrainLeftIn(ctx, db, prod, feedURLTTL)
	if err != nil {
		return false, "", err
	}
	if left > 0 {
		return false, fmt.Sprintf("the update feeds were gated recently; what the public feed handed out stays usable for another %s", left.Round(time.Second)), nil
	}
	return true, "", nil
}

// ProductHasEverPublishedReleaseIn answers on a caller's transaction.
func ProductHasEverPublishedReleaseIn(ctx context.Context, db bun.IDB, productID string) (bool, error) {
	return db.NewSelect().Model((*model.Release)(nil)).
		Where("product_id = ? AND status IN (?, ?)", productID,
			model.ReleaseStatusPublished, model.ReleaseStatusYanked).Exists(ctx)
}

// CreateRelease inserts a new release in the draft state. No artifacts —
// callers add platform-specific artifacts via AddArtifact.
// CreateRelease inserts a release, while the product's type has
// releases (ErrProductNoReleases otherwise, see ProductShapeLockIn).
func (s *Store) CreateRelease(ctx context.Context, r *model.Release) error {
	if r.ID == "" {
		r.ID = newID()
	}
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if err := releasesAllowedIn(ctx, tx, r.ProductID); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(r).Exec(ctx)
		return err
	})
	if isUniqueViolation(err) {
		return ErrReleaseAlreadyExists
	}
	return err
}

// GetReleaseProductID resolves which product a release belongs to
// in one column lookup. Used by handlers that only need the FK for
// API-key product scoping; FindReleaseByID would eager-load product
// + artifacts unnecessarily.
func (s *Store) GetReleaseProductID(ctx context.Context, id string) (string, error) {
	var pid string
	err := s.DB.NewSelect().Model((*model.Release)(nil)).
		Column("product_id").
		Where("id = ?", id).
		Scan(ctx, &pid)
	return pid, err
}

// FindReleaseByID loads a release with its product + artifacts.
func (s *Store) FindReleaseByID(ctx context.Context, id string) (*model.Release, error) {
	r := new(model.Release)
	err := s.DB.NewSelect().Model(r).
		Relation("Product").
		Relation("Artifacts").
		Where("release.id = ?", id).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReleaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// FindReleaseByVersion finds an exact (product, version) tuple. The unique
// constraint excludes platform — same artifact bytes for the same version
// would conflict across platforms otherwise.
func (s *Store) FindReleaseByVersion(ctx context.Context, productID, version string) (*model.Release, error) {
	r := new(model.Release)
	err := s.DB.NewSelect().Model(r).
		Relation("Artifacts").
		Where("release.product_id = ? AND release.version = ?", productID, version).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReleaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ListReleases returns releases matching the filter (with their artifacts).
// Defaults: limit=50 (clamped to [1,200]), offset clamped to >=0.
//
// bun requires `Model(&dst)` (pointer to slice) for the Relation eager-load
// to wire the join correctly — using `Model((*Release)(nil))` + Scan(ctx, &dst)
// silently drops the Relation join, returning an empty slice.
func (s *Store) ListReleases(ctx context.Context, f ReleaseFilter) ([]*model.Release, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := max(f.Offset, 0)

	var out []*model.Release
	q := s.DB.NewSelect().Model(&out).
		Relation("Product").
		Relation("Artifacts").
		Limit(limit).
		Offset(offset)
	if f.Search != "" {
		q = q.OrderExpr("CASE WHEN release.version = ? THEN 0 ELSE 1 END", f.Search)
	}
	q = q.OrderExpr("release.created_at DESC, release.id DESC")

	q = applyReleaseFilters(q, f)

	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// CountReleases returns total rows matching the filter (ignores limit/offset).
func (s *Store) CountReleases(ctx context.Context, f ReleaseFilter) (int, error) {
	q := s.DB.NewSelect().Model((*model.Release)(nil))
	q = applyReleaseFilters(q, f)
	return q.Count(ctx)
}

func applyReleaseFilters(q *bun.SelectQuery, f ReleaseFilter) *bun.SelectQuery {
	if f.Version != "" {
		q = q.Where("release.version = ?", f.Version)
	}
	if f.Search != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Search)
		q = q.Where("release.version ILIKE ?", "%"+escaped+"%")
	}
	if f.ProductID != "" {
		q = q.Where("release.product_id = ?", f.ProductID)
	}
	if f.Channel != "" {
		q = q.Where("release.channel = ?", f.Channel)
	}
	if f.Status != "" {
		q = q.Where("release.status = ?", f.Status)
	}
	return q
}

// LatestPublished is the highest published version of one product on
// one channel.
type LatestPublished struct {
	ProductID string `json:"product_id"`
	Channel   string `json:"channel"`
	Version   string `json:"version"`
}

// LatestPublishedVersions returns, for each product (or the one named)
// and channel, its highest published version by semver. The release
// list is paged and filtered; this is what it is compared with, so a
// draft below what is already out says so whatever page it is on.
func (s *Store) LatestPublishedVersions(ctx context.Context, productID string) ([]LatestPublished, error) {
	var rows []LatestPublished
	q := s.DB.NewSelect().Model((*model.Release)(nil)).
		Column("product_id", "channel", "version").
		Where("status = ?", model.ReleaseStatusPublished)
	if productID != "" {
		q = q.Where("product_id = ?", productID)
	}
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	best := map[[2]string]string{}
	for _, r := range rows {
		k := [2]string{r.ProductID, r.Channel}
		if cur, ok := best[k]; !ok || semver.Compare("v"+r.Version, "v"+cur) > 0 {
			best[k] = r.Version
		}
	}
	out := make([]LatestPublished, 0, len(best))
	for k, v := range best {
		out = append(out, LatestPublished{ProductID: k[0], Channel: k[1], Version: v})
	}
	slices.SortFunc(out, func(a, b LatestPublished) int {
		return strings.Compare(a.ProductID+"|"+a.Channel, b.ProductID+"|"+b.Channel)
	})
	return out, nil
}

// PublishRelease atomically transitions draft → published. The UPDATE
// inlines every invariant a published release must satisfy:
//
//   - status is still 'draft'
//   - the release has at least one artifact
//   - every artifact has non-empty file_key AND non-empty sha256
//   - when requireSignatures is true, every artifact has non-empty
//     ed25519_sig AND signing_key_id (defends the race where a
//     concurrent UpdateArtifactFile cleared a signature between
//     the service-layer sign loop and this commit)
//
// Concurrent AddArtifact / FinalizeArtifact / DeleteArtifact requests
// committed before this UPDATE evaluates are visible to the subqueries;
// uncommitted ones are blocked from racing past the gate because every
// mutating artifact path takes a row-level lock on the parent release
// (see CreateArtifact, DeleteArtifact, UpdateArtifactFile).
//
// On 0 rows affected, diagnoseUnpublishable inspects current state and
// returns the most specific sentinel.
// signingKeyID, when non-empty, is the key the caller signed with:
// every artifact must carry a signature from exactly that key. Two
// publishes overlapping a key rotation can otherwise interleave their
// per-artifact signature writes and ship a release whose platforms
// verify against different public keys.
//
// The key must still be active when the release goes live, and is held
// so until the publish commits: a rotation or deactivation waits for it,
// or, coming first, refuses the publish (ErrReleaseSigningKeyRetired).
// Otherwise a release could go out signed by a key already withdrawn,
// which clients holding the published public key reject.
func (s *Store) PublishRelease(ctx context.Context, id string, requireSignatures bool, signingKeyID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	// The release row first, in its own statement, as every artifact
	// writer takes it (UpdateArtifactFile, UpdateArtifactSignature,
	// DeleteArtifact). The gate below then runs on a snapshot taken once
	// the lock is held. As one statement that waited for the lock, it
	// re-checked only the release row and judged the artifacts by the
	// snapshot from before it waited: a finalize that cleared a signature
	// meanwhile left a release published unsigned, its bytes never
	// signed.
	var status string
	if err := tx.NewRaw(`SELECT status FROM releases WHERE id = ? FOR UPDATE`, id).Scan(ctx, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReleaseNotFound
		}
		return err
	}
	if status != model.ReleaseStatusDraft {
		// Published by another call meanwhile: that, not the key.
		return ErrReleaseNotPublishable
	}
	if signingKeyID != "" {
		// No such key matches no artifact's signing_key_id, which the
		// gate below refuses as unsigned.
		var active bool
		err := tx.NewRaw(`SELECT active FROM release_signing_keys WHERE id = ? FOR SHARE`, signingKeyID).Scan(ctx, &active)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && !active {
			return ErrReleaseSigningKeyRetired
		}
	}
	res, err := tx.NewRaw(`
		UPDATE releases
		SET status = ?, published_at = now(), updated_at = now()
		WHERE id = ?
		  AND status = ?
		  AND EXISTS (SELECT 1 FROM release_artifacts WHERE release_id = releases.id)
		  AND NOT EXISTS (
		      SELECT 1 FROM release_artifacts
		      WHERE release_id = releases.id
		        AND (file_key = '' OR sha256 = '')
		  )
		  AND (
		      NOT ?  -- requireSignatures off → skip the sig check
		      OR NOT EXISTS (
		          SELECT 1 FROM release_artifacts
		          WHERE release_id = releases.id
		            AND (ed25519_sig = '' OR signing_key_id IS NULL
		                 OR (? <> '' AND signing_key_id <> ?))
		      )
		  )
	`, model.ReleaseStatusPublished, id, model.ReleaseStatusDraft, requireSignatures, signingKeyID, signingKeyID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		tx.Rollback() //nolint:errcheck
		return s.diagnoseUnpublishable(ctx, id, requireSignatures, signingKeyID)
	}
	return tx.Commit()
}

// diagnoseUnpublishable returns the most specific reason a publish gate
// rejected a release. Run only after PublishRelease's UPDATE returned 0
// rows; the answer can be slightly stale (state may have moved again
// between the UPDATE and these reads) but the user-facing error code is
// still correct for "the gate rejected your request".
func (s *Store) diagnoseUnpublishable(ctx context.Context, id string, requireSignatures bool, signingKeyID string) error {
	var status string
	if err := s.DB.NewRaw(`SELECT status FROM releases WHERE id = ?`, id).
		Scan(ctx, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReleaseNotFound
		}
		return err
	}
	if status != model.ReleaseStatusDraft {
		return ErrReleaseNotPublishable
	}

	var total int
	if err := s.DB.NewRaw(
		`SELECT COUNT(*) FROM release_artifacts WHERE release_id = ?`, id,
	).Scan(ctx, &total); err != nil {
		return err
	}
	if total == 0 {
		return ErrReleaseNoArtifacts
	}

	var unfinished int
	if err := s.DB.NewRaw(
		`SELECT COUNT(*) FROM release_artifacts WHERE release_id = ? AND (file_key = '' OR sha256 = '')`, id,
	).Scan(ctx, &unfinished); err != nil {
		return err
	}
	if unfinished > 0 {
		return ErrReleaseArtifactsNotReady
	}

	// Signature check is conditional. If the caller asked for sigs
	// and at least one artifact is missing one, that's the precise
	// reason — surface it instead of the generic "not publishable".
	if requireSignatures {
		var unsigned int
		if err := s.DB.NewRaw(
			`SELECT COUNT(*) FROM release_artifacts WHERE release_id = ?
			   AND (ed25519_sig = '' OR signing_key_id IS NULL OR (? <> '' AND signing_key_id <> ?))`,
			id, signingKeyID, signingKeyID,
		).Scan(ctx, &unsigned); err != nil {
			return err
		}
		if unsigned > 0 {
			return ErrReleaseArtifactsNotSigned
		}
	}

	// All preconditions pass now but didn't at UPDATE time — race
	// resolved itself; surface a generic publishable error so caller
	// can retry.
	return ErrReleaseNotPublishable
}

// YankRelease transitions a published release → yanked with a reason.
// All artifacts are implicitly hidden from feeds (they're queried via the
// release status, not their own state).
func (s *Store) YankRelease(ctx context.Context, id, reason string) error {
	res, err := s.DB.NewUpdate().Model((*model.Release)(nil)).
		Set("status = ?, yanked_reason = ?, yanked_at = now(), updated_at = now()",
			model.ReleaseStatusYanked, reason).
		Where("id = ? AND status = ?", id, model.ReleaseStatusPublished).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.classifyReleaseUpdateMiss(ctx, id, ErrReleaseNotYankable)
	}
	return nil
}

// UnyankRelease undoes a yank: yanked → published.
// UnyankRelease puts a yanked release back on sale, while the product's
// type has releases (ErrProductNoReleases otherwise): the type may
// change once every release is yanked, and one restored on a type
// without releases would be unreachable.
func (s *Store) UnyankRelease(ctx context.Context, id string) error {
	var missed bool
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		var productID string
		if err := tx.NewRaw("SELECT product_id FROM releases WHERE id = ?", id).Scan(ctx, &productID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrReleaseNotFound
			}
			return err
		}
		if err := releasesAllowedIn(ctx, tx, productID); err != nil {
			return err
		}
		res, err := tx.NewUpdate().Model((*model.Release)(nil)).
			Set("status = ?, yanked_reason = '', yanked_at = NULL, updated_at = now()",
				model.ReleaseStatusPublished).
			Where("id = ? AND status = ?", id, model.ReleaseStatusYanked).
			Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		missed = n == 0
		return nil
	})
	if err != nil {
		return err
	}
	if missed {
		return s.classifyReleaseUpdateMiss(ctx, id, ErrReleaseNotYankable)
	}
	return nil
}

// UpdateReleaseNotes updates user-facing display fields. Allowed in any
// state — typos in published notes happen.
func (s *Store) UpdateReleaseNotes(ctx context.Context, id, name, notes string) error {
	res, err := s.DB.NewUpdate().Model((*model.Release)(nil)).
		Set("name = ?, release_notes = ?, updated_at = now()", name, notes).
		Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrReleaseNotFound
	}
	return nil
}

// DeleteRelease removes a draft release and CASCADE-removes its artifacts.
// Returns the file_keys of all artifacts so the caller can clean up storage.
// Only DRAFT releases are deletable; published / yanked must stay.
func (s *Store) DeleteRelease(ctx context.Context, id string) (fileKeys []string, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	if err := tx.NewRaw(
		`SELECT status FROM releases WHERE id = ? FOR UPDATE`, id,
	).Scan(ctx, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrReleaseNotFound
		}
		return nil, err
	}
	if status != model.ReleaseStatusDraft {
		return nil, ErrReleaseNotDeletable
	}

	// Collect file_keys before cascade delete drops the artifact rows.
	var keys []string
	if err := tx.NewRaw(
		`SELECT file_key FROM release_artifacts WHERE release_id = ? AND file_key <> ''`, id,
	).Scan(ctx, &keys); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if _, err := tx.NewDelete().Model((*model.Release)(nil)).
		Where("id = ? AND status = ?", id, model.ReleaseStatusDraft).Exec(ctx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return keys, nil
}

// ListPublishedReleasesForFeed returns published, non-yanked releases for
// (product, channel), most recent first, up to limit. Each release has its
// artifacts preloaded.
//
// When platform is non-empty, only releases that have an uploaded
// artifact for that platform are returned. This filter is pushed into
// SQL (EXISTS subquery) so that `limit` counts platform-compatible
// releases — not raw rows. Without this, a Tauri client requesting
// "latest for windows-x64" could see 204 even when older versions
// shipped a windows-x64 build, because the latest release happened to
// only ship macOS. Platform filtering at the handler level after a
// LIMIT can't fix that; only pushing the predicate down can.
//
// `uploaded` is encoded as `file_key <> ” AND sha256 <> ”` to mirror
// the model's IsUploaded() check.
//
// publishedBefore, when set, keeps only releases published at or
// before that instant. It belongs in the query, not in a filter on
// the result: for a license whose update period ended long ago, the
// newest limit rows may all be too new, and the entitled releases
// would never be seen.
func (s *Store) ListPublishedReleasesForFeed(ctx context.Context, productID, channel, platform string, limit int, publishedBefore *time.Time) ([]*model.Release, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > MaxFeedListLimit {
		limit = MaxFeedListLimit
	}
	// bun requires Model(&dst) (pointer to slice) for the Relation
	// eager-load to wire up correctly — Model((*Release)(nil)) +
	// Scan(ctx, &dst) silently drops the join.
	var out []*model.Release
	q := s.DB.NewSelect().Model(&out).
		Relation("Artifacts").
		Where("release.product_id = ? AND release.channel = ? AND release.status = ?",
			productID, channel, model.ReleaseStatusPublished)
	if platform != "" {
		q = q.Where(`EXISTS (
            SELECT 1 FROM release_artifacts ra
            WHERE ra.release_id = release.id
              AND ra.platform   = ?
              AND ra.file_key  <> ''
              AND ra.sha256    <> ''
        )`, platform)
	}
	if publishedBefore != nil {
		q = q.Where("release.published_at <= ?", *publishedBefore)
	}
	err := q.OrderExpr("release.published_at DESC, release.id DESC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ─── Release Artifact CRUD ───

// CreateArtifact attaches a new artifact to a release.
//
// Wrapped in a tx with `SELECT ... FOR UPDATE` on the parent release so
// PublishRelease's atomic gate cannot flip the release to 'published'
// between the status check and the artifact INSERT. Either path is
// consistent:
//
//   - We acquire FOR UPDATE first → publisher's UPDATE waits on us →
//     after our commit publisher sees the new (pending) artifact and
//     fails its NOT EXISTS gate.
//   - Publisher's UPDATE acquires the row-X lock first → we wait → after
//     publisher commits, status='published' so we return ErrReleaseNotPublishable.
//
// Returns ErrArtifactAlreadyExists if the (release_id, platform) tuple
// already exists.
func (s *Store) CreateArtifact(ctx context.Context, a *model.ReleaseArtifact) error {
	if a.ID == "" {
		a.ID = newID()
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	if err := tx.NewRaw(
		`SELECT status FROM releases WHERE id = ? FOR UPDATE`, a.ReleaseID,
	).Scan(ctx, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReleaseNotFound
		}
		return err
	}
	if status != model.ReleaseStatusDraft {
		return ErrReleaseNotPublishable
	}

	if _, err := tx.NewInsert().Model(a).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return ErrArtifactAlreadyExists
		}
		return err
	}
	return tx.Commit()
}

// FindArtifact returns an artifact by ID.
func (s *Store) FindArtifact(ctx context.Context, id string) (*model.ReleaseArtifact, error) {
	a := new(model.ReleaseArtifact)
	err := s.DB.NewSelect().Model(a).
		Where("release_artifact.id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// FindArtifactByPlatform returns the artifact for (release, platform).
func (s *Store) FindArtifactByPlatform(ctx context.Context, releaseID, platform string) (*model.ReleaseArtifact, error) {
	a := new(model.ReleaseArtifact)
	err := s.DB.NewSelect().Model(a).
		Where("release_artifact.release_id = ? AND release_artifact.platform = ?", releaseID, platform).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// UpdateArtifactFile sets the artifact upload metadata after a successful
// PUT to storage. Allowed only when the parent release is DRAFT — once
// published, the (sha256, sig) become contracts with already-distributed
// clients.
//
// Wrapped in a tx with `SELECT … FOR UPDATE` on the parent release so a
// concurrent PublishRelease can't observe the artifact's new file_key
// while we're mid-rewrite, AND so we don't clear the ed25519_sig
// between a sign-loop's UpdateArtifactSignature and the corresponding
// publish commit. Without this, finalizing an upload during the small
// sign-then-publish window would race the publisher into shipping an
// empty-sig artifact.
func (s *Store) UpdateArtifactFile(ctx context.Context, id, fileKey string, size int64, sha256, contentType string) error {
	_, err := s.ReplaceArtifactFile(ctx, id, fileKey, size, sha256, contentType)
	return err
}

// ReplaceArtifactFile is UpdateArtifactFile, and returns the file key the
// artifact pointed at until this write, read under the release lock, so
// the caller removes the object this write replaced. One read before the
// lock may be another finalize's, recorded since: two finalizes at once
// each removed the key they had read, and the first one's copy was left
// with nothing pointing at it.
func (s *Store) ReplaceArtifactFile(ctx context.Context, id, fileKey string, size int64, sha256, contentType string) (replaced string, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck

	var status, releaseID string
	if err := tx.NewRaw(`
		SELECT r.status, r.id
		FROM release_artifacts ra
		JOIN releases r ON r.id = ra.release_id
		WHERE ra.id = ?
		FOR UPDATE OF r
	`, id).Scan(ctx, &status, &releaseID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrArtifactNotFound
		}
		return "", err
	}
	if status != model.ReleaseStatusDraft {
		return "", ErrReleaseNotPublishable
	}
	// Under the release lock, which every artifact writer takes first:
	// what is read now is what this write replaces.
	if err := tx.NewRaw(`SELECT file_key FROM release_artifacts WHERE id = ?`, id).Scan(ctx, &replaced); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrArtifactNotFound
		}
		return "", err
	}

	// The artifact was read before the release lock was held: deleted
	// while this waited for it, nothing is updated, and that is a miss.
	if err := deletedOne(tx.NewRaw(`
		UPDATE release_artifacts
		SET file_key = ?, file_size = ?, sha256 = ?, content_type = ?,
		    ed25519_sig = '', tauri_signature = '', signing_key_id = NULL, updated_at = now()
		WHERE id = ?
	`, fileKey, size, sha256, contentType, id).Exec(ctx)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrArtifactNotFound
		}
		return "", err
	}
	return replaced, tx.Commit()
}

// UpdateArtifactSignature stores the Ed25519 signature for an artifact.
// Bound to a specific signing key by ID so post-rotation clients can
// pick the correct public key.
//
// INTENTIONALLY lock-free. A concurrent UpdateArtifactFile (which DOES
// take FOR UPDATE on the parent release) can clear the sig right after
// this write, which would leave a published-without-sig artifact —
// EXCEPT that PublishRelease's atomic UPDATE re-validates `ed25519_sig
// != ” AND signing_key_id IS NOT NULL` at commit time when the caller
// passes `requireSignatures=true`. So the worst outcome of a race here
// is the publish UPDATE rejecting with ErrReleaseArtifactsNotSigned;
// the caller (Publish) re-runs the sign loop and retries.
// ClearReleaseSignatures drops every signature on a draft release. Used
// when a release is deliberately published unsigned, so nothing from an
// earlier signing attempt ships.
//
// Locks the release row first: under READ COMMITTED a plain UPDATE with
// a status subquery can see "draft" while a concurrent publish is
// committing, and then erase signatures from a release that is live.
func (s *Store) ClearReleaseSignatures(ctx context.Context, releaseID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	if err := tx.NewRaw(`SELECT status FROM releases WHERE id = ? FOR UPDATE`, releaseID).
		Scan(ctx, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReleaseNotFound
		}
		return err
	}
	if status != model.ReleaseStatusDraft {
		return ErrReleaseNotPublishable
	}
	if _, err := tx.NewRaw(`
		UPDATE release_artifacts
		SET ed25519_sig = '', tauri_signature = '', signing_key_id = NULL, updated_at = now()
		WHERE release_id = ?
	`, releaseID).Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateArtifactSignature writes a signature onto an artifact whose
// release is still a draft. A shipped release's signatures are
// immutable: a publish that lost the race to another publish (or ran
// after a rotation) must not rewrite what clients already verify.
//
// Same shape as DeleteArtifact: FOR UPDATE on the parent release so this
// waits behind an in-flight PublishRelease and then sees its committed
// status, instead of racing it on an MVCC snapshot.
func (s *Store) UpdateArtifactSignature(ctx context.Context, id, sig, signingKeyID, tauriSig string) error {
	return s.UpdateArtifactSignatureOf(ctx, id, "", "", sig, signingKeyID, tauriSig)
}

// UpdateArtifactSignatureOf is UpdateArtifactSignature for the file the
// signer read: written only while the artifact still points at fileKey
// with that sha256, ErrArtifactReplaced otherwise. A finalize landing
// while a publish signed the artifact would otherwise be given the
// signature of the file it replaced, and clients would reject the
// release. An empty fileKey writes whatever the artifact points at.
func (s *Store) UpdateArtifactSignatureOf(ctx context.Context, id, fileKey, sha256, sig, signingKeyID, tauriSig string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var releaseStatus string
	if err := tx.NewRaw(`
		SELECT r.status
		FROM release_artifacts ra
		JOIN releases r ON r.id = ra.release_id
		WHERE ra.id = ?
		FOR UPDATE OF r
	`, id).Scan(ctx, &releaseStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrArtifactNotFound
		}
		return err
	}
	if releaseStatus != model.ReleaseStatusDraft {
		return ErrReleaseNotPublishable
	}
	// Deleted while this waited for the release lock (see
	// UpdateArtifactFile): the publish signing it learns it is gone,
	// rather than going live without that platform.
	q := tx.NewUpdate().Model((*model.ReleaseArtifact)(nil)).
		Set("ed25519_sig = ?, signing_key_id = ?, tauri_signature = ?, updated_at = now()", sig, signingKeyID, tauriSig).
		Where("id = ?", id)
	if fileKey != "" {
		q = q.Where("file_key = ? AND sha256 = ?", fileKey, sha256)
	}
	if err := deletedOne(q.Exec(ctx)); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var still bool
		if err := tx.NewRaw("SELECT EXISTS (SELECT 1 FROM release_artifacts WHERE id = ?)", id).Scan(ctx, &still); err != nil {
			return err
		}
		if still {
			return ErrArtifactReplaced
		}
		return ErrArtifactNotFound
	}
	return tx.Commit()
}

// ListArtifactsMissingTauriSignature returns signed artifacts that have
// no Tauri signature yet, with their release loaded, oldest first. These
// were signed before Tauri signatures were stored.
func (s *Store) ListArtifactsMissingTauriSignature(ctx context.Context, limit, offset int) ([]*model.ReleaseArtifact, error) {
	var out []*model.ReleaseArtifact
	err := s.DB.NewSelect().Model(&out).
		Relation("Release").
		Where("release_artifact.ed25519_sig <> ''").
		Where("release_artifact.signing_key_id IS NOT NULL").
		Where("release_artifact.tauri_signature = ''").
		Order("release_artifact.created_at ASC", "release_artifact.id ASC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	return out, err
}

// SetArtifactTauriSignature fills in the Tauri signature of an artifact
// that has none, without touching its Ed25519 signature. Allowed on
// published releases: it adds a second encoding of the signature the
// release already ships, made with the same key.
func (s *Store) SetArtifactTauriSignature(ctx context.Context, id, signingKeyID, tauriSig string) error {
	_, err := s.DB.NewUpdate().Model((*model.ReleaseArtifact)(nil)).
		Set("tauri_signature = ?, updated_at = now()", tauriSig).
		Where("id = ?", id).
		Where("signing_key_id = ?", signingKeyID).
		Where("tauri_signature = ''").
		Exec(ctx)
	return err
}

// DeleteArtifact removes an artifact from a draft release. Returns the
// file_key for storage cleanup. Cannot delete artifacts of a published or
// yanked release.
func (s *Store) DeleteArtifact(ctx context.Context, id string) (fileKey string, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck

	// The release is locked first and the artifact only by the delete,
	// in the order UpdateArtifactSignature and UpdateArtifactFile take
	// them; locking both at once took them in whatever order the plan
	// joined them, and could deadlock with a publish signing it. The
	// file key is read by the delete, as it is once the lock is held.
	var releaseStatus string
	if err := tx.NewRaw(`
		SELECT r.status
		FROM release_artifacts ra
		JOIN releases r ON r.id = ra.release_id
		WHERE ra.id = ?
		FOR UPDATE OF r
	`, id).Scan(ctx, &releaseStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrArtifactNotFound
		}
		return "", err
	}
	if releaseStatus != model.ReleaseStatusDraft {
		return "", ErrReleaseNotPublishable
	}

	if err := tx.NewRaw(`DELETE FROM release_artifacts WHERE id = ? RETURNING file_key`, id).
		Scan(ctx, &fileKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrArtifactNotFound
		}
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return fileKey, nil
}

// ─── Helpers ───

// classifyReleaseUpdateMiss returns the right sentinel when an UPDATE
// affected 0 rows: ErrReleaseNotFound vs the supplied state-mismatch error.
func (s *Store) classifyReleaseUpdateMiss(ctx context.Context, id string, defaultErr error) error {
	var dummy string
	err := s.DB.NewRaw("SELECT id FROM releases WHERE id = ?", id).Scan(ctx, &dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReleaseNotFound
	}
	if err != nil {
		return err
	}
	return defaultErr
}
