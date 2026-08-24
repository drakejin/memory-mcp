// Package blob is the content-addressed local cache for original document
// bytes at {home}/blobs/{sha256} (architecture-v2.md §1, §6). Blobs are
// cold-first: S3 is authoritative (uploaded at ingest); this cache is
// evictable and repopulated on demand from cold.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// ErrNotCached is returned when a blob is absent locally; callers fall back to
// cold storage (§6 step 6).
var ErrNotCached = errors.New("blob: not in local cache")

// Cache is the local blob cache contract. Implementations must be safe for
// concurrent use and must write atomically (temp+rename).
type Cache interface {
	// Put streams data to the cache, returning its lowercase hex sha256 and
	// size. Re-putting an existing sha is an idempotent no-op.
	Put(ctx context.Context, data io.Reader) (sha string, size int64, err error)
	// Get opens a cached blob for reading, or ErrNotCached.
	Get(ctx context.Context, sha string) (io.ReadCloser, error)
	// Has reports whether the blob is cached without opening it.
	Has(ctx context.Context, sha string) (bool, error)
	// Evict removes a blob from the cache; absent sha is a no-op (cold copy
	// remains the backstop).
	Evict(ctx context.Context, sha string) error
}

const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// shaPattern is a full lowercase-hex sha256. Validating every caller-supplied
// sha keeps path handling contained to {dir}/{sha}.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validSHA rejects anything that is not a lowercase hex sha256, so a sha can
// never escape the cache directory.
func validSHA(sha string) error {
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("blob: invalid sha256 %q", sha)
	}
	return nil
}

// FileCache is the on-disk Cache rooted at dir ({home}/blobs). Blobs are
// stored flat as {dir}/{sha256} (§1).
type FileCache struct {
	dir string
}

// Compile-time contract check.
var _ Cache = (*FileCache)(nil)

// New returns a FileCache rooted at dir; the directory is created lazily.
func New(dir string) *FileCache {
	return &FileCache{dir: dir}
}

func (c *FileCache) path(sha string) string {
	return filepath.Join(c.dir, sha)
}

// Put implements Cache. Bytes stream through the hasher into a temp file in
// the cache directory; the temp is renamed to {sha} only after the full
// content (and therefore the address) is known, so partial writes are never
// visible under a content address.
func (c *FileCache) Put(ctx context.Context, data io.Reader) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, fmt.Errorf("blob: context: %w", err)
	}
	if err := os.MkdirAll(c.dir, dirPerm); err != nil {
		return "", 0, fmt.Errorf("blob: create dir %s: %w", c.dir, err)
	}
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return "", 0, fmt.Errorf("blob: create temp in %s: %w", c.dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(hasher, tmp), data)
	if err != nil {
		tmp.Close()
		return "", 0, fmt.Errorf("blob: stream to temp %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", 0, fmt.Errorf("blob: sync temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("blob: close temp %s: %w", tmpName, err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))

	final := c.path(sha)
	if _, err := os.Stat(final); err == nil {
		// Idempotent re-put: identical content is already addressed here.
		return sha, size, nil
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return "", 0, fmt.Errorf("blob: chmod temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, fmt.Errorf("blob: rename %s -> %s: %w", tmpName, final, err)
	}
	return sha, size, nil
}

// Get implements Cache.
func (c *FileCache) Get(ctx context.Context, sha string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("blob: context: %w", err)
	}
	if err := validSHA(sha); err != nil {
		return nil, err
	}
	f, err := os.Open(c.path(sha))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("blob: %s: %w", sha, ErrNotCached)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: open %s: %w", sha, err)
	}
	return f, nil
}

// Has implements Cache.
func (c *FileCache) Has(ctx context.Context, sha string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("blob: context: %w", err)
	}
	if err := validSHA(sha); err != nil {
		return false, err
	}
	_, err := os.Stat(c.path(sha))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("blob: stat %s: %w", sha, err)
	}
	return true, nil
}

// Evict implements Cache.
func (c *FileCache) Evict(ctx context.Context, sha string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("blob: context: %w", err)
	}
	if err := validSHA(sha); err != nil {
		return err
	}
	if err := os.Remove(c.path(sha)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blob: evict %s: %w", sha, err)
	}
	return nil
}
