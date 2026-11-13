package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/mod/semver"

	"github.com/tabloy/keygate/internal/branding"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/version"
	"github.com/tabloy/keygate/pkg/response"
)

type SystemHandler struct {
	Store        *store.Store
	updateCache  *updateInfo
	cacheMu      sync.RWMutex
	cacheTime    time.Time
	updateFlight *updateCheckFlight
	httpClient   *http.Client
	RepoOwner    string
	RepoName     string
}

type updateCheckFlight struct {
	done   chan struct{}
	result *updateInfo
}

type updateInfo struct {
	Available     bool   `json:"available"`
	Latest        string `json:"latest_version"`
	Current       string `json:"current_version"`
	ReleaseURL    string `json:"release_url,omitempty"`
	ReleaseDate   string `json:"release_date,omitempty"`
	Changelog     string `json:"changelog,omitempty"`
	UpdateCommand string `json:"update_command,omitempty"`
	CheckedAt     string `json:"checked_at"`
	Error         string `json:"error,omitempty"`
}

func NewSystemHandler(s *store.Store) *SystemHandler {
	return &SystemHandler{
		Store:     s,
		RepoOwner: "tabloy",
		RepoName:  "keygate",
	}
}

func (h *SystemHandler) GetVersion(c *gin.Context) {
	response.OK(c, gin.H{
		"version":     version.Version,
		"commit":      version.Commit,
		"build_date":  version.BuildDate,
		"project":     branding.Project,
		"project_url": branding.URL,
	})
}

// CheckUpdate caches successful reads for one hour. Concurrent checks
// share one upstream request, including its failure.
func (h *SystemHandler) CheckUpdate(c *gin.Context) {
	response.OK(c, h.checkLatestRelease(c.Request.Context()))
}

func (h *SystemHandler) checkLatestRelease(ctx context.Context) *updateInfo {
	h.cacheMu.Lock()
	if h.updateCache != nil && h.updateCache.Error == "" && time.Since(h.cacheTime) < time.Hour {
		info := h.updateCache
		h.cacheMu.Unlock()
		return info
	}
	if flight := h.updateFlight; flight != nil {
		h.cacheMu.Unlock()
		select {
		case <-flight.done:
			return flight.result
		case <-ctx.Done():
			return &updateInfo{Current: version.Version, Error: ctx.Err().Error()}
		}
	}
	flight := &updateCheckFlight{done: make(chan struct{})}
	h.updateFlight = flight
	h.cacheMu.Unlock()

	info := h.fetchLatestRelease()
	h.cacheMu.Lock()
	if info.Error == "" {
		h.updateCache = info
		h.cacheTime = time.Now()
	}
	flight.result = info
	h.updateFlight = nil
	close(flight.done)
	h.cacheMu.Unlock()
	return info
}

// StartAutoCheck runs a background loop that checks for updates every 6 hours.
// The result is cached so the admin dashboard always has fresh data.
func (h *SystemHandler) StartAutoCheck(done <-chan struct{}) {
	// Check once on startup (after 30s delay to let server fully start)
	time.AfterFunc(30*time.Second, func() {
		info := h.checkLatestRelease(context.Background())
		if info.Available {
			slog.Info("update available", "current", info.Current, "latest", info.Latest, "url", info.ReleaseURL)
		}
	})

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			h.checkLatestRelease(context.Background())
		}
	}
}

func (h *SystemHandler) fetchLatestRelease() *updateInfo {
	info := &updateInfo{
		Current:   version.Version,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}

	client := h.httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	url := "https://api.github.com/repos/" + h.RepoOwner + "/" + h.RepoName + "/releases/latest"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		info.Error = "failed to create GitHub request"
		return info
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "Keygate/"+version.Version)

	resp, err := client.Do(req)
	if err != nil {
		info.Error = "failed to reach GitHub: " + err.Error()
		return info
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		// No releases published yet — not an error
		info.Latest = info.Current
		return info
	}
	if resp.StatusCode != 200 {
		info.Error = "GitHub API returned " + resp.Status
		return info
	}

	var release struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Body        string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		info.Error = "failed to parse GitHub response"
		return info
	}

	if !semver.IsValid(normalizeSemver(release.TagName)) {
		info.Error = "GitHub release has an invalid version"
		return info
	}
	info.Latest = release.TagName
	info.ReleaseURL = release.HTMLURL
	info.ReleaseDate = release.PublishedAt
	info.Changelog = release.Body
	info.Available = semverNewer(stripV(release.TagName), stripV(version.Version))
	info.UpdateCommand = "docker pull ghcr.io/" + h.RepoOwner + "/" + h.RepoName + ":" + stripV(release.TagName)

	return info
}

// normalizeSemver pads partial versions (e.g. "1" -> "v1.0.0", "1.1" -> "v1.1.0")
// and ensures a "v" prefix so that golang.org/x/mod/semver can parse it per SemVer 2.0.
func normalizeSemver(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	// The core ends at the first "-" (pre-release) or "+" (build).
	i := strings.IndexAny(v, "-+")
	if i < 0 {
		i = len(v)
	}
	base, suffix := v[:i], v[i:]
	switch strings.Count(base, ".") {
	case 0:
		base += ".0.0"
	case 1:
		base += ".0"
	}
	return base + suffix
}

// describeSuffix is what `git describe --tags --dirty`, the Makefile's
// version, adds after the tag: the commits since it with the commit's
// hash, and a mark for uncommitted changes. A build carrying it is the
// tag or later, never a pre-release of it as semver would read it.
var describeSuffix = regexp.MustCompile(`(-[0-9]+-g[0-9a-f]+)?(-dirty)?$`)

// semverNewer returns true if latest > current using semantic versioning.
func semverNewer(latest, current string) bool {
	if latest == "" || current == "" || current == "dev" {
		return false
	}
	vl := normalizeSemver(latest)
	vc := normalizeSemver(describeSuffix.ReplaceAllString(current, ""))
	if !semver.IsValid(vl) || !semver.IsValid(vc) {
		return false
	}
	return semver.Compare(vl, vc) > 0
}

func stripV(v string) string {
	if len(v) > 0 && v[0] == 'v' {
		return v[1:]
	}
	return v
}

func (h *SystemHandler) GetMigrationStatus(c *gin.Context) {
	migrations, err := h.Store.ListAppliedMigrations(c)
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"migrations": response.Array(migrations)})
}
