package knowledge

// This file is the service half of the package: the §7 knowledge orchestration
// the HTTP handlers used to run inline (create-with-supersede, patch, purge,
// edge create, fulltext search, neighborhood traversal), extracted behind a
// Service interface so the transport layer depends on a contract instead of on
// storage clients (feature-inventory.md §4.1 rule 3).
//
// The domain half (knowledge.go, lifecycle.go) stays pure. The service half
// does I/O, but only through the consumer-side ports declared below
// (code-standards §1.1, feature-inventory.md §4.1 rule 2): this package never
// imports internal/external/*. The graph-index port is satisfied structurally
// by the Neo4j client; the hot-store port's manifest methods are plane-less
// (naming the hotstore Plane type would need the forbidden import), so the
// composition root supplies a thin adapter that pins the knowledge plane.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
	"github.com/drakejin/memory-mcp/internal/x/ulid"
)

// Ops carried by the service-layer errors (code-standards §2.1). The domain
// ops (knowledge.Supersede, knowledge.Transition, knowledge.Purge) keep their
// own names; these name the orchestration entry points.
const (
	opNew          = "knowledge.New"
	opCreateNode   = "knowledge.CreateNode"
	opPatchNode    = "knowledge.PatchNode"
	opPurgeNode    = "knowledge.PurgeNode"
	opCreateEdge   = "knowledge.CreateEdge"
	opSearch       = "knowledge.Search"
	opNeighborhood = "knowledge.Neighborhood"
)

// entityConfig is the entity a New rejection addresses.
const entityConfig = "config"

// degradedGraph is the §5 degraded note for a failed best-effort Neo4j mirror.
// It mirrors the server's degraded vocabulary word for word, so a client reads
// one wording whether the note came from a handler or from this service.
const degradedGraph = "graph unavailable"

// Public messages preserved byte-for-byte from the transport layer the
// orchestration was extracted from: the extraction must not change a response
// body (code-standards §2.2 — the message is the rule that was broken).
const (
	msgNodeNotFound         = "node not found"
	msgEdgeEndpointNotFound = "edge endpoint node not found"
	msgPurgeNeedsConfirm    = "purge requires confirm=true (§3 — S3 versioning is the backstop)"
	msgIDMustBeULID         = "id must be a ULID"
)

// Service is the knowledge-plane contract the HTTP layer depends on (F4-F9).
// Every write follows §0 principle 1: the hot JSON commit decides success, the
// Neo4j mirror is best-effort and a failure is carried as data — the Degraded
// notes on each result — never as an error (§5).
type Service interface {
	// CreateNode creates an active node, optionally superseding others (F4):
	// mint ULID, apply the non-destructive revision to the hot graph in one
	// atomic update, then best-effort MERGE of every touched node and new edge.
	// A missing supersede target is KindNotFound.
	CreateNode(ctx context.Context, key projectkey.Key, in CreateNodeInput) (NodeResult, error)
	// PatchNode transitions node lifecycle state (F5). Legality is
	// knowledge.Transition's: an illegal move is KindConflict.
	PatchNode(ctx context.Context, key projectkey.Key, id string, in PatchNodeInput) (NodeResult, error)
	// PurgeNode removes a node from hot after the P5 gates: confirm must be
	// true (KindInvalid otherwise) and the node must sit in the
	// archived/deprecated buffer (KindConflict otherwise). Remaining chain
	// references are repaired in the same atomic update (F6), then the node is
	// best-effort deleted from Neo4j.
	PurgeNode(ctx context.Context, key projectkey.Key, id string, confirm bool) (PurgeResult, error)
	// CreateEdge inserts an edge whose endpoints exist in the hot graph (F7).
	// Re-posting the same (from,to,rel) is idempotent. A self-supersede edge
	// is KindInvalid — the P5 gate the transport-side validation missed
	// (docs/spec/05 §"경계 검증") is closed here.
	CreateEdge(ctx context.Context, key projectkey.Key, edge Edge) (EdgeResult, error)
	// Search runs fulltext over the graph index (F8); archived/deprecated
	// nodes appear only when includeArchived opts in. A dead index is
	// KindUnavailable — reads have no honest degraded answer (§5).
	Search(ctx context.Context, key projectkey.Key, q string, includeArchived bool) ([]Node, error)
	// Neighborhood traverses up to depth hops around an entity name or alias
	// (F9). depth 0 selects DefaultDepth; out of [DefaultDepth,MaxDepth] is
	// KindInvalid. Same availability rule as Search.
	Neighborhood(ctx context.Context, key projectkey.Key, entity string, depth int) (Graph, error)
}

// HotStore is the slice of the canonical JSON store this service consumes
// (code-standards §1.1). UpdateKnowledge is the read-decide-write cycle: every
// mutation runs inside its closure so a concurrent write can never be resurrected
// from a stale snapshot. MarkDirty and MarkIndexed are the §5 manifest
// bookkeeping for the knowledge plane of key — dirty when a best-effort mirror
// failed (rehydration will converge it), indexed when it succeeded (freshness
// refreshed so drift detection stays quiet). Both are plane-less on purpose:
// this port may not name the hotstore Plane type, so the composition root
// adapts them onto the store with the knowledge plane pinned.
type HotStore interface {
	UpdateKnowledge(ctx context.Context, key projectkey.Key, fn func(Graph) (Graph, error)) error
	MarkDirty(ctx context.Context, key projectkey.Key) error
	MarkIndexed(ctx context.Context, key projectkey.Key) error
}

// GraphIndex is the derived Neo4j mirror (a disposable non-persistent cache,
// architecture-v2.md §0.1). Writes are best-effort; only Search and
// Neighborhood turn its unavailability into an error. The production client
// (internal/external/memory/knowledge) satisfies it structurally.
type GraphIndex interface {
	UpsertNodes(ctx context.Context, key projectkey.Key, nodes []Node) error
	UpsertEdges(ctx context.Context, key projectkey.Key, edges []Edge) error
	DeleteNode(ctx context.Context, key projectkey.Key, id string) error
	Search(ctx context.Context, key projectkey.Key, q string, includeArchived bool) ([]Node, error)
	Neighborhood(ctx context.Context, key projectkey.Key, entity string, depth int) (Graph, error)
}

// Clock supplies wall-clock time. Injected so node timestamps and transition
// stamps are deterministic under test (code-standards §1.1 — no time.Now()).
type Clock interface {
	Now() time.Time
}

// IDGenerator mints the ULIDs of server-assigned node ids. Only the
// timestamped form is consumed: the id's time half must agree with the
// injected Clock, never with the wall clock.
type IDGenerator interface {
	GenerateAt(unixMillis int64) (string, error)
}

// Config is the single construction path for a Service.
type Config struct {
	// Store is the canonical hot JSON store. Required.
	Store HotStore
	// Graph is the Neo4j mirror. Optional: nil degrades writes to dirty marks
	// plus a degraded note, and makes Search/Neighborhood unavailable (§5).
	Graph GraphIndex
	// IDs mints node ids. Required.
	IDs IDGenerator
	// Clock stamps created/updated and transition times. Required.
	Clock Clock
	// Logger receives degraded-mode and bookkeeping reports. Optional: nil
	// uses slog.Default().
	Logger *slog.Logger
}

// service is the concrete orchestrator. It holds no mutable state, so it is
// safe for concurrent use by the HTTP handlers.
type service struct {
	store HotStore
	graph GraphIndex
	ids   IDGenerator
	clock Clock
	log   *slog.Logger
}

// Compile-time contract check.
var _ Service = (*service)(nil)

// New wires a knowledge Service from cfg. It performs no I/O.
func New(cfg Config) (Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errs.Invalid(opNew, entityConfig, "hot store must not be nil")
	case cfg.IDs == nil:
		return nil, errs.Invalid(opNew, entityConfig, "id generator must not be nil")
	case cfg.Clock == nil:
		return nil, errs.Invalid(opNew, entityConfig, "clock must not be nil")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &service{
		store: cfg.Store,
		graph: cfg.Graph,
		ids:   cfg.IDs,
		clock: cfg.Clock,
		log:   log,
	}, nil
}

// validULIDs checks every id in ids and names the offending field, mirroring
// the transport-side wording so extraction changes no response byte.
func validULIDs(op, entity, field string, ids []string) error {
	for _, id := range ids {
		if !ulid.Valid(id) {
			return errs.Invalid(op, entity, fmt.Sprintf("%s contains a malformed ULID %q", field, id))
		}
	}
	return nil
}

// notFoundMsg builds a node not-found whose public message keeps the exact
// transport wording ("node not found", "edge endpoint node not found") rather
// than the errs.NotFound default ("knowledge_node not found"): the extraction
// must not change what a client reads. Built as a literal so no error value is
// ever mutated after construction.
func notFoundMsg(op, id, msg string) *errs.Error {
	return &errs.Error{Kind: errs.KindNotFound, Op: op, Entity: EntityNode, ID: id, Msg: msg}
}
