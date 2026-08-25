// Package errs is the semantic error vocabulary of every layer below the HTTP
// handler (code-standards §2.1). Storage, search, graph, document and pipeline
// code return *Error so callers can branch on a Kind instead of parsing
// strings, and so the transport layer can turn that Kind into a status exactly
// once — in internal/server/apierr.
//
// Nothing here knows about HTTP. Comparisons are always errors.Is against a
// sentinel (ErrNotFound and friends), never a Kind string comparison.
package errs

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
)

// Kind classifies a failure. It is the only property the transport layer maps
// to an HTTP status.
type Kind string

const (
	KindInvalid     Kind = "invalid"     // input violates a rule
	KindNotFound    Kind = "not_found"   // the addressed entity does not exist
	KindConflict    Kind = "conflict"    // state machine violation, duplicate, supersede clash
	KindUnavailable Kind = "unavailable" // a derived store is down — the degraded-mode signal
	KindInternal    Kind = "internal"    // anything we failed to classify
)

// Default public messages. They are deliberately free of causes, paths and
// credentials: apierr copies them straight into the response body.
const (
	msgNotFound    = "not found"
	msgUnavailable = "service unavailable"
	msgInternal    = "internal error"
)

// segmentSep joins the parts of Error(); idFormat labels the id segment so a
// bare ULID is never mistaken for a message.
const (
	segmentSep = ": "
	idPrefix   = "id="
)

// fieldPath is the structured-field key IO attaches. Log-only, never public.
const fieldPath = "path"

// sentinel is the comparison target behind the exported sentinels. It is a
// distinct type so that (*Error).Is matches a sentinel by Kind while two
// unrelated *Error values of the same kind stay distinct.
type sentinel Kind

func (s sentinel) Error() string { return string(s) }

// The five sentinels. Compare with errors.Is(err, errs.ErrNotFound); never
// compare Kind strings by hand.
var (
	ErrInvalid     error = sentinel(KindInvalid)
	ErrNotFound    error = sentinel(KindNotFound)
	ErrConflict    error = sentinel(KindConflict)
	ErrUnavailable error = sentinel(KindUnavailable)
	ErrInternal    error = sentinel(KindInternal)
)

// Error is the semantic error carried by every layer below the handler.
type Error struct {
	Kind   Kind           // what went wrong, in transport-independent terms
	Op     string         // "hotstore.AppendEpisode" — a readable path, not a stack
	Entity string         // "episode", "knowledge_node", "blob"
	ID     string         // the target identifier, when there is one
	Msg    string         // human-readable, client-safe: no secrets, no full paths
	Fields map[string]any // structured detail; flows to slog, never to the body
	err    error          // cause
}

// Error renders the chain for logs and test assertions. The response body never
// contains this string — only Msg travels to the client, via apierr.
func (e *Error) Error() string {
	segments := make([]string, 0, 4)
	if e.Op != "" {
		segments = append(segments, e.Op)
	}
	// A wrap carries no message of its own; its cause supplies one.
	switch {
	case e.Msg != "":
		segments = append(segments, e.Msg)
	case e.err == nil:
		segments = append(segments, string(e.Kind))
	}
	if e.ID != "" {
		segments = append(segments, idPrefix+e.ID)
	}
	if e.err != nil {
		segments = append(segments, e.err.Error())
	}
	return strings.Join(segments, segmentSep)
}

// Unwrap exposes the cause so errors.Is/As reach through the whole chain.
func (e *Error) Unwrap() error { return e.err }

// Is matches the package sentinels by Kind. Any other target falls through to
// the standard chain walk, so wrapped causes still match by identity.
func (e *Error) Is(target error) bool {
	s, ok := target.(sentinel)
	return ok && Kind(s) == e.Kind
}

// WithField returns a copy carrying one more structured field. The receiver is
// never mutated, so a shared error value cannot grow fields behind its owner.
func (e *Error) WithField(key string, value any) *Error {
	next := *e
	next.Fields = maps.Clone(e.Fields)
	if next.Fields == nil {
		next.Fields = make(map[string]any, 1)
	}
	next.Fields[key] = value
	return &next
}

// LogValue renders the error as a slog group so Fields reach the log untouched
// (§2.1). A domain cause is nested as its own group; a foreign cause is
// rendered as text.
func (e *Error) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, 6+len(e.Fields))
	attrs = append(attrs, slog.String("kind", string(e.Kind)))
	for _, kv := range []struct{ key, value string }{
		{"op", e.Op},
		{"entity", e.Entity},
		{"id", e.ID},
		{"msg", e.Msg},
	} {
		if kv.value != "" {
			attrs = append(attrs, slog.String(kv.key, kv.value))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(e.Fields)) {
		attrs = append(attrs, slog.Any(key, e.Fields[key]))
	}
	if e.err != nil {
		var domain *Error
		if errors.As(e.err, &domain) {
			attrs = append(attrs, slog.Any("cause", domain))
		} else {
			attrs = append(attrs, slog.String("cause", e.err.Error()))
		}
	}
	return slog.GroupValue(attrs...)
}

// Invalid reports input that violates a rule: a bad enum, an empty required
// field, an out-of-range depth. msg is shown to the client.
func Invalid(op, entity, msg string) *Error {
	return &Error{Kind: KindInvalid, Op: op, Entity: entity, Msg: msg}
}

// NotFound reports that the addressed entity does not exist.
func NotFound(op, entity, id string) *Error {
	msg := msgNotFound
	if entity != "" {
		msg = entity + " " + msgNotFound
	}
	return &Error{Kind: KindNotFound, Op: op, Entity: entity, ID: id, Msg: msg}
}

// Conflict reports a state machine violation: a duplicate id, a supersede clash,
// a purge without confirmation. msg is shown to the client.
func Conflict(op, entity, id, msg string) *Error {
	return &Error{Kind: KindConflict, Op: op, Entity: entity, ID: id, Msg: msg}
}

// Unavailable reports that a derived store (OpenSearch, Neo4j, S3) could not be
// reached. It is the signal degraded-mode decisions are built on: a write path
// reports it as degraded, a read search path turns it into 503.
func Unavailable(op string, cause error) *Error {
	return &Error{Kind: KindUnavailable, Op: op, Msg: msgUnavailable, err: cause}
}

// Internal reports a failure we could not classify. The cause stays in the
// chain for logs; the client only ever sees Msg.
func Internal(op string, cause error) *Error {
	return &Error{Kind: KindInternal, Op: op, Msg: msgInternal, err: cause}
}

// IO classifies a filesystem failure. The path travels as a structured field
// for logs only — it never reaches a response body (§2.1). Single home for a
// helper hotstore and blob previously defined identically (§4).
func IO(op, path string, cause error) *Error {
	return Internal(op, cause).WithField(fieldPath, path)
}

// FromContext converts context cancellation or deadline expiry into a domain
// error, and returns nil while ctx is still live. The cause stays in the chain
// so errors.Is(err, context.Canceled) still holds. Single home for a helper
// hotstore and blob previously defined identically (§4).
func FromContext(ctx context.Context, op string) error {
	return Wrap(op, ctx.Err())
}

// Wrap adds op context as an error crosses a package boundary. The kind of an
// existing *Error is preserved; a foreign error becomes KindInternal. The
// wrapped error keeps its message authority — the wrapper adds no Msg of its
// own — so apierr still reports the innermost public message.
//
// It returns the error interface, not *Error, so that Wrap(op, nil) is a true
// nil and cannot be mistaken for a failure by a caller returning it directly.
func Wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	kind := KindInternal
	var domain *Error
	if errors.As(err, &domain) {
		kind = domain.Kind
	}
	return &Error{Kind: kind, Op: op, err: err}
}
