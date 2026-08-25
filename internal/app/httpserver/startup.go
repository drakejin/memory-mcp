package httpserver

import "context"

// Startup runs the boot-time manifest comparison (§5): drift between hot JSON
// and the derived stores triggers a full rehydration (episodic drop+bulk,
// knowledge MERGE replay). Degraded-tolerant by design — failures are logged
// and reported by /v1/status, never fatal, because derived stores may be down
// at boot and hot writes must still work.
func (s *Server) Startup(ctx context.Context) {
	if s.rehydrator == nil {
		s.log.Warn("startup: rehydrator not configured; derived stores unmanaged")
		return
	}
	drift, err := s.rehydrator.CheckDrift(ctx)
	if err != nil {
		s.log.Error("startup: drift check failed", "error", err)
		return
	}
	s.log.Info("startup drift check",
		"episodic_detected", drift.Episodic.Detected, "episodic_reason", drift.Episodic.Reason,
		"knowledge_detected", drift.Knowledge.Detected, "knowledge_reason", drift.Knowledge.Reason,
		"episodic_unavailable", drift.Episodic.Unavailable, "knowledge_unavailable", drift.Knowledge.Unavailable)
	if !drift.Episodic.Detected && !drift.Knowledge.Detected {
		return
	}
	rep, err := s.rehydrator.RehydrateAll(ctx, false)
	if err != nil {
		s.log.Error("startup: rehydration failed", "error", err)
		return
	}
	s.log.Info("startup rehydration complete",
		"episodes_indexed", rep.EpisodesIndexed,
		"nodes_upserted", rep.NodesUpserted,
		"edges_upserted", rep.EdgesUpserted,
		"failures", len(rep.Failures))
	for _, f := range rep.Failures {
		s.log.Warn("startup rehydration failure", "detail", f)
	}
}
