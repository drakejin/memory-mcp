package hotstore

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// On-disk layout under home (§1):
//
//	episodic/{ws}/{team}/{proj}.json   — JSON array of episode.Record
//	knowledge/{ws}/{team}/{proj}.json  — knowledge.Graph document
//	manifest.json                      — Manifest
//
// The store holds personal memory, so directories are 0700 and files 0600.
const (
	dirPerm  = 0o700
	filePerm = 0o600

	manifestName = "manifest.json"
	jsonExt      = ".json"
	tmpPattern   = ".tmp-*"

	// jsonIndent is the canonical encoding indent: identical content must
	// always hash to the same sha256 (manifest drift detection, §5).
	jsonIndent = "  "

	// planeSegments is the number of path segments below a plane root:
	// {ws}/{team}/{proj}.json.
	planeSegments = 3
)

func (c *client) planePath(key projectkey.Key, plane Plane) string {
	return filepath.Join(c.home, string(plane), key.Workspace, key.Team, key.Project+jsonExt)
}

func (c *client) manifestPath() string {
	return filepath.Join(c.home, manifestName)
}

// writeFileAtomic writes data via temp+rename in the target directory,
// creating parent directories as needed. fsync before rename so a crash never
// leaves a truncated canonical file.
func writeFileAtomic(op, path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return errs.IO(op, dir, err)
	}
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return errs.IO(op, dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errs.IO(op, tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errs.IO(op, tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return errs.IO(op, tmpName, err)
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return errs.IO(op, tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errs.IO(op, path, err)
	}
	return nil
}

// marshalCanonical is the single JSON encoding for hot files, so identical
// content always hashes to the same sha256.
func marshalCanonical(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", jsonIndent)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ListProjects implements Client. Keys present in either plane are returned
// once, sorted by String(). Files whose path segments would not survive
// projectkey.Key.Validate are skipped (they cannot have been written by us).
func (c *client) ListProjects(ctx context.Context) ([]projectkey.Key, error) {
	if err := errs.FromContext(ctx, opListProjects); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := map[string]projectkey.Key{}
	for _, plane := range []Plane{PlaneEpisodic, PlaneKnowledge} {
		pattern := filepath.Join(c.home, string(plane), "*", "*", "*"+jsonExt)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, errs.IO(opListProjects, pattern, err)
		}
		root := filepath.Join(c.home, string(plane))
		for _, match := range matches {
			key, ok := projectKeyFromPath(root, match)
			if !ok {
				continue
			}
			seen[key.String()] = key
		}
	}
	keys := make([]projectkey.Key, 0, len(seen))
	for _, key := range seen {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(x, y projectkey.Key) int { return strings.Compare(x.String(), y.String()) })
	return keys, nil
}

// projectKeyFromPath recovers a key from a plane-relative file path, reporting
// false for anything we could not have written.
func projectKeyFromPath(root, path string) (projectkey.Key, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return projectkey.Key{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), pathSep)
	if len(parts) != planeSegments {
		return projectkey.Key{}, false
	}
	key := projectkey.Key{
		Workspace: parts[0],
		Team:      parts[1],
		Project:   strings.TrimSuffix(parts[2], jsonExt),
	}
	if key.Validate() != nil {
		return projectkey.Key{}, false
	}
	return key, true
}

// FileInfo implements Client.
func (c *client) FileInfo(ctx context.Context, key projectkey.Key, plane Plane) (int64, time.Time, error) {
	if err := guard(ctx, opFileInfo, key); err != nil {
		return 0, time.Time{}, err
	}
	path := c.planePath(key, plane)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, time.Time{}, errs.NotFound(opFileInfo, entityHotFile, ManifestFileKey(plane, key))
	}
	if err != nil {
		return 0, time.Time{}, errs.IO(opFileInfo, path, err)
	}
	return info.Size(), info.ModTime(), nil
}
