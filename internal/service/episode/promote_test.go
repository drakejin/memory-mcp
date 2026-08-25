package episode

// Promote tests: the §3 provenance promotion is wholly best-effort — the only
// outputs are hot-state changes, converge side effects and degraded notes,
// never an error.

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// seedPromotion stores one unconsolidated (ulidA), one consolidated (ulidB)
// record under testKey.
func seedPromotion(f *fixture) {
	f.store.episodes[testKey.String()] = []Record{
		{ID: ulidA, Kind: KindEvent, Actor: ActorAgent, Text: "raw", Consolidated: false},
		{ID: ulidB, Kind: KindEvent, Actor: ActorAgent, Text: "done", Consolidated: true},
	}
}

func TestPromoteMarksNamedUnconsolidatedAndConverges(t *testing.T) {
	// Arrange
	f := newFixture()
	seedPromotion(f)
	svc := f.service(t, nil)

	// Act: names the pending record, an already-consolidated one, and an id
	// this project has never seen (aged to cold, or another project's).
	notes := svc.Promote(context.Background(), testKey, []string{ulidA, ulidB, ulidC})

	// Assert
	if notes != nil {
		t.Fatalf("notes = %v, want none", notes)
	}
	recs := f.store.episodes[testKey.String()]
	if !recs[0].Consolidated {
		t.Error("named unconsolidated record must be marked consolidated")
	}
	if f.store.updateCalls != 1 {
		t.Errorf("update calls = %d, want exactly one batch", f.store.updateCalls)
	}
	// Converge follows: hot rewrite first, then the derived refresh (P2).
	want := []string{"store.List", "store.Update", "store.List", "index.Index", "books.MarkIndexed"}
	if !slices.Equal(f.j.events, want) {
		t.Errorf("call order = %v, want %v", f.j.events, want)
	}
	if got := f.index.indexed[testKey.String()]; len(got) != 1 || got[0].ID != ulidA || !got[0].Consolidated {
		t.Errorf("indexed = %+v, want the consolidated form of %s", got, ulidA)
	}
}

func TestPromoteSkipsWhenNothingPending(t *testing.T) {
	tests := []struct {
		name       string
		provenance []string
	}{
		{"empty provenance", nil},
		{"only consolidated ids", []string{ulidB}},
		{"only unknown ids", []string{ulidC}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			f := newFixture()
			seedPromotion(f)
			svc := f.service(t, nil)

			// Act
			notes := svc.Promote(context.Background(), testKey, tc.provenance)

			// Assert
			if notes != nil {
				t.Fatalf("notes = %v, want none", notes)
			}
			if f.store.updateCalls != 0 {
				t.Error("nothing pending must rewrite nothing")
			}
			if slices.Contains(f.j.events, "index.Index") {
				t.Error("nothing pending must not touch the index")
			}
		})
	}
}

func TestPromoteFailuresDegradeNotFail(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*fixture)
		wantLog string
	}{
		{
			name:    "list episodes fails",
			mutate:  func(f *fixture) { f.store.listErr = errBoom },
			wantLog: "provenance promotion: list episodes failed",
		},
		{
			name: "mark consolidated fails",
			mutate: func(f *fixture) {
				seedPromotion(f)
				f.store.updateErr = errBoom
			},
			wantLog: "provenance promotion: mark consolidated failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			f := newFixture()
			tc.mutate(f)
			svc := f.service(t, nil)

			// Act
			notes := svc.Promote(context.Background(), testKey, []string{ulidA})

			// Assert
			if len(notes) != 1 || notes[0] != DegradedPromotion {
				t.Fatalf("notes = %v, want [%q]", notes, DegradedPromotion)
			}
			if !strings.Contains(f.logBuf.String(), tc.wantLog) {
				t.Errorf("log %q must contain %q", f.logBuf.String(), tc.wantLog)
			}
		})
	}
}

func TestPromoteWithoutIndexMarksDirty(t *testing.T) {
	// Arrange: hot succeeds, no index wired — the promotion stands (§1) and the
	// dirty mark hands derived convergence to rehydration.
	f := newFixture()
	seedPromotion(f)
	svc := f.service(t, func(c *Config) { c.Index = nil })

	// Act
	notes := svc.Promote(context.Background(), testKey, []string{ulidA})

	// Assert
	if notes != nil {
		t.Fatalf("notes = %v, want none — the hot promotion succeeded", notes)
	}
	if !f.store.episodes[testKey.String()][0].Consolidated {
		t.Error("hot record must still be marked consolidated")
	}
	if !slices.Contains(f.j.events, "books.MarkDirty") {
		t.Errorf("events = %v, want a dirty mark for the unconverged plane", f.j.events)
	}
}

func TestPromoteConvergeFailureIsLogOnly(t *testing.T) {
	// Arrange: the batch update succeeds, then the converge re-list fails. The
	// promotion already landed in hot, so the answer stays un-degraded; the gap
	// is a dirty mark plus a log line.
	f := newFixture()
	seedPromotion(f)
	svc := f.service(t, nil)
	var calls int
	f.store.listHook = func() error {
		calls++
		if calls > 1 { // first list feeds the promotion; second is the converge
			return errBoom
		}
		return nil
	}

	// Act
	notes := svc.Promote(context.Background(), testKey, []string{ulidA})

	// Assert
	if notes != nil {
		t.Fatalf("notes = %v, want none", notes)
	}
	if !strings.Contains(f.logBuf.String(), "episode converge: list episodes failed") {
		t.Errorf("log = %q, want the converge failure recorded", f.logBuf.String())
	}
	if !slices.Contains(f.j.events, "books.MarkDirty") {
		t.Errorf("events = %v, want a dirty mark", f.j.events)
	}
}
