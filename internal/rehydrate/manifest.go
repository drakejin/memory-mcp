package rehydrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/hotstore"
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
func IndexKeyFor(plane hotstore.Plane) string {
	if plane == hotstore.PlaneKnowledge {
		return indexKeyKnowledge
	}
	return indexKeyEpisodic
}

// PlaneStateSHA computes a deterministic digest over every manifest file entry
// of one plane (sorted key + content sha). It is stored as the index's
// last_hydrated_sha after a successful hydration so CheckDrift can detect
// content changes that count comparisons alone would miss.
func PlaneStateSHA(m hotstore.Manifest, plane hotstore.Plane) string {
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
func planeHydrationStale(m hotstore.Manifest, plane hotstore.Plane) bool {
	st, ok := m.Indexes[IndexKeyFor(plane)]
	if !ok || st.LastHydratedSHA == "" {
		return false
	}
	return st.LastHydratedSHA != PlaneStateSHA(m, plane)
}

// planeTotals sums manifest record counts and dirty files for one plane.
func planeTotals(m hotstore.Manifest, plane hotstore.Plane) (records, dirty int) {
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
func (s *service) commitManifest(ctx context.Context, projects []hotstore.ProjectKey, epOK, knOK map[string]bool, epFull, knFull bool) error {
	now := s.clock.Now().UTC()
	return s.store.UpdateManifest(ctx, func(m hotstore.Manifest) (hotstore.Manifest, error) {
		nm := cloneManifest(m)
		for _, p := range projects {
			if epOK[p.String()] {
				touchFile(&nm, hotstore.ManifestFileKey(hotstore.PlaneEpisodic, p), now)
			}
			if knOK[p.String()] {
				touchFile(&nm, hotstore.ManifestFileKey(hotstore.PlaneKnowledge, p), now)
			}
		}
		if epFull {
			nm.Indexes[indexKeyEpisodic] = hotstore.IndexState{LastHydratedSHA: PlaneStateSHA(nm, hotstore.PlaneEpisodic)}
		}
		if knFull {
			nm.Indexes[indexKeyKnowledge] = hotstore.IndexState{LastHydratedSHA: PlaneStateSHA(nm, hotstore.PlaneKnowledge)}
		}
		nm.UpdatedAt = now
		return nm, nil
	})
}

// touchFile marks one manifest file entry as freshly hydrated.
func touchFile(m *hotstore.Manifest, fileKey string, now time.Time) {
	fs, ok := m.Files[fileKey]
	if !ok {
		return
	}
	fs.Dirty = false
	fs.IndexedAt = now
	m.Files[fileKey] = fs
}

// cloneManifest deep-copies a manifest so UpdateManifest callbacks never
// mutate shared map state (immutability rule).
func cloneManifest(m hotstore.Manifest) hotstore.Manifest {
	out := hotstore.Manifest{
		Files:     make(map[string]hotstore.FileState, len(m.Files)),
		Indexes:   make(map[string]hotstore.IndexState, len(m.Indexes)),
		UpdatedAt: m.UpdatedAt,
	}
	for k, v := range m.Files {
		out.Files[k] = v
	}
	for k, v := range m.Indexes {
		out.Indexes[k] = v
	}
	return out
}
