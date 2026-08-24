// Package search wraps OpenSearch for the episodic plane: nori-analyzed
// Korean full-text queries with time-range filters, plus bulk rehydration
// (architecture-v2.md §2, §5). The index is derived and disposable — it can be
// dropped and rebuilt from hot JSON at any time.
package search

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// ErrUnavailable signals the derived index cannot be reached. Writes to hot
// must still succeed and report degraded; only read searches surface 503 (§5).
var ErrUnavailable = errors.New("search: opensearch unavailable")

// Query is an episodic search request (§7: q, from, to, kinds).
type Query struct {
	// Text is the nori-analyzed match query; empty matches all.
	Text string
	// From/To bound occurred_at; zero values mean unbounded.
	From time.Time
	To   time.Time
	// Kinds filters record kinds; empty means all kinds.
	Kinds []episodic.Kind
	// Size caps hits (default per implementation, e.g. 20).
	Size int
}

// Hit is one search result. Per the recall principle (§7) responses carry
// excerpt + metadata + score components only — never a full-body injection.
type Hit struct {
	Record episodic.Record `json:"record"`
	Score  float64         `json:"score"`
	// Excerpt is a highlighted fragment of Text, not the full body.
	Excerpt string `json:"excerpt"`
}

// Index is the OpenSearch contract. Unit tests use a fake; the blackbox suite
// uses the real container.
type Index interface {
	// Ping reports reachability; wrap failures in ErrUnavailable.
	Ping(ctx context.Context) error
	// EnsureIndex creates the episodic index with the nori analyzer mapping
	// if absent. Idempotent.
	EnsureIndex(ctx context.Context) error
	// IndexRecords bulk-upserts records for a project. Used for both live
	// best-effort upserts and rehydration.
	IndexRecords(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error
	// DeleteRecords removes the given episode ids (cold aging, §4).
	DeleteRecords(ctx context.Context, key hotstore.ProjectKey, ids []string) error
	// Search runs a project-scoped query, newest first on ties.
	Search(ctx context.Context, key hotstore.ProjectKey, q Query) ([]Hit, error)
	// DocCount returns the indexed doc count for manifest drift checks (§5);
	// key zero-value counts all projects.
	DocCount(ctx context.Context, key hotstore.ProjectKey) (int, error)
	// Drop deletes the whole index (full rehydration path: drop then bulk).
	Drop(ctx context.Context) error
}

// Client is the real OpenSearch-backed Index. Method implementations live in
// client.go; the index mapping lives in mapping.go.
type Client struct {
	os *opensearch.Client
	// index is IndexName in production; tests may point at a scratch index.
	index string

	// schemaMu guards lazy one-time mapping convergence before the first
	// write. OpenSearch auto-creates a missing index on the first _bulk with
	// a *dynamic* mapping — no nori analyzer and id as text — so a write that
	// skips EnsureIndex silently destroys Korean morphological recall (§2)
	// and breaks sorting on id. Every write path therefore ensures first.
	schemaMu    sync.Mutex
	schemaReady bool
}

// Compile-time contract check.
var _ Index = (*Client)(nil)

// NewClient builds a Client for the given URL without dialing; connectivity is
// probed via Ping.
func NewClient(url string) (*Client, error) {
	osc, err := opensearch.NewClient(opensearch.Config{Addresses: []string{url}})
	if err != nil {
		return nil, err
	}
	return &Client{os: osc, index: IndexName}, nil
}
