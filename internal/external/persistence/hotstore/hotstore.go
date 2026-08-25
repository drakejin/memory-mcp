// Package hotstore owns the canonical JSON store under ~/.local/dj-memory
// (architecture-v2.md §1). Every write is atomic (temp+rename). OpenSearch and
// Neo4j are derived, disposable views; content that exists only in a derived
// store is a bug. The manifest tracks per-file hashes and index freshness so
// rehydrate (§5) can detect drift.
//
// Every error leaving this package is a *errs.Error: an unknown project key is
// KindInvalid, a missing episode is KindNotFound, a duplicate append is
// KindConflict, and a broken filesystem is KindInternal. Nothing here knows
// about HTTP (code-standards §2).
package hotstore

import (
	"context"
	"sync"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Ops carried by the errors this package returns. They read as a path through
// the system, which is more useful in a log than a stack (code-standards §2.1).
const (
	opNew             = "hotstore.New"
	opValidateConfig  = "hotstore.Config.Validate"
	opAppendEpisode   = "hotstore.AppendEpisode"
	opListEpisodes    = "hotstore.ListEpisodes"
	opGetEpisode      = "hotstore.GetEpisode"
	opUpdateEpisodes  = "hotstore.UpdateEpisodes"
	opRemoveEpisodes  = "hotstore.RemoveEpisodes"
	opReadKnowledge   = "hotstore.ReadKnowledge"
	opUpdateKnowledge = "hotstore.UpdateKnowledge"
	opListProjects    = "hotstore.ListProjects"
	opManifest        = "hotstore.Manifest"
	opUpdateManifest  = "hotstore.UpdateManifest"
	opMarkDirty       = "hotstore.MarkDirty"
	opFileInfo        = "hotstore.FileInfo"
)

// Entities addressed by this package's errors. Internal (filesystem) failures
// carry no entity: their op already names the document that broke.
const (
	entityConfig  = "config"
	entityEpisode = "episode"
	entityHotFile = "hot_file"
)

// Client is the canonical-store contract every other package depends on. All
// mutations are atomic on disk; implementations never mutate arguments.
type Client interface {
	// AppendEpisode appends one record to the project's episodic file,
	// creating the file (and directories) on first write, and updates the
	// manifest file state. A duplicate id is KindConflict.
	AppendEpisode(ctx context.Context, key projectkey.Key, rec episode.Record) error
	// ListEpisodes returns every episodic record for the project in file
	// (append/ULID) order. Missing file yields an empty slice, not an error.
	ListEpisodes(ctx context.Context, key projectkey.Key) ([]episode.Record, error)
	// GetEpisode returns one record by id, or KindNotFound.
	GetEpisode(ctx context.Context, key projectkey.Key, id string) (episode.Record, error)
	// UpdateEpisodes rewrites matching records via fn (used for consolidated
	// flags and recall stats). fn receives a copy and returns the replacement.
	UpdateEpisodes(ctx context.Context, key projectkey.Key, ids []string, fn func(episode.Record) episode.Record) error
	// RemoveEpisodes deletes the given ids from the hot file (aging step —
	// only legal AFTER cold upload succeeded, §4). Absent ids are skipped so
	// aging retries stay idempotent.
	RemoveEpisodes(ctx context.Context, key projectkey.Key, ids []string) error

	// ReadKnowledge loads the project's knowledge graph document. Missing
	// file yields an empty Graph, not an error.
	ReadKnowledge(ctx context.Context, key projectkey.Key) (knowledge.Graph, error)
	// UpdateKnowledge replaces the project's knowledge document with the graph
	// fn derives from the current one, reading and writing under a single lock
	// hold. The graph is stored as one document, so a plain read-then-write by
	// two concurrent callers loses whatever the loser's snapshot did not
	// contain; this is the only mutation path, so that interleave cannot
	// happen. fn receives a graph nobody else references and must not mutate
	// its input; an error from fn aborts the write and travels out with its
	// cause intact, so callers can still match it.
	UpdateKnowledge(ctx context.Context, key projectkey.Key, fn func(knowledge.Graph) (knowledge.Graph, error)) error

	// ListProjects enumerates every project key present in either plane.
	ListProjects(ctx context.Context) ([]projectkey.Key, error)
	// Manifest returns a copy of the current manifest (empty maps when absent).
	Manifest(ctx context.Context) (Manifest, error)
	// UpdateManifest applies fn to a copy of the manifest and atomically
	// persists the result; fn returning an error aborts without writing.
	UpdateManifest(ctx context.Context, fn func(m Manifest) (Manifest, error)) error
	// MarkDirty flags a project file whose derived upsert failed (§1).
	MarkDirty(ctx context.Context, key projectkey.Key, plane Plane) error
	// FileInfo returns size in bytes and mtime of one project plane file for
	// stat-gate checks (§5) and pressure thresholds (§3.1). KindNotFound when
	// the file does not exist.
	FileInfo(ctx context.Context, key projectkey.Key, plane Plane) (sizeBytes int64, mtime time.Time, err error)
}

// Config is the single construction input for the store.
type Config struct {
	// Home is the canonical store root (~/.local/dj-memory).
	Home string
	// Clock stamps the manifest. Injected so aging and freshness assertions
	// stay deterministic in tests (code-standards §1.1).
	Clock Clock
}

// Validate reports whether the config can produce a usable store.
func (c Config) Validate() error {
	if c.Home == "" {
		return errs.Invalid(opValidateConfig, entityConfig, "home must be non-empty")
	}
	if c.Clock == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "clock must be set")
	}
	return nil
}

// client is the on-disk Client rooted at Config.Home.
type client struct {
	home  string
	clock Clock
	// mu guards the whole store. Invariant: a hot file and its manifest entry
	// are rewritten as one unit, so read-modify-write cycles (append, update,
	// manifest) never interleave and no reader observes a file whose manifest
	// sha or record count is stale.
	mu sync.Mutex
}

// Compile-time contract check.
var _ Client = (*client)(nil)

// New returns a Client rooted at cfg.Home. It does not touch the disk;
// directories are created lazily on first write.
func New(cfg Config) (Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errs.Wrap(opNew, err)
	}
	return &client{home: cfg.Home, clock: cfg.Clock}, nil
}

// guard rejects a dead context and an invalid key before any IO, so a bad key
// can never reach a filesystem path.
func guard(ctx context.Context, op string, key projectkey.Key) error {
	if err := errs.FromContext(ctx, op); err != nil {
		return err
	}
	return errs.Wrap(op, key.Validate())
}
