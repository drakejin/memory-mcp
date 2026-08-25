package consolidate

// HotState tests. These pin the /v1/status counting behaviour that used to
// live in the transport layer: exact counts (never over- or under-reported),
// the TTL staleness boundary, sorted dirty files, and the §0-principle-3 rule
// that a failed count degrades the answer instead of hiding it.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// hotStateTTL mirrors newTestService's TTLDays for staleness arithmetic.
const hotStateTTL = 30 * 24 * time.Hour

// staleRec builds a record age old; consolidated records are never counted.
func staleRec(id string, age time.Duration, consolidated bool) episode.Record {
	return episode.Record{
		ID: id, Kind: episode.KindEvent, Actor: episode.ActorAgent, Text: "t",
		OccurredAt: runNow.Add(-age), Consolidated: consolidated,
	}
}

func TestHotStateCountsUnconsolidatedAndStale(t *testing.T) {
	// Arrange
	c := &calls{}
	store := newFakeStore(c)
	store.episodes[runKey.String()] = []episode.Record{
		staleRec("01JD00000000000000000000A0", hotStateTTL+48*time.Hour, false), // stale + unconsolidated
		staleRec("01JD00000000000000000000B0", time.Hour, false),                // fresh + unconsolidated
		staleRec("01JD00000000000000000000C0", hotStateTTL+48*time.Hour, true),  // consolidated: not counted
	}
	svc := newTestService(t, store, nil, nil)

	// Act
	hs, err := svc.HotState(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("HotState() = %v", err)
	}
	if hs.Unconsolidated != 2 {
		t.Errorf("unconsolidated = %d, want 2", hs.Unconsolidated)
	}
	if hs.StaleUnconsolidated != 1 {
		t.Errorf("stale_unconsolidated = %d, want 1", hs.StaleUnconsolidated)
	}
	if len(hs.Degraded) != 0 {
		t.Errorf("degraded = %v, want none", hs.Degraded)
	}
}

func TestHotStateCountsAcrossProjects(t *testing.T) {
	// Arrange
	c := &calls{}
	store := newFakeStore(c)
	other := projectkey.Key{Workspace: "vms", Team: "core", Project: "alpha"}
	store.episodes[runKey.String()] = []episode.Record{staleRec("01JD00000000000000000000A0", time.Hour, false)}
	store.episodes[other.String()] = []episode.Record{staleRec("01JD00000000000000000000B0", time.Hour, false)}
	svc := newTestService(t, store, nil, nil)

	// Act
	hs, err := svc.HotState(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("HotState() = %v", err)
	}
	if hs.Unconsolidated != 2 {
		t.Fatalf("unconsolidated = %d, want 2 across both projects", hs.Unconsolidated)
	}
}

func TestHotStateReportsDirtyFilesSorted(t *testing.T) {
	// Arrange
	c := &calls{}
	store := newFakeStore(c)
	other := projectkey.Key{Workspace: "vms", Team: "core", Project: "alpha"}
	store.manifest = rehydrate.Manifest{
		Files: map[string]rehydrate.FileState{
			rehydrate.ManifestFileKey(rehydrate.PlaneKnowledge, runKey): {Dirty: true},
			rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, other):   {Dirty: true},
			rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, runKey):  {Dirty: false},
		},
		UpdatedAt: runNow.Add(-time.Minute),
	}
	svc := newTestService(t, store, nil, nil)

	// Act
	hs, err := svc.HotState(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("HotState() = %v", err)
	}
	want := []string{
		rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, other),
		rehydrate.ManifestFileKey(rehydrate.PlaneKnowledge, runKey),
	}
	if !slices.Equal(hs.DirtyFiles, want) {
		t.Fatalf("dirty_files = %v, want %v (sorted)", hs.DirtyFiles, want)
	}
	if !hs.ManifestUpdatedAt.Equal(store.manifest.UpdatedAt) {
		t.Errorf("manifest_updated_at = %v, want %v", hs.ManifestUpdatedAt, store.manifest.UpdatedAt)
	}
}

func TestHotStateEmptyStateHasNonNilSlices(t *testing.T) {
	// Arrange
	c := &calls{}
	svc := newTestService(t, newFakeStore(c), nil, nil)

	// Act
	hs, err := svc.HotState(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("HotState() = %v", err)
	}
	if hs.DirtyFiles == nil || hs.Degraded == nil {
		t.Fatal("DirtyFiles and Degraded must serialize as [], never null")
	}
}

func TestHotStateManifestFailureFailsTheCall(t *testing.T) {
	// Arrange
	c := &calls{}
	store := newFakeStore(c)
	store.readErr = errors.New("boom")
	svc := newTestService(t, store, nil, nil)

	// Act
	_, err := svc.HotState(context.Background())

	// Assert
	if err == nil {
		t.Fatal("an unreadable manifest must fail HotState — there is no honest partial answer")
	}
}

func TestHotStateCountFailuresDegradeNotFail(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*fakeStore)
		wantNote   string
		wantPrefix bool
	}{
		{
			name:     "project listing fails",
			mutate:   func(s *fakeStore) { s.listProjErr = errors.New("boom") },
			wantNote: DegradedCountUnavailable,
		},
		{
			name: "episode listing fails",
			mutate: func(s *fakeStore) {
				s.episodes[runKey.String()] = []episode.Record{staleRec("01JD00000000000000000000A0", time.Hour, false)}
				s.listErr = errors.New("boom")
			},
			wantNote:   DegradedCountIncompletePrefix,
			wantPrefix: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			c := &calls{}
			store := newFakeStore(c)
			tc.mutate(store)
			svc := newTestService(t, store, nil, nil)

			// Act
			hs, err := svc.HotState(context.Background())

			// Assert
			if err != nil {
				t.Fatalf("HotState() = %v, want a degraded answer, not a failure", err)
			}
			if len(hs.Degraded) == 0 {
				t.Fatal("an unreadable count must be disclosed in Degraded")
			}
			if tc.wantPrefix {
				if !strings.HasPrefix(hs.Degraded[0], tc.wantNote) {
					t.Fatalf("degraded = %v, want prefix %q", hs.Degraded, tc.wantNote)
				}
				return
			}
			if hs.Degraded[0] != tc.wantNote {
				t.Fatalf("degraded = %v, want %q", hs.Degraded, tc.wantNote)
			}
		})
	}
}
