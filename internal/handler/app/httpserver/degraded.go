package httpserver

import (
	"context"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Degraded-note strings (§5: hot writes succeed and report degraded; derived
// reads return 503 with the same wording, so a client reads one vocabulary).
// The services publish the same words (episode.DegradedSearch and friends);
// these transport copies exist so a nil *service* — a collaborator the process
// could not build at all — still answers in the frozen vocabulary.
const (
	degradedSearch = "search unavailable"
	degradedGraph  = "graph unavailable"
	degradedCold   = "cold storage unavailable"
)

// Messages for a collaborator the process could not build at all. They are
// 503s rather than degraded notes: without the canonical store or the document
// pipeline there is no honest partial answer to give.
const (
	msgHotStoreUnavailable     = "hot store unavailable"
	msgDocumentsUnavailable    = "document pipeline unavailable"
	msgConsolidatorUnavailable = "consolidation unavailable"
	msgRehydratorUnavailable   = "rehydrator unavailable"
)

// statGate runs the request-entry freshness check (§5) best-effort; a failing
// gate never blocks the request — rehydration problems surface in /status.
// Only the knowledge read paths call it: the episodic search gates inside its
// service, where the gate must precede the recall bump.
func (s *Handler) statGate(ctx context.Context, key projectkey.Key) {
	if s.Rehydrator == nil {
		return
	}
	if err := s.Rehydrator.StatGate(ctx, key); err != nil {
		s.Log.Warn("stat-gate rehydration incomplete", "project", key.String(), "error", err)
	}
}
