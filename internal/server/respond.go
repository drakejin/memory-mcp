package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/drakejin/memory-mcp/internal/server/apierr"
)

// msgInternal is the last-resort public message: it names no cause, so it is
// always safe to publish.
const msgInternal = "internal error"

// Envelope is the mandatory response shape for every endpoint (§7):
// {success, data, error}. A failure carries the transport error itself, so the
// client always sees a stable {code, message} pair instead of a free-form
// sentence. Data has no omitempty: a failure body always contains "data": null.
type Envelope struct {
	Success bool          `json:"success"`
	Data    any           `json:"data"`
	Error   *apierr.Error `json:"error,omitempty"`
}

// writeJSON emits a success envelope.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, data any) {
	log = orDefaultLogger(log)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(Envelope{Success: true, Data: data}); err != nil {
		log.Error("respond: encode failed", "error", err)
	}
}

// writeAPIError emits a failure envelope from a transport error. A domain error
// reaches it through the §2.2 boundary:
//
//	writeAPIError(w, s.log, apierr.From(err))
//
// The cause is logged and never rendered: only Code, Message and Details of
// apierr.Error are serialised. This is the single place a failed request is
// logged — a domain cause is an *errs.Error whose LogValue already carries its
// op, entity, id and structured fields, so handlers do not log it again. What
// handlers do log is the best-effort work that never reaches this function:
// degraded upserts, manifest bookkeeping, recall convergence.
func writeAPIError(w http.ResponseWriter, log *slog.Logger, apiErr *apierr.Error) {
	log = orDefaultLogger(log)
	if apiErr == nil {
		// A nil transport error means a handler failed without saying why.
		// Report internal rather than panicking on the response path.
		apiErr = apierr.New(http.StatusInternalServerError, apierr.CodeInternal, msgInternal)
	}
	if cause := apiErr.Unwrap(); cause != nil {
		log.Error("request failed", "status", apiErr.Status, "code", apiErr.Code, "cause", cause)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(apiErr.Status)
	if err := json.NewEncoder(w).Encode(Envelope{Success: false, Error: apiErr}); err != nil {
		log.Error("respond: encode failed", "error", err)
	}
}

// orDefaultLogger keeps the response path total. New guarantees a non-nil
// logger for handlers; this only covers a direct call with none, where dropping
// the log line is far better than panicking while writing a response.
func orDefaultLogger(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.Default()
	}
	return log
}

// The three constructors below build the transport-only failures of §2.2 —
// failures decided by this layer, from information the domain never sees. Every
// other failure arrives as a domain error and is converted with apierr.From.

// badRequest rejects a malformed body, path or query parameter.
func badRequest(msg string) *apierr.Error {
	return apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, msg)
}

// notFound reports a resource the transport layer itself resolved as absent.
func notFound(msg string) *apierr.Error {
	return apierr.New(http.StatusNotFound, apierr.CodeNotFound, msg)
}

// unavailable reports a derived store a read could not use: either it was never
// built, or it answered KindUnavailable. It is built here rather than by
// apierr.From so the 503 body reuses the degraded vocabulary of §5 word for
// word — the same string a write would have carried as a degraded note. Attach
// the store's own error with WithCause when there is one.
func unavailable(msg string) *apierr.Error {
	return apierr.New(http.StatusServiceUnavailable, apierr.CodeUnavailable, msg)
}
