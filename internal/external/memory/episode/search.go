// Package episodemem wraps OpenSearch for the episodic plane: nori-analyzed
// Korean full-text queries with time-range filters, plus bulk rehydration
// (architecture-v2.md §2, §5). The index is derived and disposable — it can be
// dropped and rebuilt from hot JSON at any time.
//
// Every error crossing this boundary is an *errs.Error. Failures to reach the
// cluster carry errs.KindUnavailable, which is the degraded-mode signal (§5):
// a write path reports "degraded" and still succeeds, only a read search turns
// it into 503. Nothing here knows about HTTP status codes of our own API.
package episodemem

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/opensearch-project/opensearch-go/v4"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Op names carried by the errors of each exported method (code-standards §2.1).
const (
	opNew           = "search.New"
	opPing          = "search.Ping"
	opEnsureIndex   = "search.EnsureIndex"
	opIndexRecords  = "search.IndexRecords"
	opDeleteRecords = "search.DeleteRecords"
	opDeleteProject = "search.DeleteProject"
	opSearch        = "search.Search"
	opDocCount      = "search.DocCount"
	opDrop          = "search.Drop"
)

// Entities named by those errors.
const (
	entityConfig = "config"
	entityIndex  = "index"
)

// DefaultSearchSize caps hits when Query.Size is unset.
const DefaultSearchSize = 20

// Query and Hit are the episode domain's search request/result value types
// (feature-inventory.md §4.1 rule 4). The aliases keep this package's
// historical spelling while making Client satisfy the service's Index port
// structurally — same types, no conversion.
type (
	// Query is an episodic search request (§7: q, from, to, kinds).
	Query = episode.Query
	// Hit is one search result: excerpt + metadata + score only, never a
	// full-body injection (§7).
	Hit = episode.Hit
)

// Client is the episodic search index. All methods are context-first and
// return semantic errors from internal/x/errs. Unit tests use a fake
// implementing this interface; the blackbox suite uses the real container.
type Client interface {
	// Ping reports reachability; failures carry errs.KindUnavailable.
	Ping(ctx context.Context) error
	// EnsureIndex creates the episodic index with the nori analyzer mapping
	// if absent. Idempotent.
	EnsureIndex(ctx context.Context) error
	// IndexRecords bulk-upserts records for a project. Used for both live
	// best-effort upserts and rehydration.
	IndexRecords(ctx context.Context, key projectkey.Key, recs []episode.Record) error
	// DeleteRecords removes the given episode ids (cold aging, §4).
	DeleteRecords(ctx context.Context, key projectkey.Key, ids []string) error
	// DeleteProject removes every indexed document of one project. It is the
	// project-scoped analogue of Drop, and it is what lets partial rehydration
	// converge removals: IndexRecords can only add or update, so a record that
	// left hot would stay searchable forever (§3.1 — search covers hot only).
	// A zero-value key is rejected rather than treated as "every project".
	DeleteProject(ctx context.Context, key projectkey.Key) error
	// Search runs a project-scoped query, newest first on ties.
	Search(ctx context.Context, key projectkey.Key, q Query) ([]Hit, error)
	// DocCount returns the indexed doc count for manifest drift checks (§5);
	// a zero-value key counts all projects.
	DocCount(ctx context.Context, key projectkey.Key) (int, error)
	// Drop deletes the whole index (full rehydration path: drop then bulk).
	Drop(ctx context.Context) error
}

// doer is the narrow slice of *opensearch.Client this package consumes: one
// raw round trip (code-standards §1.1). Depending on the method instead of the
// concrete client keeps the transport swappable in tests.
type doer interface {
	Do(ctx context.Context, method string, req opensearch.Request, dataPointer any) (*opensearch.Response, error)
}

// Config configures New.
type Config struct {
	// URL is the OpenSearch endpoint, e.g. "http://127.0.0.1:9200". Required.
	URL string
	// Logger receives index-repair notices. Optional: slog.Default() is used
	// when nil, so a caller that has not wired logging still gets the warning.
	Logger *slog.Logger
}

// client is the OpenSearch-backed Client.
type client struct {
	os    doer
	index string
	log   *slog.Logger

	// schemaMu guards schemaReady, the "mapping already converged" latch.
	// OpenSearch auto-creates a missing index on the first _bulk with a
	// *dynamic* mapping — no nori analyzer and id as text — so a write that
	// skips EnsureIndex silently destroys Korean morphological recall (§2)
	// and breaks sorting on id. Every write path therefore ensures first, and
	// the latch keeps that to one round trip per process.
	schemaMu    sync.Mutex
	schemaReady bool
}

// Compile-time contract check.
var _ Client = (*client)(nil)

// New returns a Client bound to cfg. It does not dial; use Ping to verify.
func New(cfg Config) (Client, error) {
	c, err := newClient(cfg, IndexName)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// newClient is New with an explicit index name, so in-package tests can target
// a scratch index without widening Config with a knob production never sets.
func newClient(cfg Config, index string) (*client, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errs.Invalid(opNew, entityConfig, "url must not be empty")
	}
	osc, err := opensearch.NewClient(opensearch.Config{Addresses: []string{cfg.URL}})
	if err != nil {
		return nil, errs.Internal(opNew, err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &client{os: osc, index: index, log: log}, nil
}
