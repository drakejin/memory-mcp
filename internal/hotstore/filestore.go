package hotstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// On-disk layout under home (§1):
//
//	episodic/{ws}/{team}/{proj}.json   — JSON array of episodic.Record
//	knowledge/{ws}/{team}/{proj}.json  — knowledge.Graph document
//	manifest.json                      — Manifest
//
// The store holds personal memory, so directories are 0700 and files 0600.
const (
	dirPerm  = 0o700
	filePerm = 0o600

	manifestName = "manifest.json"
	jsonExt      = ".json"
)

// FileStore is the on-disk Store implementation rooted at home
// (~/.local/dj-memory). It is safe for concurrent use within one process: a
// single mutex serializes every operation so read-modify-write cycles (append,
// update, manifest) never interleave.
type FileStore struct {
	home  string
	clock Clock
	mu    sync.Mutex
}

// Compile-time contract check.
var _ Store = (*FileStore)(nil)

// New returns a FileStore rooted at home. It does not touch the disk;
// directories are created lazily on first write.
func New(home string, clock Clock) *FileStore {
	return &FileStore{home: home, clock: clock}
}

func (s *FileStore) planePath(key ProjectKey, plane Plane) string {
	return filepath.Join(s.home, string(plane), key.Workspace, key.Team, key.Project+jsonExt)
}

func (s *FileStore) manifestPath() string {
	return filepath.Join(s.home, manifestName)
}

// checkCtx validates the key and honors context cancellation before any IO.
func checkCtx(ctx context.Context, key ProjectKey) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("hotstore: context: %w", err)
	}
	return key.Validate()
}

// writeFileAtomic writes data via temp+rename in the target directory,
// creating parent directories as needed. fsync before rename so a crash never
// leaves a truncated canonical file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("hotstore: create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("hotstore: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("hotstore: write temp %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("hotstore: sync temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("hotstore: close temp %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return fmt.Errorf("hotstore: chmod temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("hotstore: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// readEpisodesLocked loads the project's episodic array; missing file yields
// an empty slice. Caller holds s.mu.
func (s *FileStore) readEpisodesLocked(key ProjectKey) ([]episodic.Record, error) {
	path := s.planePath(key, PlaneEpisodic)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []episodic.Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hotstore: read %s: %w", path, err)
	}
	var recs []episodic.Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("hotstore: parse %s: %w", path, err)
	}
	return recs, nil
}

// writeEpisodesLocked atomically persists the record array and updates the
// manifest file state (sha256 + record count; IndexedAt/Dirty preserved).
// Caller holds s.mu.
func (s *FileStore) writeEpisodesLocked(key ProjectKey, recs []episodic.Record) error {
	data, err := marshalCanonical(recs)
	if err != nil {
		return fmt.Errorf("hotstore: encode episodic %s: %w", key, err)
	}
	if err := writeFileAtomic(s.planePath(key, PlaneEpisodic), data); err != nil {
		return err
	}
	return s.updateFileStateLocked(PlaneEpisodic, key, data, len(recs))
}

// readKnowledgeLocked loads the project's knowledge document; missing file
// yields an empty Graph. Caller holds s.mu.
func (s *FileStore) readKnowledgeLocked(key ProjectKey) (knowledge.Graph, error) {
	path := s.planePath(key, PlaneKnowledge)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return knowledge.Graph{Nodes: []knowledge.Node{}, Edges: []knowledge.Edge{}}, nil
	}
	if err != nil {
		return knowledge.Graph{}, fmt.Errorf("hotstore: read %s: %w", path, err)
	}
	var g knowledge.Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return knowledge.Graph{}, fmt.Errorf("hotstore: parse %s: %w", path, err)
	}
	return g, nil
}

// marshalCanonical is the single JSON encoding for hot files, so identical
// content always hashes to the same sha256.
func marshalCanonical(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// updateFileStateLocked refreshes the manifest entry for one project plane
// file after its bytes changed. IndexedAt and Dirty are preserved: they belong
// to the derived-store sync flow (MarkDirty / UpdateManifest). Caller holds
// s.mu.
func (s *FileStore) updateFileStateLocked(plane Plane, key ProjectKey, data []byte, count int) error {
	sum := sha256.Sum256(data)
	return s.updateManifestLocked(func(m Manifest) (Manifest, error) {
		fk := ManifestFileKey(plane, key)
		st := m.Files[fk]
		st.SHA256 = hex.EncodeToString(sum[:])
		st.RecordCount = count
		m.Files[fk] = st
		return m, nil
	})
}

// readManifestLocked loads manifest.json; missing file yields an empty
// manifest with non-nil maps. Caller holds s.mu.
func (s *FileStore) readManifestLocked() (Manifest, error) {
	empty := Manifest{Files: map[string]FileState{}, Indexes: map[string]IndexState{}}
	data, err := os.ReadFile(s.manifestPath())
	if os.IsNotExist(err) {
		return empty, nil
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("hotstore: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("hotstore: parse manifest: %w", err)
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
// Caller holds s.mu.
func (s *FileStore) updateManifestLocked(fn func(m Manifest) (Manifest, error)) error {
	current, err := s.readManifestLocked()
	if err != nil {
		return err
	}
	next, err := fn(current.clone())
	if err != nil {
		return err
	}
	if next.Files == nil {
		next.Files = map[string]FileState{}
	}
	if next.Indexes == nil {
		next.Indexes = map[string]IndexState{}
	}
	next.UpdatedAt = s.clock.Now().UTC()
	data, err := marshalCanonical(next)
	if err != nil {
		return fmt.Errorf("hotstore: encode manifest: %w", err)
	}
	return writeFileAtomic(s.manifestPath(), data)
}

// AppendEpisode implements Store. Duplicate ids are rejected: episodic is
// append-only and ids are the immutable provenance anchor (§2).
func (s *FileStore) AppendEpisode(ctx context.Context, key ProjectKey, rec episodic.Record) error {
	if err := checkCtx(ctx, key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	recs, err := s.readEpisodesLocked(key)
	if err != nil {
		return err
	}
	for _, existing := range recs {
		if existing.ID == rec.ID {
			return fmt.Errorf("hotstore: episode %s already exists in %s", rec.ID, key)
		}
	}
	return s.writeEpisodesLocked(key, append(recs, rec))
}

// ListEpisodes implements Store.
func (s *FileStore) ListEpisodes(ctx context.Context, key ProjectKey) ([]episodic.Record, error) {
	if err := checkCtx(ctx, key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readEpisodesLocked(key)
}

// GetEpisode implements Store.
func (s *FileStore) GetEpisode(ctx context.Context, key ProjectKey, id string) (episodic.Record, error) {
	if err := checkCtx(ctx, key); err != nil {
		return episodic.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	recs, err := s.readEpisodesLocked(key)
	if err != nil {
		return episodic.Record{}, err
	}
	for _, rec := range recs {
		if rec.ID == id {
			return rec, nil
		}
	}
	return episodic.Record{}, fmt.Errorf("hotstore: episode %s in %s: %w", id, key, ErrNotFound)
}

// UpdateEpisodes implements Store. Every id must exist (wraps ErrNotFound
// otherwise) and fn must not change record ids — ids are immutable (§2).
func (s *FileStore) UpdateEpisodes(ctx context.Context, key ProjectKey, ids []string, fn func(episodic.Record) episodic.Record) error {
	if err := checkCtx(ctx, key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	recs, err := s.readEpisodesLocked(key)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	next := make([]episodic.Record, len(recs))
	for i, rec := range recs {
		if !wanted[rec.ID] {
			next[i] = rec
			continue
		}
		replacement := fn(rec)
		if replacement.ID != rec.ID {
			return fmt.Errorf("hotstore: update fn changed id %s to %s (ids are immutable)", rec.ID, replacement.ID)
		}
		next[i] = replacement
		delete(wanted, rec.ID)
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for id := range wanted {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return fmt.Errorf("hotstore: episodes %v in %s: %w", missing, key, ErrNotFound)
	}
	return s.writeEpisodesLocked(key, next)
}

// RemoveEpisodes implements Store. Absent ids (and an absent file) are
// skipped so aging retries stay idempotent (§4).
func (s *FileStore) RemoveEpisodes(ctx context.Context, key ProjectKey, ids []string) error {
	if err := checkCtx(ctx, key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	recs, err := s.readEpisodesLocked(key)
	if err != nil {
		return err
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	next := make([]episodic.Record, 0, len(recs))
	for _, rec := range recs {
		if !drop[rec.ID] {
			next = append(next, rec)
		}
	}
	if len(next) == len(recs) {
		return nil // nothing removed — no rewrite, no manifest churn
	}
	return s.writeEpisodesLocked(key, next)
}

// ReadKnowledge implements Store.
func (s *FileStore) ReadKnowledge(ctx context.Context, key ProjectKey) (knowledge.Graph, error) {
	if err := checkCtx(ctx, key); err != nil {
		return knowledge.Graph{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readKnowledgeLocked(key)
}

// WriteKnowledge implements Store. The manifest RecordCount is the NODE count
// so rehydrate can compare it against the Neo4j node count (§5).
func (s *FileStore) WriteKnowledge(ctx context.Context, key ProjectKey, g knowledge.Graph) error {
	if err := checkCtx(ctx, key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := marshalCanonical(g)
	if err != nil {
		return fmt.Errorf("hotstore: encode knowledge %s: %w", key, err)
	}
	if err := writeFileAtomic(s.planePath(key, PlaneKnowledge), data); err != nil {
		return err
	}
	return s.updateFileStateLocked(PlaneKnowledge, key, data, len(g.Nodes))
}

// ListProjects implements Store. Keys present in either plane are returned
// once, sorted by String(). Files whose path segments would not survive
// ProjectKey.Validate are skipped (they cannot have been written by us).
func (s *FileStore) ListProjects(ctx context.Context) ([]ProjectKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("hotstore: context: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := map[string]ProjectKey{}
	for _, plane := range []Plane{PlaneEpisodic, PlaneKnowledge} {
		pattern := filepath.Join(s.home, string(plane), "*", "*", "*"+jsonExt)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("hotstore: glob %s: %w", pattern, err)
		}
		root := filepath.Join(s.home, string(plane))
		for _, match := range matches {
			rel, err := filepath.Rel(root, match)
			if err != nil {
				continue
			}
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) != 3 {
				continue
			}
			key := ProjectKey{
				Workspace: parts[0],
				Team:      parts[1],
				Project:   strings.TrimSuffix(parts[2], jsonExt),
			}
			if key.Validate() != nil {
				continue
			}
			seen[key.String()] = key
		}
	}
	keys := make([]ProjectKey, 0, len(seen))
	for _, key := range seen {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys, nil
}

// Manifest implements Store.
func (s *FileStore) Manifest(ctx context.Context) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, fmt.Errorf("hotstore: context: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	m, err := s.readManifestLocked()
	if err != nil {
		return Manifest{}, err
	}
	return m.clone(), nil
}

// UpdateManifest implements Store.
func (s *FileStore) UpdateManifest(ctx context.Context, fn func(m Manifest) (Manifest, error)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("hotstore: context: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateManifestLocked(fn)
}

// MarkDirty implements Store. The entry is created when absent so a dirty
// mark is never lost even if the file-state write raced a crash.
func (s *FileStore) MarkDirty(ctx context.Context, key ProjectKey, plane Plane) error {
	if err := checkCtx(ctx, key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateManifestLocked(func(m Manifest) (Manifest, error) {
		fk := ManifestFileKey(plane, key)
		st := m.Files[fk]
		st.Dirty = true
		m.Files[fk] = st
		return m, nil
	})
}

// FileInfo implements Store.
func (s *FileStore) FileInfo(ctx context.Context, key ProjectKey, plane Plane) (int64, time.Time, error) {
	if err := checkCtx(ctx, key); err != nil {
		return 0, time.Time{}, err
	}
	path := s.planePath(key, plane)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, time.Time{}, fmt.Errorf("hotstore: %s file for %s: %w", plane, key, ErrNotFound)
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("hotstore: stat %s: %w", path, err)
	}
	return info.Size(), info.ModTime(), nil
}
