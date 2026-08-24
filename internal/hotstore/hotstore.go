// Package hotstore owns the canonical JSON store under ~/.local/dj-memory
// (architecture-v2.md §1). Every write is atomic (temp+rename). OpenSearch and
// Neo4j are derived, disposable views; content that exists only in a derived
// store is a bug. The manifest tracks per-file hashes and index freshness so
// rehydrate (§5) can detect drift.
package hotstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// Clock abstracts wall-clock time so unit tests across all packages can use a
// fixed fake. This is the single shared clock interface for the project.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// ProjectKey addresses one project file: {workspace}/{team}/{project}.
type ProjectKey struct {
	Workspace string
	Team      string
	Project   string
}

// segmentPattern is the allowed charset for one key segment. The charset
// itself excludes path separators, so containment reduces to rejecting dot
// segments below.
var segmentPattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// Validate checks each segment is non-empty, lowercase [a-z0-9._-], and free
// of path separators or "..", so keys can be embedded in file and S3 paths.
func (k ProjectKey) Validate() error {
	segments := []struct {
		name  string
		value string
	}{
		{"workspace", k.Workspace},
		{"team", k.Team},
		{"project", k.Project},
	}
	for _, seg := range segments {
		if seg.value == "" {
			return fmt.Errorf("hotstore: project key %s must be non-empty", seg.name)
		}
		if !segmentPattern.MatchString(seg.value) {
			return fmt.Errorf("hotstore: project key %s %q must match [a-z0-9._-]+", seg.name, seg.value)
		}
		if seg.value == "." || strings.Contains(seg.value, "..") {
			return fmt.Errorf("hotstore: project key %s %q must not contain dot path segments", seg.name, seg.value)
		}
	}
	return nil
}

// String renders "ws/team/proj" for logs, manifest keys, and S3 layouts.
func (k ProjectKey) String() string {
	return k.Workspace + "/" + k.Team + "/" + k.Project
}

// Plane names one of the two memory planes for manifest bookkeeping.
type Plane string

const (
	PlaneEpisodic  Plane = "episodic"
	PlaneKnowledge Plane = "knowledge"
)

// ManifestFileKey renders the Manifest.Files key for one project plane file:
// "{plane}/{ws}/{team}/{proj}". Single source of truth for the format.
func ManifestFileKey(plane Plane, key ProjectKey) string {
	return string(plane) + "/" + key.String()
}

// FileState is the manifest entry for one hot file (§5).
type FileState struct {
	SHA256 string `json:"sha256"`
	// RecordCount is the derived-store unit count: episodic records for the
	// episodic plane, knowledge NODES (not edges) for the knowledge plane, so
	// rehydrate can compare it against OpenSearch doc-count / Neo4j node-count.
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

// clone returns a deep copy so callers and mutation funcs never share maps.
func (m Manifest) clone() Manifest {
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

// ErrNotFound is returned when a record, node, or project file does not exist.
var ErrNotFound = errors.New("hotstore: not found")

// Store is the canonical-store contract every other package depends on. All
// mutations must be atomic on disk; implementations never mutate arguments.
type Store interface {
	// AppendEpisode appends one record to the project's episodic file,
	// creating the file (and directories) on first write, and updates the
	// manifest file state.
	AppendEpisode(ctx context.Context, key ProjectKey, rec episodic.Record) error
	// ListEpisodes returns every episodic record for the project in file
	// (append/ULID) order. Missing file yields an empty slice, not an error.
	ListEpisodes(ctx context.Context, key ProjectKey) ([]episodic.Record, error)
	// GetEpisode returns one record by id, or ErrNotFound.
	GetEpisode(ctx context.Context, key ProjectKey, id string) (episodic.Record, error)
	// UpdateEpisodes rewrites matching records via fn (used for consolidated
	// flags and recall stats). fn receives a copy and returns the replacement.
	UpdateEpisodes(ctx context.Context, key ProjectKey, ids []string, fn func(episodic.Record) episodic.Record) error
	// RemoveEpisodes deletes the given ids from the hot file (aging step —
	// only legal AFTER cold upload succeeded, §4). Absent ids are skipped so
	// aging retries stay idempotent.
	RemoveEpisodes(ctx context.Context, key ProjectKey, ids []string) error

	// ReadKnowledge loads the project's knowledge graph document. Missing
	// file yields an empty Graph, not an error.
	ReadKnowledge(ctx context.Context, key ProjectKey) (knowledge.Graph, error)
	// WriteKnowledge atomically replaces the project's knowledge document and
	// updates the manifest file state.
	WriteKnowledge(ctx context.Context, key ProjectKey, g knowledge.Graph) error

	// ListProjects enumerates every project key present in either plane.
	ListProjects(ctx context.Context) ([]ProjectKey, error)
	// Manifest returns the current manifest (empty maps when absent).
	Manifest(ctx context.Context) (Manifest, error)
	// UpdateManifest applies fn to a copy of the manifest and atomically
	// persists the result; fn returning an error aborts without writing.
	UpdateManifest(ctx context.Context, fn func(m Manifest) (Manifest, error)) error
	// MarkDirty flags a project file whose derived upsert failed (§1).
	MarkDirty(ctx context.Context, key ProjectKey, plane Plane) error
	// FileInfo returns size in bytes and mtime of one project plane file for
	// stat-gate checks (§5) and pressure thresholds (§3.1). ErrNotFound when
	// the file does not exist.
	FileInfo(ctx context.Context, key ProjectKey, plane Plane) (sizeBytes int64, mtime time.Time, err error)
}
