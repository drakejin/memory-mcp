// Package rehydrate keeps derived stores truthful to hot JSON
// (architecture-v2.md §5). Containers run without volumes and may die at any
// time; truth is decided by comparing manifest hashes/counts against live
// index state. Episodic rehydration drops and bulk-rebuilds; knowledge
// rehydration replays idempotent MERGEs.
package rehydrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/drakejin/memory-mcp/internal/graph"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/search"
)

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

// Rehydrator is the contract for startup checks, the request-time stat-gate,
// and POST /v1/reindex.
type Rehydrator interface {
	// CheckDrift compares OpenSearch index existence/doc-count and Neo4j
	// node-count against the manifest without mutating anything.
	CheckDrift(ctx context.Context) (DriftReport, error)
	// RehydrateAll rebuilds both derived stores from hot JSON (episodic:
	// drop+bulk; knowledge: MERGE replay). verify additionally audits every
	// record hash. Clears manifest dirty flags on success.
	RehydrateAll(ctx context.Context, verify bool) (Report, error)
	// RehydrateProject converges one project only — the request-time partial
	// path taken when the stat-gate (2s debounce) sees dirty marks or mtime
	// changes (§5).
	RehydrateProject(ctx context.Context, key hotstore.ProjectKey) (Report, error)
	// StatGate runs the debounced freshness check for a project and triggers
	// RehydrateProject when needed. Cheap: file mtime + manifest age only.
	StatGate(ctx context.Context, key hotstore.ProjectKey) error
}

// Runner is the concrete Rehydrator.
type Runner struct {
	store hotstore.Store
	index search.Index
	graph graph.Store
	clock hotstore.Clock

	// mu guards lastGate, the per-project debounce timestamps for StatGate.
	mu       sync.Mutex
	lastGate map[string]time.Time
}

// Compile-time contract check.
var _ Rehydrator = (*Runner)(nil)

// New wires a Runner. index and gr may be nil when the backing service failed
// to initialize; every method then reports that plane as unavailable instead
// of panicking (§5 degraded semantics).
func New(store hotstore.Store, index search.Index, gr graph.Store, clock hotstore.Clock) *Runner {
	return &Runner{
		store:    store,
		index:    index,
		graph:    gr,
		clock:    clock,
		lastGate: make(map[string]time.Time),
	}
}

// CheckDrift implements Rehydrator.
func (r *Runner) CheckDrift(ctx context.Context) (DriftReport, error) {
	m, err := r.store.Manifest(ctx)
	if err != nil {
		return DriftReport{}, fmt.Errorf("rehydrate: read manifest: %w", err)
	}
	return DriftReport{
		Episodic:  r.episodicDrift(ctx, m),
		Knowledge: r.knowledgeDrift(ctx, m),
	}, nil
}

// episodicDrift compares manifest record counts (and dirty flags / hydration
// sha) against the live OpenSearch doc count.
func (r *Runner) episodicDrift(ctx context.Context, m hotstore.Manifest) Drift {
	if r.index == nil {
		return Drift{Unavailable: true, Reason: "opensearch not configured"}
	}
	if err := r.index.Ping(ctx); err != nil {
		return Drift{Unavailable: true, Reason: "opensearch unreachable"}
	}
	want, dirty := planeTotals(m, hotstore.PlaneEpisodic)
	got, err := r.index.DocCount(ctx, hotstore.ProjectKey{})
	if err != nil {
		// Reachable but uncountable — the index itself is absent (fresh
		// container) and must be rebuilt.
		return Drift{Detected: true, Reason: "episodic index absent or uncountable"}
	}
	switch {
	case got != want:
		return Drift{Detected: true, Reason: fmt.Sprintf("indexed docs %d != manifest records %d", got, want)}
	case dirty > 0:
		return Drift{Detected: true, Reason: fmt.Sprintf("%d dirty episodic file(s) await rehydration", dirty)}
	}
	if st, ok := m.Indexes[IndexKeyEpisodic]; ok && st.LastHydratedSHA != "" &&
		st.LastHydratedSHA != PlaneStateSHA(m, hotstore.PlaneEpisodic) {
		return Drift{Detected: true, Reason: "episodic hot content changed since last hydration"}
	}
	return Drift{}
}

// knowledgeDrift compares actual hot node counts against the live Neo4j node
// count. Counting from the hot files (not the manifest) keeps this check
// independent of how the hotstore accounts knowledge records.
func (r *Runner) knowledgeDrift(ctx context.Context, m hotstore.Manifest) Drift {
	if r.graph == nil {
		return Drift{Unavailable: true, Reason: "neo4j not configured"}
	}
	if err := r.graph.Ping(ctx); err != nil {
		return Drift{Unavailable: true, Reason: "neo4j unreachable"}
	}
	want, err := r.hotNodeCount(ctx)
	if err != nil {
		return Drift{Detected: true, Reason: "hot knowledge unreadable: " + err.Error()}
	}
	got, err := r.graph.NodeCount(ctx, hotstore.ProjectKey{})
	if err != nil {
		return Drift{Detected: true, Reason: "knowledge graph uncountable"}
	}
	_, dirty := planeTotals(m, hotstore.PlaneKnowledge)
	switch {
	case got != want:
		return Drift{Detected: true, Reason: fmt.Sprintf("graph nodes %d != hot nodes %d", got, want)}
	case dirty > 0:
		return Drift{Detected: true, Reason: fmt.Sprintf("%d dirty knowledge file(s) await rehydration", dirty)}
	}
	if st, ok := m.Indexes[IndexKeyKnowledge]; ok && st.LastHydratedSHA != "" &&
		st.LastHydratedSHA != PlaneStateSHA(m, hotstore.PlaneKnowledge) {
		return Drift{Detected: true, Reason: "knowledge hot content changed since last hydration"}
	}
	return Drift{}
}

// hotNodeCount sums knowledge nodes across every hot project file.
func (r *Runner) hotNodeCount(ctx context.Context) (int, error) {
	projects, err := r.store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, p := range projects {
		g, err := r.store.ReadKnowledge(ctx, p)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", p.String(), err)
		}
		total += len(g.Nodes)
	}
	return total, nil
}

// planeTotals sums manifest record counts and dirty files for one plane.
func planeTotals(m hotstore.Manifest, plane hotstore.Plane) (records, dirty int) {
	prefix := string(plane) + "/"
	for k, fs := range m.Files {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		records += fs.RecordCount
		if fs.Dirty {
			dirty++
		}
	}
	return records, dirty
}

// RehydrateAll implements Rehydrator.
func (r *Runner) RehydrateAll(ctx context.Context, verify bool) (Report, error) {
	rep := Report{Failures: []string{}}
	projects, err := r.store.ListProjects(ctx)
	if err != nil {
		return rep, fmt.Errorf("rehydrate: list projects: %w", err)
	}

	epOK := make(map[string]bool, len(projects))
	knOK := make(map[string]bool, len(projects))
	epFull := r.rehydrateEpisodicAll(ctx, projects, verify, &rep, epOK)
	knFull := r.rehydrateKnowledgeAll(ctx, projects, verify, &rep, knOK)
	rep.Verified = verify

	if err := r.commitManifest(ctx, projects, epOK, knOK, epFull, knFull); err != nil {
		rep.Failures = append(rep.Failures, "manifest update: "+err.Error())
	}
	return rep, nil
}

// rehydrateEpisodicAll drops and bulk-rebuilds the episodic index. Returns
// true when every project converged (plane fully hydrated).
func (r *Runner) rehydrateEpisodicAll(ctx context.Context, projects []hotstore.ProjectKey, verify bool, rep *Report, ok map[string]bool) bool {
	if r.index == nil {
		rep.Failures = append(rep.Failures, "episodic: opensearch unavailable")
		return false
	}
	full := true
	if err := r.index.Drop(ctx); err != nil {
		// A missing index is fine — Drop before first hydration. Report but
		// continue; EnsureIndex decides whether the plane is usable.
		rep.Failures = append(rep.Failures, "episodic: drop index: "+err.Error())
	}
	if err := r.index.EnsureIndex(ctx); err != nil {
		rep.Failures = append(rep.Failures, "episodic: ensure index: "+err.Error())
		return false
	}
	for _, p := range projects {
		recs, err := r.store.ListEpisodes(ctx, p)
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("episodic %s: list: %v", p.String(), err))
			full = false
			continue
		}
		if len(recs) > 0 {
			if err := r.index.IndexRecords(ctx, p, recs); err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("episodic %s: bulk index: %v", p.String(), err))
				full = false
				continue
			}
		}
		if verify {
			got, err := r.index.DocCount(ctx, p)
			if err != nil || got != len(recs) {
				rep.Failures = append(rep.Failures, fmt.Sprintf("episodic %s: verify: indexed %d != hot %d (err=%v)", p.String(), got, len(recs), err))
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
func (r *Runner) rehydrateKnowledgeAll(ctx context.Context, projects []hotstore.ProjectKey, verify bool, rep *Report, ok map[string]bool) bool {
	if r.graph == nil {
		rep.Failures = append(rep.Failures, "knowledge: neo4j unavailable")
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
	if err := r.graph.Clear(ctx); err != nil {
		rep.Failures = append(rep.Failures, "knowledge: clear graph: "+err.Error())
		full = false
	}
	for _, p := range projects {
		g, err := r.store.ReadKnowledge(ctx, p)
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("knowledge %s: read: %v", p.String(), err))
			full = false
			continue
		}
		if len(g.Nodes) > 0 {
			if err := r.graph.UpsertNodes(ctx, p, g.Nodes); err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("knowledge %s: upsert nodes: %v", p.String(), err))
				full = false
				continue
			}
		}
		if len(g.Edges) > 0 {
			if err := r.graph.UpsertEdges(ctx, p, g.Edges); err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("knowledge %s: upsert edges: %v", p.String(), err))
				full = false
				continue
			}
		}
		if verify {
			got, err := r.graph.NodeCount(ctx, p)
			if err != nil || got != len(g.Nodes) {
				rep.Failures = append(rep.Failures, fmt.Sprintf("knowledge %s: verify: graph %d != hot %d (err=%v)", p.String(), got, len(g.Nodes), err))
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

// commitManifest clears dirty flags and refreshes IndexedAt for every project
// that converged, and records the plane hydration sha when the whole plane
// converged.
func (r *Runner) commitManifest(ctx context.Context, projects []hotstore.ProjectKey, epOK, knOK map[string]bool, epFull, knFull bool) error {
	now := r.clock.Now().UTC()
	return r.store.UpdateManifest(ctx, func(m hotstore.Manifest) (hotstore.Manifest, error) {
		nm := cloneManifest(m)
		for _, p := range projects {
			if epOK[p.String()] {
				touchFile(&nm, FileKey(hotstore.PlaneEpisodic, p), now)
			}
			if knOK[p.String()] {
				touchFile(&nm, FileKey(hotstore.PlaneKnowledge, p), now)
			}
		}
		if epFull {
			nm.Indexes[IndexKeyEpisodic] = hotstore.IndexState{LastHydratedSHA: PlaneStateSHA(nm, hotstore.PlaneEpisodic)}
		}
		if knFull {
			nm.Indexes[IndexKeyKnowledge] = hotstore.IndexState{LastHydratedSHA: PlaneStateSHA(nm, hotstore.PlaneKnowledge)}
		}
		nm.UpdatedAt = now
		return nm, nil
	})
}

// touchFile marks one manifest file entry as freshly hydrated.
func touchFile(m *hotstore.Manifest, fileKey string, now time.Time) {
	fs, ok := m.Files[fileKey]
	if !ok {
		return
	}
	fs.Dirty = false
	fs.IndexedAt = now
	m.Files[fileKey] = fs
}

// RehydrateProject implements Rehydrator.
func (r *Runner) RehydrateProject(ctx context.Context, key hotstore.ProjectKey) (Report, error) {
	rep := Report{Failures: []string{}}
	epOK := map[string]bool{}
	knOK := map[string]bool{}

	if r.index == nil {
		rep.Failures = append(rep.Failures, "episodic: opensearch unavailable")
	} else if err := r.index.EnsureIndex(ctx); err != nil {
		rep.Failures = append(rep.Failures, "episodic: ensure index: "+err.Error())
	} else {
		recs, err := r.store.ListEpisodes(ctx, key)
		switch {
		case err != nil:
			rep.Failures = append(rep.Failures, fmt.Sprintf("episodic %s: list: %v", key.String(), err))
		case len(recs) == 0:
			epOK[key.String()] = true
		default:
			if err := r.index.IndexRecords(ctx, key, recs); err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("episodic %s: bulk index: %v", key.String(), err))
			} else {
				rep.EpisodesIndexed = len(recs)
				epOK[key.String()] = true
			}
		}
	}

	if r.graph == nil {
		rep.Failures = append(rep.Failures, "knowledge: neo4j unavailable")
	} else {
		g, err := r.store.ReadKnowledge(ctx, key)
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("knowledge %s: read: %v", key.String(), err))
		} else {
			nodeErr := error(nil)
			if len(g.Nodes) > 0 {
				nodeErr = r.graph.UpsertNodes(ctx, key, g.Nodes)
			}
			if nodeErr == nil && len(g.Edges) > 0 {
				nodeErr = r.graph.UpsertEdges(ctx, key, g.Edges)
			}
			if nodeErr != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("knowledge %s: upsert: %v", key.String(), nodeErr))
			} else {
				rep.NodesUpserted = len(g.Nodes)
				rep.EdgesUpserted = len(g.Edges)
				knOK[key.String()] = true
			}
		}
	}

	if err := r.commitManifest(ctx, []hotstore.ProjectKey{key}, epOK, knOK, epOK[key.String()], knOK[key.String()]); err != nil {
		rep.Failures = append(rep.Failures, "manifest update: "+err.Error())
	}
	if len(rep.Failures) > 0 {
		return rep, errors.New("rehydrate: " + strings.Join(rep.Failures, "; "))
	}
	return rep, nil
}

// StatGate implements Rehydrator.
func (r *Runner) StatGate(ctx context.Context, key hotstore.ProjectKey) error {
	now := r.clock.Now()
	r.mu.Lock()
	last, seen := r.lastGate[key.String()]
	if seen && now.Sub(last) < DebounceInterval {
		r.mu.Unlock()
		return nil
	}
	r.lastGate[key.String()] = now
	r.mu.Unlock()

	m, err := r.store.Manifest(ctx)
	if err != nil {
		return fmt.Errorf("rehydrate: stat-gate manifest: %w", err)
	}
	if !r.needsRehydration(ctx, m, key) {
		return nil
	}
	_, err = r.RehydrateProject(ctx, key)
	return err
}

// needsRehydration is the cheap freshness check: dirty flag, file mtime newer
// than the manifest IndexedAt, or a hot file the manifest has never seen.
func (r *Runner) needsRehydration(ctx context.Context, m hotstore.Manifest, key hotstore.ProjectKey) bool {
	for _, plane := range []hotstore.Plane{hotstore.PlaneEpisodic, hotstore.PlaneKnowledge} {
		fs, tracked := m.Files[FileKey(plane, key)]
		if tracked && fs.Dirty {
			return true
		}
		_, mtime, err := r.store.FileInfo(ctx, key, plane)
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
