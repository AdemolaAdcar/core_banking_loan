package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store"
)

type contextKey string

const idempotencyKeyContextKey contextKey = "idempotencyKey"

func idempotencyKeyFromContext(ctx context.Context) string {
	v, _ := ctx.Value(idempotencyKeyContextKey).(string)
	return v
}

// idempotencyEnvelope is the cached-replay payload stored via
// store.Tx.SaveIdempotentResponse. Bundling requestHash alongside the
// response keeps store.Store's GetIdempotentResponse signature narrow
// (found/body/err) while still letting this middleware detect "same key,
// different payload" and return 409, per invariant 4.
type idempotencyEnvelope struct {
	RequestHash string          `json:"requestHash"`
	Status      int             `json:"status"`
	Body        json.RawMessage `json:"body"`
}

// withIdempotency enforces invariant 4: a retried postJournalEntry (or
// closePeriod) call with the same key and the same payload replays the
// original response rather than re-running the handler; the same key
// reused with a materially different payload is refused with 409.
//
// The replay cache is saved via store.WithinTx AFTER the handler
// succeeds, in its own follow-up transaction — not inside the handler's
// own business-write transaction. A crash between the handler's commit
// and this save means the next retry re-runs the handler rather than
// replaying a cached response; that is safe here specifically because
// the database's own UNIQUE constraint on journal_entries.source_event_id
// is a second, independent backstop (see store.ErrDuplicateSourceEventID)
// — a re-run that races a still-uncached prior success resolves to the
// original entry, never a duplicate posting. This is deliberately
// "at-least-once, safe on retry" rather than a strict single-transaction
// idempotency guarantee across the business write and the cache write.
func withIdempotency(st store.Store, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "Idempotency-Key header is required")
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "failed to read request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		sum := sha256.Sum256(bodyBytes)
		requestHash := hex.EncodeToString(sum[:])

		found, cached, err := st.GetIdempotentResponse(r.Context(), key)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to check idempotency cache")
			return
		}
		if found {
			var env idempotencyEnvelope
			if err := json.Unmarshal(cached, &env); err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL", "corrupt idempotency cache entry")
				return
			}
			if env.RequestHash != requestHash {
				writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "Idempotency-Key was already used with a different request payload")
				return
			}
			writeJSON(w, env.Status, env.Body)
			return
		}

		ctx := context.WithValue(r.Context(), idempotencyKeyContextKey, key)
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r.WithContext(ctx))

		if rec.status >= 200 && rec.status < 300 && rec.body.Len() > 0 {
			env := idempotencyEnvelope{RequestHash: requestHash, Status: rec.status, Body: json.RawMessage(rec.body.Bytes())}
			if envJSON, err := json.Marshal(env); err == nil {
				_ = st.WithinTx(r.Context(), func(tx store.Tx) error {
					return tx.SaveIdempotentResponse(r.Context(), key, requestHash, envJSON)
				})
			}
		}
	}
}

// responseRecorder captures the handler's status and body so they can be
// cached after a successful write, while still forwarding the real
// response to the client immediately — no buffering delay on the
// client-visible path.
type responseRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}
