// Package rehydrate keeps derived stores truthful to hot JSON
// (architecture-v2.md §5). Containers run without volumes and may die at any
// time; truth is decided by comparing manifest hashes/counts against live
// index state. Episodic rehydration drops and bulk-rebuilds; knowledge
// rehydration clears and replays idempotent MERGEs.
//
// Dependencies are narrow consumer-side interfaces satisfied structurally by
// hotstore, search and graph (code-standards §1.1). Errors crossing the
// package boundary are *errs.Error: an unreachable derived store is
// KindUnavailable, which is what the degraded-mode decision reads (§2.1).
package rehydrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// Ops carried by this package's errors (code-standards §2.1).
const (
	opNew              = "rehydrate.New"
	opValidateConfig   = "rehydrate.Config.Validate"
	opCheckDrift       = "rehydrate.CheckDrift"
	opRehydrateAll     = "rehydrate.RehydrateAll"
	opRehydrateProject = "rehydrate.RehydrateProject"
	opStatGate         = "rehydrate.StatGate"
	opHotNodeCount     = "rehydrate.hotNodeCount"
)

// entityConfig is the entity addressed by construction errors.
const entityConfig = "config"

// Drift reasons reported to /status. They are the honesty contract of §0
// principle 3, so they are declared once instead of formatted inline.
const (
	reasonIndexNotConfigured = "opensearch not configured"
	reasonIndexUnreachable   = "opensearch unreachable"
	reasonIndexAbsent        = "episodic index absent or uncountable"
	reasonEpisodicChanged    = "episodic hot content changed since last hydration"
	reasonGraphNotConfigured = "neo4j not configured"
	reasonGraphUnreachable   = "neo4j unreachable"
	reasonGraphUncountable   = "knowledge graph uncountable"
	reasonKnowledgeChanged   = "knowledge hot content changed since last hydration"
	reasonHotUnreadable      = "hot knowledge unreadable: "
)

// HotStore is the canonical-store surface rehydration consumes.
type HotStore interface {
	ListProjects(ctx context.Context) ([]hotstore.ProjectKey, error)
	ListEpisodes(ctx context.Context, key hotstore.ProjectKey) ([]episodic.Record, error)
	ReadKnowledge(ctx context.Context, key hotstore.ProjectKey) (knowledge.Graph, error)
	Manifest(ctx context.Context) (hotstore.Manifest, error)
	UpdateManifest(ctx context.Context, fn func(hotstore.Manifest) (hotstore.Manifest, error)) error
	FileInfo(ctx context.Context, key hotstore.ProjectKey, plane hotstore.Plane) (int64, time.Time, error)
}

// EpisodeIndex is the derived episodic index: drop, rebuild, count.
// DeleteProject is the project-scoped drop the partial path needs, so that one
// project can converge its removals without discarding the others.
type EpisodeIndex interface {
	Ping(ctx context.Context) error
	EnsureIndex(ctx context.Context) error
	IndexRecords(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error
	DeleteProject(ctx context.Context, key hotstore.ProjectKey) error
	DocCount(ctx context.Context, key hotstore.ProjectKey) (int, error)
	Drop(ctx context.Context) error
}

// KnowledgeGraph is the derived knowledge graph: clear, MERGE replay, count.
// DeleteMissing is the project-scoped counterpart of Clear, for the same reason
// EpisodeIndex needs DeleteProject.
type KnowledgeGraph interface {
	Ping(ctx context.Context) error
	UpsertNodes(ctx context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error
	UpsertEdges(ctx context.Context, key hotstore.ProjectKey, edges []knowledge.Edge) error
	DeleteMissing(ctx context.Context, key hotstore.ProjectKey, keep []string) error
	NodeCount(ctx context.Context, key hotstore.ProjectKey) (int, error)
	Clear(ctx context.Context) error
}

// Clock is the injected time source; the stat-gate debounce and manifest
// stamps must stay deterministic in tests (code-standards §1.1).
type Clock interface {
	Now() time.Time
}

// Drift describes one derived store's divergence from the manifest.
type Drift struct {
	// Detected is true when counts/hashes disagree or the index is absent.
	Detected bool `json:"detected"`
	// Reason is a human-readable explanation for /status honesty.
	Reason string `json:"reason,omitempty"`
	// Unavailable is true when the store cannot even be reached; writes then
	// proceed hot-only in degraded mode (§5).
	Unavailable bool `json:"unavailable"`
}

// DriftReport is the startup / status-time comparison result (§5).
type DriftReport struct {
	Episodic  Drift `json:"episodic"`
	Knowledge Drift `json:"knowledge"`
}

// Report summarizes one rehydration pass.
type Report struct {
	// EpisodesIndexed / NodesUpserted / EdgesUpserted count replayed items.
	EpisodesIndexed int `json:"episodes_indexed"`
	NodesUpserted   int `json:"nodes_upserted"`
	EdgesUpserted   int `json:"edges_upserted"`
	// Verified is set when verify=true ran a full hash audit (§5 reindex).
	Verified bool `json:"verified"`
	// Failures lists per-project errors; partial success is reported, not
	// hidden.
	Failures []string `json:"failures"`
}

// Service is the contract for startup checks, the request-time stat-gate, and
// POST /v1/reindex.
type Service interface {
	// CheckDrift compares OpenSearch index existence/doc-count and Neo4j
	// node-count against the manifest without mutating anything.
	CheckDrift(ctx context.Context) (DriftReport, error)
	// RehydrateAll rebuilds both derived stores from hot JSON (episodic:
	// drop+bulk; knowledge: clear+MERGE replay). verify additionally audits
	// every record count. Clears manifest dirty flags on success.
	RehydrateAll(ctx context.Context, verify bool) (Report, error)
	// RehydrateProject converges one project only — the request-time partial
	// path taken when the stat-gate sees dirty marks or mtime changes (§5).
	RehydrateProject(ctx context.Context, key hotstore.ProjectKey) (Report, error)
	// StatGate runs the debounced freshness check for a project and triggers
	// RehydrateProject when needed. Cheap: file mtime + manifest age only.
	StatGate(ctx context.Context, key hotstore.ProjectKey) error
}

// Config is the single construction input.
type Config struct {
	// Store is the canonical hot store, the source of truth every replay
	// reads from. Required.
	Store HotStore
	// Index is the derived episodic index. Nil means OpenSearch was never
	// configured: every method then reports that plane unavailable instead of
	// failing (§5 degraded semantics).
	Index EpisodeIndex
	// Graph is the derived knowledge graph; nil behaves like Index.
	Graph KnowledgeGraph
	// Clock stamps manifest freshness and drives the stat-gate debounce.
	// Required.
	Clock Clock
	// Logger receives best-effort failures. Nil discards them; the Report
	// still carries every failure.
	Logger *slog.Logger
}

// Validate reports whether the config can produce a usable service.
func (c Config) Validate() error {
	if c.Store == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "store must be set")
	}
	if c.Clock == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "clock must be set")
	}
	return nil
}

// service is the concrete Service.
type service struct {
	store HotStore
	index EpisodeIndex
	graph KnowledgeGraph
	clock Clock
	log   *slog.Logger

	// mu guards lastGate. Invariant: at most one stat-gate pass per project
	// per debounceInterval, so a burst of requests cannot stampede the
	// derived stores with concurrent rehydrations.
	mu       sync.Mutex
	lastGate map[string]time.Time
}

// Compile-time contract check.
var _ Service = (*service)(nil)

// New returns a Service bound to cfg. It performs no I/O; reachability is
// probed by CheckDrift.
func New(cfg Config) (Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errs.Wrap(opNew, err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &service{
		store:    cfg.Store,
		index:    cfg.Index,
		graph:    cfg.Graph,
		clock:    cfg.Clock,
		log:      log,
		lastGate: make(map[string]time.Time),
	}, nil
}

// CheckDrift implements Service.
func (s *service) CheckDrift(ctx context.Context) (DriftReport, error) {
	m, err := s.store.Manifest(ctx)
	if err != nil {
		return DriftReport{}, errs.Wrap(opCheckDrift, err)
	}
	return DriftReport{
		Episodic:  s.episodicDrift(ctx, m),
		Knowledge: s.knowledgeDrift(ctx, m),
	}, nil
}

// episodicDrift compares manifest record counts (and dirty flags / hydration
// sha) against the live OpenSearch doc count.
func (s *service) episodicDrift(ctx context.Context, m hotstore.Manifest) Drift {
	if s.index == nil {
		return Drift{Unavailable: true, Reason: reasonIndexNotConfigured}
	}
	if err := s.index.Ping(ctx); err != nil {
		return Drift{Unavailable: true, Reason: reasonIndexUnreachable}
	}
	want, dirty := planeTotals(m, hotstore.PlaneEpisodic)
	got, err := s.index.DocCount(ctx, hotstore.ProjectKey{})
	if err != nil {
		// Reachable but uncountable — the index itself is absent (fresh
		// container) and must be rebuilt.
		return Drift{Detected: true, Reason: reasonIndexAbsent}
	}
	switch {
	case got != want:
		return Drift{Detected: true, Reason: fmt.Sprintf("indexed docs %d != manifest records %d", got, want)}
	case dirty > 0:
		return Drift{Detected: true, Reason: fmt.Sprintf("%d dirty episodic file(s) await rehydration", dirty)}
	}
	if planeHydrationStale(m, hotstore.PlaneEpisodic) {
		return Drift{Detected: true, Reason: reasonEpisodicChanged}
	}
	return Drift{}
}

// knowledgeDrift compares actual hot node counts against the live Neo4j node
// count. Counting from the hot files (not the manifest) keeps this check
// independent of how the hotstore accounts knowledge records.
func (s *service) knowledgeDrift(ctx context.Context, m hotstore.Manifest) Drift {
	if s.graph == nil {
		return Drift{Unavailable: true, Reason: reasonGraphNotConfigured}
	}
	if err := s.graph.Ping(ctx); err != nil {
		return Drift{Unavailable: true, Reason: reasonGraphUnreachable}
	}
	want, err := s.hotNodeCount(ctx)
	if err != nil {
		return Drift{Detected: true, Reason: reasonHotUnreadable + err.Error()}
	}
	got, err := s.graph.NodeCount(ctx, hotstore.ProjectKey{})
	if err != nil {
		return Drift{Detected: true, Reason: reasonGraphUncountable}
	}
	_, dirty := planeTotals(m, hotstore.PlaneKnowledge)
	switch {
	case got != want:
		return Drift{Detected: true, Reason: fmt.Sprintf("graph nodes %d != hot nodes %d", got, want)}
	case dirty > 0:
		return Drift{Detected: true, Reason: fmt.Sprintf("%d dirty knowledge file(s) await rehydration", dirty)}
	}
	if planeHydrationStale(m, hotstore.PlaneKnowledge) {
		return Drift{Detected: true, Reason: reasonKnowledgeChanged}
	}
	return Drift{}
}

// hotNodeCount sums knowledge nodes across every hot project file.
func (s *service) hotNodeCount(ctx context.Context) (int, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return 0, errs.Wrap(opHotNodeCount, err)
	}
	total := 0
	for _, p := range projects {
		g, err := s.store.ReadKnowledge(ctx, p)
		if err != nil {
			// The project key names which hot file broke; /status shows it.
			return 0, errs.Wrap(opHotNodeCount, fmt.Errorf("%s: %w", p.String(), err))
		}
		total += len(g.Nodes)
	}
	return total, nil
}

// failureSep joins accumulated failure lines into one cause string.
const failureSep = "; "

// failures accumulates per-step problems for a Report while remembering
// whether any of them was a derived store being unreachable — the difference
// between "degraded" and "broken" (§5).
type failures struct {
	lines       []string
	unavailable bool
}

// add records a failure line.
func (f *failures) add(line string) {
	f.lines = append(f.lines, line)
}

// addUnavailable records a failure caused by a derived store being absent or
// unreachable.
func (f *failures) addUnavailable(line string) {
	f.unavailable = true
	f.lines = append(f.lines, line)
}

// err returns the semantic error for the accumulated failures, or nil when
// there were none. The joined detail stays in the cause: it belongs in logs,
// not in a response body.
func (f *failures) err(op string) error {
	if len(f.lines) == 0 {
		return nil
	}
	joined := errors.New(strings.Join(f.lines, failureSep))
	if f.unavailable {
		return errs.Unavailable(op, joined)
	}
	return errs.Internal(op, joined)
}
