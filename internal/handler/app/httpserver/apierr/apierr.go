// Package apierr is the transport-layer error (code-standards §2.2). It owns
// the HTTP status and the public code/message that travel in the
// {success,data,error} envelope.
//
// From is the single place where an errs.Kind becomes a status. Handlers build
// an Error directly only for transport-specific failures — a malformed body, a
// path parameter that never reaches the domain — and let every domain failure
// arrive through From.
package apierr

import (
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// Public codes. The five below are the fixed projection of the domain kinds;
// transport-specific failures may introduce their own code through New.
const (
	CodeInvalidRequest = "invalid_request"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeUnavailable    = "unavailable"
	CodeInternal       = "internal"
)

// Default messages. They carry no cause, so they are always safe to publish.
const (
	msgInvalidRequest = "invalid request"
	msgNotFound       = "not found"
	msgConflict       = "conflict"
	msgUnavailable    = "service unavailable"
	msgInternal       = "internal error"
)

const segmentSep = ": "

// mapping is one row of the fixed table of §2.2.
type mapping struct {
	status  int
	code    string
	message string
}

// mappingFor is that table. KindInternal, an unclassified error and any kind
// added later without a case here all land on the internal row.
func mappingFor(kind errs.Kind) mapping {
	switch kind {
	case errs.KindInvalid:
		return mapping{http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest}
	case errs.KindNotFound:
		return mapping{http.StatusNotFound, CodeNotFound, msgNotFound}
	case errs.KindConflict:
		return mapping{http.StatusConflict, CodeConflict, msgConflict}
	case errs.KindUnavailable:
		return mapping{http.StatusServiceUnavailable, CodeUnavailable, msgUnavailable}
	default:
		return mapping{http.StatusInternalServerError, CodeInternal, msgInternal}
	}
}

// Error is the transport-layer error. Only Code, Message and Details are
// serialised: Status belongs to the status line and cause belongs to the log.
type Error struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	cause   error          // logging only — never rendered into a response
}

// Error renders status, code, message and cause for logs. The response body is
// built from the JSON tags above, so this string cannot leak into it.
func (e *Error) Error() string {
	segments := []string{strconv.Itoa(e.Status) + " " + e.Code, e.Message}
	if e.cause != nil {
		segments = append(segments, e.cause.Error())
	}
	return strings.Join(segments, segmentSep)
}

// Unwrap exposes the cause so a handler can log it and errors.Is still reaches
// the domain error underneath.
func (e *Error) Unwrap() error { return e.cause }

// WithCause returns a copy carrying the cause to log. The receiver is never
// mutated.
func (e *Error) WithCause(err error) *Error {
	next := *e
	next.Details = maps.Clone(e.Details)
	next.cause = err
	return &next
}

// WithDetail returns a copy carrying one more public detail. Everything put
// here is deliberately visible to the client (§2.2).
func (e *Error) WithDetail(key string, value any) *Error {
	next := *e
	next.Details = maps.Clone(e.Details)
	if next.Details == nil {
		next.Details = make(map[string]any, 1)
	}
	next.Details[key] = value
	return &next
}

// New builds a transport-layer failure: malformed JSON, an unusable path
// parameter, a body that never reaches the domain. A status outside the HTTP
// range is normalised to 500 so it can never reach http.ResponseWriter as is.
func New(status int, code, message string) *Error {
	if status < http.StatusContinue || status > 599 {
		status = http.StatusInternalServerError
	}
	return &Error{Status: status, Code: code, Message: message}
}

// From maps a domain error to its transport representation — the only place
// where errs.Kind becomes an HTTP status. An error that is already a transport
// error passes through unchanged; an error with no domain kind becomes 500
// internal and its cause stays out of Message.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var transport *Error
	if errors.As(err, &transport) {
		return transport
	}

	var domain *errs.Error
	if !errors.As(err, &domain) {
		m := mappingFor(errs.KindInternal)
		return &Error{Status: m.status, Code: m.code, Message: m.message, cause: err}
	}

	m := mappingFor(domain.Kind)
	return &Error{
		Status:  m.status,
		Code:    m.code,
		Message: publicMessage(err, m.message),
		cause:   err,
	}
}

// publicMessage returns the innermost authored message in the chain. errs.Wrap
// adds no message of its own, so the message travels up from wherever the
// failure was actually recognised; a chain of pure wraps falls back to the
// message of its kind.
func publicMessage(err error, fallback string) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if domain, ok := e.(*errs.Error); ok && domain.Msg != "" {
			return domain.Msg
		}
	}
	return fallback
}
