package rehydrate

import (
	"context"
	"fmt"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// Failure prefixes reported in Report.Failures. They are read by operators via
// /v1/reindex, so the vocabulary is declared once.
const (
	planeEpisodicLabel  = "episodic"
	planeKnowledgeLabel = "knowledge"
)

// Degraded notes for a plane whose backing service was never configured.
const (
	noteIndexUnavailable = planeEpisodicLabel + ": opensearch unavailable"
	noteGraphUnavailable = planeKnowledgeLabel + ": neo4j unavailable"
)

// RehydrateAll implements Service.
func (s *service) RehydrateAll(ctx context.Context, verify bool) (Report, error) {
	rep := Report{Failures: []string{}}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return rep, errs.Wrap(opRehydrateAll, err)
	}

	epOK := make(map[string]bool, len(projects))
	knOK := make(map[string]bool, len(projects))
	epFull := s.rehydrateEpisodicAll(ctx, projects, verify, &rep, epOK)
	knFull := s.rehydrateKnowledgeAll(ctx, projects, verify, &rep, knOK)
	rep.Verified = verify

	if err := s.commitManifest(ctx, projects, epOK, knOK, epFull, knFull); err != nil {
		rep.Failures = append(rep.Failures, "manifest update: "+err.Error())
	}
	if len(rep.Failures) > 0 {
		s.log.WarnContext(ctx, "rehydration completed with failures",
			"failures", len(rep.Failures), "projects", len(projects), "verify", verify)
	}
	return rep, nil
}

// rehydrateEpisodicAll drops and bulk-rebuilds the episodic index. Returns
// true when every project converged (plane fully hydrated).
func (s *service) rehydrateEpisodicAll(ctx context.Context, projects []hotstore.ProjectKey, verify bool, rep *Report, ok map[string]bool) bool {
	if s.index == nil {
		rep.Failures = append(rep.Failures, noteIndexUnavailable)
		return false
	}
	full := true
	if err := s.index.Drop(ctx); err != nil {
		// A missing index is fine — Drop before first hydration. Report but
		// continue; EnsureIndex decides whether the plane is usable.
		rep.Failures = append(rep.Failures, planeEpisodicLabel+": drop index: "+err.Error())
	}
	if err := s.index.EnsureIndex(ctx); err != nil {
		rep.Failures = append(rep.Failures, planeEpisodicLabel+": ensure index: "+err.Error())
		return false
	}
	for _, p := range projects {
		recs, err := s.store.ListEpisodes(ctx, p)
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s %s: list: %v", planeEpisodicLabel, p.String(), err))
			full = false
			continue
		}
		if len(recs) > 0 {
			if err := s.index.IndexRecords(ctx, p, recs); err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s %s: bulk index: %v", planeEpisodicLabel, p.String(), err))
				full = false
				continue
			}
		}
		if verify {
			got, err := s.index.DocCount(ctx, p)
			if err != nil || got != len(recs) {
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s %s: verify: indexed %d != hot %d (err=%v)", planeEpisodicLabel, p.String(), got, len(recs), err))
				full = false
				continue
			}
		}
		rep.EpisodesIndexed += len(recs)
		ok[p.String()] = true
	}
	return full
}

// rehydrateKnowledgeAll replays every hot knowledge graph through idempotent
// MERGE upserts. Returns true when every project converged.
func (s *service) rehydrateKnowledgeAll(ctx context.Context, projects []hotstore.ProjectKey, verify bool, rep *Report, ok map[string]bool) bool {
	if s.graph == nil {
		rep.Failures = append(rep.Failures, noteGraphUnavailable)
		return false
	}
	full := true
	// A full rebuild starts from a clean slate, mirroring the episodic
	// drop-then-bulk. MERGE alone can only add or update: nodes that left hot
	// (purged, or left behind by a previous hot state) would survive every
	// replay, so CheckDrift would report "graph nodes N != hot nodes M"
	// forever and no rehydration could ever clear it (§5). Only the full path
	// clears — RehydrateProject converges one project and must not touch the
	// others. Hot JSON is the source of truth (§0 principle 1), so a clear
	// followed by replay is always recoverable by re-running.
	if err := s.graph.Clear(ctx); err != nil {
		rep.Failures = append(rep.Failures, planeKnowledgeLabel+": clear graph: "+err.Error())
		full = false
	}
	for _, p := range projects {
		g, err := s.store.ReadKnowledge(ctx, p)
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s %s: read: %v", planeKnowledgeLabel, p.String(), err))
			full = false
			continue
		}
		if len(g.Nodes) > 0 {
			if err := s.graph.UpsertNodes(ctx, p, g.Nodes); err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s %s: upsert nodes: %v", planeKnowledgeLabel, p.String(), err))
				full = false
				continue
			}
		}
		if len(g.Edges) > 0 {
			if err := s.graph.UpsertEdges(ctx, p, g.Edges); err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s %s: upsert edges: %v", planeKnowledgeLabel, p.String(), err))
				full = false
				continue
			}
		}
		if verify {
			got, err := s.graph.NodeCount(ctx, p)
			if err != nil || got != len(g.Nodes) {
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s %s: verify: graph %d != hot %d (err=%v)", planeKnowledgeLabel, p.String(), got, len(g.Nodes), err))
				full = false
				continue
			}
		}
		rep.NodesUpserted += len(g.Nodes)
		rep.EdgesUpserted += len(g.Edges)
		ok[p.String()] = true
	}
	return full
}

// RehydrateProject implements Service.
func (s *service) RehydrateProject(ctx context.Context, key hotstore.ProjectKey) (Report, error) {
	rep := Report{Failures: []string{}}
	f := failures{lines: rep.Failures}
	epOK := map[string]bool{}
	knOK := map[string]bool{}

	s.rehydrateProjectEpisodic(ctx, key, &rep, &f, epOK)
	s.rehydrateProjectKnowledge(ctx, key, &rep, &f, knOK)

	if err := s.commitManifest(ctx, []hotstore.ProjectKey{key}, epOK, knOK, epOK[key.String()], knOK[key.String()]); err != nil {
		f.add("manifest update: " + err.Error())
	}
	rep.Failures = f.lines
	return rep, f.err(opRehydrateProject)
}

// rehydrateProjectEpisodic converges one project's episodic plane.
func (s *service) rehydrateProjectEpisodic(ctx context.Context, key hotstore.ProjectKey, rep *Report, f *failures, ok map[string]bool) {
	if s.index == nil {
		f.addUnavailable(noteIndexUnavailable)
		return
	}
	if err := s.index.EnsureIndex(ctx); err != nil {
		f.add(planeEpisodicLabel + ": ensure index: " + err.Error())
		return
	}
	recs, err := s.store.ListEpisodes(ctx, key)
	if err != nil {
		f.add(fmt.Sprintf("%s %s: list: %v", planeEpisodicLabel, key.String(), err))
		return
	}
	// Delete-then-replay, the project-scoped form of the full path's
	// drop-then-bulk. IndexRecords can only add or update, so without the
	// delete an episode that left hot — aged to cold after §3.1, or removed by
	// a consolidation whose index delete failed — would stay searchable while
	// commitManifest below clears its dirty flag and swears the plane is fresh.
	// An empty project still needs it: "no hot records" is exactly the state
	// where every indexed document is stale.
	if err := s.index.DeleteProject(ctx, key); err != nil {
		f.add(fmt.Sprintf("%s %s: delete stale: %v", planeEpisodicLabel, key.String(), err))
		return
	}
	if len(recs) > 0 {
		if err := s.index.IndexRecords(ctx, key, recs); err != nil {
			f.add(fmt.Sprintf("%s %s: bulk index: %v", planeEpisodicLabel, key.String(), err))
			return
		}
		rep.EpisodesIndexed = len(recs)
	}
	ok[key.String()] = true
}

// rehydrateProjectKnowledge converges one project's knowledge plane. It never
// clears the whole graph: the partial path must not touch other projects (§5).
// Removals are converged with a project-scoped reconciliation instead.
func (s *service) rehydrateProjectKnowledge(ctx context.Context, key hotstore.ProjectKey, rep *Report, f *failures, ok map[string]bool) {
	if s.graph == nil {
		f.addUnavailable(noteGraphUnavailable)
		return
	}
	g, err := s.store.ReadKnowledge(ctx, key)
	if err != nil {
		f.add(fmt.Sprintf("%s %s: read: %v", planeKnowledgeLabel, key.String(), err))
		return
	}
	upsertErr := error(nil)
	if len(g.Nodes) > 0 {
		upsertErr = s.graph.UpsertNodes(ctx, key, g.Nodes)
	}
	if upsertErr == nil && len(g.Edges) > 0 {
		upsertErr = s.graph.UpsertEdges(ctx, key, g.Edges)
	}
	if upsertErr != nil {
		f.add(fmt.Sprintf("%s %s: upsert: %v", planeKnowledgeLabel, key.String(), upsertErr))
		return
	}
	// Reconcile after the replay, so a failure here leaves more data rather
	// than less. Without it a node purged from hot whose live Neo4j delete
	// failed would survive every MERGE replay while the dirty flag that marked
	// it gets cleared — permanent, unflagged graph-only content (§0 principle 1).
	if err := s.graph.DeleteMissing(ctx, key, nodeIDs(g.Nodes)); err != nil {
		f.add(fmt.Sprintf("%s %s: delete stale: %v", planeKnowledgeLabel, key.String(), err))
		return
	}
	rep.NodesUpserted = len(g.Nodes)
	rep.EdgesUpserted = len(g.Edges)
	ok[key.String()] = true
}

// nodeIDs projects a node slice onto its ids — the keep-list of a project-scoped
// graph reconciliation.
func nodeIDs(nodes []knowledge.Node) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return ids
}

// StatGate implements Service.
func (s *service) StatGate(ctx context.Context, key hotstore.ProjectKey) error {
	if !s.claimGate(key) {
		return nil
	}

	m, err := s.store.Manifest(ctx)
	if err != nil {
		return errs.Wrap(opStatGate, err)
	}
	if !s.needsRehydration(ctx, m, key) {
		return nil
	}
	if _, err := s.RehydrateProject(ctx, key); err != nil {
		return errs.Wrap(opStatGate, err)
	}
	return nil
}

// claimGate reports whether this call owns the current debounce window for
// key, recording the claim. It holds mu only around the map access.
func (s *service) claimGate(key hotstore.ProjectKey) bool {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, seen := s.lastGate[key.String()]; seen && now.Sub(last) < debounceInterval {
		return false
	}
	s.lastGate[key.String()] = now
	return true
}

// needsRehydration is the freshness check run at most once per debounce window.
// It asks two independent questions, because hot and derived state can each move
// without the other: did the canonical side change, and did the derived side
// lose what it had?
func (s *service) needsRehydration(ctx context.Context, m hotstore.Manifest, key hotstore.ProjectKey) bool {
	return s.hotChanged(ctx, m, key) || s.derivedLost(ctx, m, key)
}

// hotChanged is the cheap local check: dirty flag, file mtime newer than the
// manifest IndexedAt, or a hot file the manifest has never seen.
func (s *service) hotChanged(ctx context.Context, m hotstore.Manifest, key hotstore.ProjectKey) bool {
	for _, plane := range []hotstore.Plane{hotstore.PlaneEpisodic, hotstore.PlaneKnowledge} {
		fs, tracked := m.Files[hotstore.ManifestFileKey(plane, key)]
		if tracked && fs.Dirty {
			return true
		}
		_, mtime, err := s.store.FileInfo(ctx, key, plane)
		if err != nil {
			// Missing file (or unreadable) — nothing to converge for this plane.
			continue
		}
		if !tracked || mtime.After(fs.IndexedAt) {
			return true
		}
	}
	return false
}

// derivedLost reports whether a derived store now holds fewer records than the
// manifest says it should — the signature of a container that was replaced (§1).
// Without this check that loss is invisible to the gate: destroying only the
// *derived* side changes neither the dirty flag nor the hot file mtime, so every
// read would keep answering from an empty index until someone ran /reindex by
// hand.
//
// Two deliberate asymmetries:
//   - an unreachable store is not "lost". Rehydrating against a dead container
//     would only churn; writes stay degraded and reads answer 503 until it is
//     back (§5).
//   - only a shortfall counts, never a surplus. A derived store holding more
//     than hot cannot be fixed by the partial path (it replays, it does not
//     drop), so treating that as drift would rehydrate on every single request
//     forever. /reindex owns that repair.
func (s *service) derivedLost(ctx context.Context, m hotstore.Manifest, key hotstore.ProjectKey) bool {
	if s.index != nil {
		want := m.Files[hotstore.ManifestFileKey(hotstore.PlaneEpisodic, key)].RecordCount
		if got, err := s.index.DocCount(ctx, key); err == nil && got < want {
			return true
		}
	}
	if s.graph != nil {
		want := m.Files[hotstore.ManifestFileKey(hotstore.PlaneKnowledge, key)].RecordCount
		if got, err := s.graph.NodeCount(ctx, key); err == nil && got < want {
			return true
		}
	}
	return false
}
