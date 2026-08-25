package rehydrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Manifest index keys (§5: manifest.json tracks per-index freshness under
// "opensearch" and "neo4j").
const (
	indexKeyEpisodic  = "opensearch"
	indexKeyKnowledge = "neo4j"
)

// debounceInterval is the stat-gate debounce window (§5: 2 seconds).
const debounceInterval = 2 * time.Second

// shaFieldSep / shaEntrySep frame the plane digest input so a key ending in
// the other separator cannot forge a different plane's digest.
const (
	shaFieldSep = "\x00"
	shaEntrySep = "\n"
)

// IndexKeyFor maps a plane to its manifest index key. It is the one place that
// decides which derived index owns a plane's freshness record.
func IndexKeyFor(plane Plane) string {
	if plane == PlaneKnowledge {
		return indexKeyKnowledge
	}
	return indexKeyEpisodic
}

// PlaneStateSHA computes a deterministic digest over every manifest file entry
// of one plane (sorted key + content sha). It is stored as the index's
// last_hydrated_sha after a successful hydration so CheckDrift can detect
// content changes that count comparisons alone would miss.
func PlaneStateSHA(m Manifest, plane Plane) string {
	prefix := string(plane) + "/"
	keys := make([]string, 0, len(m.Files))
	for k := range m.Files {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(shaFieldSep))
		h.Write([]byte(m.Files[k].SHA256))
		h.Write([]byte(shaEntrySep))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// planeHydrationStale reports whether the plane's recorded hydration digest no
// longer matches its hot content. An empty record means "never hydrated",
// which the count comparison already covers.
func planeHydrationStale(m Manifest, plane Plane) bool {
	st, ok := m.Indexes[IndexKeyFor(plane)]
	if !ok || st.LastHydratedSHA == "" {
		return false
	}
	return st.LastHydratedSHA != PlaneStateSHA(m, plane)
}

// planeTotals sums manifest record counts and dirty files for one plane.
func planeTotals(m Manifest, plane Plane) (records, dirty int) {
	prefix := string(plane) + "/"
	for k, fs := range m.Files {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		records += fs.RecordCount
		if fs.Dirty {
			dirty++
		}
	}
	return records, dirty
}

// commitManifest clears dirty flags and refreshes IndexedAt for every project
// that converged, and records the plane hydration sha when the whole plane
// converged.
func (s *service) commitManifest(ctx context.Context, projects []projectkey.Key, epOK, knOK map[string]bool, epFull, knFull bool) error {
	now := s.clock.Now().UTC()
	return s.store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) {
		nm := m.Clone()
		for _, p := range projects {
			if epOK[p.String()] {
				touchFile(&nm, ManifestFileKey(PlaneEpisodic, p), now)
			}
			if knOK[p.String()] {
				touchFile(&nm, ManifestFileKey(PlaneKnowledge, p), now)
			}
		}
		if epFull {
			nm.Indexes[indexKeyEpisodic] = IndexState{LastHydratedSHA: PlaneStateSHA(nm, PlaneEpisodic)}
		}
		if knFull {
			nm.Indexes[indexKeyKnowledge] = IndexState{LastHydratedSHA: PlaneStateSHA(nm, PlaneKnowledge)}
		}
		nm.UpdatedAt = now
		return nm, nil
	})
}

// touchFile marks one manifest file entry as freshly hydrated.
func touchFile(m *Manifest, fileKey string, now time.Time) {
	fs, ok := m.Files[fileKey]
	if !ok {
		return
	}
	fs.Dirty = false
	fs.IndexedAt = now
	m.Files[fileKey] = fs
}
