package hotstore

import (
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// The manifest vocabulary (Plane, Manifest, FileState, IndexState and the file
// key format) is owned by internal/service/rehydrate — it is the freshness
// contract between hot JSON and the derived stores (§5), a domain value type
// this adapter persists but does not define (feature-inventory.md §4.1 rule
// 4). The aliases below keep this package's historical spelling: they are the
// same types, so the rehydrate ports are satisfied structurally with no
// conversion layer.

// Plane names one of the two memory planes for manifest bookkeeping.
type Plane = rehydrate.Plane

const (
	PlaneEpisodic  = rehydrate.PlaneEpisodic
	PlaneKnowledge = rehydrate.PlaneKnowledge
)

// FileState is the manifest entry for one hot file (§5).
type FileState = rehydrate.FileState

// IndexState is the manifest entry for one derived index (§5).
type IndexState = rehydrate.IndexState

// Manifest mirrors manifest.json: per-file hashes plus per-index freshness.
type Manifest = rehydrate.Manifest

// pathSep joins plane and key segments in manifest keys. It is "/" on every
// platform: these are logical keys, not host paths.
const pathSep = "/"

// ManifestFileKey renders the Manifest.Files key for one project plane file:
// "{plane}/{ws}/{team}/{proj}". It delegates to the owning package so there is
// exactly one source of truth for the format.
func ManifestFileKey(plane Plane, key projectkey.Key) string {
	return rehydrate.ManifestFileKey(plane, key)
}
