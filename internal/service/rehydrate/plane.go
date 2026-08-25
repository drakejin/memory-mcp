package rehydrate

// The manifest vocabulary lives here, not in the hot store adapter: the
// manifest is the freshness contract between canonical hot JSON and the
// derived stores (§5), and this service is its consumer-side owner. The hot
// store (internal/external/persistence/hotstore) persists these values the
// same way it persists episode.Record — an external adapter depending inward
// on a domain value type (feature-inventory.md §4.1 rule 4), never the other
// way around.

import (
	"time"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Plane names one of the two memory planes for manifest bookkeeping.
type Plane string

const (
	PlaneEpisodic  Plane = "episodic"
	PlaneKnowledge Plane = "knowledge"
)

// pathSep joins plane and key segments in manifest keys. It is "/" on every
// platform: these are logical keys, not host paths.
const pathSep = "/"

// ManifestFileKey renders the Manifest.Files key for one project plane file:
// "{plane}/{ws}/{team}/{proj}". Single source of truth for the format.
func ManifestFileKey(plane Plane, key projectkey.Key) string {
	return string(plane) + pathSep + key.String()
}

// FileState is the manifest entry for one hot file (§5).
type FileState struct {
	SHA256 string `json:"sha256"`
	// RecordCount is the derived-store unit count: episodic records for the
	// episodic plane, knowledge NODES (not edges) for the knowledge plane, so
	// drift checks can compare it against OpenSearch doc-count / Neo4j
	// node-count.
	RecordCount int       `json:"record_count"`
	IndexedAt   time.Time `json:"indexed_at"`
	// Dirty marks a best-effort derived upsert that failed; the next
	// rehydration pass converges it (§1).
	Dirty bool `json:"dirty"`
}

// IndexState is the manifest entry for one derived index (§5).
type IndexState struct {
	LastHydratedSHA string `json:"last_hydrated_sha"`
}

// Manifest mirrors manifest.json: per-file hashes plus per-index freshness.
// File keys are "{plane}/{ws}/{team}/{proj}" (see ManifestFileKey); index keys
// are "opensearch" and "neo4j".
type Manifest struct {
	Files   map[string]FileState  `json:"files"`
	Indexes map[string]IndexState `json:"indexes"`
	// UpdatedAt is when the manifest itself was last rewritten.
	UpdatedAt time.Time `json:"updated_at"`
}

// Clone returns a deep copy so callers and mutation funcs never share maps
// (immutability rule). Exported because the hot store hands cloned manifests
// across its own boundary with it.
func (m Manifest) Clone() Manifest {
	out := Manifest{
		Files:     make(map[string]FileState, len(m.Files)),
		Indexes:   make(map[string]IndexState, len(m.Indexes)),
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

// MarkFileIndexed returns m with the plane file of key marked freshly indexed:
// dirty cleared, IndexedAt refreshed, and the plane hydration sha updated so
// CheckDrift stays quiet for converged content (§5). It is the pure half of
// the bookkeeping the composition root's plane adapters run after a
// successful best-effort derived upsert; m is never mutated.
func MarkFileIndexed(m Manifest, plane Plane, key projectkey.Key, now time.Time) Manifest {
	nm := m.Clone()
	fk := ManifestFileKey(plane, key)
	if fs, ok := nm.Files[fk]; ok {
		fs.Dirty = false
		fs.IndexedAt = now
		nm.Files[fk] = fs
	}
	nm.Indexes[IndexKeyFor(plane)] = IndexState{LastHydratedSHA: PlaneStateSHA(nm, plane)}
	nm.UpdatedAt = now
	return nm
}
