package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
)

// Idempotency wraps a handler so retries with the same `Idempotency-Key`
// header return the original response without re-executing the handler.
// Stripe / Mailgun / GitHub conventions:
//
//   - Header is OPTIONAL. Without it, the handler runs normally — no
//     dedup, no cache.
//   - Same key + same body → cached response (status + body).
//   - Same key + different body → 422 IDEMPOTENCY_KEY_CONFLICT (the
//     client is reusing the key for a different operation, which is
//     a programming error).
//   - Concurrent retries of the same key → 409 IDEMPOTENCY_IN_FLIGHT
//     while the first one is still running. Client retries with backoff.
//   - 24h TTL — set by the table default.
//
// Apply selectively to write endpoints where dedup matters
// (`/license/activate`, `/license/usage`, `/license/floating/checkout`).
// Read endpoints don't need it; brief caches like `/license/verify`
// don't either since they're already idempotent by definition.
//
// Safety notes:
//
//   - The body is read into memory and re-injected so the handler can
//     bind it again. Capped at 256 KiB; oversized requests skip the
//     idempotency layer entirely (and the handler validates body limits
//     itself).
//   - 5xx responses are NOT cached — they're transient, retries SHOULD
//     re-execute. We delete the slot so the retry can claim it fresh.
//   - 4xx responses ARE cached — they represent a deterministic outcome
//     (validation failure, license-not-found, etc.) so retrying just
//     gets the same answer.
func Idempotency(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only POSTs need this layer; GETs are idempotent by HTTP semantics.
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		key := c.GetHeader("Idempotency-Key")
		if key == "" {
			c.Next()
			return
		}
		if !store.ValidIdempotencyKey(key) {
			abortWithError(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY",
				"Idempotency-Key must be 1–256 chars, no control characters")
			return
		}

		// Read the body so we can hash + replay it for the handler.
		// Bound HARD at 256 KiB. Endpoints guarded by Idempotency-Key
		// (license activate / usage / floating checkout / seat add) are
		// all small-body JSON — a few hundred bytes typical, a couple
		// KiB at the extreme. A request bigger than 256 KiB on these
		// endpoints is either misuse or an attempt to exhaust server
		// memory through the cache buffer. Reject outright instead of
		// silently bypassing idempotency.
		const maxBody = 256 * 1024
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody+1))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "BAD_REQUEST",
				"could not read request body")
			return
		}
		if len(body) > maxBody {
			abortWithError(c, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
				"request body exceeds idempotency limit (256 KiB)")
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		bodyHash := sha256Hex(body)
		endpoint := c.FullPath()
		if endpoint == "" {
			endpoint = c.Request.URL.Path
		}

		existing, err := s.IdempotencyClaim(c.Request.Context(), key, endpoint, bodyHash)
		if err != nil {
			if errors.Is(err, store.ErrIdempotencyBodyMismatch) {
				abortWithError(c, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_CONFLICT",
					"Idempotency-Key reused with a different request body")
				return
			}
			// INTERNAL_ERROR, via the same helper as everywhere else.
			// This line used to answer INTERNAL with its own wording,
			// which is a code no client had any reason to expect.
			abortInternal(c, err)
			return
		}

		if existing != nil {
			if existing.ResponseComplete {
				// Cache hit — replay.
				c.Header("Idempotent-Replayed", "true")
				c.Data(existing.ResponseStatus, "application/json; charset=utf-8",
					[]byte(existing.ResponseBody))
				c.Abort()
				return
			}
			// Concurrent in-flight request still running.
			c.Header("Retry-After", "1")
			abortWithError(c, http.StatusConflict, "IDEMPOTENCY_IN_FLIGHT",
				"an earlier request with this Idempotency-Key is still being processed")
			return
		}

		// We claimed the slot. Wrap the writer to capture the response,
		// then run the handler. After it returns: cache 2xx/4xx, drop 5xx.
		writer := &captureWriter{ResponseWriter: c.Writer, status: 0, buf: bytes.Buffer{}}
		c.Writer = writer

		// If the handler panics or the request is canceled, we MUST
		// release the slot so a retry isn't stuck IN_FLIGHT until TTL.
		// detached ctx so cleanup runs even if the request ctx died.
		defer func() {
			detached := context.WithoutCancel(c.Request.Context())
			status := writer.status
			if writer.aborted || status == 0 {
				// Handler crashed before status set.
				s.IdempotencyAbandon(detached, key, endpoint)
				return
			}
			if status >= 500 {
				// Transient — let retries hit the handler again.
				s.IdempotencyAbandon(detached, key, endpoint)
				return
			}
			_ = s.IdempotencyComplete(detached, key, endpoint, status, writer.buf.String())
		}()

		c.Next()
	}
}

// captureWriter mirrors writes to the real response while keeping a
// copy for the idempotency cache. status defaults to 200 if the handler
// uses c.JSON / c.String without explicit WriteHeader.
type captureWriter struct {
	gin.ResponseWriter
	status  int
	buf     bytes.Buffer
	aborted bool
}

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
