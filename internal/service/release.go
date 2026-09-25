package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/storage"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// maxFinalizeReadSize caps how many bytes FinalizeArtifact will stream
// from storage to compute the authoritative sha256. Anything larger
// is rejected — admin should split into multi-part artifacts. The cap
// also prevents an attacker who tampers with the bucket from forcing
// the server to spend unbounded I/O during finalize.
const maxFinalizeReadSize int64 = 8 << 30 // 8 GiB

// ReleaseService coordinates the release lifecycle:
//
//  1. CreateRelease(product, version, channel, notes) → draft release.
//  2. AddArtifact(releaseID, platform, ...) → presigned PUT URL for browser direct upload.
//  3. FinalizeArtifact(artifactID, sha256) → mark artifact ready.
//  4. (repeat 2+3 per platform)
//  5. Publish(releaseID) → signs all artifacts + flips state.
//  6. Yank/Unyank/Delete on release level.
//
// One release ↔ many platform-specific artifacts. Lifecycle (draft / published / yanked)
// is on the release; yanking pulls all platforms from the feed at once.
type ReleaseService struct {
	store       *store.Store
	storage     storage.Storage
	signer      *ReleaseSigningService // optional; nil = no auto-sign
	logger      *slog.Logger
	webhook     *WebhookService
	uploadTTL   time.Duration
	downloadTTL time.Duration
}

type ReleaseServiceConfig struct {
	Store       *store.Store
	Storage     storage.Storage
	Signer      *ReleaseSigningService
	Logger      *slog.Logger
	Webhook     *WebhookService
	UploadTTL   time.Duration
	DownloadTTL time.Duration
}

func NewReleaseService(c ReleaseServiceConfig) *ReleaseService {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Storage == nil {
		c.Storage = storage.Disabled{}
	}
	if c.UploadTTL <= 0 {
		c.UploadTTL = time.Hour
	}
	if c.DownloadTTL <= 0 {
		c.DownloadTTL = 10 * time.Minute
	}
	return &ReleaseService{
		store:       c.Store,
		storage:     c.Storage,
		signer:      c.Signer,
		logger:      c.Logger,
		webhook:     c.Webhook,
		uploadTTL:   c.UploadTTL,
		downloadTTL: c.DownloadTTL,
	}
}

// ─── Inputs / Outputs ───

// CreateReleaseInput describes a new logical release. No platform fields.
type CreateReleaseInput struct {
	ProductID    string
	Version      string
	Channel      string
	Name         string
	ReleaseNotes string
}

// AddArtifactInput attaches a platform-specific binary to an existing
// draft release.
type AddArtifactInput struct {
	ReleaseID    string
	Platform     string
	ContentType  string
	ExpectedSize int64
	Filename     string
}

// AddArtifactResult bundles the artifact row + the presigned upload URL.
type AddArtifactResult struct {
	Artifact  *model.ReleaseArtifact `json:"artifact"`
	UploadURL string                 `json:"upload_url"`
	ExpiresAt time.Time              `json:"expires_at"`
}

// FinalizeArtifactInput is sent after a successful PUT.
//
// ExpectedSHA256 is optional. The server reads the uploaded bytes back
// from storage and computes the authoritative sha256; ExpectedSHA256, if
// provided, is compared to the computed value and finalize fails with
// SHA256_MISMATCH on disagreement. Clients should NOT trust their own
// hash to become the published one — that would let a tampered upload
// be misrepresented.
type FinalizeArtifactInput struct {
	ArtifactID     string
	ExpectedSHA256 string
}

// DownloadInput is the license-gated download request.
type DownloadInput struct {
	LicenseKey string
	ProductID  string
	Version    string // empty = latest in channel
	Platform   string
	Channel    string
}

// DownloadResult is what's returned to the client on successful authz.
type DownloadResult struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Version   string    `json:"version"`
	Platform  string    `json:"platform"`
	SHA256    string    `json:"sha256"`
	FileSize  int64     `json:"file_size"`
}

// ─── Sentinel errors ───

var (
	ErrReleaseInvalidVersion    = errors.New("invalid version (must be semver: 1.2.3 or 1.2.3-beta.1)")
	ErrReleaseInvalidChannel    = errors.New("invalid channel (allowed: stable, beta, alpha, dev)")
	ErrReleaseUploadIncomplete  = errors.New("artifact upload not yet completed in storage")
	ErrReleaseLicenseInvalid    = errors.New("license is not in a state that permits download")
	ErrReleaseNoneAvailable     = errors.New("no published release matches the request")
	ErrReleaseYanked            = errors.New("requested release version was yanked")
	ErrReleaseNoArtifacts       = errors.New("release has no artifacts")
	ErrReleaseArtifactsNotReady = errors.New("one or more artifacts have not finished uploading")
)

func invalidPlatformError() error {
	return fmt.Errorf("invalid platform (allowed: %s)", strings.Join(allowedPlatforms, ", "))
}

// ─── Validation ───

var semverPattern = regexp.MustCompile(
	`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`,
)

var allowedPlatforms = []string{
	"darwin-arm64",
	"darwin-x64",
	"windows-arm64",
	"windows-x64",
	"linux-arm64",
	"linux-x64",
	"linux-armhf",
}

// AllowedPlatforms returns the canonical list of platform identifiers
// the release subsystem accepts. Exposed so handlers can render the
// allowed set in 400 responses without duplicating it.
func AllowedPlatforms() []string {
	out := make([]string, len(allowedPlatforms))
	copy(out, allowedPlatforms)
	return out
}

// platformAliases maps identifiers that updaters produce on their own
// ({{target}}-{{arch}} in Tauri, uname-style arch names) onto the
// canonical list. Public endpoints normalise before validating; the
// admin side keeps canonical names only.
var platformAliases = map[string]string{
	"darwin-aarch64":  "darwin-arm64",
	"darwin-x86_64":   "darwin-x64",
	"windows-x86_64":  "windows-x64",
	"windows-aarch64": "windows-arm64",
	"linux-x86_64":    "linux-x64",
	"linux-aarch64":   "linux-arm64",
	"linux-armv7":     "linux-armhf",
}

// NormalizePlatform returns the canonical identifier for p, or p
// unchanged when there is no alias (validation then rejects it).
func NormalizePlatform(p string) string {
	if canon, ok := platformAliases[strings.ToLower(p)]; ok {
		return canon
	}
	return p
}

// IsValidPlatform reports whether p is in the canonical platform list.
// Handler-side validation calls this so feed/download endpoints reject
// unknown platforms with 400 before reaching the service layer.
func IsValidPlatform(p string) bool {
	return slices.Contains(allowedPlatforms, p)
}

func validatePlatform(p string) error {
	if IsValidPlatform(p) {
		return nil
	}
	return invalidPlatformError()
}

// validateVersion combines our regex with semver.IsValid for full
// SemVer 2.0 compliance (rejects leading zeros etc.).
func validateVersion(v string) error {
	// Build metadata is ignored by semver ordering, so "1.2.3" and
	// "1.2.3+7" would be two rows that clients cannot tell apart.
	if strings.Contains(v, "+") {
		return errors.New("build metadata (+...) is not allowed in release versions")
	}
	if !semverPattern.MatchString(v) {
		return ErrReleaseInvalidVersion
	}
	if !semver.IsValid("v" + v) {
		return ErrReleaseInvalidVersion
	}
	return nil
}

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ─── CreateRelease ───

// CreateRelease creates a draft release with no artifacts. Admin attaches
// platform binaries via AddArtifact.
func (s *ReleaseService) CreateRelease(ctx context.Context, in CreateReleaseInput) (*model.Release, error) {
	if err := validateVersion(in.Version); err != nil {
		return nil, apperr.New(400, "INVALID_VERSION", err.Error())
	}
	if in.Channel == "" {
		in.Channel = model.ReleaseChannelStable
	}
	if !model.IsValidReleaseChannel(in.Channel) {
		return nil, apperr.New(400, "INVALID_CHANNEL", ErrReleaseInvalidChannel.Error())
	}
	// Channel is what clients subscribe to and the version's prerelease
	// tag is what they display; letting "2.0.0-beta.1" ship on stable
	// would make it the stable channel's latest by semver.
	if in.Channel == model.ReleaseChannelStable && semver.Prerelease("v"+in.Version) != "" {
		return nil, apperr.New(400, "INVALID_VERSION",
			"prerelease versions cannot ship on the stable channel — use beta, alpha or dev")
	}
	if in.ProductID == "" {
		return nil, apperr.New(400, "MISSING_PRODUCT", "product_id is required")
	}
	if len(in.Name) > 256 {
		return nil, apperr.New(400, "NAME_TOO_LONG", "name too long")
	}
	if len(in.ReleaseNotes) > 65536 {
		return nil, apperr.New(400, "NOTES_TOO_LONG", "release notes too long")
	}

	rel := &model.Release{
		ProductID:    in.ProductID,
		Version:      in.Version,
		Channel:      in.Channel,
		Name:         in.Name,
		ReleaseNotes: in.ReleaseNotes,
		Status:       model.ReleaseStatusDraft,
	}

	if err := s.store.CreateRelease(ctx, rel); err != nil {
		if errors.Is(err, store.ErrReleaseAlreadyExists) {
			return nil, apperr.New(409, "RELEASE_EXISTS", "a release for this product/version already exists")
		}
		return nil, apperr.Internal(err)
	}
	return rel, nil
}

// ─── AddArtifact ───

// AddArtifact attaches a platform-specific artifact to a draft release.
// Creates the artifact row + returns a presigned PUT URL for the admin
// browser to upload directly to storage.
func (s *ReleaseService) AddArtifact(ctx context.Context, in AddArtifactInput) (*AddArtifactResult, error) {
	if err := validatePlatform(in.Platform); err != nil {
		return nil, apperr.New(400, "INVALID_PLATFORM", err.Error())
	}
	if in.ContentType == "" {
		in.ContentType = "application/octet-stream"
	}

	// Look up the parent release for product slug + draft check.
	rel, err := s.store.FindReleaseByID(ctx, in.ReleaseID)
	if err != nil {
		if errors.Is(err, store.ErrReleaseNotFound) {
			return nil, apperr.New(404, "RELEASE_NOT_FOUND", "release not found")
		}
		return nil, apperr.Internal(err)
	}
	if rel.Status != model.ReleaseStatusDraft {
		return nil, apperr.New(409, "RELEASE_NOT_DRAFT", "artifacts can only be added to draft releases")
	}

	fileKey := buildFileKey(rel.ProductID, in.Platform, rel.Version, in.Filename)

	artifact := &model.ReleaseArtifact{
		ReleaseID:   in.ReleaseID,
		Platform:    in.Platform,
		FileKey:     fileKey,
		ContentType: in.ContentType,
	}
	if err := s.store.CreateArtifact(ctx, artifact); err != nil {
		switch {
		case errors.Is(err, store.ErrArtifactAlreadyExists):
			return nil, apperr.New(409, "ARTIFACT_EXISTS",
				"this release already has an artifact for this platform; delete it first")
		case errors.Is(err, store.ErrReleaseNotFound):
			return nil, apperr.New(404, "RELEASE_NOT_FOUND", "release not found")
		case errors.Is(err, store.ErrReleaseNotPublishable):
			// Race: caller saw draft via FindRelease, but a concurrent
			// Publish flipped the state before our INSERT could land.
			return nil, apperr.New(409, "RELEASE_NOT_DRAFT",
				"artifacts can only be added to draft releases")
		default:
			return nil, apperr.Internal(err)
		}
	}

	url, err := s.storage.PresignedPut(ctx, fileKey, in.ContentType, in.ExpectedSize, s.uploadTTL)
	if err != nil {
		// Roll back the artifact row to keep the release consistent.
		if _, delErr := s.store.DeleteArtifact(context.Background(), artifact.ID); delErr != nil {
			s.logger.Warn("artifact rollback failed", "artifact_id", artifact.ID, "err", delErr)
		}
		if errors.Is(err, storage.ErrStorageDisabled) {
			return nil, apperr.New(503, "STORAGE_DISABLED", "release storage is not configured on this server")
		}
		return nil, apperr.Internal(err)
	}

	return &AddArtifactResult{
		Artifact:  artifact,
		UploadURL: url,
		ExpiresAt: time.Now().Add(s.uploadTTL),
	}, nil
}

// FinalizeArtifact pins (size, sha256, content_type) onto the artifact
// after the admin's browser has PUT the file.
//
// The server is the authority on sha256: it streams the uploaded object
// back from storage and computes the digest itself. The client may
// optionally supply ExpectedSHA256, which is compared to the server's
// computed value and rejected with SHA256_MISMATCH on disagreement.
// This keeps a tampered upload (e.g. via a leaked presigned URL replay)
// from being labeled with attacker-chosen bytes' hash.
//
// Status on the parent release remains draft.
func (s *ReleaseService) FinalizeArtifact(ctx context.Context, in FinalizeArtifactInput) (*model.ReleaseArtifact, error) {
	if in.ExpectedSHA256 != "" && !sha256Pattern.MatchString(in.ExpectedSHA256) {
		return nil, apperr.New(400, "INVALID_SHA256",
			"expected_sha256 must be 64 lowercase hex characters")
	}

	a, err := s.store.FindArtifact(ctx, in.ArtifactID)
	if err != nil {
		if errors.Is(err, store.ErrArtifactNotFound) {
			return nil, apperr.New(404, "ARTIFACT_NOT_FOUND", "artifact not found")
		}
		return nil, apperr.Internal(err)
	}

	info, err := s.storage.Head(ctx, a.FileKey)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return nil, apperr.New(409, "UPLOAD_INCOMPLETE", ErrReleaseUploadIncomplete.Error())
		}
		return nil, apperr.Internal(err)
	}
	if info.Size > maxFinalizeReadSize {
		return nil, apperr.New(413, "ARTIFACT_TOO_LARGE",
			fmt.Sprintf("artifact size %d exceeds finalize limit %d", info.Size, maxFinalizeReadSize))
	}

	computed, err := s.computeArtifactSHA256(ctx, a.FileKey, info.Size)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return nil, apperr.New(409, "UPLOAD_INCOMPLETE", ErrReleaseUploadIncomplete.Error())
		}
		return nil, apperr.Internal(err)
	}

	if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != computed {
		return nil, apperr.New(409, "SHA256_MISMATCH",
			fmt.Sprintf("uploaded bytes hash to %s; client expected %s", computed, in.ExpectedSHA256))
	}

	contentType := info.ContentType
	if contentType == "" {
		contentType = a.ContentType
	}
	// The uploader controls the bucket's Content-Type and the column
	// has a 128-char CHECK; fall back rather than 500 on a long value.
	if len(contentType) > 128 {
		contentType = "application/octet-stream"
	}

	if err := s.store.UpdateArtifactFile(ctx, a.ID, a.FileKey, info.Size, computed, contentType); err != nil {
		switch {
		case errors.Is(err, store.ErrReleaseNotPublishable):
			return nil, apperr.New(409, "RELEASE_NOT_DRAFT", "artifact's release is no longer draft")
		case errors.Is(err, store.ErrArtifactNotFound):
			return nil, apperr.New(404, "ARTIFACT_NOT_FOUND", "artifact not found")
		default:
			return nil, apperr.Internal(err)
		}
	}

	out, err := s.store.FindArtifact(ctx, a.ID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return out, nil
}

// computeArtifactSHA256 streams the artifact body and returns its hex
// sha256. We bound the read at maxFinalizeReadSize+1 so a bucket-side
// truncation/extension can't be misrepresented to the user.
func (s *ReleaseService) computeArtifactSHA256(ctx context.Context, key string, expectedSize int64) (string, error) {
	body, err := s.storage.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer body.Close()

	h := sha256.New()
	limit := maxFinalizeReadSize + 1
	read, err := io.Copy(h, io.LimitReader(body, limit))
	// Drain any remainder so connection pooling can reuse the underlying
	// transport. Errors here are best-effort.
	_, _ = io.Copy(io.Discard, body)
	if err != nil {
		return "", fmt.Errorf("read artifact: %w", err)
	}
	if read > maxFinalizeReadSize {
		return "", fmt.Errorf("artifact exceeded finalize size cap (%d > %d)", read, maxFinalizeReadSize)
	}
	if read != expectedSize {
		return "", fmt.Errorf("artifact size mismatch: head=%d body=%d", expectedSize, read)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DeleteArtifact removes an artifact from a draft release + its storage object.
func (s *ReleaseService) DeleteArtifact(ctx context.Context, artifactID string) error {
	fileKey, err := s.store.DeleteArtifact(ctx, artifactID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrArtifactNotFound):
			return apperr.New(404, "ARTIFACT_NOT_FOUND", "artifact not found")
		case errors.Is(err, store.ErrReleaseNotPublishable):
			return apperr.New(409, "RELEASE_NOT_DRAFT", "artifacts of published / yanked releases cannot be deleted")
		default:
			return apperr.Internal(err)
		}
	}
	if fileKey != "" {
		if err := s.storage.Delete(ctx, fileKey); err != nil {
			s.logger.Warn("artifact storage cleanup failed", "artifact_id", artifactID, "file_key", fileKey, "err", err)
		}
	}
	return nil
}

// ─── Lifecycle ───

// Publish transitions draft → published. Pre-conditions:
//
//   - At least one artifact exists.
//   - All artifacts are uploaded (sha256 + file_key non-empty).
//   - Storage still holds each artifact.
//
// If a signing key is configured for the product, every artifact is signed
// before flipping state. Failure to sign any artifact aborts the publish.
func (s *ReleaseService) Publish(ctx context.Context, releaseID string) (*model.Release, error) {
	rel, err := s.store.FindReleaseByID(ctx, releaseID)
	if err != nil {
		if errors.Is(err, store.ErrReleaseNotFound) {
			return nil, apperr.New(404, "RELEASE_NOT_FOUND", "release not found")
		}
		return nil, apperr.Internal(err)
	}
	// Check the state before doing any work. The publish UPDATE below
	// enforces draft too, but only after the sign loop has already
	// rewritten every artifact's signature — which, after a key
	// rotation, silently re-signed shipped releases with the new key.
	if rel.Status != model.ReleaseStatusDraft {
		return nil, apperr.New(409, "NOT_PUBLISHABLE", "release must be a draft")
	}
	if len(rel.Artifacts) == 0 {
		return nil, apperr.New(409, "NO_ARTIFACTS", ErrReleaseNoArtifacts.Error())
	}
	for _, a := range rel.Artifacts {
		if !a.IsUploaded() {
			return nil, apperr.New(409, "ARTIFACTS_NOT_READY",
				fmt.Sprintf("artifact for platform %q has not finished uploading", a.Platform))
		}
	}

	// Verify each artifact still exists in storage.
	for _, a := range rel.Artifacts {
		exists, headErr := s.storage.Exists(ctx, a.FileKey)
		if headErr != nil && !errors.Is(headErr, storage.ErrStorageDisabled) {
			return nil, apperr.Internal(headErr)
		}
		if headErr == nil && !exists {
			return nil, apperr.New(409, "ARTIFACT_MISSING",
				fmt.Sprintf("artifact for platform %q is missing in storage", a.Platform))
		}
	}

	// All-or-nothing signing + secure-default enforcement.
	//
	// We probe the active key ONCE before iterating. Combined with the
	// product's RequireSigning flag, this gives four states:
	//
	//   1. RequireSigning=true  + active key  → sign all (mandatory).
	//   2. RequireSigning=true  + no key      → 409. Refuse to publish
	//      unsigned when the product policy demands sigs.
	//   3. RequireSigning=false + active key  → sign all (best effort).
	//   4. RequireSigning=false + no key      → unsigned (deliberate).
	//
	// (2) is the safe-default branch: a fresh product gets RequireSigning=true,
	// so an admin who hasn't configured keys can't accidentally ship
	// updaters that Sparkle/Tauri silently drop on the floor.
	prod, err := s.store.FindProductByID(ctx, rel.ProductID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	signRelease := false
	var signKey *model.ReleaseSigningKey
	if s.signer != nil {
		// Resolve the key once and sign every artifact with it — a
		// rotation landing mid-loop must not leave a release whose
		// platforms carry signatures from two different keys.
		key, err := s.signer.ActiveKey(ctx, rel.ProductID)
		switch {
		case err == nil:
			signRelease = true
			signKey = key
		case errors.Is(err, ErrSigningKeyMissing), errors.Is(err, ErrSigningDisabled):
			// No active key. RequireSigning decides whether this is fatal.
		default:
			return nil, apperr.Internal(err)
		}
	}
	if !signRelease && prod.RequireSigning {
		return nil, apperr.New(409, "SIGNING_NOT_CONFIGURED",
			"this product requires release signing — generate a signing key, "+
				"or set require_signing=false to ship unsigned releases")
	}
	if !signRelease {
		// Shipping unsigned (require_signing=false, no active key): drop
		// any signature a previous attempt left behind, so the feed does
		// not advertise a signature from a retired key that clients
		// then fail to verify. Signatures are frozen at publish.
		if err := s.store.ClearReleaseSignatures(ctx, releaseID); err != nil {
			if errors.Is(err, store.ErrReleaseNotPublishable) {
				// A concurrent publish got there first.
				return nil, apperr.New(409, "NOT_PUBLISHABLE", "release must be a draft")
			}
			return nil, apperr.Internal(err)
		}
	}
	if signRelease {
		for _, a := range rel.Artifacts {
			result, err := s.signer.SignArtifactWith(ctx, rel, a, signKey)
			if err != nil {
				switch {
				case errors.Is(err, ErrSigningKeyMissing):
					// Race: key disappeared between probe and sign.
					// Abort rather than half-sign the release.
					return nil, apperr.New(409, "SIGNING_KEY_MISSING",
						"active signing key disappeared during publish; retry")
				case errors.Is(err, ErrArtifactContentChanged):
					return nil, apperr.New(409, "ARTIFACT_MODIFIED",
						fmt.Sprintf("artifact for platform %q does not match the sha256 recorded at finalize — if it was just re-uploaded, retry publish; otherwise re-upload and finalize it", a.Platform))
				case errors.Is(err, ErrArtifactTooLargeToSign):
					return nil, apperr.New(413, "ARTIFACT_TOO_LARGE",
						fmt.Sprintf("artifact for platform %q exceeds the configured signing size limit", a.Platform))
				case errors.Is(err, ErrArtifactNotInStorage):
					return nil, apperr.New(409, "ARTIFACT_MISSING",
						fmt.Sprintf("artifact for platform %q is missing in storage", a.Platform))
				default:
					s.logger.Error("publish: sign failed",
						"release_id", rel.ID, "artifact_id", a.ID, "error", err)
					return nil, apperr.Internal(err)
				}
			}
			if storeErr := s.store.UpdateArtifactSignature(ctx, a.ID, result.Signature, result.SigningKeyID); storeErr != nil {
				if errors.Is(storeErr, store.ErrReleaseNotPublishable) {
					// Another publish won while we were signing.
					return nil, apperr.New(409, "NOT_PUBLISHABLE", "release must be a draft")
				}
				s.logger.Error("publish: store artifact signature failed",
					"release_id", rel.ID, "artifact_id", a.ID, "error", storeErr)
				return nil, apperr.Internal(storeErr)
			}
		}
	}

	// Pass signRelease as the requireSignatures flag — when we chose
	// to sign, the publish gate MUST see every artifact carrying a
	// fresh ed25519_sig + signing_key_id, otherwise we'd be shipping
	// a release that lost a signature to a concurrent re-upload
	// between our sign loop and the publish UPDATE.
	signKeyID := ""
	if signKey != nil {
		signKeyID = signKey.ID
	}
	if err := s.store.PublishRelease(ctx, releaseID, signRelease, signKeyID); err != nil {
		switch {
		case errors.Is(err, store.ErrReleaseNotFound):
			return nil, apperr.New(404, "RELEASE_NOT_FOUND", "release not found")
		case errors.Is(err, store.ErrReleaseNoArtifacts):
			return nil, apperr.New(409, "NO_ARTIFACTS", ErrReleaseNoArtifacts.Error())
		case errors.Is(err, store.ErrReleaseArtifactsNotReady):
			return nil, apperr.New(409, "ARTIFACTS_NOT_READY", ErrReleaseArtifactsNotReady.Error())
		case errors.Is(err, store.ErrReleaseArtifactsNotSigned):
			// A concurrent UpdateArtifactFile cleared a signature, or a
			// concurrent publish signed with a different key, after we
			// signed but before we committed. Retrying re-signs every
			// artifact with one key.
			return nil, apperr.New(409, "ARTIFACTS_NOT_SIGNED",
				"a concurrent upload cleared one or more signatures; retry publish")
		case errors.Is(err, store.ErrReleaseNotPublishable):
			return nil, apperr.New(409, "NOT_PUBLISHABLE", "release must be a draft")
		default:
			return nil, apperr.Internal(err)
		}
	}

	out, err := s.store.FindReleaseByID(ctx, releaseID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	s.dispatchWebhook(ctx, out, model.EventReleasePublished)
	return out, nil
}

// Yank transitions published → yanked at the release level. All artifacts
// are implicitly hidden from feeds.
func (s *ReleaseService) Yank(ctx context.Context, releaseID, reason string) (*model.Release, error) {
	if err := s.store.YankRelease(ctx, releaseID, reason); err != nil {
		switch {
		case errors.Is(err, store.ErrReleaseNotFound):
			return nil, apperr.New(404, "RELEASE_NOT_FOUND", "release not found")
		case errors.Is(err, store.ErrReleaseNotYankable):
			return nil, apperr.New(409, "NOT_YANKABLE", "only published releases can be yanked")
		default:
			return nil, apperr.Internal(err)
		}
	}
	rel, err := s.store.FindReleaseByID(ctx, releaseID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	s.dispatchWebhook(ctx, rel, model.EventReleaseYanked)
	return rel, nil
}

// Unyank yanked → published.
func (s *ReleaseService) Unyank(ctx context.Context, releaseID string) (*model.Release, error) {
	// The product may have changed type while this release was yanked
	// (the type change is allowed once every release is yanked).
	// Restoring it would put a published release on a product whose
	// feeds and downloads 404 — the state that guard exists to prevent.
	rel, err := s.store.FindReleaseByID(ctx, releaseID)
	if err != nil {
		if errors.Is(err, store.ErrReleaseNotFound) {
			return nil, apperr.New(404, "RELEASE_NOT_FOUND", "release not found")
		}
		return nil, apperr.Internal(err)
	}
	prod, err := s.store.FindProductByID(ctx, rel.ProductID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if !model.ProductSupports(prod.Type, model.CapReleases) {
		// NOT_UNYANKABLE, not INCOMPATIBLE_PRODUCT_TYPE: everywhere else
		// that code answers a request body asking for something the
		// product type cannot do, and it answers 400. Here the body is
		// fine and the product moved underneath the release, which is
		// the same shape as the other unyank refusals. One code must
		// mean one status, or a client cannot branch on it.
		return nil, apperr.New(409, "NOT_UNYANKABLE",
			"release management is not available for "+prod.Type+" products; change the product type back before unyanking")
	}
	if err := s.store.UnyankRelease(ctx, releaseID); err != nil {
		switch {
		case errors.Is(err, store.ErrReleaseNotFound):
			return nil, apperr.New(404, "RELEASE_NOT_FOUND", "release not found")
		case errors.Is(err, store.ErrReleaseNotYankable):
			return nil, apperr.New(409, "NOT_UNYANKABLE", "only yanked releases can be unyanked")
		default:
			return nil, apperr.Internal(err)
		}
	}
	rel, err = s.store.FindReleaseByID(ctx, releaseID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	s.dispatchWebhook(ctx, rel, model.EventReleaseUnyanked)
	return rel, nil
}

// DeleteDraft removes a draft release + all its artifacts (CASCADE) +
// cleans up storage objects best-effort.
func (s *ReleaseService) DeleteDraft(ctx context.Context, releaseID string) error {
	fileKeys, err := s.store.DeleteRelease(ctx, releaseID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrReleaseNotFound):
			return apperr.New(404, "RELEASE_NOT_FOUND", "release not found")
		case errors.Is(err, store.ErrReleaseNotDeletable):
			return apperr.New(409, "NOT_DELETABLE", "only draft releases can be hard-deleted; yank instead")
		default:
			return apperr.Internal(err)
		}
	}
	for _, key := range fileKeys {
		if err := s.storage.Delete(ctx, key); err != nil {
			s.logger.Warn("draft delete: storage cleanup failed", "release_id", releaseID, "file_key", key, "err", err)
		}
	}
	return nil
}

// ─── Download ───

// GenerateDownload returns a license-gated presigned GET URL for the
// (release, platform) artifact matching the request.
func (s *ReleaseService) GenerateDownload(ctx context.Context, in DownloadInput) (*DownloadResult, error) {
	in.Platform = NormalizePlatform(in.Platform)
	if err := validatePlatform(in.Platform); err != nil {
		return nil, apperr.New(400, "INVALID_PLATFORM", err.Error())
	}
	if in.Channel == "" {
		in.Channel = model.ReleaseChannelStable
	}
	if !model.IsValidReleaseChannel(in.Channel) {
		return nil, apperr.New(400, "INVALID_CHANNEL", ErrReleaseInvalidChannel.Error())
	}

	// Public SDK endpoint: collapse every "license unavailable" condition
	// (no such key / wrong product / SaaS product without releases /
	// suspended/revoked/expired) into one 404 LICENSE_NOT_FOUND so the
	// download URL can't be used as an oracle for license existence or
	// state. Paid users learn lifecycle state via email + the portal.
	lic, err := loadLicenseForSDK(ctx, s.store, in.LicenseKey, in.ProductID, model.CapReleases, true)
	if err != nil {
		return nil, err
	}

	var rel *model.Release
	if in.Version != "" {
		// Explicit version pin: hand back the requested release as-is
		// and let the artifact-platform check below produce a precise
		// PLATFORM_NOT_AVAILABLE error. The user pinned the version,
		// so silently falling back to an older version would be wrong.
		rel, err = s.findVersionWithFallback(ctx, lic.ProductID, in.Version, in.Channel)
	} else {
		// No pin: scope "latest" to releases the client can actually
		// install on its platform. Without this filter, a Windows
		// client asking for the latest would see 1.2.0 (macOS-only)
		// and get PLATFORM_NOT_AVAILABLE even though 1.1.0 (Windows)
		// is sitting right there.
		// A license whose maintenance period has ended still gets
		// "latest": the newest release published before the period
		// ended, so an old install keeps a version it may run.
		rel, err = s.findLatestPublished(ctx, lic.ProductID, in.Channel, in.Platform, lic.EffectiveUpdatesUntil())
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrReleaseYanked):
			return nil, apperr.New(410, "RELEASE_YANKED",
				"this release was yanked; pick a different version")
		case errors.Is(err, store.ErrReleaseNotFound), errors.Is(err, ErrReleaseNoneAvailable):
			return nil, apperr.New(404, "RELEASE_NOT_FOUND", ErrReleaseNoneAvailable.Error())
		default:
			return nil, apperr.Internal(err)
		}
	}
	if until := lic.EffectiveUpdatesUntil(); !releaseWithinUpdates(rel, until) {
		return nil, apperr.New(403, "UPDATES_EXPIRED",
			fmt.Sprintf("release %s was published after this license's update period ended on %s; renew to install it",
				rel.Version, until.Format("2006-01-02")))
	}

	// Find the artifact for the requested platform within this release.
	var artifact *model.ReleaseArtifact
	for _, a := range rel.Artifacts {
		if a.Platform == in.Platform && a.IsUploaded() {
			artifact = a
			break
		}
	}
	if artifact == nil {
		return nil, apperr.New(404, "PLATFORM_NOT_AVAILABLE",
			fmt.Sprintf("release %s has no artifact for platform %q", rel.Version, in.Platform))
	}

	url, err := s.storage.PresignedGet(ctx, artifact.FileKey, DownloadFilename(rel, artifact), s.downloadTTL)
	if err != nil {
		if errors.Is(err, storage.ErrStorageDisabled) {
			return nil, apperr.New(503, "STORAGE_DISABLED", "release storage is not configured")
		}
		return nil, apperr.Internal(err)
	}

	return &DownloadResult{
		URL:       url,
		ExpiresAt: time.Now().Add(s.downloadTTL),
		Version:   rel.Version,
		Platform:  artifact.Platform,
		SHA256:    artifact.SHA256,
		FileSize:  artifact.FileSize,
	}, nil
}

// ─── Latest / fallback (semver-aware) ───

// channelFallbackChain mirrors the visible-channels semantic: a beta user
// also sees stable releases (max wins), etc.
func channelFallbackChain(c string) []string {
	switch c {
	case model.ReleaseChannelDev:
		return []string{model.ReleaseChannelDev, model.ReleaseChannelAlpha, model.ReleaseChannelBeta, model.ReleaseChannelStable}
	case model.ReleaseChannelAlpha:
		return []string{model.ReleaseChannelAlpha, model.ReleaseChannelBeta, model.ReleaseChannelStable}
	case model.ReleaseChannelBeta:
		return []string{model.ReleaseChannelBeta, model.ReleaseChannelStable}
	default:
		return []string{model.ReleaseChannelStable}
	}
}

const latestComputeWindow = 1000

// releaseWithinUpdates reports whether a license whose maintenance
// period ends at updatesUntil may install rel. Nil means no limit. A
// release whose publish date is unknown cannot be shown to predate
// the cutoff, so under a cutoff it is not installable — the same
// answer the feed query gives, where a NULL date never satisfies
// "published before". The migration backfills the date on published
// rows, so this only guards against data written outside it.
func releaseWithinUpdates(rel *model.Release, updatesUntil *time.Time) bool {
	if updatesUntil == nil {
		return true
	}
	if rel.PublishedAt == nil {
		return false
	}
	return !rel.PublishedAt.After(*updatesUntil)
}

// findLatestPublished picks the highest semver among recently
// published releases the platform can install. With publishedBefore
// set, releases published after that instant are left out.
func (s *ReleaseService) findLatestPublished(ctx context.Context, productID, channel, platform string, publishedBefore *time.Time) (*model.Release, error) {
	chain := channelFallbackChain(channel)

	var pool []*model.Release
	for _, ch := range chain {
		batch, err := s.store.ListPublishedReleasesForFeed(ctx, productID, ch, platform, latestComputeWindow, publishedBefore)
		if err != nil {
			return nil, err
		}
		if len(batch) == latestComputeWindow {
			s.logger.Warn("release: feed window saturated; semver max may be missed",
				"product_id", productID, "channel", ch, "window", latestComputeWindow)
		}
		pool = append(pool, batch...)
	}
	if len(pool) == 0 {
		return nil, ErrReleaseNoneAvailable
	}
	sortBySemverDesc(pool)
	return pool[0], nil
}

func (s *ReleaseService) findVersionWithFallback(ctx context.Context, productID, version, channel string) (*model.Release, error) {
	rel, err := s.store.FindReleaseByVersion(ctx, productID, version)
	if err != nil {
		return nil, err
	}
	if rel.Status == model.ReleaseStatusYanked {
		return nil, ErrReleaseYanked
	}
	if rel.Status != model.ReleaseStatusPublished {
		return nil, ErrReleaseNoneAvailable
	}
	if !slices.Contains(channelFallbackChain(channel), rel.Channel) {
		return nil, ErrReleaseNoneAvailable
	}
	return rel, nil
}

func sortBySemverDesc(rels []*model.Release) {
	sort.SliceStable(rels, func(i, j int) bool {
		vi := semverNormalize(rels[i].Version)
		vj := semverNormalize(rels[j].Version)
		switch {
		case vi == "" && vj == "":
			return false
		case vi == "":
			return false
		case vj == "":
			return true
		}
		return semver.Compare(vi, vj) > 0
	})
}

// VersionAtMost reports whether v is a valid version no newer than
// ceiling; an unparsable v is treated as not exceeding anything.
func VersionAtMost(v, ceiling string) bool {
	nv, nc := semverNormalize(v), semverNormalize(ceiling)
	if nv == "" || nc == "" {
		return true
	}
	return semver.Compare(nv, nc) <= 0
}

func semverNormalize(v string) string {
	if v == "" {
		return ""
	}
	if v[0] != 'v' {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// ─── Feed access for renderer ───

const feedQueryTimeout = 3 * time.Second

// ListForFeed returns up to limit recent published releases (semver-sorted)
// for changelog-style feeds. Channel fallback applies. Each release has its
// artifacts preloaded.
//
// When platform is non-empty, the SQL layer drops releases that don't
// have an uploaded artifact for that platform BEFORE the limit is
// applied. That guarantees a client asking for limit=20 sees 20
// downloadable rows (when that many exist) rather than 20 raw rows
// thinned to a handful by post-filtering.
//
// publishedBefore limits the feed to releases inside a license's
// maintenance period; nil lists everything published.
func (s *ReleaseService) ListForFeed(ctx context.Context, productID, channel, platform string, limit int, publishedBefore *time.Time) ([]*model.Release, error) {
	if !model.IsValidReleaseChannel(channel) {
		return nil, ErrReleaseInvalidChannel
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	ctx, cancel := context.WithTimeout(ctx, feedQueryTimeout)
	defer cancel()

	chain := channelFallbackChain(channel)
	var pool []*model.Release
	for _, ch := range chain {
		batch, err := s.store.ListPublishedReleasesForFeed(ctx, productID, ch, platform, latestComputeWindow, publishedBefore)
		if err != nil {
			return nil, err
		}
		if len(batch) == latestComputeWindow {
			s.logger.Warn("release: feed window saturated; older releases may be hidden",
				"product_id", productID, "channel", ch, "window", latestComputeWindow)
		}
		pool = append(pool, batch...)
	}
	if len(pool) == 0 {
		return nil, nil
	}
	sortBySemverDesc(pool)
	if len(pool) > limit {
		pool = pool[:limit]
	}
	return pool, nil
}

// FeedCutoff resolves the maintenance cutoff a feed must honour for a
// license key: the end of the license's update period, or nil when
// the license may install everything. Every "license unavailable"
// condition collapses into LICENSE_NOT_FOUND, as on the download
// endpoint, so a feed URL cannot be used to probe keys.
func (s *ReleaseService) FeedCutoff(ctx context.Context, licenseKey, productID string) (*time.Time, error) {
	lic, err := loadLicenseForSDK(ctx, s.store, licenseKey, productID, model.CapReleases, true)
	if err != nil {
		return nil, err
	}
	return lic.EffectiveUpdatesUntil(), nil
}

// FeedCutoffForToken is FeedCutoff for an updater that identified
// itself with a signed license token instead of the license key. The
// cutoff comes from the license row, never from the token: a token
// signed before a renewal, a revocation or a plan change must not
// decide what the feed shows.
func (s *ReleaseService) FeedCutoffForToken(ctx context.Context, licenseID, productID string) (*time.Time, error) {
	lic, err := s.store.FindLicenseByID(ctx, licenseID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, licenseNotFound()
		}
		return nil, apperr.Internal(err)
	}
	if lic.ProductID != productID ||
		lic.Product == nil || !model.ProductSupports(lic.Product.Type, model.CapReleases) ||
		!IsLicenseUsable(lic) {
		return nil, licenseNotFound()
	}
	return lic.EffectiveUpdatesUntil(), nil
}

// ─── Helpers ───

// buildFileKey produces the storage path for an artifact.
//
//	releases/{product_id}/{version}/{platform}{ext}
//
// The product ID, not the slug: slugs can be renamed and reused, and
// a second product taking over an old slug would otherwise presign
// PUTs (and deletes) onto the first product's published binaries.
func buildFileKey(productID, platform, version, originalFilename string) string {
	ext := normalizeExt(originalFilename)
	if !validExtension(ext) {
		ext = ""
	}
	return fmt.Sprintf("releases/%s/%s/%s%s",
		safeKeyComponent(productID),
		safeKeyComponent(version),
		safeKeyComponent(platform),
		ext)
}

func normalizeExt(filename string) string {
	lower := strings.ToLower(filename)
	for _, compound := range []string{".tar.gz", ".tar.xz", ".tar.bz2", ".tar.zst"} {
		if strings.HasSuffix(lower, compound) {
			return compound
		}
	}
	return strings.ToLower(path.Ext(filename))
}

func validExtension(ext string) bool {
	switch ext {
	case ".dmg", ".pkg", ".zip", ".tar.gz", ".tar.xz", ".tar.bz2", ".tar.zst",
		".tgz", ".tar", ".gz", ".xz",
		".exe", ".msi", ".nupkg",
		".appimage", ".deb", ".rpm", ".snap", ".flatpak",
		".bin", ".app", ".7z":
		return true
	}
	return false
}

var keyComponentBad = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeKeyComponent(s string) string {
	return keyComponentBad.ReplaceAllString(s, "_")
}

// DownloadFilename returns the human-friendly Content-Disposition filename
// for an artifact. Format: {product-or-name}-{version}-{platform}{ext}
func DownloadFilename(rel *model.Release, a *model.ReleaseArtifact) string {
	if rel == nil || a == nil {
		return ""
	}
	ext := normalizeExt(a.FileKey)
	prefix := rel.Name
	if prefix == "" {
		prefix = "release"
	}
	return fmt.Sprintf("%s-%s-%s%s",
		safeKeyComponent(prefix),
		safeKeyComponent(rel.Version),
		safeKeyComponent(a.Platform),
		ext)
}

// IsLicenseUsable mirrors LicenseService.assertUsable's logic for download
// gating. Exported so handlers can share the same check.
func IsLicenseUsable(lic *model.License) bool {
	now := time.Now()
	graceDays := 7
	if lic.Plan != nil {
		graceDays = lic.Plan.GraceDays
	}
	switch lic.Status {
	case model.StatusActive, model.StatusTrialing, model.StatusPastDue:
		if lic.ValidUntil == nil {
			return true
		}
		if now.Before(*lic.ValidUntil) {
			return true
		}
		grace := time.Duration(graceDays) * 24 * time.Hour
		return now.Before(lic.ValidUntil.Add(grace))
	case model.StatusCanceled:
		return lic.ValidUntil != nil && now.Before(*lic.ValidUntil)
	default:
		return false
	}
}

// dispatchWebhook fires a release-level lifecycle event.
func (s *ReleaseService) dispatchWebhook(ctx context.Context, rel *model.Release, event string) {
	if s.webhook == nil || rel == nil {
		return
	}
	platforms := make([]string, 0, len(rel.Artifacts))
	for _, a := range rel.Artifacts {
		platforms = append(platforms, a.Platform)
	}
	s.webhook.Dispatch(ctx, rel.ProductID, event, map[string]any{
		"release_id": rel.ID,
		"version":    rel.Version,
		"channel":    rel.Channel,
		"platforms":  platforms,
	})
}
