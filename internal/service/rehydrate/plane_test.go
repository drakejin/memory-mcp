package rehydrate

import (
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

func planeTestKey() projectkey.Key {
	return projectkey.Key{Workspace: "ws", Team: "team", Project: "proj"}
}

func TestManifestFileKey(t *testing.T) {
	tests := []struct {
		name  string
		plane Plane
		want  string
	}{
		{"episodic", PlaneEpisodic, "episodic/ws/team/proj"},
		{"knowledge", PlaneKnowledge, "knowledge/ws/team/proj"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ManifestFileKey(tt.plane, planeTestKey()); got != tt.want {
				t.Errorf("ManifestFileKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestManifestCloneSharesNoState(t *testing.T) {
	// Arrange
	orig := Manifest{
		Files:   map[string]FileState{"episodic/ws/team/proj": {SHA256: "aa", RecordCount: 1}},
		Indexes: map[string]IndexState{indexKeyEpisodic: {LastHydratedSHA: "bb"}},
	}

	// Act
	cp := orig.Clone()
	cp.Files["episodic/ws/team/proj"] = FileState{SHA256: "changed"}
	cp.Indexes[indexKeyEpisodic] = IndexState{LastHydratedSHA: "changed"}

	// Assert
	if orig.Files["episodic/ws/team/proj"].SHA256 != "aa" {
		t.Error("Clone must not share the Files map")
	}
	if orig.Indexes[indexKeyEpisodic].LastHydratedSHA != "bb" {
		t.Error("Clone must not share the Indexes map")
	}
}

func TestMarkFileIndexed(t *testing.T) {
	key := planeTestKey()
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	fk := ManifestFileKey(PlaneEpisodic, key)

	t.Run("clears dirty, stamps freshness and hydration sha", func(t *testing.T) {
		// Arrange
		m := Manifest{
			Files:   map[string]FileState{fk: {SHA256: "aa", Dirty: true}},
			Indexes: map[string]IndexState{},
		}

		// Act
		got := MarkFileIndexed(m, PlaneEpisodic, key, now)

		// Assert
		fs := got.Files[fk]
		if fs.Dirty {
			t.Error("dirty flag must be cleared")
		}
		if !fs.IndexedAt.Equal(now) {
			t.Errorf("IndexedAt = %v, want %v", fs.IndexedAt, now)
		}
		if !got.UpdatedAt.Equal(now) {
			t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, now)
		}
		want := PlaneStateSHA(got, PlaneEpisodic)
		if got.Indexes[IndexKeyFor(PlaneEpisodic)].LastHydratedSHA != want {
			t.Error("hydration sha must cover the current plane state")
		}
		// The input must stay untouched (immutability rule).
		if !m.Files[fk].Dirty {
			t.Error("input manifest was mutated")
		}
	})

	t.Run("unknown file entry only records the hydration sha", func(t *testing.T) {
		// Arrange
		m := Manifest{Files: map[string]FileState{}, Indexes: map[string]IndexState{}}

		// Act
		got := MarkFileIndexed(m, PlaneKnowledge, key, now)

		// Assert
		if len(got.Files) != 0 {
			t.Errorf("no file entry may be invented, got %v", got.Files)
		}
		if _, ok := got.Indexes[IndexKeyFor(PlaneKnowledge)]; !ok {
			t.Error("plane hydration sha must still be recorded")
		}
	})
}
