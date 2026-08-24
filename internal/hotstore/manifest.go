package hotstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/drakejin/memory-mcp/internal/errs"
)

// FileState is the manifest entry for one hot file (§5).
type FileState struct {
	SHA256 string `json:"sha256"`
	// RecordCount is the derived-store unit count: episodic records for the
	// episodic plane, knowledge NODES (not edges) for the knowledge plane, so
	// rehydrate can compare it against OpenSearch doc-count / Neo4j node-count.
	RecordCount int       `json:"record_count"`
	IndexedAt   time.Time `json:"indexed_at"`
	// Dirty marks a best-effort derived upsert that failed; the next
	// rehydration pass converges it (§1).
	Dirty bool `json:"dirty"`
}

// IndexState is the manifest entry for one derived index (§5).
type IndexState struct {
	LastHydratedSHA string `json:"last_hydrated_sha"`
}

// Manifest mirrors manifest.json: per-file hashes plus per-index freshness.
// File keys are "{plane}/{ws}/{team}/{proj}" (see ManifestFileKey); index keys
// are "opensearch" and "neo4j".
type Manifest struct {
	Files   map[string]FileState  `json:"files"`
	Indexes map[string]IndexState `json:"indexes"`
	// UpdatedAt is when the manifest itself was last rewritten.
	UpdatedAt time.Time `json:"updated_at"`
}

// clone returns a deep copy so callers and mutation funcs never share maps.
func (m Manifest) clone() Manifest {
	out := Manifest{
		Files:     make(map[string]FileState, len(m.Files)),
		Indexes:   make(map[string]IndexState, len(m.Indexes)),
		UpdatedAt: m.UpdatedAt,
	}
	for k, v := range m.Files {
		out.Files[k] = v
	}
	for k, v := range m.Indexes {
		out.Indexes[k] = v
	}
	return out
}

// Manifest implements Client.
func (c *client) Manifest(ctx context.Context) (Manifest, error) {
	if err := errs.FromContext(ctx, opManifest); err != nil {
		return Manifest{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	m, err := c.readManifestLocked(opManifest)
	if err != nil {
		return Manifest{}, err
	}
	return m.clone(), nil
}

// UpdateManifest implements Client. An error from fn aborts the write and
// travels out with its cause intact, so callers can still match it.
func (c *client) UpdateManifest(ctx context.Context, fn func(m Manifest) (Manifest, error)) error {
	if err := errs.FromContext(ctx, opUpdateManifest); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updateManifestLocked(opUpdateManifest, fn)
}

// MarkDirty implements Client. The entry is created when absent so a dirty
// mark is never lost even if the file-state write raced a crash.
func (c *client) MarkDirty(ctx context.Context, key ProjectKey, plane Plane) error {
	if err := guard(ctx, opMarkDirty, key); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.updateManifestLocked(opMarkDirty, func(m Manifest) (Manifest, error) {
		fk := ManifestFileKey(plane, key)
		st := m.Files[fk]
		st.Dirty = true
		m.Files[fk] = st
		return m, nil
	})
}

// readManifestLocked loads manifest.json; missing file yields an empty
// manifest with non-nil maps. Caller holds c.mu.
func (c *client) readManifestLocked(op string) (Manifest, error) {
	empty := Manifest{Files: map[string]FileState{}, Indexes: map[string]IndexState{}}
	path := c.manifestPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return Manifest{}, errs.IO(op, path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		// The manifest is canonical bookkeeping: unreadable bytes are our
		// corruption, never caller input.
		return Manifest{}, errs.IO(op, path, err)
	}
	if m.Files == nil {
		m.Files = map[string]FileState{}
	}
	if m.Indexes == nil {
		m.Indexes = map[string]IndexState{}
	}
	return m, nil
}

// updateManifestLocked runs one read-modify-write cycle on the manifest.
// Caller holds c.mu.
func (c *client) updateManifestLocked(op string, fn func(m Manifest) (Manifest, error)) error {
	current, err := c.readManifestLocked(op)
	if err != nil {
		return err
	}
	next, err := fn(current.clone())
	if err != nil {
		return errs.Wrap(op, err)
	}
	if next.Files == nil {
		next.Files = map[string]FileState{}
	}
	if next.Indexes == nil {
		next.Indexes = map[string]IndexState{}
	}
	next.UpdatedAt = c.clock.Now().UTC()
	path := c.manifestPath()
	data, err := marshalCanonical(next)
	if err != nil {
		return errs.IO(op, path, err)
	}
	return writeFileAtomic(op, path, data)
}

// updateFileStateLocked refreshes the manifest entry for one project plane
// file after its bytes changed. IndexedAt and Dirty are preserved: they belong
// to the derived-store sync flow (MarkDirty / UpdateManifest). Caller holds
// c.mu.
func (c *client) updateFileStateLocked(op string, plane Plane, key ProjectKey, data []byte, count int) error {
	sum := sha256.Sum256(data)
	return c.updateManifestLocked(op, func(m Manifest) (Manifest, error) {
		fk := ManifestFileKey(plane, key)
		st := m.Files[fk]
		st.SHA256 = hex.EncodeToString(sum[:])
		st.RecordCount = count
		m.Files[fk] = st
		return m, nil
	})
}
