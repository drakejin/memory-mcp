// Package graph wraps Neo4j for the knowledge plane: idempotent MERGE upserts
// and traversal queries (architecture-v2.md §2, §5). The graph is derived and
// disposable — rehydration replays hot JSON through MERGE at any time.
package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// ErrUnavailable signals Neo4j cannot be reached. Hot writes still succeed and
// report degraded; only graph reads surface 503 (§5).
var ErrUnavailable = errors.New("graph: neo4j unavailable")

// Store is the Neo4j contract. Unit tests use a fake; the blackbox suite uses
// the real container. All upserts must be MERGE-idempotent so replays converge.
type Store interface {
	// Ping reports reachability; wrap failures in ErrUnavailable.
	Ping(ctx context.Context) error
	// UpsertNodes MERGEs nodes (matched on id) with all §2.2 properties,
	// scoped to the project key.
	UpsertNodes(ctx context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error
	// UpsertEdges MERGEs relationships (matched on from,to,rel).
	UpsertEdges(ctx context.Context, key hotstore.ProjectKey, edges []knowledge.Edge) error
	// DeleteNode detaches and deletes one node (purge path only, §3).
	DeleteNode(ctx context.Context, key hotstore.ProjectKey, id string) error
	// Search runs lucene full-text over name/body/aliases. Archived and
	// deprecated nodes are excluded unless includeArchived (§7 opt-in).
	Search(ctx context.Context, key hotstore.ProjectKey, q string, includeArchived bool) ([]knowledge.Node, error)
	// Neighborhood returns the subgraph within depth hops of the node whose
	// name or alias equals entity (§7 GET .../graph?entity=&depth=).
	Neighborhood(ctx context.Context, key hotstore.ProjectKey, entity string, depth int) (knowledge.Graph, error)
	// SupersedeChain walks supersedes links from the node id in both
	// directions, oldest first (blackbox scenario 3).
	SupersedeChain(ctx context.Context, key hotstore.ProjectKey, id string) ([]knowledge.Node, error)
	// NodeCount returns the stored node count for manifest drift checks (§5);
	// key zero-value counts all projects.
	NodeCount(ctx context.Context, key hotstore.ProjectKey) (int, error)
	// Clear removes all project data (disaster-recovery reindex).
	Clear(ctx context.Context) error
}

// Neo4j schema names. One label carries every knowledge node (scoped by
// ws/team/proj properties); one relationship type carries every edge with the
// §2.2 rel stored as a property so MERGE can match on (from, to, rel).
const (
	nodeLabel     = "KnowledgeNode"
	fulltextIndex = "knowledgeFulltext"
	searchLimit   = 50
)

// Client is the real Neo4j-backed Store.
type Client struct {
	driver neo4j.DriverWithContext

	// schemaMu guards lazy one-time schema creation (fulltext + id index).
	// The graph interface has no EnsureIndex hook, so schema is converged
	// lazily before the first upsert/search after each process start.
	schemaMu    sync.Mutex
	schemaReady bool
}

// Compile-time contract check.
var _ Store = (*Client)(nil)

// NewClient builds a Client for the bolt URL with the fixed local credentials
// (§8) without dialing; connectivity is probed via Ping.
func NewClient(url, user, password string) (*Client, error) {
	driver, err := neo4j.NewDriverWithContext(url, neo4j.BasicAuth(user, password, ""))
	if err != nil {
		return nil, err
	}
	return &Client{driver: driver}, nil
}

// Close releases the underlying driver.
func (c *Client) Close(ctx context.Context) error {
	return c.driver.Close(ctx)
}

// Ping implements Store.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.driver.VerifyConnectivity(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// UpsertNodes implements Store. MERGE matches on (id, ws, team, proj) and SET
// rewrites every §2.2 property, so replays from hot JSON always converge.
func (c *Client) UpsertNodes(ctx context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error {
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
	_, err := c.write(ctx, query, map[string]any{"nodes": params})
	return err
}

// UpsertEdges implements Store. MERGE matches on (from, to, rel); rows whose
// endpoints are missing are skipped (hot-first writes guarantee endpoints, so
// a skip only happens on out-of-order replays and is logged).
func (c *Client) UpsertEdges(ctx context.Context, key hotstore.ProjectKey, edges []knowledge.Edge) error {
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
MERGE (a)-[r:REL {rel: e.rel}]->(b)
SET r.provenance = e.provenance, r.confidence = e.confidence
RETURN count(r) AS merged`
	records, err := c.write(ctx, query, withKey(key, map[string]any{"edges": params}))
	if err != nil {
		return err
	}
	if merged := singleInt(records, "merged"); merged < len(edges) {
		slog.Warn("graph: some edges skipped (endpoint missing)",
			"project", key.String(), "requested", len(edges), "merged", merged)
	}
	return nil
}

// DeleteNode implements Store (purge path only, §3).
func (c *Client) DeleteNode(ctx context.Context, key hotstore.ProjectKey, id string) error {
	query := `
MATCH (n:` + nodeLabel + ` {id: $id, ws: $ws, team: $team, proj: $proj})
DETACH DELETE n`
	_, err := c.write(ctx, query, withKey(key, map[string]any{"id": id}))
	return err
}

// Search implements Store. Lucene fulltext over name/body/aliases_text;
// archived and deprecated nodes are excluded unless includeArchived (§7).
func (c *Client) Search(ctx context.Context, key hotstore.ProjectKey, q string, includeArchived bool) ([]knowledge.Node, error) {
	if err := c.ensureSchema(ctx); err != nil {
		return nil, err
	}
	query := `
CALL db.index.fulltext.queryNodes($index, $q) YIELD node, score
WHERE node.ws = $ws AND node.team = $team AND node.proj = $proj
  AND ($includeArchived OR node.state = 'active')
RETURN node ORDER BY score DESC LIMIT ` + fmt.Sprint(searchLimit)
	records, err := c.read(ctx, query, withKey(key, map[string]any{
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

// Neighborhood implements Store. The center is matched by exact name or alias;
// every path within depth hops (any direction) that stays inside the project
// is folded into a deduplicated subgraph. A missing entity wraps
// knowledge.ErrNodeNotFound.
func (c *Client) Neighborhood(ctx context.Context, key hotstore.ProjectKey, entity string, depth int) (knowledge.Graph, error) {
	// Variable-length bounds cannot be parameterized in Cypher; depth is
	// clamped to [1,maxDepth] before interpolation.
	query := fmt.Sprintf(`
MATCH (c:%[1]s {ws: $ws, team: $team, proj: $proj})
WHERE c.name = $entity OR $entity IN coalesce(c.aliases, [])
WITH c LIMIT 1
OPTIONAL MATCH p = (c)-[:REL*1..%[2]d]-(m:%[1]s)
WHERE all(x IN nodes(p) WHERE x.ws = $ws AND x.team = $team AND x.proj = $proj)
RETURN c, collect(p) AS paths`, nodeLabel, clampDepth(depth))
	records, err := c.read(ctx, query, withKey(key, map[string]any{"entity": entity}))
	if err != nil {
		return knowledge.Graph{}, err
	}
	if len(records) == 0 {
		return knowledge.Graph{}, fmt.Errorf("%w: entity %q", knowledge.ErrNodeNotFound, entity)
	}
	center, ok := recordNode(records[0], "c")
	if !ok {
		return knowledge.Graph{}, fmt.Errorf("%w: entity %q", knowledge.ErrNodeNotFound, entity)
	}
	return graphFromPaths(center, recordPaths(records[0], "paths")), nil
}

// SupersedeChain implements Store: walks supersedes links from the node in
// both directions (what it replaced, and what replaced it) and returns the
// full chain oldest first. A missing id wraps knowledge.ErrNodeNotFound.
func (c *Client) SupersedeChain(ctx context.Context, key hotstore.ProjectKey, id string) ([]knowledge.Node, error) {
	query := fmt.Sprintf(`
MATCH (n:%[1]s {id: $id, ws: $ws, team: $team, proj: $proj})
OPTIONAL MATCH (n)-[:REL*1..%[2]d {rel: 'supersedes'}]->(older:%[1]s)
WITH n, collect(DISTINCT older) AS olders
OPTIONAL MATCH (newer:%[1]s)-[:REL*1..%[2]d {rel: 'supersedes'}]->(n)
RETURN n, olders, collect(DISTINCT newer) AS newers`, nodeLabel, maxChainHops)
	records, err := c.read(ctx, query, withKey(key, map[string]any{"id": id}))
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%w: %s", knowledge.ErrNodeNotFound, id)
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

// NodeCount implements Store; a zero-value key counts every project (§5 drift
// checks compare this against the manifest).
func (c *Client) NodeCount(ctx context.Context, key hotstore.ProjectKey) (int, error) {
	query := `MATCH (n:` + nodeLabel + `) RETURN count(n) AS c`
	params := map[string]any{}
	if key != (hotstore.ProjectKey{}) {
		query = `MATCH (n:` + nodeLabel + ` {ws: $ws, team: $team, proj: $proj}) RETURN count(n) AS c`
		params = withKey(key, params)
	}
	records, err := c.read(ctx, query, params)
	if err != nil {
		return 0, err
	}
	return singleInt(records, "c"), nil
}

// Clear implements Store (disaster-recovery reindex): drops every knowledge
// node and its relationships. Schema (indexes) survives; the next rehydration
// MERGE-replays hot JSON.
func (c *Client) Clear(ctx context.Context) error {
	_, err := c.write(ctx, `MATCH (n:`+nodeLabel+`) DETACH DELETE n`, nil)
	return err
}

// ensureSchema lazily creates the id index and the lucene fulltext index over
// name/body/aliases_text. Idempotent (IF NOT EXISTS); retried on every call
// until it first succeeds.
func (c *Client) ensureSchema(ctx context.Context) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	if c.schemaReady {
		return nil
	}
	statements := []string{
		`CREATE INDEX knowledge_node_id IF NOT EXISTS FOR (n:` + nodeLabel + `) ON (n.id)`,
		`CREATE FULLTEXT INDEX ` + fulltextIndex + ` IF NOT EXISTS FOR (n:` + nodeLabel + `) ON EACH [n.name, n.body, n.aliases_text]`,
	}
	for _, stmt := range statements {
		if _, err := c.write(ctx, stmt, nil); err != nil {
			return err
		}
	}
	c.schemaReady = true
	return nil
}

// write runs one auto-commit write transaction and collects all records.
func (c *Client) write(ctx context.Context, query string, params map[string]any) ([]*neo4j.Record, error) {
	return c.run(ctx, neo4j.AccessModeWrite, query, params)
}

// read runs one read transaction and collects all records.
func (c *Client) read(ctx context.Context, query string, params map[string]any) ([]*neo4j.Record, error) {
	return c.run(ctx, neo4j.AccessModeRead, query, params)
}

func (c *Client) run(ctx context.Context, mode neo4j.AccessMode, query string, params map[string]any) ([]*neo4j.Record, error) {
	session := c.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: mode})
	defer func() {
		if err := session.Close(ctx); err != nil {
			slog.Debug("graph: session close", "error", err)
		}
	}()

	work := func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, err
		}
		return result.Collect(ctx)
	}
	var out any
	var err error
	if mode == neo4j.AccessModeRead {
		out, err = session.ExecuteRead(ctx, work)
	} else {
		out, err = session.ExecuteWrite(ctx, work)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	records, _ := out.([]*neo4j.Record)
	return records, nil
}

// mapErr wraps connectivity failures in ErrUnavailable (§5 degraded
// semantics); genuine query errors pass through untouched.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if neo4j.IsConnectivityError(err) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return err
}

// withKey merges the project-scope parameters into params and returns it.
func withKey(key hotstore.ProjectKey, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["ws"] = key.Workspace
	params["team"] = key.Team
	params["proj"] = key.Project
	return params
}

// recordNode extracts a dbtype.Node value from a record by key.
func recordNode(rec *neo4j.Record, key string) (dbtype.Node, bool) {
	v, ok := rec.Get(key)
	if !ok {
		return dbtype.Node{}, false
	}
	n, ok := v.(dbtype.Node)
	return n, ok
}

// recordNodes extracts a list of dbtype.Node values from a record by key.
func recordNodes(rec *neo4j.Record, key string) []dbtype.Node {
	v, ok := rec.Get(key)
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]dbtype.Node, 0, len(list))
	for _, item := range list {
		if n, ok := item.(dbtype.Node); ok {
			out = append(out, n)
		}
	}
	return out
}

// recordPaths extracts a list of dbtype.Path values from a record by key.
func recordPaths(rec *neo4j.Record, key string) []dbtype.Path {
	v, ok := rec.Get(key)
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]dbtype.Path, 0, len(list))
	for _, item := range list {
		if p, ok := item.(dbtype.Path); ok {
			out = append(out, p)
		}
	}
	return out
}

// singleInt reads an integer column from the first record, defaulting to 0.
func singleInt(records []*neo4j.Record, key string) int {
	if len(records) == 0 {
		return 0
	}
	v, ok := records[0].Get(key)
	if !ok {
		return 0
	}
	n, ok := v.(int64)
	if !ok {
		return 0
	}
	return int(n)
}
