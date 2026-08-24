package errs_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/errs"
)

// errBoom is a foreign (non-*errs.Error) cause used to prove wrapping keeps the
// original error reachable through errors.Is.
var errBoom = errors.New("dial tcp 127.0.0.1:9200: connect: connection refused")

func TestConstructors(t *testing.T) {
	tests := []struct {
		name       string
		err        *errs.Error
		wantKind   errs.Kind
		wantOp     string
		wantEntity string
		wantID     string
		wantMsg    string
		wantCause  error
	}{
		{
			name:       "invalid",
			err:        errs.Invalid("episodic.Validate", "episode", "kind must be one of event|conversation"),
			wantKind:   errs.KindInvalid,
			wantOp:     "episodic.Validate",
			wantEntity: "episode",
			wantMsg:    "kind must be one of event|conversation",
		},
		{
			name:       "not found",
			err:        errs.NotFound("hotstore.ReadEpisode", "episode", "01JD"),
			wantKind:   errs.KindNotFound,
			wantOp:     "hotstore.ReadEpisode",
			wantEntity: "episode",
			wantID:     "01JD",
			wantMsg:    "episode not found",
		},
		{
			name:     "not found without entity",
			err:      errs.NotFound("hotstore.ReadEpisode", "", "01JD"),
			wantKind: errs.KindNotFound,
			wantOp:   "hotstore.ReadEpisode",
			wantID:   "01JD",
			wantMsg:  "not found",
		},
		{
			name:       "conflict",
			err:        errs.Conflict("knowledge.Supersede", "knowledge_node", "01JX", "node already superseded"),
			wantKind:   errs.KindConflict,
			wantOp:     "knowledge.Supersede",
			wantEntity: "knowledge_node",
			wantID:     "01JX",
			wantMsg:    "node already superseded",
		},
		{
			name:      "unavailable",
			err:       errs.Unavailable("search.Search", errBoom),
			wantKind:  errs.KindUnavailable,
			wantOp:    "search.Search",
			wantMsg:   "service unavailable",
			wantCause: errBoom,
		},
		{
			name:      "internal",
			err:       errs.Internal("cold.PutEpisodeBatch", errBoom),
			wantKind:  errs.KindInternal,
			wantOp:    "cold.PutEpisodeBatch",
			wantMsg:   "internal error",
			wantCause: errBoom,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", tc.err.Kind, tc.wantKind)
			}
			if tc.err.Op != tc.wantOp {
				t.Errorf("Op = %q, want %q", tc.err.Op, tc.wantOp)
			}
			if tc.err.Entity != tc.wantEntity {
				t.Errorf("Entity = %q, want %q", tc.err.Entity, tc.wantEntity)
			}
			if tc.err.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", tc.err.ID, tc.wantID)
			}
			if tc.err.Msg != tc.wantMsg {
				t.Errorf("Msg = %q, want %q", tc.err.Msg, tc.wantMsg)
			}
			if got := tc.err.Unwrap(); !errors.Is(got, tc.wantCause) {
				t.Errorf("Unwrap() = %v, want %v", got, tc.wantCause)
			}
			if tc.err.Fields != nil {
				t.Errorf("Fields = %v, want nil until WithField is used", tc.err.Fields)
			}
		})
	}
}

func TestErrorString(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "op message and id",
			err:  errs.NotFound("hotstore.ReadEpisode", "episode", "01JD"),
			want: "hotstore.ReadEpisode: episode not found: id=01JD",
		},
		{
			name: "no id",
			err:  errs.Invalid("episodic.Validate", "episode", "text must not be empty"),
			want: "episodic.Validate: text must not be empty",
		},
		{
			name: "cause appended",
			err:  errs.Unavailable("search.Search", errBoom),
			want: "search.Search: service unavailable: dial tcp 127.0.0.1:9200: connect: connection refused",
		},
		{
			name: "wrap keeps one path, not two messages",
			err:  errs.Wrap("consolidate.Run", errs.NotFound("hotstore.ReadEpisode", "episode", "01JD")),
			want: "consolidate.Run: hotstore.ReadEpisode: episode not found: id=01JD",
		},
		{
			name: "wrapped foreign error",
			err:  errs.Wrap("graph.Upsert", errBoom),
			want: "graph.Upsert: dial tcp 127.0.0.1:9200: connect: connection refused",
		},
		{
			name: "bare kind when nothing else is set",
			err:  &errs.Error{Kind: errs.KindInternal},
			want: "internal",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsMatchesSentinels pins the sentinel matrix: every kind matches its own
// sentinel and no other.
func TestIsMatchesSentinels(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
		kind errs.Kind
	}{
		{"ErrInvalid", errs.ErrInvalid, errs.KindInvalid},
		{"ErrNotFound", errs.ErrNotFound, errs.KindNotFound},
		{"ErrConflict", errs.ErrConflict, errs.KindConflict},
		{"ErrUnavailable", errs.ErrUnavailable, errs.KindUnavailable},
		{"ErrInternal", errs.ErrInternal, errs.KindInternal},
	}

	for _, subject := range sentinels {
		for _, target := range sentinels {
			t.Run(string(subject.kind)+"/"+target.name, func(t *testing.T) {
				err := &errs.Error{Kind: subject.kind, Op: "pkg.Op", Msg: "boom"}
				want := subject.kind == target.kind
				if got := errors.Is(err, target.err); got != want {
					t.Errorf("errors.Is(%s error, %s) = %v, want %v", subject.kind, target.name, got, want)
				}
				// The sentinel itself must match itself and nothing else.
				if got := errors.Is(subject.err, target.err); got != want {
					t.Errorf("errors.Is(%s, %s) = %v, want %v", subject.name, target.name, got, want)
				}
			})
		}
	}
}

// TestIsThroughWrapLevels walks a chain that alternates errs.Wrap and
// fmt.Errorf: the sentinel, every intermediate error and the root cause must
// all stay reachable.
func TestIsThroughWrapLevels(t *testing.T) {
	root := errs.NotFound("hotstore.ReadEpisode", "episode", "01JD")
	l1 := fmt.Errorf("read project file: %w", root)
	l2 := errs.Wrap("consolidate.collect", l1)
	l3 := fmt.Errorf("consolidate: %w", l2)
	l4 := errs.Wrap("server.handleConsolidate", l3)

	tests := []struct {
		name   string
		target error
		want   bool
	}{
		{"sentinel not found", errs.ErrNotFound, true},
		{"sentinel invalid", errs.ErrInvalid, false},
		{"sentinel internal", errs.ErrInternal, false},
		{"root domain error", root, true},
		{"intermediate fmt error", l1, true},
		{"intermediate wrap", l2, true},
		{"unrelated error", errBoom, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errors.Is(l4, tc.target); got != tc.want {
				t.Errorf("errors.Is(chain, %v) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}

	t.Run("errors.As yields the outermost domain error", func(t *testing.T) {
		var got *errs.Error
		if !errors.As(l4, &got) {
			t.Fatal("errors.As found no *errs.Error in the chain")
		}
		if got.Op != "server.handleConsolidate" {
			t.Errorf("Op = %q, want the outermost op", got.Op)
		}
		if got.Kind != errs.KindNotFound {
			t.Errorf("Kind = %q, want the kind carried through the wraps", got.Kind)
		}
	})
}

func TestSentinelStrings(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{errs.ErrInvalid, "invalid"},
		{errs.ErrNotFound, "not_found"},
		{errs.ErrConflict, "conflict"},
		{errs.ErrUnavailable, "unavailable"},
		{errs.ErrInternal, "internal"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsRejectsNonSentinelTargets(t *testing.T) {
	a := errs.NotFound("hotstore.ReadEpisode", "episode", "01JD")
	b := errs.NotFound("hotstore.ReadEpisode", "episode", "01JD")
	if errors.Is(a, b) {
		t.Error("two distinct *Error values must not compare equal; compare against sentinels instead")
	}
	if !errors.Is(a, a) {
		t.Error("an error must match itself")
	}
	if errors.Is(a, errBoom) {
		t.Error("an unrelated error must not match")
	}
}

func TestWrap(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantNil  bool
		wantKind errs.Kind
		wantRoot error // deepest cause that must stay reachable, nil when there is none
	}{
		{name: "nil in nil out", err: nil, wantNil: true},
		{name: "foreign error becomes internal", err: errBoom, wantKind: errs.KindInternal, wantRoot: errBoom},
		{
			name:     "domain kind is preserved",
			err:      errs.Unavailable("search.Search", errBoom),
			wantKind: errs.KindUnavailable,
			wantRoot: errBoom,
		},
		{
			name:     "kind survives an intermediate fmt wrap",
			err:      fmt.Errorf("layer: %w", errs.Conflict("knowledge.Supersede", "knowledge_node", "01JX", "already superseded")),
			wantKind: errs.KindConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := errs.Wrap("consolidate.Run", tc.err)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("Wrap(op, nil) = %#v, want a nil error interface", got)
				}
				return
			}
			var domain *errs.Error
			if !errors.As(got, &domain) {
				t.Fatalf("Wrap returned %T, want *errs.Error", got)
			}
			if domain.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", domain.Kind, tc.wantKind)
			}
			if domain.Op != "consolidate.Run" {
				t.Errorf("Op = %q, want consolidate.Run", domain.Op)
			}
			if domain.Msg != "" {
				t.Errorf("Msg = %q, want empty so the inner message stays authoritative", domain.Msg)
			}
			if !errors.Is(got, tc.err) {
				t.Error("the wrapped error must stay reachable through errors.Is")
			}
			if tc.wantRoot != nil && !errors.Is(got, tc.wantRoot) {
				t.Error("the root cause must stay reachable through errors.Is")
			}
			if tc.wantRoot == nil && errors.Is(got, errBoom) {
				t.Error("an unrelated root cause must not match")
			}
		})
	}
}

func TestWithFieldDoesNotMutate(t *testing.T) {
	base := errs.Invalid("document.Chunk", "document", "chunk limit exceeded")
	withOne := base.WithField("total", 620)
	withTwo := withOne.WithField("indexed", 500)

	if base.Fields != nil {
		t.Errorf("base.Fields = %v, want the original untouched", base.Fields)
	}
	if len(withOne.Fields) != 1 || withOne.Fields["total"] != 620 {
		t.Errorf("withOne.Fields = %v, want {total:620}", withOne.Fields)
	}
	if len(withTwo.Fields) != 2 || withTwo.Fields["indexed"] != 500 {
		t.Errorf("withTwo.Fields = %v, want {total:620, indexed:500}", withTwo.Fields)
	}
	if withOne.Kind != base.Kind || withOne.Op != base.Op || withOne.Msg != base.Msg {
		t.Error("WithField must copy every other field verbatim")
	}
	if !errors.Is(withTwo, errs.ErrInvalid) {
		t.Error("WithField must keep the kind matchable")
	}
}

func TestLogValue(t *testing.T) {
	err := errs.Wrap("consolidate.Run",
		errs.NotFound("hotstore.ReadEpisode", "episode", "01JD").WithField("project", "ws/team/proj"))

	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatal("errors.As found no *errs.Error")
	}

	outer := attrsOf(t, domain.LogValue())
	if outer["kind"] != string(errs.KindNotFound) {
		t.Errorf("kind = %v, want %q", outer["kind"], errs.KindNotFound)
	}
	if outer["op"] != "consolidate.Run" {
		t.Errorf("op = %v, want consolidate.Run", outer["op"])
	}

	cause, ok := outer["cause"].(map[string]any)
	if !ok {
		t.Fatalf("cause = %#v, want a nested group for a domain cause", outer["cause"])
	}
	if cause["entity"] != "episode" || cause["id"] != "01JD" {
		t.Errorf("cause group = %v, want entity/id of the inner error", cause)
	}
	if cause["project"] != "ws/team/proj" {
		t.Errorf("cause group = %v, want the structured field to flow into slog", cause)
	}
	if cause["msg"] != "episode not found" {
		t.Errorf("cause msg = %v, want the inner message", cause["msg"])
	}
}

func TestLogValueRendersForeignCauseAsString(t *testing.T) {
	got := attrsOf(t, errs.Unavailable("search.Ping", errBoom).LogValue())
	if got["cause"] != errBoom.Error() {
		t.Errorf("cause = %v, want the foreign error text", got["cause"])
	}
}

// TestIO pins the shared filesystem-error shape hotstore and blob both build
// on: KindInternal, the cause reachable, and the path in a log-only field that
// never reaches Msg.
func TestIO(t *testing.T) {
	err := errs.IO("hotstore.WriteKnowledge", "/Users/jin/.local/dj-memory/knowledge/ws/team/proj.json", errBoom)

	if !errors.Is(err, errs.ErrInternal) {
		t.Errorf("kind = %v, want internal", err.Kind)
	}
	if !errors.Is(err, errBoom) {
		t.Error("cause must stay reachable through errors.Is")
	}
	if err.Op != "hotstore.WriteKnowledge" {
		t.Errorf("op = %q", err.Op)
	}
	wantPath := "/Users/jin/.local/dj-memory/knowledge/ws/team/proj.json"
	if err.Fields["path"] != wantPath {
		t.Errorf("fields[path] = %v, want %q", err.Fields["path"], wantPath)
	}
	// The path is log-only: it must not leak into the client-facing message.
	if strings.Contains(err.Msg, wantPath) {
		t.Errorf("msg %q must not carry the path", err.Msg)
	}
}

// TestFromContext pins the ctx-to-domain-error conversion shared by every
// guard: nil while live, and a wrap that keeps context.Canceled matchable.
func TestFromContext(t *testing.T) {
	t.Run("live context is nil", func(t *testing.T) {
		if err := errs.FromContext(context.Background(), "blob.Get"); err != nil {
			t.Fatalf("want nil for a live context, got %v", err)
		}
	})

	tests := []struct {
		name    string
		ctx     func() context.Context
		wantErr error
	}{
		{
			name: "cancelled",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantErr: context.Canceled,
		},
		{
			name: "deadline exceeded",
			ctx: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			wantErr: context.DeadlineExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := errs.FromContext(tt.ctx(), "hotstore.ListProjects")
			if err == nil {
				t.Fatal("want an error for a dead context")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("want errors.Is(err, %v), got %v", tt.wantErr, err)
			}
			// A dead context is not classifiable as anything but internal.
			if !errors.Is(err, errs.ErrInternal) {
				t.Errorf("kind must be internal, got %v", err)
			}
			var domain *errs.Error
			if !errors.As(err, &domain) || domain.Op != "hotstore.ListProjects" {
				t.Errorf("op not attached: %v", err)
			}
		})
	}
}

// attrsOf flattens a resolved slog group into a map, recursing into nested
// groups so tests can assert on the whole shape.
func attrsOf(t *testing.T, v slog.Value) map[string]any {
	t.Helper()
	v = v.Resolve()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue kind = %v, want a group", v.Kind())
	}
	out := make(map[string]any)
	for _, a := range v.Group() {
		rv := a.Value.Resolve()
		if rv.Kind() == slog.KindGroup {
			out[a.Key] = attrsOf(t, rv)
			continue
		}
		out[a.Key] = rv.Any()
	}
	return out
}
