package rehydrate

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// Manifest index keys (§5: manifest.json tracks per-index freshness under
// "opensearch" and "neo4j").
const (
	IndexKeyEpisodic  = "opensearch"
	IndexKeyKnowledge = "neo4j"
)

// DebounceInterval is the stat-gate debounce window (§5: 2 seconds).
const DebounceInterval = 2 * time.Second

// FileKey renders the manifest file key "{plane}/{ws}/{team}/{proj}" for one
// project plane file.
func FileKey(plane hotstore.Plane, key hotstore.ProjectKey) string {
	return string(plane) + "/" + key.String()
}

// IndexKeyFor maps a plane to its manifest index key.
func IndexKeyFor(plane hotstore.Plane) string {
	if plane == hotstore.PlaneKnowledge {
		return IndexKeyKnowledge
	}
	return IndexKeyEpisodic
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
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		io.WriteString(h, k)
		h.Write([]byte{0})
		io.WriteString(h, m.Files[k].SHA256)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
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
