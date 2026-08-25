package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/drakejin/memory-mcp/internal/handler/app/httpserver/apierr"
	"github.com/drakejin/memory-mcp/internal/service/consolidate"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
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

// HandleStatus godoc
//
//	@Summary	Honest system status: freshness, drift, unconsolidated, S3 sync
//	@Tags		ops
//	@Produce	json
//	@Success	200	{object}	Envelope{data=StatusReport}
//	@Router		/v1/status [get]
//
// The report is assembled from three service surfaces: drift from the
// rehydrator, the §3.1 hot bookkeeping from the consolidator, cold
// reachability from the document pipeline. The consolidator doubles as the
// hot-store availability signal — it cannot exist without the store, so its
// absence is the store's.
func (s *Handler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if s.Consolidator == nil {
		writeAPIError(w, s.Log, unavailable(msgHotStoreUnavailable))
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

	hs, err := s.Consolidator.HotState(r.Context())
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	rep.ManifestUpdatedAt = hs.ManifestUpdatedAt
	// append keeps DirtyFiles non-nil even when the service returned none, so
	// the body renders [] rather than null.
	rep.DirtyFiles = append(rep.DirtyFiles, hs.DirtyFiles...)
	rep.Unconsolidated = hs.Unconsolidated
	rep.StaleUnconsolidated = hs.StaleUnconsolidated
	rep.Degraded = append(rep.Degraded, hs.Degraded...)

	rep.S3 = S3SyncStatus{Bucket: s.S3Bucket}
	if s.Documents != nil {
		rep.S3.Reachable = s.Documents.ColdReachable(r.Context())
	}
	if !rep.S3.Reachable {
		rep.Degraded = append(rep.Degraded, degradedCold)
	}
	s.statusMu.Lock()
	rep.S3.LastArchiveAt = s.lastArchiveAt
	rep.S3.LastSnapshotAt = s.lastSnapshotAt
	s.statusMu.Unlock()

	writeJSON(w, s.Log, http.StatusOK, rep)
}

// driftReport answers the manifest-vs-derived question even when it cannot be
// asked: an absent or failing rehydrator is reported as unavailable with a
// reason, never as "no drift" (§0 principle 3).
func (s *Handler) driftReport(ctx context.Context) rehydrate.DriftReport {
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
	if s.Rehydrator == nil {
		return unavailableWith(reasonNoRehydrator)
	}
	drift, err := s.Rehydrator.CheckDrift(ctx)
	if err != nil {
		s.Log.Error("status drift check failed", "error", err)
		return unavailableWith(reasonCheckFailed)
	}
	return drift
}

// HandleConsolidate godoc
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
func (s *Handler) HandleConsolidate(w http.ResponseWriter, r *http.Request) {
	if s.Consolidator == nil {
		writeAPIError(w, s.Log, unavailable(msgConsolidatorUnavailable))
		return
	}
	var req ConsolidateRequest
	if apiErr := decodeJSON(w, r, &req, true); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	opts := consolidate.Options{DryRun: req.DryRun}
	for _, raw := range req.Projects {
		key, apiErr := parseProjectString(raw)
		if apiErr != nil {
			writeAPIError(w, s.Log, apiErr)
			return
		}
		opts.Projects = append(opts.Projects, key)
	}

	rep, err := s.Consolidator.Run(r.Context(), opts)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	s.recordColdActivity(rep)
	writeJSON(w, s.Log, http.StatusOK, rep)
}

// recordColdActivity refreshes the S3 activity timestamps /v1/status reports.
// Guarded by statusMu, which is the only lock in this package.
func (s *Handler) recordColdActivity(rep consolidate.Report) {
	now := s.Clock.Now().UTC()
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if len(rep.ArchiveKeys) > 0 {
		s.lastArchiveAt = now
	}
	if len(rep.SnapshotKeys) > 0 {
		s.lastSnapshotAt = now
	}
}

// HandleReindex godoc
//
//	@Summary	Force full rehydration of derived stores (§5)
//	@Tags		ops
//	@Produce	json
//	@Param		verify	query		bool	false	"full hash audit"
//	@Success	200		{object}	Envelope{data=rehydrate.Report}
//	@Failure	503		{object}	Envelope
//	@Router		/v1/reindex [post]
func (s *Handler) HandleReindex(w http.ResponseWriter, r *http.Request) {
	if s.Rehydrator == nil {
		writeAPIError(w, s.Log, unavailable(msgRehydratorUnavailable))
		return
	}
	verify := r.URL.Query().Get(paramVerify) == valueTrue
	rep, err := s.Rehydrator.RehydrateAll(r.Context(), verify)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusOK, rep)
}

// HandleHealthz godoc
//
//	@Summary	Liveness probe
//	@Tags		ops
//	@Produce	json
//	@Success	200	{object}	Envelope
//	@Router		/healthz [get]
func (s *Handler) HandleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Log, http.StatusOK, map[string]string{"status": "ok"})
}
