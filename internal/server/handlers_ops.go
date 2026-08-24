package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/consolidate"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
)

// emptySHA256 is sha256("") — a cheap, always-valid probe key for cold-store
// reachability (BlobExists on it answers false without erroring when S3 is up).
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

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
	if s.deps.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "hot store unavailable")
		return
	}
	rep := StatusReport{DirtyFiles: []string{}, Degraded: []string{}}

	if s.deps.Rehydrator == nil {
		rep.Drift = rehydrate.DriftReport{
			Episodic:  rehydrate.Drift{Unavailable: true, Reason: "rehydrator not configured"},
			Knowledge: rehydrate.Drift{Unavailable: true, Reason: "rehydrator not configured"},
		}
	} else if drift, err := s.deps.Rehydrator.CheckDrift(r.Context()); err != nil {
		s.deps.Logger.Error("status drift check failed", "error", err)
		rep.Drift = rehydrate.DriftReport{
			Episodic:  rehydrate.Drift{Unavailable: true, Reason: "drift check failed"},
			Knowledge: rehydrate.Drift{Unavailable: true, Reason: "drift check failed"},
		}
	} else {
		rep.Drift = drift
	}
	if rep.Drift.Episodic.Unavailable {
		rep.Degraded = append(rep.Degraded, degradedSearch)
	}
	if rep.Drift.Knowledge.Unavailable {
		rep.Degraded = append(rep.Degraded, degradedGraph)
	}

	m, err := s.deps.Store.Manifest(r.Context())
	if err != nil {
		s.deps.Logger.Error("status manifest read failed", "error", err)
		writeError(w, http.StatusInternalServerError, "manifest unreadable")
		return
	}
	rep.ManifestUpdatedAt = m.UpdatedAt
	for fk, fs := range m.Files {
		if fs.Dirty {
			rep.DirtyFiles = append(rep.DirtyFiles, fk)
		}
	}
	sort.Strings(rep.DirtyFiles)

	s.countUnconsolidated(r, &rep)

	rep.S3 = S3SyncStatus{Bucket: s.cfg.S3Bucket}
	if s.deps.Archiver != nil {
		rep.S3.Reachable = s.coldReachable(r.Context())
	}
	if !rep.S3.Reachable {
		rep.Degraded = append(rep.Degraded, degradedCold)
	}
	s.statusMu.Lock()
	rep.S3.LastArchiveAt = s.lastArchiveAt
	rep.S3.LastSnapshotAt = s.lastSnapshotAt
	s.statusMu.Unlock()

	writeJSON(w, http.StatusOK, rep)
}

// coldReachable probes the cold store for /status honesty (§0 principle 3).
//
// It deliberately uses a GET (FetchBlob) rather than a HEAD (BlobExists): S3
// answers HeadObject against a *non-existent bucket* with a bare 404 that is
// byte-for-byte indistinguishable from a missing key, so a HEAD-based probe
// reports a bucket that does not exist as "reachable". GetObject returns
// NoSuchBucket, so only a hit or an explicit ErrNotFound proves the bucket is
// really there.
func (s *Server) coldReachable(ctx context.Context) bool {
	body, err := s.deps.Archiver.FetchBlob(ctx, emptySHA256)
	if err == nil {
		body.Close()
		return true
	}
	if errors.Is(err, cold.ErrNotFound) {
		// Bucket answered; the probe key simply is not stored.
		return true
	}
	s.deps.Logger.Warn("cold store unreachable", "bucket", s.cfg.S3Bucket, "error", err)
	return false
}

// countUnconsolidated scans every hot project for undistilled episodes and the
// stale subset older than the TTL (§3.1 — these are surfaced forever, never
// auto-deleted). Failures degrade to logs; /status must still answer.
func (s *Server) countUnconsolidated(r *http.Request, rep *StatusReport) {
	projects, err := s.deps.Store.ListProjects(r.Context())
	if err != nil {
		s.deps.Logger.Error("status project listing failed", "error", err)
		rep.Degraded = append(rep.Degraded, "unconsolidated count unavailable")
		return
	}
	ttl := time.Duration(s.cfg.EpisodicTTLDays) * 24 * time.Hour
	staleBefore := s.deps.Clock.Now().UTC().Add(-ttl)
	for _, p := range projects {
		recs, err := s.deps.Store.ListEpisodes(r.Context(), p)
		if err != nil {
			s.deps.Logger.Error("status episode listing failed", "project", p.String(), "error", err)
			rep.Degraded = append(rep.Degraded, "unconsolidated count incomplete: "+p.String())
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
	if s.deps.Consolidator == nil {
		writeError(w, http.StatusServiceUnavailable, "consolidation unavailable")
		return
	}
	var req ConsolidateRequest
	if err := decodeJSON(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts := consolidate.Options{DryRun: req.DryRun}
	for _, raw := range req.Projects {
		key, err := parseProjectString(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		opts.Projects = append(opts.Projects, key)
	}

	rep, err := s.deps.Consolidator.Run(r.Context(), opts)
	if err != nil {
		s.deps.Logger.Error("consolidation run failed", "error", err)
		writeError(w, http.StatusInternalServerError, "consolidation failed")
		return
	}
	now := s.deps.Clock.Now().UTC()
	s.statusMu.Lock()
	if len(rep.ArchiveKeys) > 0 {
		s.lastArchiveAt = now
	}
	if len(rep.SnapshotKeys) > 0 {
		s.lastSnapshotAt = now
	}
	s.statusMu.Unlock()
	writeJSON(w, http.StatusOK, rep)
}

// parseProjectString parses a "ws/team/proj" selector.
func parseProjectString(raw string) (hotstore.ProjectKey, error) {
	var key hotstore.ProjectKey
	parts := splitProject(raw)
	if parts == nil {
		return key, errInvalidProject(raw)
	}
	key = hotstore.ProjectKey{Workspace: parts[0], Team: parts[1], Project: parts[2]}
	if err := validateProjectKey(key); err != nil {
		return key, errInvalidProject(raw)
	}
	return key, nil
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
	if s.deps.Rehydrator == nil {
		writeError(w, http.StatusServiceUnavailable, "rehydrator unavailable")
		return
	}
	verify := r.URL.Query().Get("verify") == "true"
	rep, err := s.deps.Rehydrator.RehydrateAll(r.Context(), verify)
	if err != nil {
		s.deps.Logger.Error("reindex failed", "error", err)
		writeError(w, http.StatusInternalServerError, "reindex failed")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
