package episode

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// seedSearchable stocks the hot store with one record and mirrors it as the
// index's canned hit, the minimal world where a search has consequences.
func seedSearchable(f *fixture) {
	f.store.episodes[testKey.String()] = []Record{{
		ID: ulidA, Kind: KindEvent, Actor: ActorAgent, Text: "보안", OccurredAt: fixedNow,
	}}
	f.index.hits = []Hit{{Record: Record{ID: ulidA, Kind: KindEvent}, Score: 1.5, Excerpt: "…<em>보안</em>…"}}
}

func TestSearchUnavailableIndexIs503Material(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*fixture)
		mutate func(*Config)
		// wantCause must stay reachable when the index supplied one.
		wantCause error
		// wantGateCalls pins the §5 order: an absent index fails before the
		// gate, a present-but-dead one fails after it.
		wantGateCalls int
	}{
		{
			name:   "index not configured",
			mutate: func(c *Config) { c.Index = nil },
		},
		{
			name:          "index unreachable",
			setup:         func(f *fixture) { f.index.searchErr = errIndexDown },
			wantCause:     errBoom,
			wantGateCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.setup != nil {
				tc.setup(f)
			}
			svc := f.service(t, tc.mutate)

			res, err := svc.Search(context.Background(), testKey, Query{Text: "보안"})

			if !errors.Is(err, errs.ErrUnavailable) {
				t.Fatalf("error = %v, want errs.ErrUnavailable", err)
			}
			// The §5 contract: the error's public message is the degraded
			// vocabulary word for word, so the handler's 503 body reads the
			// same whether it maps the kind or matches the sentinel.
			if domain := asDomain(t, err); domain.Msg != DegradedSearch {
				t.Errorf("public message = %q, want %q", domain.Msg, DegradedSearch)
			}
			if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
				t.Errorf("cause %v lost from chain %v", tc.wantCause, err)
			}
			if len(res.Hits) != 0 {
				t.Errorf("hits = %v, want none on failure", res.Hits)
			}
			if len(f.gate.calls) != tc.wantGateCalls {
				t.Errorf("gate calls = %v, want %d", f.gate.calls, tc.wantGateCalls)
			}
			if f.store.updateCalls != 0 {
				t.Error("a failed search must not touch hot records")
			}
		})
	}
}

func TestSearchUnclassifiedIndexFailureStaysInternal(t *testing.T) {
	f := newFixture()
	f.index.searchErr = errBoom
	svc := f.service(t, nil)

	_, err := svc.Search(context.Background(), testKey, Query{Text: "a"})
	if !errors.Is(err, errs.ErrInternal) {
		t.Fatalf("error = %v, want errs.ErrInternal", err)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("cause lost from chain %v", err)
	}
	// The wrap authors no message of its own, so the transport fallback
	// ("internal error") stays in charge — same body as before the split.
	if domain := asDomain(t, err); domain.Msg != "" {
		t.Errorf("wrap message = %q, want none", domain.Msg)
	}
}

func TestSearchHappyPathGatesBumpsAndConverges(t *testing.T) {
	f := newFixture()
	seedSearchable(f)
	svc := f.service(t, nil)

	res, err := svc.Search(context.Background(), testKey, Query{Text: "보안"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if !reflect.DeepEqual(res.Hits, f.index.hits) {
		t.Errorf("hits = %+v, want the index hits unmodified %+v", res.Hits, f.index.hits)
	}
	if len(res.Degraded) != 0 {
		t.Errorf("degraded = %v, want none", res.Degraded)
	}
	if !slices.Equal(f.gate.calls, []string{testKey.String()}) {
		t.Errorf("stat-gate calls = %v, want [%s]", f.gate.calls, testKey.String())
	}

	// P2: the recall material lands in hot.
	got := f.store.episodes[testKey.String()][0]
	if got.RecallCount != 1 || got.LastRecalled != fixedNow.Format(time.RFC3339) {
		t.Fatalf("recall bookkeeping = (%d, %q), want (1, %q)",
			got.RecallCount, got.LastRecalled, fixedNow.Format(time.RFC3339))
	}

	// …and the derived doc is refreshed from hot, carrying the bumped values.
	reindexed := f.index.indexed[testKey.String()]
	if len(reindexed) != 1 || reindexed[0].ID != ulidA || reindexed[0].RecallCount != 1 {
		t.Fatalf("converged docs = %+v, want the bumped hot copy of %s", reindexed, ulidA)
	}
	if !slices.Equal(f.books.indexed, []string{testKey.String()}) {
		t.Errorf("freshness marks = %v, want [%s]", f.books.indexed, testKey.String())
	}

	// P2 ordering, end to end: gate → search → hot bump → hot re-read →
	// index refresh → freshness mark. Hot strictly precedes the index.
	want := []string{"gate.StatGate", "index.Search", "store.Update", "store.List", "index.Index", "books.MarkIndexed"}
	if !slices.Equal(f.j.events, want) {
		t.Errorf("call order = %v, want %v", f.j.events, want)
	}
}

// TestSearchRecallBumpPersists proves the recall material accumulates in hot
// across searches (P2): the ranking signal must survive any derived rebuild.
func TestSearchRecallBumpPersists(t *testing.T) {
	f := newFixture()
	seedSearchable(f)
	svc := f.service(t, nil)

	for i := 0; i < 2; i++ {
		if _, err := svc.Search(context.Background(), testKey, Query{Text: "보안"}); err != nil {
			t.Fatalf("Search #%d: %v", i+1, err)
		}
	}

	got := f.store.episodes[testKey.String()][0]
	if got.RecallCount != 2 {
		t.Fatalf("recall_count = %d after two searches, want 2", got.RecallCount)
	}
	// Every converge pass re-indexed the then-current hot copy.
	reindexed := f.index.indexed[testKey.String()]
	if len(reindexed) != 2 || reindexed[1].RecallCount != 2 {
		t.Fatalf("converged docs = %+v, want the second carrying recall_count 2", reindexed)
	}
}

// TestSearchConvergeFailures drives every best-effort branch after a recall
// bump: each failure leaves the request successful and the manifest honest.
func TestSearchConvergeFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fixture)
		// wantIndexedIDs are the ids the converge step re-indexed.
		wantIndexedIDs []string
		wantDirty      []string
		wantMarks      []string
		wantLog        string
	}{
		{
			name:           "happy path re-indexes the bumped record",
			wantIndexedIDs: []string{ulidA},
			wantMarks:      []string{testKey.String()},
		},
		{
			name:      "index upsert failure marks the plane dirty",
			setup:     func(f *fixture) { f.index.indexErr = errBoom },
			wantDirty: []string{testKey.String()},
			wantLog:   "episode converge: index upsert failed; degraded",
		},
		{
			name:      "hot re-read failure marks dirty rather than indexing stale copies",
			setup:     func(f *fixture) { f.store.listErr = errBoom },
			wantDirty: []string{testKey.String()},
			wantLog:   "episode converge: list episodes failed",
		},
		{
			name:    "recall bump failure leaves hot unchanged so nothing needs converging",
			setup:   func(f *fixture) { f.store.updateErr = errBoom },
			wantLog: "recall bump failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			seedSearchable(f)
			if tc.setup != nil {
				tc.setup(f)
			}
			svc := f.service(t, nil)

			res, err := svc.Search(context.Background(), testKey, Query{Text: "보안"})
			if err != nil {
				t.Fatalf("a best-effort failure must never fail the search: %v", err)
			}
			if len(res.Hits) != 1 {
				t.Fatalf("hits = %+v, want the search result untouched", res.Hits)
			}

			var gotIDs []string
			for _, r := range f.index.indexed[testKey.String()] {
				gotIDs = append(gotIDs, r.ID)
			}
			if !slices.Equal(gotIDs, tc.wantIndexedIDs) {
				t.Errorf("re-indexed ids = %v, want %v", gotIDs, tc.wantIndexedIDs)
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

// TestSearchConvergeSkipsIdsAbsentFromHot pins the guard that keeps a hit for
// an already-aged record from re-indexing anything: nothing changed in hot, so
// neither the index nor the manifest is touched.
func TestSearchConvergeSkipsIdsAbsentFromHot(t *testing.T) {
	f := newFixture()
	// The index still serves a hit whose record has left hot.
	f.index.hits = []Hit{{Record: Record{ID: ulidC}, Score: 0.5, Excerpt: "stale"}}
	svc := f.service(t, nil)

	res, err := svc.Search(context.Background(), testKey, Query{Text: "a"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %+v, want the stale hit still served", res.Hits)
	}
	if f.store.updateCalls != 1 {
		t.Errorf("update calls = %d, want the bump attempted once", f.store.updateCalls)
	}
	if len(f.index.indexed[testKey.String()]) != 0 {
		t.Error("nothing changed in hot, so nothing may be re-indexed")
	}
	if len(f.books.dirty) != 0 || len(f.books.indexed) != 0 {
		t.Errorf("manifest touched (dirty=%v indexed=%v), want untouched", f.books.dirty, f.books.indexed)
	}
}

func TestSearchNilHitsBecomeEmptySliceWithoutBump(t *testing.T) {
	f := newFixture()
	f.index.hits = nil
	svc := f.service(t, nil)

	res, err := svc.Search(context.Background(), testKey, Query{Text: "a"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// A nil slice would serialize as null; clients get [] instead (§7).
	if res.Hits == nil || len(res.Hits) != 0 {
		t.Fatalf("hits = %#v, want a non-nil empty slice", res.Hits)
	}
	if f.store.updateCalls != 0 {
		t.Fatal("a zero-hit search must not touch hot records for recall bookkeeping")
	}
}

func TestSearchGateFailureNeverBlocks(t *testing.T) {
	f := newFixture()
	seedSearchable(f)
	f.gate.err = errBoom
	svc := f.service(t, nil)

	res, err := svc.Search(context.Background(), testKey, Query{Text: "보안"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %+v, want the result despite the failing gate", res.Hits)
	}
	if !strings.Contains(f.logBuf.String(), "stat-gate rehydration incomplete") {
		t.Errorf("log %q does not report the gate failure", f.logBuf.String())
	}
}

// TestSearchWithoutGateSkipsIt covers the nil-Gate wiring: the service runs,
// no gate event appears.
func TestSearchWithoutGateSkipsIt(t *testing.T) {
	f := newFixture()
	seedSearchable(f)
	svc := f.service(t, func(c *Config) { c.Gate = nil })

	if _, err := svc.Search(context.Background(), testKey, Query{Text: "보안"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if slices.Contains(f.j.events, "gate.StatGate") {
		t.Fatalf("call order %v contains a gate call despite nil Gate", f.j.events)
	}
}

func TestSearchPassesQueryThroughUnmodified(t *testing.T) {
	f := newFixture()
	svc := f.service(t, nil)

	q := Query{
		Text:  "보안을 끄고",
		From:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		Kinds: []Kind{KindEvent, KindDecision},
		Size:  7,
	}
	if _, err := svc.Search(context.Background(), testKey, q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !reflect.DeepEqual(f.index.lastQuery, q) {
		t.Fatalf("index received %+v, want %+v", f.index.lastQuery, q)
	}
}
