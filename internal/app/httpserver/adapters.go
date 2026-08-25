package httpserver

// Plane adapters: the glue between the plane-agnostic bookkeeping ports the
// services declare (episode.Bookkeeper, the manifest half of
// knowledge.HotStore) and the hot store's plane-scoped manifest API. The
// services must not name a manifest plane — which plane their writes land on
// is wiring, not orchestration — so the composition root pins it here.
//
// The adapters never log: each service already reports its own bookkeeping
// failures in the §5 vocabulary, and a second line from here would double
// every incident.

import (
	"context"

	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// planeBookkeeper pins one manifest plane onto the two bookkeeping calls a
// service makes. MarkDirty flags the project file for the next rehydration
// pass (§1); MarkIndexed runs the §5 freshness rewrite — dirty cleared,
// IndexedAt refreshed, plane hydration sha updated — after a successful
// best-effort derived upsert.
type planeBookkeeper struct {
	store HotStore
	clock Clock
	plane rehydrate.Plane
}

func (b planeBookkeeper) MarkDirty(ctx context.Context, key projectkey.Key) error {
	return b.store.MarkDirty(ctx, key, b.plane)
}

func (b planeBookkeeper) MarkIndexed(ctx context.Context, key projectkey.Key) error {
	now := b.clock.Now().UTC()
	return b.store.UpdateManifest(ctx, func(m rehydrate.Manifest) (rehydrate.Manifest, error) {
		return rehydrate.MarkFileIndexed(m, b.plane, key, now), nil
	})
}

// knowledgeStore adapts the hot store onto knowledge.HotStore: the atomic
// graph update passes straight through, the manifest marks pin the knowledge
// plane.
type knowledgeStore struct {
	store HotStore
	books planeBookkeeper
}

func (a knowledgeStore) UpdateKnowledge(ctx context.Context, key projectkey.Key, fn func(knowledge.Graph) (knowledge.Graph, error)) error {
	return a.store.UpdateKnowledge(ctx, key, fn)
}

func (a knowledgeStore) MarkDirty(ctx context.Context, key projectkey.Key) error {
	return a.books.MarkDirty(ctx, key)
}

func (a knowledgeStore) MarkIndexed(ctx context.Context, key projectkey.Key) error {
	return a.books.MarkIndexed(ctx, key)
}
