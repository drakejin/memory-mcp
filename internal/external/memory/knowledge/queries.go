package knowledgemem

import (
	"context"
	"fmt"

	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// UpsertNodes implements Client. MERGE matches on (id, ws, team, proj) and SET
// rewrites every §2.2 property, so replays from hot JSON always converge.
func (c *client) UpsertNodes(ctx context.Context, key projectkey.Key, nodes []knowledge.Node) error {
	if len(nodes) == 0 {
		return nil
	}
	if err := c.ensureSchema(ctx); err != nil {
		return err
	}
	params := make([]any, 0, len(nodes))
	for _, n := range nodes {
		params = append(params, nodeProps(key, n))
	}
	query := `
UNWIND $nodes AS n
MERGE (k:` + nodeLabel + ` {id: n.id, ws: n.ws, team: n.team, proj: n.proj})
SET k.kind = n.kind, k.name = n.name, k.body = n.body,
    k.aliases = n.aliases, k.aliases_text = n.aliases_text,
    k.state = n.state, k.trust = n.trust,
    k.supersedes = n.supersedes, k.superseded_by = n.superseded_by,
    k.provenance = n.provenance,
    k.created = n.created, k.updated = n.updated, k.review_after = n.review_after`
	_, err := c.write(ctx, opUpsertNodes, query, map[string]any{"nodes": params})
	return err
}

// UpsertEdges implements Client. MERGE matches on (from, to, rel); rows whose
// endpoints are missing are skipped (hot-first writes guarantee endpoints, so
// a skip only happens on out-of-order replays and is logged).
func (c *client) UpsertEdges(ctx context.Context, key projectkey.Key, edges []knowledge.Edge) error {
	if len(edges) == 0 {
		return nil
	}
	params := make([]any, 0, len(edges))
	for _, e := range edges {
		params = append(params, edgeProps(e))
	}
	query := `
UNWIND $edges AS e
MATCH (a:` + nodeLabel + ` {id: e.from, ws: $ws, team: $team, proj: $proj})
MATCH (b:` + nodeLabel + ` {id: e.to, ws: $ws, team: $team, proj: $proj})
MERGE (a)-[r:` + relType + ` {rel: e.rel}]->(b)
SET r.provenance = e.provenance, r.confidence = e.confidence
RETURN count(r) AS merged`
	records, err := c.write(ctx, opUpsertEdges, query, withKey(key, map[string]any{"edges": params}))
	if err != nil {
		return err
	}
	if merged := singleInt(records, "merged"); merged < len(edges) {
		c.log.WarnContext(ctx, "graph edges skipped (endpoint missing)",
			"project", key.String(), "requested", len(edges), "merged", merged)
	}
	return nil
}

// DeleteNode implements Client (purge path only, §3).
func (c *client) DeleteNode(ctx context.Context, key projectkey.Key, id string) error {
	query := `
MATCH (n:` + nodeLabel + ` {id: $id, ws: $ws, team: $team, proj: $proj})
DETACH DELETE n`
	_, err := c.write(ctx, opDeleteNode, query, withKey(key, map[string]any{"id": id}))
	return err
}

// DeleteMissing implements Client. The match is scoped to the project, so the
// partial rehydration path converges one project's removals without touching
// another's derived data (§5). Relationships go with their node via DETACH, and
// no hot mutation drops an edge while keeping both endpoints, so reconciling
// nodes reconciles edges too.
func (c *client) DeleteMissing(ctx context.Context, key projectkey.Key, keep []string) error {
	// A nil list must delete every node of the project (hot holds none), not be
	// sent as Cypher null, against which `IN` is never false.
	if keep == nil {
		keep = []string{}
	}
	query := `
MATCH (n:` + nodeLabel + ` {ws: $ws, team: $team, proj: $proj})
WHERE NOT n.id IN $keep
DETACH DELETE n`
	_, err := c.write(ctx, opDeleteMissing, query, withKey(key, map[string]any{"keep": keep}))
	return err
}

// Search implements Client. Lucene fulltext over name/body/aliases_text;
// archived and deprecated nodes are excluded unless includeArchived (§7).
func (c *client) Search(ctx context.Context, key projectkey.Key, q string, includeArchived bool) ([]knowledge.Node, error) {
	if err := c.ensureSchema(ctx); err != nil {
		return nil, err
	}
	query := `
CALL db.index.fulltext.queryNodes($index, $q) YIELD node, score
WHERE node.ws = $ws AND node.team = $team AND node.proj = $proj
  AND ($includeArchived OR node.state = 'active')
RETURN node ORDER BY score DESC LIMIT ` + fmt.Sprint(searchLimit)
	records, err := c.read(ctx, opSearch, query, withKey(key, map[string]any{
		"index":           fulltextIndex,
		"q":               q,
		"includeArchived": includeArchived,
	}))
	if err != nil {
		return nil, err
	}
	nodes := make([]knowledge.Node, 0, len(records))
	for _, rec := range records {
		if n, ok := recordNode(rec, "node"); ok {
			nodes = append(nodes, propsToNode(n.Props))
		}
	}
	return nodes, nil
}

// Neighborhood implements Client. The center is matched by exact name or alias;
// every path within depth hops (any direction) that stays inside the project
// is folded into a deduplicated subgraph. A missing entity is KindNotFound.
func (c *client) Neighborhood(ctx context.Context, key projectkey.Key, entity string, depth int) (knowledge.Graph, error) {
	// Variable-length bounds cannot be parameterized in Cypher; depth is
	// clamped to [1,maxDepth] before interpolation.
	query := fmt.Sprintf(`
MATCH (c:%[1]s {ws: $ws, team: $team, proj: $proj})
WHERE c.name = $entity OR $entity IN coalesce(c.aliases, [])
WITH c LIMIT 1
OPTIONAL MATCH p = (c)-[:%[3]s*1..%[2]d]-(m:%[1]s)
WHERE all(x IN nodes(p) WHERE x.ws = $ws AND x.team = $team AND x.proj = $proj)
RETURN c, collect(p) AS paths`, nodeLabel, clampDepth(depth), relType)
	records, err := c.read(ctx, opNeighborhood, query, withKey(key, map[string]any{"entity": entity}))
	if err != nil {
		return knowledge.Graph{}, err
	}
	if len(records) == 0 {
		return knowledge.Graph{}, errs.NotFound(opNeighborhood, knowledge.EntityNode, entity)
	}
	center, ok := recordNode(records[0], "c")
	if !ok {
		return knowledge.Graph{}, errs.NotFound(opNeighborhood, knowledge.EntityNode, entity)
	}
	return graphFromPaths(center, recordPaths(records[0], "paths")), nil
}

// SupersedeChain implements Client: walks supersedes links from the node in
// both directions (what it replaced, and what replaced it) and returns the
// full chain oldest first. A missing id is KindNotFound.
func (c *client) SupersedeChain(ctx context.Context, key projectkey.Key, id string) ([]knowledge.Node, error) {
	query := fmt.Sprintf(`
MATCH (n:%[1]s {id: $id, ws: $ws, team: $team, proj: $proj})
OPTIONAL MATCH (n)-[:%[3]s*1..%[2]d {rel: '%[4]s'}]->(older:%[1]s)
WITH n, collect(DISTINCT older) AS olders
OPTIONAL MATCH (newer:%[1]s)-[:%[3]s*1..%[2]d {rel: '%[4]s'}]->(n)
RETURN n, olders, collect(DISTINCT newer) AS newers`,
		nodeLabel, maxChainHops, relType, knowledge.RelSupersedes)
	records, err := c.read(ctx, opSupersedeChain, query, withKey(key, map[string]any{"id": id}))
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errs.NotFound(opSupersedeChain, knowledge.EntityNode, id)
	}
	rec := records[0]
	chain := []knowledge.Node{}
	if n, ok := recordNode(rec, "n"); ok {
		chain = append(chain, propsToNode(n.Props))
	}
	for _, listKey := range []string{"olders", "newers"} {
		for _, n := range recordNodes(rec, listKey) {
			chain = append(chain, propsToNode(n.Props))
		}
	}
	return sortChainOldestFirst(chain), nil
}

// NodeCount implements Client; a zero-value key counts every project (§5 drift
// checks compare this against the manifest).
func (c *client) NodeCount(ctx context.Context, key projectkey.Key) (int, error) {
	query := `MATCH (n:` + nodeLabel + `) RETURN count(n) AS c`
	params := map[string]any{}
	if key != (projectkey.Key{}) {
		query = `MATCH (n:` + nodeLabel + ` {ws: $ws, team: $team, proj: $proj}) RETURN count(n) AS c`
		params = withKey(key, params)
	}
	records, err := c.read(ctx, opNodeCount, query, params)
	if err != nil {
		return 0, err
	}
	return singleInt(records, "c"), nil
}

// Clear implements Client (disaster-recovery reindex): drops every knowledge
// node and its relationships. Schema (indexes) survives; the next rehydration
// MERGE-replays hot JSON.
func (c *client) Clear(ctx context.Context) error {
	_, err := c.write(ctx, opClear, `MATCH (n:`+nodeLabel+`) DETACH DELETE n`, nil)
	return err
}
