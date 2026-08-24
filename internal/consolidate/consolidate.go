// Package consolidate implements the deterministic consolidation pipeline
// (architecture-v2.md §4): candidate clustering, entity statistics, cold
// aging per §3.1, knowledge snapshots, and manifest updates. Distillation
// itself (summarize → knowledge) is the calling agent's job, never the
// server's.
package consolidate

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/config"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/search"
)

// Candidate is one distillation suggestion: unconsolidated episodes grouped by
// shared entities and time proximity (§4 step 1).
type Candidate struct {
	Project    string    `json:"project"` // ws/team/proj
	Entities   []string  `json:"entities"`
	EpisodeIDs []string  `json:"episode_ids"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
}

// EntityStat is a dictionary-normalized entity with its co-occurrence weight
// updates (§4 step 2).
type EntityStat struct {
	Entity       string         `json:"entity"`
	Count        int            `json:"count"`
	CoOccurrence map[string]int `json:"co_occurrence"`
}

// Report is the honest result of one consolidation run (§4 step 5): moved
// counts, snapshot keys, and every failure — nothing silently swallowed.
type Report struct {
	Candidates []Candidate  `json:"candidates"`
	Entities   []EntityStat `json:"entities"`
	// MovedEpisodes counts records shifted to cold; ArchiveKeys lists the
	// {yyyy-mm}.json objects written.
	MovedEpisodes int      `json:"moved_episodes"`
	ArchiveKeys   []string `json:"archive_keys"`
	// SnapshotKeys lists knowledge latest+snapshot objects written.
	SnapshotKeys []string `json:"snapshot_keys"`
	// Failures lists per-step errors; a failure never blocks other steps.
	Failures []string `json:"failures"`
}

// Options tunes one run.
type Options struct {
	// Projects limits the run; empty means all projects.
	Projects []hotstore.ProjectKey
	// DryRun computes candidates/stats and reports what WOULD age, without
	// any S3 upload, hot removal, or index deletion.
	DryRun bool
}

// Consolidator is the pipeline contract the HTTP layer depends on.
type Consolidator interface {
	// Run executes §4 steps 1-5 in order. Aging strictly follows: S3 put
	// confirmed → hot removal → OpenSearch delete. Unconsolidated episodes
	// are never aged regardless of age (§3.1).
	Run(ctx context.Context, opts Options) (Report, error)
}

// Runner is the concrete Consolidator.
type Runner struct {
	store    hotstore.Store
	index    search.Index
	archiver cold.Archiver
	clock    hotstore.Clock
	ttlDays  int
}

// Compile-time contract check.
var _ Consolidator = (*Runner)(nil)

// New wires a Runner. ttlDays is config.EpisodicTTLDays (§3.1).
func New(store hotstore.Store, index search.Index, archiver cold.Archiver, clock hotstore.Clock, ttlDays int) *Runner {
	return &Runner{store: store, index: index, archiver: archiver, clock: clock, ttlDays: ttlDays}
}

// Run implements Consolidator. Per-project failures are recorded in
// Report.Failures and never abort the remaining steps or projects (§4 step 5:
// honest reporting over fail-fast).
func (r *Runner) Run(ctx context.Context, opts Options) (Report, error) {
	report := Report{
		Candidates:   []Candidate{},
		Entities:     []EntityStat{},
		ArchiveKeys:  []string{},
		SnapshotKeys: []string{},
		Failures:     []string{},
	}

	projects := opts.Projects
	if len(projects) == 0 {
		var err error
		projects, err = r.store.ListProjects(ctx)
		if err != nil {
			return report, fmt.Errorf("consolidate: list projects: %w", err)
		}
	}

	stats := newEntityAccumulator()
	now := r.clock.Now()

	for _, key := range projects {
		recs, err := r.store.ListEpisodes(ctx, key)
		if err != nil {
			report.Failures = append(report.Failures, failure("list", key, err))
			continue
		}

		// Step 1: distillation candidates from unconsolidated episodes.
		var unconsolidated []episodic.Record
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
		r.ageProject(ctx, key, recs, now, opts.DryRun, &report)

		// Step 4: knowledge snapshot every run (§3.1).
		if !opts.DryRun {
			r.snapshotProject(ctx, key, now, &report)
		}
	}

	report.Entities = stats.sorted()

	// Step 5: manifest refresh (record counts were updated by the mutations;
	// this bumps the manifest timestamp so /status reflects the run).
	if !opts.DryRun {
		if err := r.store.UpdateManifest(ctx, func(m hotstore.Manifest) (hotstore.Manifest, error) {
			return m, nil
		}); err != nil {
			report.Failures = append(report.Failures, "manifest: "+err.Error())
		}
	}

	return report, nil
}

// ageProject archives eligible records for one project. A dry run only counts
// what would move.
func (r *Runner) ageProject(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record, now time.Time, dryRun bool, report *Report) {
	fileBytes, _, err := r.store.FileInfo(ctx, key, hotstore.PlaneEpisodic)
	if err != nil {
		fileBytes = 0 // missing file: no pressure
	}

	views := make([]RecordView, 0, len(recs))
	byID := make(map[string]episodic.Record, len(recs))
	for _, rec := range recs {
		views = append(views, RecordView{ID: rec.ID, OccurredAt: rec.OccurredAt, Consolidated: rec.Consolidated})
		byID[rec.ID] = rec
	}
	ids := AgeEligible(views, now, r.ttlDays, fileBytes, config.MaxProjectFileBytes, config.MaxProjectRecords)
	if len(ids) == 0 {
		return
	}

	if dryRun {
		report.MovedEpisodes += len(ids)
		return
	}
	if r.archiver == nil {
		report.Failures = append(report.Failures, "age: "+key.String()+": cold storage unavailable")
		return
	}

	// Batch by archive month so each {yyyy-mm}.json is written once.
	byMonth := map[string][]episodic.Record{}
	for _, id := range ids {
		rec := byID[id]
		month := cold.ArchiveMonth(rec.OccurredAt)
		byMonth[month] = append(byMonth[month], rec)
	}

	for _, month := range slices.Sorted(mapsKeys(byMonth)) {
		batch := byMonth[month]
		batchIDs := make([]string, 0, len(batch))
		for _, rec := range batch {
			batchIDs = append(batchIDs, rec.ID)
		}

		// 3a: S3 put — must succeed before anything local is touched.
		s3Key, err := r.archiver.ArchiveEpisodes(ctx, key, month, batch)
		if err != nil {
			report.Failures = append(report.Failures, failure("archive "+month, key, err))
			continue
		}
		report.ArchiveKeys = append(report.ArchiveKeys, s3Key)

		// 3b: hot removal, only after the cold copy is confirmed.
		if err := r.store.RemoveEpisodes(ctx, key, batchIDs); err != nil {
			// Cold copy exists but hot still holds the records — safe
			// direction; the next run will re-archive idempotently.
			report.Failures = append(report.Failures, failure("hot-remove "+month, key, err))
			continue
		}
		report.MovedEpisodes += len(batchIDs)

		// 3c: index delete, best-effort; drift converges via rehydration.
		if err := r.deleteFromIndex(ctx, key, batchIDs); err != nil {
			report.Failures = append(report.Failures, failure("index-delete "+month, key, err))
			if derr := r.store.MarkDirty(ctx, key, hotstore.PlaneEpisodic); derr != nil {
				slog.Warn("consolidate: mark dirty failed", "project", key.String(), "error", derr)
			}
		}
	}
}

func (r *Runner) deleteFromIndex(ctx context.Context, key hotstore.ProjectKey, ids []string) error {
	if r.index == nil {
		return search.ErrUnavailable
	}
	return r.index.DeleteRecords(ctx, key, ids)
}

// snapshotProject uploads the project's knowledge graph as latest.json plus a
// timestamped snapshot (§4 step 4).
func (r *Runner) snapshotProject(ctx context.Context, key hotstore.ProjectKey, now time.Time, report *Report) {
	if r.archiver == nil {
		report.Failures = append(report.Failures, "snapshot: "+key.String()+": cold storage unavailable")
		return
	}
	g, err := r.store.ReadKnowledge(ctx, key)
	if err != nil {
		report.Failures = append(report.Failures, failure("snapshot-read", key, err))
		return
	}
	latestKey, snapshotKey, err := r.archiver.SnapshotKnowledge(ctx, key, g, now)
	if err != nil {
		report.Failures = append(report.Failures, failure("snapshot", key, err))
		return
	}
	report.SnapshotKeys = append(report.SnapshotKeys, latestKey, snapshotKey)
}

func failure(step string, key hotstore.ProjectKey, err error) string {
	return step + ": " + key.String() + ": " + err.Error()
}

// mapsKeys adapts a map to the iterator slices.Sorted wants.
func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// AgeEligible returns the ids of records that qualify for cold aging at now
// per §3.1: consolidated AND at least ttlDays old; plus, when the file exceeds
// the size/count pressure thresholds, the oldest consolidated records beyond
// the limit. Unconsolidated records are never eligible, regardless of age or
// pressure. maxBytes/maxRecords <= 0 disables that pressure check. Returned
// ids are sorted oldest-first (OccurredAt, then ID). Pure function.
func AgeEligible(recs []RecordView, now time.Time, ttlDays int, fileBytes int64, maxBytes int64, maxRecords int) []string {
	cutoff := now.AddDate(0, 0, -ttlDays)

	var consolidated []RecordView
	for _, rec := range recs {
		if rec.Consolidated {
			consolidated = append(consolidated, rec)
		}
	}
	slices.SortFunc(consolidated, func(x, y RecordView) int {
		if !x.OccurredAt.Equal(y.OccurredAt) {
			if x.OccurredAt.Before(y.OccurredAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(x.ID, y.ID)
	})

	eligible := map[string]bool{}
	for _, rec := range consolidated {
		if !rec.OccurredAt.After(cutoff) {
			eligible[rec.ID] = true
		}
	}

	// Pressure: how many removals the thresholds demand (§3.1 row 2).
	need := 0
	if maxRecords > 0 && len(recs) > maxRecords {
		need = len(recs) - maxRecords
	}
	if maxBytes > 0 && fileBytes > maxBytes && len(recs) > 0 {
		// Estimate per-record size from the file average; keep enough of the
		// newest records to fit under maxBytes.
		keep := int(maxBytes * int64(len(recs)) / fileBytes)
		if over := len(recs) - keep; over > need {
			need = over
		}
	}
	for _, rec := range consolidated {
		if len(eligible) >= need {
			break
		}
		eligible[rec.ID] = true
	}

	ids := make([]string, 0, len(eligible))
	for _, rec := range consolidated { // already oldest-first
		if eligible[rec.ID] {
			ids = append(ids, rec.ID)
		}
	}
	return ids
}

// RecordView is the minimal projection AgeEligible needs; construct from
// episodic.Record.
type RecordView struct {
	ID           string
	OccurredAt   time.Time
	Consolidated bool
}
