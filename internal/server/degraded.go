package server

import (
	"context"

	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
)

// Degraded-note strings (§5: hot writes succeed and report degraded; derived
// reads return 503 with the same wording).
const (
	degradedSearch = "search unavailable"
	degradedGraph  = "graph unavailable"
	degradedCold   = "cold storage unavailable"
)

// markIndexed records a successful best-effort derived upsert for one project
// plane file: dirty cleared, IndexedAt refreshed, and the plane hydration sha
// updated so CheckDrift stays quiet for converged content. Failures are logged
// only — manifest bookkeeping must never fail a request that already wrote hot.
func (s *Server) markIndexed(ctx context.Context, key hotstore.ProjectKey, plane hotstore.Plane) {
	now := s.deps.Clock.Now().UTC()
	err := s.deps.Store.UpdateManifest(ctx, func(m hotstore.Manifest) (hotstore.Manifest, error) {
		nm := hotstore.Manifest{
			Files:     make(map[string]hotstore.FileState, len(m.Files)),
			Indexes:   make(map[string]hotstore.IndexState, len(m.Indexes)+1),
			UpdatedAt: now,
		}
		for k, v := range m.Files {
			nm.Files[k] = v
		}
		for k, v := range m.Indexes {
			nm.Indexes[k] = v
		}
		fk := rehydrate.FileKey(plane, key)
		if fs, ok := nm.Files[fk]; ok {
			fs.Dirty = false
			fs.IndexedAt = now
			nm.Files[fk] = fs
		}
		nm.Indexes[rehydrate.IndexKeyFor(plane)] = hotstore.IndexState{LastHydratedSHA: rehydrate.PlaneStateSHA(nm, plane)}
		return nm, nil
	})
	if err != nil {
		s.deps.Logger.Error("manifest freshness update failed", "project", key.String(), "plane", plane, "error", err)
	}
}

// markDirty flags a project plane whose best-effort derived upsert failed so
// the next rehydration pass converges it (§1). Log-only on failure.
func (s *Server) markDirty(ctx context.Context, key hotstore.ProjectKey, plane hotstore.Plane) {
	if err := s.deps.Store.MarkDirty(ctx, key, plane); err != nil {
		s.deps.Logger.Error("manifest dirty mark failed", "project", key.String(), "plane", plane, "error", err)
	}
}

// statGate runs the request-entry freshness check (§5) best-effort; a failing
// gate never blocks the request — rehydration problems surface in /status.
func (s *Server) statGate(ctx context.Context, key hotstore.ProjectKey) {
	if s.deps.Rehydrator == nil {
		return
	}
	if err := s.deps.Rehydrator.StatGate(ctx, key); err != nil {
		s.deps.Logger.Warn("stat-gate rehydration incomplete", "project", key.String(), "error", err)
	}
}
