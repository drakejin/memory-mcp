package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/drakejin/memory-mcp/internal/consolidate"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
	"github.com/drakejin/memory-mcp/internal/server/apierr"
)

// emptySHA256 is sha256("") — a cheap, always-valid probe key for cold-store
// reachability.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// hoursPerDay converts the configured TTL in days into a duration.
const hoursPerDay = 24 * time.Hour

// Degraded notes /v1/status raises for itself when a count could not be taken.
// They are strings rather than errors: /status stays 200 and discloses the gap.
const (
	degradedCountUnavailable = "unconsolidated count unavailable"
	degradedCountIncomplete  = "unconsolidated count incomplete: "
)

// StatusReport is the honesty contract (§0 principle 3, §5): index freshness,
// rehydration need, unconsolidated counts, and S3 sync state — always
// reported, never hidden.
type StatusReport struct {
	// Drift is the live manifest-vs-derived comparison.
	Drift rehydrate.DriftReport `json:"drift"`
	// Unconsolidated counts episodes not yet distilled; StaleUnconsolidated
	// counts those older than the TTL that still refuse to age (§3.1 —
	// surfaced forever, never auto-deleted).
	Unconsolidated      int `json:"unconsolidated"`
	StaleUnconsolidated int `json:"stale_unconsolidated"`
	// ManifestUpdatedAt is the manifest's own freshness timestamp.
	ManifestUpdatedAt time.Time `json:"manifest_updated_at"`
	// DirtyFiles lists hot files whose derived upsert failed and awaits
	// rehydration.
	DirtyFiles []string `json:"dirty_files"`
	// S3 reports cold-store reachability and last successful archive/snapshot
	// activity.
	S3 S3SyncStatus `json:"s3"`
	// Degraded lists currently unavailable derived services.
	Degraded []string `json:"degraded"`
}

// S3SyncStatus summarizes cold-store sync state for /status.
type S3SyncStatus struct {
	Reachable      bool      `json:"reachable"`
	Bucket         string    `json:"bucket"`
	LastArchiveAt  time.Time `json:"last_archive_at"`
	LastSnapshotAt time.Time `json:"last_snapshot_at"`
}

// ConsolidateRequest is the POST body for /v1/consolidate.
type ConsolidateRequest struct {
	// Projects limits the run to "ws/team/proj" strings; empty = all.
	Projects []string `json:"projects,omitempty"`
	DryRun   bool     `json:"dry_run,omitempty"`
}

// handleStatus godoc
//
//	@Summary	Honest system status: freshness, drift, unconsolidated, S3 sync
//	@Tags		ops
//	@Produce	json
//	@Success	200	{object}	Envelope{data=StatusReport}
//	@Router		/v1/status [get]
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeAPIError(w, s.log, unavailable(msgHotStoreUnavailable))
		return
	}
	rep := StatusReport{DirtyFiles: []string{}, Degraded: []string{}}

	rep.Drift = s.driftReport(r.Context())
	if rep.Drift.Episodic.Unavailable {
		rep.Degraded = append(rep.Degraded, degradedSearch)
	}
	if rep.Drift.Knowledge.Unavailable {
		rep.Degraded = append(rep.Degraded, degradedGraph)
	}

	m, err := s.store.Manifest(r.Context())
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	rep.ManifestUpdatedAt = m.UpdatedAt
	for fk, fs := range m.Files {
		if fs.Dirty {
			rep.DirtyFiles = append(rep.DirtyFiles, fk)
		}
	}
	slices.Sort(rep.DirtyFiles)

	s.countUnconsolidated(r.Context(), &rep)

	rep.S3 = S3SyncStatus{Bucket: s.s3Bucket}
	if s.archiver != nil {
		rep.S3.Reachable = s.coldReachable(r.Context())
	}
	if !rep.S3.Reachable {
		rep.Degraded = append(rep.Degraded, degradedCold)
	}
	s.statusMu.Lock()
	rep.S3.LastArchiveAt = s.lastArchiveAt
	rep.S3.LastSnapshotAt = s.lastSnapshotAt
	s.statusMu.Unlock()

	writeJSON(w, s.log, http.StatusOK, rep)
}

// driftReport answers the manifest-vs-derived question even when it cannot be
// asked: an absent or failing rehydrator is reported as unavailable with a
// reason, never as "no drift" (§0 principle 3).
func (s *Server) driftReport(ctx context.Context) rehydrate.DriftReport {
	const (
		reasonNoRehydrator = "rehydrator not configured"
		reasonCheckFailed  = "drift check failed"
	)
	unavailableWith := func(reason string) rehydrate.DriftReport {
		return rehydrate.DriftReport{
			Episodic:  rehydrate.Drift{Unavailable: true, Reason: reason},
			Knowledge: rehydrate.Drift{Unavailable: true, Reason: reason},
		}
	}
	if s.rehydrator == nil {
		return unavailableWith(reasonNoRehydrator)
	}
	drift, err := s.rehydrator.CheckDrift(ctx)
	if err != nil {
		s.log.Error("status drift check failed", "error", err)
		return unavailableWith(reasonCheckFailed)
	}
	return drift
}

// coldReachable probes the cold store for /status honesty (§0 principle 3).
//
// It deliberately uses a GET (FetchBlob) rather than a HEAD: S3 answers
// HeadObject against a *non-existent bucket* with a bare 404 that is
// byte-for-byte indistinguishable from a missing key, so a HEAD-based probe
// reports a bucket that does not exist as "reachable". GetObject returns
// NoSuchBucket, so only a hit or an explicit not-found proves the bucket is
// really there.
func (s *Server) coldReachable(ctx context.Context) bool {
	body, err := s.archiver.FetchBlob(ctx, emptySHA256)
	if err == nil {
		body.Close()
		return true
	}
	if errors.Is(err, errs.ErrNotFound) {
		// Bucket answered; the probe key simply is not stored.
		return true
	}
	s.log.Warn("cold store unreachable", "bucket", s.s3Bucket, "error", err)
	return false
}

// countUnconsolidated scans every hot project for undistilled episodes and the
// stale subset older than the TTL (§3.1 — these are surfaced forever, never
// auto-deleted). Failures degrade to a disclosure; /status must still answer.
func (s *Server) countUnconsolidated(ctx context.Context, rep *StatusReport) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.log.Error("status project listing failed", "error", err)
		rep.Degraded = append(rep.Degraded, degradedCountUnavailable)
		return
	}
	staleBefore := s.clock.Now().UTC().Add(-time.Duration(s.ttlDays) * hoursPerDay)
	for _, p := range projects {
		recs, err := s.store.ListEpisodes(ctx, p)
		if err != nil {
			s.log.Error("status episode listing failed", "project", p.String(), "error", err)
			rep.Degraded = append(rep.Degraded, degradedCountIncomplete+p.String())
			continue
		}
		for _, rec := range recs {
			if rec.Consolidated {
				continue
			}
			rep.Unconsolidated++
			if rec.OccurredAt.Before(staleBefore) {
				rep.StaleUnconsolidated++
			}
		}
	}
}

// handleConsolidate godoc
//
//	@Summary	Run the deterministic consolidation pipeline (§4)
//	@Tags		ops
//	@Accept		json
//	@Produce	json
//	@Param		body	body		ConsolidateRequest	false	"options"
//	@Success	200		{object}	Envelope{data=consolidate.Report}
//	@Failure	400		{object}	Envelope
//	@Failure	503		{object}	Envelope
//	@Router		/v1/consolidate [post]
func (s *Server) handleConsolidate(w http.ResponseWriter, r *http.Request) {
	if s.consolidator == nil {
		writeAPIError(w, s.log, unavailable(msgConsolidatorUnavailable))
		return
	}
	var req ConsolidateRequest
	if apiErr := decodeJSON(w, r, &req, true); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	opts := consolidate.Options{DryRun: req.DryRun}
	for _, raw := range req.Projects {
		key, apiErr := parseProjectString(raw)
		if apiErr != nil {
			writeAPIError(w, s.log, apiErr)
			return
		}
		opts.Projects = append(opts.Projects, key)
	}

	rep, err := s.consolidator.Run(r.Context(), opts)
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	s.recordColdActivity(rep)
	writeJSON(w, s.log, http.StatusOK, rep)
}

// recordColdActivity refreshes the S3 activity timestamps /v1/status reports.
// Guarded by statusMu, which is the only lock in this package.
func (s *Server) recordColdActivity(rep consolidate.Report) {
	now := s.clock.Now().UTC()
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if len(rep.ArchiveKeys) > 0 {
		s.lastArchiveAt = now
	}
	if len(rep.SnapshotKeys) > 0 {
		s.lastSnapshotAt = now
	}
}

// handleReindex godoc
//
//	@Summary	Force full rehydration of derived stores (§5)
//	@Tags		ops
//	@Produce	json
//	@Param		verify	query		bool	false	"full hash audit"
//	@Success	200		{object}	Envelope{data=rehydrate.Report}
//	@Failure	503		{object}	Envelope
//	@Router		/v1/reindex [post]
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if s.rehydrator == nil {
		writeAPIError(w, s.log, unavailable(msgRehydratorUnavailable))
		return
	}
	verify := r.URL.Query().Get(paramVerify) == valueTrue
	rep, err := s.rehydrator.RehydrateAll(r.Context(), verify)
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	writeJSON(w, s.log, http.StatusOK, rep)
}
