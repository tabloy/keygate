package handler

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/storage"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// ReleasePublicHandler exposes the endpoints clients use:
//
//	POST /api/v1/license/download                              license-gated download URL
//	GET  /api/v1/releases/:product_slug/feed.xml               Sparkle appcast (per-platform)
//	GET  /api/v1/releases/:product_slug/feed.json              Velopack feed (per-platform)
//	GET  /api/v1/releases/:product_slug/upgrade.json           Tauri manifest (single latest)
//
// Auth model: feeds are PUBLIC for every channel — stable, beta,
// alpha, dev. Industry convention (Sparkle / Tauri / npm / GitHub
// Releases): trust = the artifact's Ed25519 signature, NOT URL secrecy.
// Genuinely private builds belong on internal CI/CD distribution
// (private R2 bucket, TestFlight, Firebase App Distribution); they
// don't ship through these feeds at all.
//
// License gating happens at app start (the SDK calls /license/verify),
// not at update time. License rotation never bricks installed clients.
type ReleasePublicHandler struct {
	svc         *service.ReleaseService
	store       *store.Store
	storage     storage.Storage
	logger      *slog.Logger
	baseURL     string
	downloadTTL time.Duration
	feedTTL     time.Duration
	verifyKey   ed25519.PublicKey
}

type ReleasePublicConfig struct {
	Service     *service.ReleaseService
	Store       *store.Store
	Storage     storage.Storage
	Logger      *slog.Logger
	BaseURL     string
	DownloadTTL time.Duration
	// VerifyKey checks the signed license tokens updaters may send
	// instead of the key. Zero value: only the key is accepted.
	VerifyKey ed25519.PublicKey
	// FeedTTL is the lifetime of enclosure URLs inside public feeds.
	// Sparkle shows the appcast and the user may click Install much
	// later, so this is deliberately long; the licence-gated
	// /license/download keeps the short DownloadTTL.
	FeedTTL time.Duration
}

func NewReleasePublicHandler(c ReleasePublicConfig) *ReleasePublicHandler {
	if c.Storage == nil {
		c.Storage = storage.Disabled{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.DownloadTTL <= 0 {
		c.DownloadTTL = 10 * time.Minute
	}
	if c.FeedTTL <= 0 {
		c.FeedTTL = 24 * time.Hour
	}
	return &ReleasePublicHandler{
		svc:         c.Service,
		store:       c.Store,
		storage:     c.Storage,
		logger:      c.Logger,
		baseURL:     c.BaseURL,
		downloadTTL: c.DownloadTTL,
		feedTTL:     c.FeedTTL,
		verifyKey:   c.VerifyKey,
	}
}

// POST /api/v1/license/download
//
// Body: { license_key, platform, version?, channel? }
func (h *ReleasePublicHandler) Download(c *gin.Context) {
	var req struct {
		LicenseKey string `json:"license_key" binding:"required"`
		Platform   string `json:"platform" binding:"required"`
		Version    string `json:"version"`
		Channel    string `json:"channel"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "license_key and platform are required")
		return
	}
	productID, _ := c.Get("product_id")
	out, err := h.svc.GenerateDownload(c.Request.Context(), service.DownloadInput{
		LicenseKey: req.LicenseKey,
		ProductID:  str(productID),
		Version:    req.Version,
		Platform:   req.Platform,
		Channel:    req.Channel,
	})
	if err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, out)
}

// ─── Feed endpoints (one per format) ───

// FeedPublicMaxAge is how long shared caches may serve the public
// feed. Switching a product's feed gate on does not reach those
// caches, so a bounded update period is refused until this much time
// has passed since the switch (feedNotGated).
const FeedPublicMaxAge = model.FeedPublicMaxAge

// feedRequest captures the validated context for a single feed request.
type feedRequest struct {
	product  *model.Product
	platform string
	channel  string
	limit    int
	// publishedBefore is the maintenance cutoff of the license the
	// updater identified itself with; nil for the public feed.
	publishedBefore *time.Time
	// licensed marks a feed built for one license: not cacheable by
	// shared caches, since another key may see a different list.
	licensed bool
}

// parseFeedRequest validates path + query params. All channels (stable
// / beta / alpha / dev) are public — trust is established by the
// artifact's ed25519 signature, not by URL secrecy. On any error the
// response is already written and ok=false.
func (h *ReleasePublicHandler) parseFeedRequest(c *gin.Context) (req feedRequest, ok bool) {
	// Every feed response varies on the key header, and a response
	// built for a key — including a 404 for a bad key and Tauri's
	// 204 — must never come out of a shared cache for another
	// client. Set before any early return.
	c.Header("Vary", "X-License-Key, X-License-Token")
	// The license key is a long-lived credential that also opens
	// activate, verify and download, so it is taken from the header
	// only: a key in the URL is written to CDN, proxy and access logs
	// before the request reaches this process, and no response header
	// takes that back. An updater that can only put things in the URL
	// (Sparkle's feedParametersForUpdater) sends the signed token
	// instead — it expires, and it is accepted on feeds alone.
	key := strings.TrimSpace(c.GetHeader("X-License-Key"))
	tok := strings.TrimSpace(c.Query("license_token"))
	if tok == "" {
		tok = strings.TrimSpace(c.GetHeader("X-License-Token"))
	}
	if key != "" || tok != "" || c.Query("license_key") != "" {
		c.Header("Cache-Control", "private, no-store")
	}
	if c.Query("license_key") != "" {
		response.BadRequest(c,
			"the license key is not accepted in the URL: send it in the X-License-Key header, or put the signed token from /license/verify in license_token")
		return
	}

	slug := strings.ToLower(strings.TrimSpace(c.Param("product_slug")))
	if slug == "" {
		response.BadRequest(c, "product_slug is required in the URL path")
		return
	}
	prod, err := h.store.FindProductBySlug(c.Request.Context(), slug)
	if err != nil {
		// Don't differentiate "no such product" from "other DB error" so
		// guesses at slugs leak nothing — 404 either way.
		response.NotFound(c, "product not found")
		return
	}
	// Capability gate: saas products don't ship installable binaries,
	// so the feed endpoints are off. Return 404 (same as "no product")
	// instead of 403 — the feed simply doesn't exist for this product.
	if !model.ProductSupports(prod.Type, model.CapReleases) {
		response.NotFound(c, "product not found")
		return
	}

	platform := service.NormalizePlatform(c.Query("platform"))
	if platform == "" {
		response.BadRequest(c, "platform is required")
		return
	}
	if !service.IsValidPlatform(platform) {
		response.BadRequest(c,
			"platform must be one of: "+strings.Join(service.AllowedPlatforms(), ", "))
		return
	}

	channel := c.DefaultQuery("channel", model.ReleaseChannelStable)
	if !model.IsValidReleaseChannel(channel) {
		response.BadRequest(c, "channel must be stable, beta, alpha, or dev")
		return
	}

	limit := 20
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 100 {
			response.BadRequest(c, "limit must be an integer between 1 and 100")
			return
		}
		limit = n
	}
	req = feedRequest{product: prod, platform: platform, channel: channel, limit: limit}

	// A perpetual license with a maintenance period may only install
	// releases published before it ended. The updater carries its key
	// (query or header) and the feed hides everything newer; without
	// a key the feed is the public list. The key is validated the
	// same way /license/download validates it: a bad key is a 404,
	// not the public feed, or the gate could be skipped by dropping
	// the key.
	if key == "" && tok == "" && prod.FeedLicenseRequired {
		// The product sells maintenance periods: the public list would
		// hand a lapsed customer the newer releases. 401 rather than
		// 404 so the integrator sees what the updater must send.
		response.Err(c, http.StatusUnauthorized, "LICENSE_KEY_REQUIRED",
			"this product's update feed requires the license: send the signed token from /license/verify (license_token query or X-License-Token header), or the license key in the X-License-Key header")
		return
	}
	switch {
	case key != "":
		cutoff, err := h.svc.FeedCutoff(c.Request.Context(), key, prod.ID)
		if err != nil {
			writeAppErr(c, err)
			return
		}
		req.publishedBefore, req.licensed = cutoff, true
	case tok != "":
		// A bad or expired token is a 404 like a bad key: answering
		// with the public list would let the gate be skipped by
		// sending nonsense.
		claims, err := license.Verify(tok, h.verifyKey)
		if err != nil {
			response.NotFound(c, "license not found")
			return
		}
		cutoff, err := h.svc.FeedCutoffForToken(c.Request.Context(), claims.LicenseID, prod.ID)
		if err != nil {
			writeAppErr(c, err)
			return
		}
		req.publishedBefore, req.licensed = cutoff, true
	}
	return req, true
}

// fetchPublishedFeedReleases pulls releases + filters their artifacts to
// the requested platform. Returns the filtered slice + per-product feed
// metadata. ok=false means the response has already been written.
//
// Each artifact's signing public key is resolved (cached per-key-id
// within the loop) so renderers can build format-specific signature
// envelopes (Tauri minisign requires the pubkey to derive its key_id).
func (h *ReleasePublicHandler) fetchPublishedFeedReleases(c *gin.Context, req feedRequest) ([]*service.FeedRelease, bool) {
	releases, err := h.svc.ListForFeed(c.Request.Context(), req.product.ID, req.channel, req.platform, req.limit, req.publishedBefore)
	if err != nil {
		writeAppErr(c, err)
		return nil, false
	}

	pubKeyCache := map[string]string{} // signing_key_id → base64 pubkey
	out := make([]*service.FeedRelease, 0, len(releases))
	for _, rel := range releases {
		// Pick the artifact for this platform. If the release has no
		// matching artifact (e.g. a stable release that didn't ship for
		// linux-armhf), skip it for this platform's feed.
		var artifact *model.ReleaseArtifact
		for _, a := range rel.Artifacts {
			if a.Platform == req.platform && a.IsUploaded() {
				artifact = a
				break
			}
		}
		if artifact == nil {
			continue
		}
		url, err := h.storage.PresignedGet(c.Request.Context(), artifact.FileKey,
			service.DownloadFilename(rel, artifact), h.feedTTL)
		if err != nil {
			if errors.Is(err, storage.ErrStorageDisabled) {
				response.Err(c, http.StatusServiceUnavailable, "STORAGE_DISABLED",
					"release storage is not configured")
				return nil, false
			}
			h.logger.Error("feed: presign failed",
				"release_id", rel.ID, "artifact_id", artifact.ID, "error", err)
			continue
		}

		var signingPubKey string
		if artifact.SigningKeyID != "" {
			if cached, ok := pubKeyCache[artifact.SigningKeyID]; ok {
				signingPubKey = cached
			} else {
				k, err := h.store.FindSigningKeyByID(c.Request.Context(), artifact.SigningKeyID)
				if err == nil {
					signingPubKey = k.PublicKey
					pubKeyCache[artifact.SigningKeyID] = signingPubKey
				} else {
					// Key was deleted post-publish. Leave empty;
					// renderers will emit unsigned manifests.
					pubKeyCache[artifact.SigningKeyID] = ""
				}
			}
		}

		out = append(out, &service.FeedRelease{
			Release:          rel,
			Artifact:         artifact,
			DownloadURL:      url,
			SigningPublicKey: signingPubKey,
		})
	}
	return out, true
}

func (h *ReleasePublicHandler) feedInput(req feedRequest, releases []*service.FeedRelease) service.FeedInput {
	minVersion, minMessage := req.product.MinimumSupportedVersion, req.product.MinimumSupportedMessage
	if req.publishedBefore != nil {
		minVersion = capMinimumVersion(minVersion, releases)
		if minVersion == "" {
			minMessage = ""
		}
	}
	return service.FeedInput{
		ProductID:               req.product.ID,
		ProductName:             req.product.Name,
		BaseURL:                 h.baseURL,
		Releases:                releases,
		MinimumSupportedVersion: minVersion,
		MinimumSupportedMessage: minMessage,
	}
}

// capMinimumVersion keeps a product's version floor within what a
// cutoff-scoped feed can deliver. A license whose update period has
// ended may only install releases from before the cutoff; telling its
// client to refuse anything below a newer floor would stop software
// the perpetual license promises keeps working. The floor becomes the
// newest entitled release, or nothing when there is none.
func capMinimumVersion(minimum string, entitled []*service.FeedRelease) string {
	if minimum == "" {
		return ""
	}
	if len(entitled) == 0 {
		return ""
	}
	newest := entitled[0].Release.Version // sorted newest first
	if service.VersionAtMost(minimum, newest) {
		return minimum
	}
	return newest
}

// GET /api/v1/releases/:product_slug/feed.xml — Sparkle appcast
func (h *ReleasePublicHandler) FeedSparkle(c *gin.Context) {
	req, ok := h.parseFeedRequest(c)
	if !ok {
		return
	}
	feedReleases, ok := h.fetchPublishedFeedReleases(c, req)
	if !ok {
		return
	}
	body, err := service.RenderSparkle(h.feedInput(req, feedReleases))
	if err != nil {
		h.logger.Error("feed: sparkle render failed", "error", err)
		response.Internal(c, err)
		return
	}
	h.writeFeedCacheHeaders(c, req)
	c.Data(http.StatusOK, "application/xml; charset=utf-8", body)
}

// GET /api/v1/releases/:product_slug/feed.json — Velopack feed
func (h *ReleasePublicHandler) FeedVelopack(c *gin.Context) {
	req, ok := h.parseFeedRequest(c)
	if !ok {
		return
	}
	feedReleases, ok := h.fetchPublishedFeedReleases(c, req)
	if !ok {
		return
	}
	body, err := json.Marshal(service.BuildVelopack(h.feedInput(req, feedReleases)))
	if err != nil {
		h.logger.Error("feed: velopack marshal failed", "error", err)
		response.Internal(c, err)
		return
	}
	h.writeFeedCacheHeaders(c, req)
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// GET /api/v1/releases/:product_slug/upgrade.json — Tauri (single-release manifest)
func (h *ReleasePublicHandler) FeedTauri(c *gin.Context) {
	req, ok := h.parseFeedRequest(c)
	if !ok {
		return
	}
	// Tauri always wants exactly the latest. Pull only 1 release.
	req.limit = 1
	feedReleases, ok := h.fetchPublishedFeedReleases(c, req)
	if !ok {
		return
	}
	manifest := service.BuildTauri(h.feedInput(req, feedReleases))
	if manifest.Version == "" {
		// 204 is Tauri's "no update" signal. A 200 with version "" used
		// to be sent here to carry minimum_supported_version, but the
		// updater parses version as semver and errors out on "" — and
		// with nothing to update to there is nothing for the floor to
		// gate anyway.
		c.Status(http.StatusNoContent)
		return
	}
	h.writeFeedCacheHeaders(c, req)
	c.JSON(http.StatusOK, manifest)
}

// writeFeedCacheHeaders sets Cache-Control for a successful feed. The
// public feed is the same for everyone, so CDNs / ISP proxies may
// serve it; a feed built for one license key keeps the no-store
// policy parseFeedRequest already set.
func (h *ReleasePublicHandler) writeFeedCacheHeaders(c *gin.Context, req feedRequest) {
	if req.licensed {
		return
	}
	c.Header("Cache-Control", "public, max-age="+strconv.Itoa(int(FeedPublicMaxAge.Seconds())))
}
