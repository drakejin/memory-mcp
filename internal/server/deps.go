package server

import (
	"context"
	"io"
	"time"

	"github.com/drakejin/memory-mcp/internal/consolidate"
	"github.com/drakejin/memory-mcp/internal/document"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
	"github.com/drakejin/memory-mcp/internal/search"
)

// The interfaces below are consumer-side and deliberately narrow
// (code-standards §1.1): they list only the methods the handlers call, so the
// server never depends on the whole surface of a provider package. Go's
// structural typing makes the production clients satisfy them for free, and
// unit tests implement the same interfaces with fakes.

// Clock is the injected time source. Handlers never call time.Now directly, so
// ULID assignment, recall timestamps and TTL comparisons stay deterministic in
// tests (§1.1).
type Clock interface {
	Now() time.Time
}

// IDGenerator mints the ids the server assigns to new episodes and knowledge
// nodes. Only the timestamped form is consumed: the id's time half must agree
// with the injected Clock, never with the wall clock.
type IDGenerator interface {
	GenerateAt(unixMillis int64) (string, error)
}

// HotStore is the canonical JSON store (§0 principle 1). Everything served or
// mirrored elsewhere originates here.
type HotStore interface {
	AppendEpisode(ctx context.Context, key hotstore.ProjectKey, rec episodic.Record) error
	ListEpisodes(ctx context.Context, key hotstore.ProjectKey) ([]episodic.Record, error)
	GetEpisode(ctx context.Context, key hotstore.ProjectKey, id string) (episodic.Record, error)
	UpdateEpisodes(ctx context.Context, key hotstore.ProjectKey, ids []string, fn func(episodic.Record) episodic.Record) error
	ReadKnowledge(ctx context.Context, key hotstore.ProjectKey) (knowledge.Graph, error)
	UpdateKnowledge(ctx context.Context, key hotstore.ProjectKey, fn func(knowledge.Graph) (knowledge.Graph, error)) error
	ListProjects(ctx context.Context) ([]hotstore.ProjectKey, error)
	Manifest(ctx context.Context) (hotstore.Manifest, error)
	UpdateManifest(ctx context.Context, fn func(hotstore.Manifest) (hotstore.Manifest, error)) error
	MarkDirty(ctx context.Context, key hotstore.ProjectKey, plane hotstore.Plane) error
}

// EpisodeIndex is the episodic search plane. Writes to it are best-effort; only
// reads turn an unavailable index into a 503 (§5).
type EpisodeIndex interface {
	IndexRecords(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error
	Search(ctx context.Context, key hotstore.ProjectKey, q search.Query) ([]search.Hit, error)
}

// KnowledgeGraph is the knowledge mirror. Same degraded rule as EpisodeIndex.
type KnowledgeGraph interface {
	UpsertNodes(ctx context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error
	UpsertEdges(ctx context.Context, key hotstore.ProjectKey, edges []knowledge.Edge) error
	DeleteNode(ctx context.Context, key hotstore.ProjectKey, id string) error
	Search(ctx context.Context, key hotstore.ProjectKey, q string, includeArchived bool) ([]knowledge.Node, error)
	Neighborhood(ctx context.Context, key hotstore.ProjectKey, entity string, depth int) (knowledge.Graph, error)
}

// DocumentIngestor is the §6 document pipeline: blob upload, deterministic text
// extraction, chunking and retrieval.
type DocumentIngestor interface {
	Ingest(ctx context.Context, key hotstore.ProjectKey, filename string, data io.Reader) (document.IngestResult, error)
	Original(ctx context.Context, sha string) (io.ReadCloser, error)
	Chunks(ctx context.Context, sha string) ([]episodic.Record, error)
}

// Consolidator runs the deterministic §4 pipeline.
type Consolidator interface {
	Run(ctx context.Context, opts consolidate.Options) (consolidate.Report, error)
}

// Rehydrator compares the manifest against the derived stores and rebuilds them
// (§5). StatGate is the per-request debounced freshness check.
type Rehydrator interface {
	CheckDrift(ctx context.Context) (rehydrate.DriftReport, error)
	RehydrateAll(ctx context.Context, verify bool) (rehydrate.Report, error)
	StatGate(ctx context.Context, key hotstore.ProjectKey) error
}

// ColdArchive is the S3 read side the handlers need: resolving a provenance
// link that already aged out of hot, and probing the bucket for /v1/status.
// Archive writes belong to the consolidation pipeline, not to a handler.
type ColdArchive interface {
	FetchArchivedEpisode(ctx context.Context, key hotstore.ProjectKey, id string) (episodic.Record, error)
	FetchBlob(ctx context.Context, sha string) (io.ReadCloser, error)
}
