package episode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

func TestAppend(t *testing.T) {
	req := func() AppendRequest {
		return AppendRequest{Kind: KindEvent, Actor: ActorAgent, Text: "hot write must survive"}
	}

	tests := []struct {
		name   string
		req    AppendRequest
		setup  func(*fixture)
		mutate func(*Config)
		// wantErr is the errs sentinel the call must match; nil = success.
		wantErr error
		// wantCause must remain reachable through the wrap chain.
		wantCause    error
		wantDegraded []string
		wantStored   int
		wantDirty    []string
		wantMarks    []string
		wantLog      string
	}{
		{
			name:       "happy path writes hot then index and refreshes freshness",
			req:        req(),
			wantStored: 1,
			wantMarks:  []string{testKey.String()},
		},
		{
			name: "id generation failure writes nothing",
			req:  req(),
			setup: func(f *fixture) {
				f.ids.err = errBoom
			},
			wantErr:   errs.ErrInternal,
			wantCause: errBoom,
		},
		{
			name: "hot write failure fails the call and keeps its kind",
			req:  req(),
			setup: func(f *fixture) {
				f.store.appendErr = errs.Conflict("hotstore.AppendEpisode", entityEpisode, ulidA, "duplicate id")
			},
			wantErr: errs.ErrConflict,
		},
		{
			name:         "absent index degrades the write and marks dirty (P1)",
			req:          req(),
			mutate:       func(c *Config) { c.Index = nil },
			wantDegraded: []string{DegradedSearch},
			wantStored:   1,
			wantDirty:    []string{testKey.String()},
		},
		{
			name: "index upsert failure degrades the write and marks dirty (P1)",
			req:  req(),
			setup: func(f *fixture) {
				f.index.indexErr = errIndexDown
			},
			wantDegraded: []string{DegradedSearch},
			wantStored:   1,
			wantDirty:    []string{testKey.String()},
			wantLog:      "episode index upsert failed; degraded",
		},
		{
			name: "dirty-mark failure is log-only",
			req:  req(),
			setup: func(f *fixture) {
				f.books.dirtyErr = errBoom
			},
			mutate:       func(c *Config) { c.Index = nil },
			wantDegraded: []string{DegradedSearch},
			wantStored:   1,
			wantLog:      "manifest dirty mark failed",
		},
		{
			name: "freshness-mark failure is log-only",
			req:  req(),
			setup: func(f *fixture) {
				f.books.indexedErr = errBoom
			},
			wantStored: 1,
			wantLog:    "manifest freshness update failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.setup != nil {
				tc.setup(f)
			}
			svc := f.service(t, tc.mutate)

			res, err := svc.Append(context.Background(), testKey, tc.req)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
					t.Errorf("cause %v lost from chain %v", tc.wantCause, err)
				}
			} else if err != nil {
				t.Fatalf("Append: %v", err)
			}

			if !slices.Equal(res.Degraded, tc.wantDegraded) {
				t.Errorf("degraded = %v, want %v", res.Degraded, tc.wantDegraded)
			}
			if got := len(f.store.episodes[testKey.String()]); got != tc.wantStored {
				t.Errorf("hot store holds %d records, want %d", got, tc.wantStored)
			}
			if !slices.Equal(f.books.dirty, tc.wantDirty) {
				t.Errorf("dirty marks = %v, want %v", f.books.dirty, tc.wantDirty)
			}
			if !slices.Equal(f.books.indexed, tc.wantMarks) {
				t.Errorf("freshness marks = %v, want %v", f.books.indexed, tc.wantMarks)
			}
			if tc.wantLog != "" && !strings.Contains(f.logBuf.String(), tc.wantLog) {
				t.Errorf("log %q does not contain %q", f.logBuf.String(), tc.wantLog)
			}
		})
	}
}

// TestAppendAssignsServerControlledFields pins the §2.1 invariants the caller
// can never influence: server-minted id from the injected clock, occurred_at
// defaulting, normalized entities, and consolidated always starting false.
func TestAppendAssignsServerControlledFields(t *testing.T) {
	f := newFixture()
	svc := f.service(t, nil)

	res, err := svc.Append(context.Background(), testKey, AppendRequest{
		Kind:     KindDecision,
		Actor:    ActorUser,
		Text:     "결정",
		Entities: []string{" memory-mcp ", "", "opensearch"},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	rec := res.Record
	if rec.ID == "" {
		t.Error("service must assign the id")
	}
	// The id's time half must come from the injected clock, never the wall
	// clock — otherwise nothing about a stored record is reproducible.
	if f.ids.lastMillis != fixedNow.UnixMilli() {
		t.Errorf("id minted at %d, want the injected clock %d", f.ids.lastMillis, fixedNow.UnixMilli())
	}
	if !rec.OccurredAt.Equal(fixedNow) {
		t.Errorf("occurred_at = %v, want clock now %v", rec.OccurredAt, fixedNow)
	}
	if rec.Consolidated {
		t.Error("consolidated must always start false (§3: the server never auto-sets it)")
	}
	if !slices.Equal(rec.Entities, []string{"memory-mcp", "opensearch"}) {
		t.Errorf("entities = %v, want trimmed non-empty entries", rec.Entities)
	}
	stored := f.store.episodes[testKey.String()]
	if len(stored) != 1 || stored[0].ID != rec.ID {
		t.Fatalf("hot store = %+v, want exactly the returned record", stored)
	}
	if got := f.index.indexed[testKey.String()]; len(got) != 1 || got[0].ID != rec.ID {
		t.Fatalf("index = %+v, want the stored record mirrored", got)
	}
	// P1 ordering: the canonical write strictly precedes the derived upsert.
	want := []string{"store.Append", "index.Index", "books.MarkIndexed"}
	if !slices.Equal(f.j.events, want) {
		t.Errorf("call order = %v, want %v", f.j.events, want)
	}
}

// TestAppendEmptyEntitiesStayNonNil pins the hot-JSON rule that entities is
// [] rather than null even when the caller sent nothing.
func TestAppendEmptyEntitiesStayNonNil(t *testing.T) {
	f := newFixture()
	svc := f.service(t, nil)

	res, err := svc.Append(context.Background(), testKey, AppendRequest{
		Kind: KindEvent, Actor: ActorAgent, Text: "t",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if res.Record.Entities == nil || len(res.Record.Entities) != 0 {
		t.Fatalf("entities = %#v, want a non-nil empty slice", res.Record.Entities)
	}
}

func TestAppendExplicitOccurredAtIsPreservedInUTC(t *testing.T) {
	f := newFixture()
	svc := f.service(t, nil)

	seoul := time.FixedZone("KST", 9*60*60)
	when := time.Date(2025, 1, 2, 12, 4, 5, 0, seoul)
	res, err := svc.Append(context.Background(), testKey, AppendRequest{
		Kind: KindObservation, Actor: ActorSystem, Text: "t", OccurredAt: when,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !res.Record.OccurredAt.Equal(when) {
		t.Fatalf("occurred_at = %v, want the same instant as %v", res.Record.OccurredAt, when)
	}
	if res.Record.OccurredAt.Location() != time.UTC {
		t.Fatalf("occurred_at zone = %v, want UTC (§2.1 time discipline)", res.Record.OccurredAt.Location())
	}
}
