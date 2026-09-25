package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// resolvedConfig is the effective email configuration for one send.
//
// It comes from one of two places, never a mix: if an admin has chosen
// a provider in the dashboard (email_provider is set in the DB), the
// whole configuration is read from the DB; otherwise it falls back to
// the SMTP_* environment variables the server booted with. The all-or-
// nothing rule keeps a half-DB, half-env configuration from ever
// existing, which is the state that would be impossible to reason about.
type resolvedConfig struct {
	provider string // "smtp" | "cloudflare"
	source   string // "db" | "env" — for the status display only
	from     string

	// smtp
	host, port, username, password string

	// cloudflare
	apiKey, apiID string
}

func (c resolvedConfig) implicitTLS() bool { return c.port == "465" }
func (c resolvedConfig) addr() string      { return c.host + ":" + c.port }

func (c resolvedConfig) configured() bool {
	if c.from == "" {
		return false
	}
	switch c.provider {
	case "cloudflare":
		return c.apiKey != "" && c.apiID != ""
	default: // smtp
		return c.host != ""
	}
}

// resolve reads the effective configuration for a send. DB wins if a
// provider is configured there; otherwise the env values captured at
// construction are used.
func (s *EmailService) resolve(ctx context.Context) (resolvedConfig, error) {
	envCfg := resolvedConfig{
		provider: "smtp", source: "env",
		from: s.from, host: s.host, port: s.port,
		username: s.username, password: s.password,
	}
	if s.store == nil {
		return envCfg, nil
	}
	provider, err := s.store.GetSetting(ctx, "email_provider")
	if errors.Is(err, sql.ErrNoRows) {
		return envCfg, nil // no provider configured → env fallback
	}
	if err != nil {
		// A transient read error is not "unconfigured". Falling back to
		// env here could hand licence and login mail to a provider the
		// operator has since disabled; report the error instead.
		return resolvedConfig{}, fmt.Errorf("read email_provider: %w", err)
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return envCfg, nil
	}

	// get reads a non-secret field. A missing row is just "not set"
	// (""); any other read error is fatal, for the same reason as above.
	get := func(k string) (string, error) {
		v, err := s.store.GetSetting(ctx, k)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read %s: %w", k, err)
		}
		return strings.TrimSpace(v), nil
	}
	// Each provider owns a complete, independently namespaced config,
	// including its own From. Switching the active provider never
	// touches another provider's fields, and adding a provider is a new
	// prefixed block with no shared keys to reason about.
	cfg := resolvedConfig{provider: provider, source: "db"}
	var e error
	switch provider {
	case "cloudflare":
		if cfg.from, e = get("cloudflare_from"); e != nil {
			return resolvedConfig{}, e
		}
		if cfg.apiID, e = get("cloudflare_account_id"); e != nil {
			return resolvedConfig{}, e
		}
		if cfg.apiKey, e = s.store.GetSecretSetting(ctx, "cloudflare_api_token"); e != nil {
			return resolvedConfig{}, fmt.Errorf("read cloudflare_api_token: %w", e)
		}
	default: // smtp
		if cfg.from, e = get("smtp_from"); e != nil {
			return resolvedConfig{}, e
		}
		if cfg.host, e = get("smtp_host"); e != nil {
			return resolvedConfig{}, e
		}
		if cfg.port, e = get("smtp_port"); e != nil {
			return resolvedConfig{}, e
		}
		if cfg.port == "" {
			cfg.port = "587"
		}
		if cfg.username, e = get("smtp_username"); e != nil {
			return resolvedConfig{}, e
		}
		if cfg.password, e = s.store.GetSecretSetting(ctx, "smtp_password"); e != nil {
			return resolvedConfig{}, fmt.Errorf("read smtp_password: %w", e)
		}
	}
	return cfg, nil
}

// A send failure has two independent properties, and the earlier
// single "permanent" flag conflated them:
//
//   - terminal: the message itself can never be delivered — a permanent
//     recipient bounce. Give up and drop the body; retrying just
//     re-hits a dead address and burns reputation. (permanentError)
//   - retryable-now: worth an immediate second attempt — a 429, a 5xx,
//     or a network blip. (retryableError)
//
// Everything else — a 401/403, a wrong account id, a rejected request —
// is neither. It is not the message's fault and an immediate retry will
// not fix it, but it is recoverable once an operator fixes the config,
// so the queue MUST keep the mail (and its body) and try again later.
// Marking those terminal would throw away licence mail on a token
// rotation. A bare error means exactly this "keep and retry later" case.

// permanentError marks a message-level failure that is terminal.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

// isPermanent reports whether err is terminal (drop it, never retry).
func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// retryableError marks a failure worth an immediate retry.
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

func retryable(format string, args ...any) error {
	return retryableError{fmt.Errorf(format, args...)}
}

// isRetryable reports whether err warrants the one immediate retry.
func isRetryable(err error) bool {
	var r retryableError
	return errors.As(err, &r)
}

// cfEndpoint is a var so tests can point it at an httptest server.
var cfEndpoint = "https://api.cloudflare.com/client/v4/accounts/%s/email/sending/send"

var cfHTTPClient = &http.Client{Timeout: 30 * time.Second}

// sendCloudflare delivers one message through the Cloudflare Email
// Service REST API. It runs over 443, so it works where a host blocks
// outbound SMTP ports.
func (s *EmailService) sendCloudflare(ctx context.Context, cfg resolvedConfig, to, subject, html, text string) error {
	body, err := json.Marshal(map[string]string{
		"from": cfg.from, "to": to, "subject": subject, "html": html, "text": text,
	})
	if err != nil {
		return fmt.Errorf("cloudflare: marshal: %w", err)
	}
	url := fmt.Sprintf(cfEndpoint, cfg.apiID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cloudflare: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := cfHTTPClient.Do(req)
	if err != nil {
		return retryable("cloudflare: %w", err) // network blip
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result struct {
			Delivered        []string `json:"delivered"`
			PermanentBounces []string `json:"permanent_bounces"`
			Queued           []string `json:"queued"`
		} `json:"result"`
	}
	// 429 and 5xx are worth another try (Cloudflare recommends retrying
	// only those). Anything else is not retried immediately, but only a
	// recipient bounce is terminal — a 4xx is most likely a credential
	// or account-id problem the operator can fix, so it stays in the
	// queue.
	retryNow := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

	if err := json.Unmarshal(raw, &out); err != nil {
		msg := fmt.Sprintf("cloudflare: bad response (HTTP %d): %s", resp.StatusCode, clip(string(raw), 200))
		if retryNow {
			return retryable("%s", msg)
		}
		return errors.New(msg) // keep in queue, do not retry now
	}
	// success:true but the recipient bounced is the one terminal case.
	for _, b := range out.Result.PermanentBounces {
		if strings.EqualFold(b, to) {
			return permanent("cloudflare: permanent bounce for %s", to)
		}
	}
	if !out.Success {
		msg := "unknown error"
		if len(out.Errors) > 0 {
			msg = fmt.Sprintf("%s (code %d)", out.Errors[0].Message, out.Errors[0].Code)
		}
		if retryNow {
			return retryable("cloudflare: %s", msg)
		}
		// A 4xx / auth / account error: recoverable once fixed. Keep it.
		return fmt.Errorf("cloudflare: %s", msg)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

var htmlTagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// htmlToText is a plain-text fallback for providers that want a text
// part (Cloudflare requires one). It is deliberately crude: strip
// tags, collapse whitespace. The HTML part is the real body.
func htmlToText(html string) string {
	// Drop script/style blocks whole so their contents don't leak in.
	html = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`).ReplaceAllString(html, "")
	html = strings.ReplaceAll(html, "</p>", "\n\n")
	html = strings.ReplaceAll(html, "<br>", "\n")
	html = strings.ReplaceAll(html, "<br/>", "\n")
	html = strings.ReplaceAll(html, "<br />", "\n")
	text := htmlTagRe.ReplaceAllString(html, "")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	// Collapse runs of blank lines and trailing spaces.
	lines := strings.Split(text, "\n")
	var out []string
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
