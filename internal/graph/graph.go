// Package graph wraps Neo4j for the knowledge plane: idempotent MERGE upserts
// and traversal queries (architecture-v2.md §2, §5). The graph is derived and
// disposable — rehydration replays hot JSON through MERGE at any time.
//
// Every error crossing the package boundary is an *errs.Error: an unreachable
// database is KindUnavailable (the degraded-mode signal — hot writes still
// succeed, only read paths surface it), a missing node is KindNotFound, and an
// unclassified driver failure is KindInternal. Nothing here knows about HTTP.
package graph

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// Client is the knowledge graph store. Unit tests use a fake; the blackbox
// suite uses the real container. All upserts are MERGE-idempotent so replays
// converge.
type Client interface {
	// Ping reports reachability. Every failure is KindUnavailable.
	Ping(ctx context.Context) error
	// UpsertNodes MERGEs nodes (matched on id) with all §2.2 properties,
	// scoped to the project key.
	UpsertNodes(ctx context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error
	// UpsertEdges MERGEs relationships (matched on from,to,rel).
	UpsertEdges(ctx context.Context, key hotstore.ProjectKey, edges []knowledge.Edge) error
	// DeleteNode detaches and deletes one node (purge path only, §3).
	DeleteNode(ctx context.Context, key hotstore.ProjectKey, id string) error
	// DeleteMissing detaches and deletes every node of the project whose id is
	// absent from keep. MERGE replay can only add or update, so this is how the
	// per-project rehydration path converges a removal — a purge whose live
	// delete failed, say — without Clear wiping the other projects (§5).
	DeleteMissing(ctx context.Context, key hotstore.ProjectKey, keep []string) error
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
	// a zero-value key counts all projects.
	NodeCount(ctx context.Context, key hotstore.ProjectKey) (int, error)
	// Clear removes all project data (disaster-recovery reindex).
	Clear(ctx context.Context) error
	// Close releases the driver's pooled connections.
	Close(ctx context.Context) error
}

// Neo4j schema names. One label carries every knowledge node (scoped by
// ws/team/proj properties); one relationship type carries every edge with the
// §2.2 rel stored as a property so MERGE can match on (from, to, rel).
const (
	nodeLabel     = "KnowledgeNode"
	relType       = "REL"
	fulltextIndex = "knowledgeFulltext"
	searchLimit   = 50
)

// entityGraph names the traversal target in errors; node-scoped failures use
// knowledge.EntityNode so the two packages spell the entity identically.
const entityGraph = "knowledge_graph"

// Op names — the readable call path recorded on each error (§2.1).
const (
	opNew            = "graph.New"
	opPing           = "graph.Ping"
	opClose          = "graph.Close"
	opSchema         = "graph.ensureSchema"
	opUpsertNodes    = "graph.UpsertNodes"
	opUpsertEdges    = "graph.UpsertEdges"
	opDeleteNode     = "graph.DeleteNode"
	opDeleteMissing  = "graph.DeleteMissing"
	opSearch         = "graph.Search"
	opNeighborhood   = "graph.Neighborhood"
	opSupersedeChain = "graph.SupersedeChain"
	opNodeCount      = "graph.NodeCount"
	opClear          = "graph.Clear"
)

// Config describes the Neo4j endpoint. Credentials are the fixed local ones
// (§8): the whole server binds to 127.0.0.1.
type Config struct {
	// URL is the bolt endpoint, e.g. bolt://127.0.0.1:7687.
	URL string
	// User and Password authenticate the bolt session.
	User     string
	Password string
	// Logger receives degraded-path warnings; nil falls back to slog.Default.
	Logger *slog.Logger
}

// client is the real Neo4j-backed Client.
type client struct {
	db  runner
	log *slog.Logger

	// schemaMu guards schemaReady: it makes the lazy DDL below run at most
	// once per process, so concurrent upserts cannot race index creation.
	// The Client interface has no EnsureIndex hook, so schema is converged
	// lazily before the first upsert/search after each process start.
	schemaMu    sync.Mutex
	schemaReady bool
}

// Compile-time contract check.
var _ Client = (*client)(nil)

// New returns a Client bound to cfg. It does not dial; use Ping to verify.
func New(cfg Config) (Client, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errs.Invalid(opNew, entityGraph, "neo4j url must not be empty")
	}
	driver, err := neo4j.NewDriverWithContext(cfg.URL, neo4j.BasicAuth(cfg.User, cfg.Password, ""))
	if err != nil {
		return nil, errs.Internal(opNew, err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &client{db: &boltRunner{driver: driver, log: log}, log: log}, nil
}

// Ping implements Client. Any verification failure means the derived store is
// unreachable, which is exactly the degraded-mode signal (§5).
func (c *client) Ping(ctx context.Context) error {
	if err := c.db.Verify(ctx); err != nil {
		return errs.Unavailable(opPing, err)
	}
	return nil
}

// Close implements Client.
func (c *client) Close(ctx context.Context) error {
	return errs.Wrap(opClose, c.db.Close(ctx))
}

// ensureSchema lazily creates the id index and the lucene fulltext index over
// name/body/aliases_text. Idempotent (IF NOT EXISTS); retried on every call
// until it first succeeds.
func (c *client) ensureSchema(ctx context.Context) error {
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
		if _, err := c.write(ctx, opSchema, stmt, nil); err != nil {
			return err
		}
	}
	c.schemaReady = true
	return nil
}

// write runs one write transaction and collects all records.
func (c *client) write(ctx context.Context, op, query string, params map[string]any) ([]*neo4j.Record, error) {
	records, err := c.db.Run(ctx, true, query, params)
	if err != nil {
		return nil, mapErr(op, err)
	}
	return records, nil
}

// read runs one read transaction and collects all records.
func (c *client) read(ctx context.Context, op, query string, params map[string]any) ([]*neo4j.Record, error) {
	records, err := c.db.Run(ctx, false, query, params)
	if err != nil {
		return nil, mapErr(op, err)
	}
	return records, nil
}
