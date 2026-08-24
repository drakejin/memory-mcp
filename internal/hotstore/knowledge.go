package hotstore

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// ReadKnowledge implements Client.
func (c *client) ReadKnowledge(ctx context.Context, key ProjectKey) (knowledge.Graph, error) {
	if err := guard(ctx, opReadKnowledge, key); err != nil {
		return knowledge.Graph{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readKnowledgeLocked(opReadKnowledge, key)
}

// UpdateKnowledge implements Client. Holding c.mu across the read, fn and the
// write is what makes graph mutation safe: the document is replaced wholesale,
// so two callers that each read the graph before either writes would silently
// drop one caller's node.
func (c *client) UpdateKnowledge(ctx context.Context, key ProjectKey, fn func(knowledge.Graph) (knowledge.Graph, error)) error {
	if err := guard(ctx, opUpdateKnowledge, key); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Freshly decoded from disk on every call, so fn already owns every slice
	// it receives and no copy is needed to protect a shared value.
	current, err := c.readKnowledgeLocked(opUpdateKnowledge, key)
	if err != nil {
		return err
	}
	next, err := fn(current)
	if err != nil {
		return errs.Wrap(opUpdateKnowledge, err)
	}
	return c.writeKnowledgeLocked(opUpdateKnowledge, key, next)
}

// writeKnowledgeLocked atomically replaces the project's knowledge document and
// refreshes its manifest entry. The manifest RecordCount is the NODE count so
// rehydrate can compare it against the Neo4j node count (§5). Caller holds c.mu.
func (c *client) writeKnowledgeLocked(op string, key ProjectKey, g knowledge.Graph) error {
	path := c.planePath(key, PlaneKnowledge)
	data, err := marshalCanonical(g)
	if err != nil {
		return errs.IO(op, path, err)
	}
	if err := writeFileAtomic(op, path, data); err != nil {
		return err
	}
	return c.updateFileStateLocked(op, PlaneKnowledge, key, data, len(g.Nodes))
}

// readKnowledgeLocked loads the project's knowledge document; missing file
// yields an empty Graph. Caller holds c.mu.
func (c *client) readKnowledgeLocked(op string, key ProjectKey) (knowledge.Graph, error) {
	path := c.planePath(key, PlaneKnowledge)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return knowledge.Graph{Nodes: []knowledge.Node{}, Edges: []knowledge.Edge{}}, nil
	}
	if err != nil {
		return knowledge.Graph{}, errs.IO(op, path, err)
	}
	var g knowledge.Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return knowledge.Graph{}, errs.IO(op, path, err)
	}
	return g, nil
}
