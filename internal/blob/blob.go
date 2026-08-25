// Package blob is the content-addressed local cache for original document
// bytes at {home}/blobs/{sha256} (architecture-v2.md §1, §6). Blobs are
// cold-first: S3 is authoritative (uploaded at ingest); this cache is
// evictable and repopulated on demand from cold.
//
// A cache miss is KindNotFound, a malformed sha is KindInvalid, and a broken
// filesystem is KindInternal (code-standards §2). Callers branch with
// errors.Is(err, errs.ErrNotFound) to fall back to cold storage.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/drakejin/memory-mcp/internal/errs"
)

// Ops carried by the errors this package returns (code-standards §2.1).
const (
	opNew            = "blob.New"
	opValidateConfig = "blob.Config.Validate"
	opValidateSHA    = "blob.validateSHA"
	opPut            = "blob.Put"
	opGet            = "blob.Get"
	opHas            = "blob.Has"
	opEvict          = "blob.Evict"
)

// Entities addressed by this package's errors. Internal (filesystem) failures
// carry no entity: their op already names what broke.
const (
	entityConfig = "config"
	entityBlob   = "blob"
)

// The cache holds personal document originals, so the directory is 0700 and
// files are 0600.
const (
	dirPerm    = 0o700
	filePerm   = 0o600
	tmpPattern = ".tmp-*"
)

// invalidSHAMsg is the client-facing reason for a rejected address. It states
// the rule instead of echoing the offending value.
const invalidSHAMsg = "sha256 must be 64 lowercase hex characters"

// Client is the local blob cache. Implementations are safe for concurrent use
// and write atomically (temp+rename).
type Client interface {
	// Put streams data to the cache, returning its lowercase hex sha256 and
	// size. Re-putting an existing sha is an idempotent no-op.
	Put(ctx context.Context, data io.Reader) (sha string, size int64, err error)
	// Get opens a cached blob for reading; a miss is KindNotFound.
	Get(ctx context.Context, sha string) (io.ReadCloser, error)
	// Has reports whether the blob is cached without opening it.
	Has(ctx context.Context, sha string) (bool, error)
	// Evict removes a blob from the cache; an absent sha is a no-op (the cold
	// copy remains the backstop).
	Evict(ctx context.Context, sha string) error
}

// Config is the single construction input for the cache.
type Config struct {
	// Dir is the cache root ({home}/blobs). It is created lazily.
	Dir string
}

// Validate reports whether the config can produce a usable cache.
func (c Config) Validate() error {
	if c.Dir == "" {
		return errs.Invalid(opValidateConfig, entityConfig, "dir must be non-empty")
	}
	return nil
}

// client is the on-disk Client rooted at Config.Dir. Blobs are stored flat as
// {dir}/{sha256} (§1). No mutex is needed: every write lands on a temp file
// first and a rename onto a content address is idempotent by construction.
type client struct {
	dir string
}

// Compile-time contract check.
var _ Client = (*client)(nil)

// New returns a Client rooted at cfg.Dir. It does not touch the disk.
func New(cfg Config) (Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errs.Wrap(opNew, err)
	}
	return &client{dir: cfg.Dir}, nil
}

// shaPattern is a full lowercase-hex sha256. Validating every caller-supplied
// sha keeps path handling contained to {dir}/{sha}.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA reports whether sha is a well-formed content address: 64 lowercase
// hex characters. It is pure, so it needs no Client.
//
// Exported because this package owns the content-address format: the HTTP
// boundary rejects a malformed {sha} path parameter before any blob call, and
// it must not keep a second copy of the pattern (code-standards §4). Mirrors
// the ulid.Valid precedent.
func ValidSHA(sha string) bool {
	return shaPattern.MatchString(sha)
}

// validateSHA rejects anything that is not a lowercase hex sha256, so a sha can
// never escape the cache directory.
func validateSHA(sha string) error {
	if !ValidSHA(sha) {
		return errs.Invalid(opValidateSHA, entityBlob, invalidSHAMsg)
	}
	return nil
}

// guard rejects a dead context and a malformed address before any IO.
func guard(ctx context.Context, op, sha string) error {
	if err := errs.FromContext(ctx, op); err != nil {
		return err
	}
	return errs.Wrap(op, validateSHA(sha))
}

func (c *client) path(sha string) string {
	return filepath.Join(c.dir, sha)
}

// Put implements Client. Bytes stream through the hasher into a temp file in
// the cache directory; the temp is renamed to {sha} only after the full
// content (and therefore the address) is known, so partial writes are never
// visible under a content address.
func (c *client) Put(ctx context.Context, data io.Reader) (string, int64, error) {
	if err := errs.FromContext(ctx, opPut); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(c.dir, dirPerm); err != nil {
		return "", 0, errs.IO(opPut, c.dir, err)
	}
	tmp, err := os.CreateTemp(c.dir, tmpPattern)
	if err != nil {
		return "", 0, errs.IO(opPut, c.dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(hasher, tmp), data)
	if err != nil {
		tmp.Close()
		return "", 0, errs.IO(opPut, tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", 0, errs.IO(opPut, tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, errs.IO(opPut, tmpName, err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))

	final := c.path(sha)
	if _, err := os.Stat(final); err == nil {
		// Idempotent re-put: identical content is already addressed here.
		return sha, size, nil
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return "", 0, errs.IO(opPut, tmpName, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, errs.IO(opPut, final, err)
	}
	return sha, size, nil
}

// Get implements Client.
func (c *client) Get(ctx context.Context, sha string) (io.ReadCloser, error) {
	if err := guard(ctx, opGet, sha); err != nil {
		return nil, err
	}
	path := c.path(sha)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.NotFound(opGet, entityBlob, sha)
	}
	if err != nil {
		return nil, errs.IO(opGet, path, err)
	}
	return f, nil
}

// Has implements Client.
func (c *client) Has(ctx context.Context, sha string) (bool, error) {
	if err := guard(ctx, opHas, sha); err != nil {
		return false, err
	}
	path := c.path(sha)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, errs.IO(opHas, path, err)
	}
	return true, nil
}

// Evict implements Client.
func (c *client) Evict(ctx context.Context, sha string) error {
	if err := guard(ctx, opEvict, sha); err != nil {
		return err
	}
	path := c.path(sha)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errs.IO(opEvict, path, err)
	}
	return nil
}
