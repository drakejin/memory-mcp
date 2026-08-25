// Package document implements the document pipeline (architecture-v2.md §6):
// one document = blob (original bytes, cold-first) + a knowledge document node
// + document_chunk episodes. Extraction is deterministic only — no OCR, no
// summarization; unextractable inputs are reported honestly.
//
// This package is a pipeline over injected clients rather than a client of its
// own external system, so it takes the Service shape of code-standards §1.1:
// an exported interface, an unexported implementation, one New(Config).
// Every error leaving it is an *errs.Error.
package document

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Ops carried by the errors of this package. They read as a call path, so a log
// line names the operation without a stack trace.
const (
	opNew      = "document.New"
	opIngest   = "document.Ingest"
	opOriginal = "document.Original"
	opChunks   = "document.Chunks"
)

// Entities carried by the errors of this package.
const (
	entityConfig   = "config"
	entityDocument = "document"
	entityChunk    = "document_chunk"
	entityGraph    = "knowledge_graph"
)

// Degraded notes carried by IngestResult.Degraded. They mirror the server's
// degraded vocabulary word for word (§5), so a client reads one wording whether
// the note came from an episode write, a knowledge write or an ingest.
const (
	degradedSearch = "search unavailable"
	degradedGraph  = "graph unavailable"
)

// Truncation reports the honesty contract of §6 step 4: when a document yields
// more than the chunk cap, the response states total vs indexed.
type Truncation struct {
	Total   int `json:"total"`
	Indexed int `json:"indexed"`
}

// IngestResult is the outcome of one document upload (§6).
type IngestResult struct {
	// SHA is the lowercase hex sha256 of the original bytes.
	SHA string `json:"sha"`
	// BlobKey is the S3 key the original was uploaded to (cold-first).
	BlobKey string `json:"blob_key"`
	// NodeID is the auto-created knowledge document node id.
	NodeID string `json:"node_id"`
	// Extractable is false when no deterministic text layer exists (e.g.
	// scanned PDF); the agent handles such documents itself.
	Extractable bool `json:"extractable"`
	// ChunkIDs are the created document_chunk episode ids, in sequence order.
	ChunkIDs []string `json:"chunk_ids"`
	// Truncated is non-nil when the chunk cap cut the tail.
	Truncated *Truncation `json:"truncated,omitempty"`
	// Degraded names every derived plane whose best-effort mirror failed. The
	// hot write succeeded regardless (§1), so an ingest that could not index
	// its chunks or MERGE its document node still reports 201 — but it says so
	// here rather than claiming a mirror that does not exist (§0 principle 3).
	Degraded []string `json:"degraded,omitempty"`
}

// Service is the document contract the HTTP layer depends on. It also owns
// the cold-store ops surface (ColdConfigured, ColdReachable): this pipeline is
// the blob/cold relationship's consumer-side owner (P12 — cold-first), so the
// transport layer asks it instead of touching the archiver directly
// (feature-inventory.md §4.1 rule 3).
type Service interface {
	// Ingest runs §6 steps 1-5: sha256, blob upload (idempotent per sha) +
	// local cache, deterministic extraction, chunking into document_chunk
	// episodes (indexed best-effort), and the knowledge document node.
	Ingest(ctx context.Context, key projectkey.Key, filename string, data io.Reader) (IngestResult, error)
	// Original opens the raw bytes by sha — local cache first, cold
	// rehydration on miss (§6 step 6). Unknown everywhere is KindNotFound.
	Original(ctx context.Context, sha string) (io.ReadCloser, error)
	// Chunks returns the document_chunk episodes for sha in chunk_seq order.
	Chunks(ctx context.Context, sha string) ([]episode.Record, error)
	// ColdConfigured reports whether a cold archiver is wired at all — the §6
	// step 2 precondition an ingest cannot start without. It does no I/O.
	ColdConfigured() bool
	// ColdReachable probes the cold store for /v1/status honesty (§0
	// principle 3). An unconfigured archiver is not reachable.
	ColdReachable(ctx context.Context) bool
}

// HotStore is the slice of the canonical JSON store this pipeline consumes
// (code-standards §1.1). hotstore's file store satisfies it.
type HotStore interface {
	AppendEpisode(ctx context.Context, key projectkey.Key, rec episode.Record) error
	ListEpisodes(ctx context.Context, key projectkey.Key) ([]episode.Record, error)
	ListProjects(ctx context.Context) ([]projectkey.Key, error)
	UpdateKnowledge(ctx context.Context, key projectkey.Key, fn func(knowledge.Graph) (knowledge.Graph, error)) error
	MarkDirty(ctx context.Context, key projectkey.Key, plane rehydrate.Plane) error
}

// BlobCache is the local content-addressed cache of original bytes.
type BlobCache interface {
	// Put streams data into the cache, returning its lowercase hex sha256 and
	// size; re-putting an existing sha is an idempotent no-op.
	Put(ctx context.Context, data io.Reader) (string, int64, error)
	// Get opens a cached blob.
	Get(ctx context.Context, sha string) (io.ReadCloser, error)
	// Has reports whether the blob is cached. It is what tells a cache miss
	// (fall back to cold) apart from a real read failure, without this package
	// having to know the cache's error vocabulary.
	Has(ctx context.Context, sha string) (bool, error)
}

// BlobArchiver is the slice of the cold archive this pipeline consumes.
type BlobArchiver interface {
	UploadBlob(ctx context.Context, sha string, r io.Reader) (string, error)
	FetchBlob(ctx context.Context, sha string) (io.ReadCloser, error)
}

// RecordIndexer mirrors chunk episodes into the episodic search index. It is
// best-effort: a failure marks the manifest dirty, it never fails an ingest.
type RecordIndexer interface {
	IndexRecords(ctx context.Context, key projectkey.Key, recs []episode.Record) error
}

// NodeUpserter mirrors the auto-created document node into the knowledge graph,
// also best-effort.
type NodeUpserter interface {
	UpsertNodes(ctx context.Context, key projectkey.Key, nodes []knowledge.Node) error
}

// Clock supplies wall-clock time. It is injected so chunk timestamps and ULIDs
// are deterministic under test (code-standards §1.1 — no direct time.Now()).
type Clock interface {
	Now() time.Time
}

// IDGenerator mints the ids of chunk episodes and of the document node. Ids
// from one generator are strictly increasing, which is what keeps chunks minted
// inside the same millisecond in sequence order.
type IDGenerator interface {
	GenerateAt(unixMillis int64) (string, error)
}

// Config is the single construction path for a Service.
type Config struct {
	// Store is the canonical hot JSON store. Required.
	Store HotStore
	// Cache is the local blob cache. Required.
	Cache BlobCache
	// Clock stamps chunk episodes and node timestamps. Required.
	Clock Clock
	// IDs mints chunk and node ids. Required.
	IDs IDGenerator
	// Archiver is cold storage. Optional: without it §6 step 2 (cold-first)
	// cannot hold, so Ingest reports the pipeline unavailable.
	Archiver BlobArchiver
	// Index mirrors chunks into search. Optional: nil degrades to dirty marks.
	Index RecordIndexer
	// Graph mirrors the document node. Optional: nil degrades to dirty marks.
	Graph NodeUpserter
	// Extractor produces deterministic text. Optional: nil selects the
	// built-in PDF/markdown/text extractor.
	Extractor Extractor
	// Bucket names the cold store in ColdReachable's unreachability log line.
	// Optional; log-only, never part of a response.
	Bucket string
	// Logger receives degraded-mode reports. Optional: nil uses slog.Default().
	Logger *slog.Logger
}

// service is the concrete pipeline. It holds no mutable state, so it is safe
// for concurrent use by the HTTP handlers.
type service struct {
	store     HotStore
	cache     BlobCache
	clock     Clock
	ids       IDGenerator
	archiver  BlobArchiver
	index     RecordIndexer
	graph     NodeUpserter
	extractor Extractor
	bucket    string
	log       *slog.Logger
}

// Compile-time contract check.
var _ Service = (*service)(nil)

// New wires a document Service from cfg. It performs no I/O.
func New(cfg Config) (Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errs.Invalid(opNew, entityConfig, "hot store must not be nil")
	case cfg.Cache == nil:
		return nil, errs.Invalid(opNew, entityConfig, "blob cache must not be nil")
	case cfg.Clock == nil:
		return nil, errs.Invalid(opNew, entityConfig, "clock must not be nil")
	case cfg.IDs == nil:
		return nil, errs.Invalid(opNew, entityConfig, "id generator must not be nil")
	}

	extractor := cfg.Extractor
	if extractor == nil {
		extractor = textExtractor{}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &service{
		store:     cfg.Store,
		cache:     cfg.Cache,
		clock:     cfg.Clock,
		ids:       cfg.IDs,
		archiver:  cfg.Archiver,
		index:     cfg.Index,
		graph:     cfg.Graph,
		extractor: extractor,
		bucket:    cfg.Bucket,
		log:       log,
	}, nil
}
