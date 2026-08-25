// Package consolidate implements the deterministic consolidation pipeline
// (architecture-v2.md §4): candidate clustering, entity statistics, cold
// aging per §3.1, knowledge snapshots, and manifest updates. Distillation
// itself (summarize → knowledge) is the calling agent's job, never the
// server's.
//
// Every dependency is a narrow consumer-side interface satisfied structurally
// by hotstore, search and cold (code-standards §1.1), and every error crossing
// the package boundary is a *errs.Error (§2.1). Nothing here knows about HTTP.
package consolidate

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Ops carried by this package's errors. They read as a path through the
// pipeline, which is more useful in a log than a stack (code-standards §2.1).
const (
	opNew            = "consolidate.New"
	opValidateConfig = "consolidate.Config.Validate"
	opRun            = "consolidate.Run"
	opHotState       = "consolidate.HotState"
	opDeleteIndexed  = "consolidate.deleteFromIndex"
)

// entityConfig is the entity addressed by construction errors.
const entityConfig = "config"

// HotStore is the canonical-store surface this pipeline consumes. hotstore
// satisfies it structurally; nothing wider is imported (code-standards §1.1).
type HotStore interface {
	ListProjects(ctx context.Context) ([]projectkey.Key, error)
	ListEpisodes(ctx context.Context, key projectkey.Key) ([]episode.Record, error)
	RemoveEpisodes(ctx context.Context, key projectkey.Key, ids []string) error
	ReadKnowledge(ctx context.Context, key projectkey.Key) (knowledge.Graph, error)
	FileInfo(ctx context.Context, key projectkey.Key, plane rehydrate.Plane) (int64, time.Time, error)
	Manifest(ctx context.Context) (rehydrate.Manifest, error)
	UpdateManifest(ctx context.Context, fn func(rehydrate.Manifest) (rehydrate.Manifest, error)) error
	MarkDirty(ctx context.Context, key projectkey.Key, plane rehydrate.Plane) error
}

// EpisodeIndexer is the derived episodic index, used only to drop the records
// that just left hot (§4 step 3c). Failures are degraded, never fatal.
type EpisodeIndexer interface {
	DeleteRecords(ctx context.Context, key projectkey.Key, ids []string) error
}

// ColdArchiver is the S3 archive surface: the monthly episode batch and the
// per-run knowledge snapshot (§4 steps 3a and 4).
type ColdArchiver interface {
	ArchiveEpisodes(ctx context.Context, key projectkey.Key, month string, recs []episode.Record) (string, error)
	SnapshotKnowledge(ctx context.Context, key projectkey.Key, g knowledge.Graph, ts time.Time) (string, string, error)
}

// Clock is the injected time source; aging must stay deterministic in tests
// (code-standards §1.1 — no direct time.Now()).
type Clock interface {
	Now() time.Time
}

// Service is the pipeline contract the HTTP layer depends on.
type Service interface {
	// Run executes §4 steps 1-5 in order. Aging strictly follows: S3 put
	// confirmed → hot removal → OpenSearch delete. Unconsolidated episodes
	// are never aged regardless of age (§3.1).
	Run(ctx context.Context, opts Options) (Report, error)
	// HotState reports the §3.1 bookkeeping /v1/status publishes: manifest
	// freshness, dirty files awaiting rehydration, and the unconsolidated /
	// stale-unconsolidated episode counts this pipeline's TTL defines. Count
	// failures degrade to a disclosure in HotState.Degraded — /status must
	// still answer — while an unreadable manifest fails the call.
	HotState(ctx context.Context) (HotState, error)
}

// Config is the single construction input.
type Config struct {
	// Store is the canonical hot store. Required.
	Store HotStore
	// Index is the derived episodic index. Nil means the index is
	// unavailable: aging still happens and the deletion is reported as a
	// failure, since the index is a disposable derivative (§5).
	Index EpisodeIndexer
	// Archiver is the cold store. Nil means cold is unavailable, and then
	// nothing may leave hot (§4 step 3: S3 first).
	Archiver ColdArchiver
	// Clock stamps knowledge snapshots and decides TTL eligibility. Required.
	Clock Clock
	// Logger receives best-effort failures. Nil discards them; the Report
	// still carries every failure (§4 step 5).
	Logger *slog.Logger
	// TTLDays is config.EpisodicTTLDays (§3.1). Required, positive.
	TTLDays int
}

// Validate reports whether the config can produce a usable pipeline.
func (c Config) Validate() error {
	if c.Store == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "store must be set")
	}
	if c.Clock == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "clock must be set")
	}
	if c.TTLDays <= 0 {
		return errs.Invalid(opValidateConfig, entityConfig, "ttl days must be positive").
			WithField("ttl_days", c.TTLDays)
	}
	return nil
}

// service is the concrete Service.
type service struct {
	store    HotStore
	index    EpisodeIndexer
	archiver ColdArchiver
	clock    Clock
	log      *slog.Logger
	ttlDays  int
}

// Compile-time contract check.
var _ Service = (*service)(nil)

// New returns a Service bound to cfg. It performs no I/O.
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
		archiver: cfg.Archiver,
		clock:    cfg.Clock,
		log:      log,
		ttlDays:  cfg.TTLDays,
	}, nil
}

// Run implements Service. Per-project failures are recorded in Report.Failures
// and never abort the remaining steps or projects (§4 step 5: honest reporting
// over fail-fast).
func (s *service) Run(ctx context.Context, opts Options) (Report, error) {
	report := newReport()

	projects := opts.Projects
	if len(projects) == 0 {
		listed, err := s.store.ListProjects(ctx)
		if err != nil {
			return report, errs.Wrap(opRun, err)
		}
		projects = listed
	}

	stats := newEntityAccumulator()
	now := s.clock.Now()

	for _, key := range projects {
		recs, err := s.store.ListEpisodes(ctx, key)
		if err != nil {
			report.addFailure(stepList, key, err)
			continue
		}

		// Step 1: distillation candidates from unconsolidated episodes.
		unconsolidated := make([]episode.Record, 0, len(recs))
		for _, rec := range recs {
			if !rec.Consolidated {
				unconsolidated = append(unconsolidated, rec)
			}
		}
		report.Candidates = append(report.Candidates, proposeCandidates(key, unconsolidated)...)

		// Step 2: entity statistics over every record of the project.
		stats.add(recs)

		// Step 3: aging per §3.1 — S3 put confirmed, then hot remove, then
		// index delete, in that order.
		s.ageProject(ctx, key, recs, now, opts.DryRun, &report)

		// Step 4: knowledge snapshot every run (§3.1).
		if !opts.DryRun {
			s.snapshotProject(ctx, key, now, &report)
		}
	}

	report.Entities = stats.sorted()

	// Step 5: manifest refresh (record counts were updated by the mutations;
	// this bumps the manifest timestamp so /status reflects the run).
	if !opts.DryRun {
		err := s.store.UpdateManifest(ctx, func(m rehydrate.Manifest) (rehydrate.Manifest, error) {
			return m, nil
		})
		if err != nil {
			report.addStepFailure(stepManifest, err)
		}
	}

	if len(report.Failures) > 0 {
		s.log.WarnContext(ctx, "consolidation completed with failures",
			"failures", len(report.Failures), "projects", len(projects))
	}
	return report, nil
}

// snapshotProject uploads the project's knowledge graph as latest.json plus a
// timestamped snapshot (§4 step 4).
func (s *service) snapshotProject(ctx context.Context, key projectkey.Key, now time.Time, report *Report) {
	if s.archiver == nil {
		report.addUnavailable(stepSnapshot, key)
		return
	}
	g, err := s.store.ReadKnowledge(ctx, key)
	if err != nil {
		report.addFailure(stepSnapshotRead, key, err)
		return
	}
	latestKey, snapshotKey, err := s.archiver.SnapshotKnowledge(ctx, key, g, now)
	if err != nil {
		report.addFailure(stepSnapshot, key, err)
		return
	}
	report.SnapshotKeys = append(report.SnapshotKeys, latestKey, snapshotKey)
}

// monthStamp is the {yyyy-mm} batch bucket format of §3.1. It is part of the
// aging contract this pipeline hands the cold archiver: the month string it
// computes here becomes the archive object name, so the cold adapter's key
// layout spells the same format.
const monthStamp = "2006-01"

// archiveMonth formats occurredAt as the {yyyy-mm} batch bucket in UTC.
func archiveMonth(occurredAt time.Time) string {
	return occurredAt.UTC().Format(monthStamp)
}

// batchByMonth groups records into their {yyyy-mm} archive buckets, returning
// the months in ascending order so each batch is written exactly once.
func batchByMonth(recs []episode.Record) ([]string, map[string][]episode.Record) {
	byMonth := make(map[string][]episode.Record, len(recs))
	for _, rec := range recs {
		month := archiveMonth(rec.OccurredAt)
		byMonth[month] = append(byMonth[month], rec)
	}
	return slices.Sorted(maps.Keys(byMonth)), byMonth
}
