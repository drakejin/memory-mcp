// Package cold owns S3 archival: aged episodic batches, knowledge snapshots,
// and blob upload/restore (architecture-v2.md §1, §3, §4). The iron rule of
// aging: the S3 put must be confirmed BEFORE anything is removed from hot.
//
// Every error leaving this package is an *errs.Error (code-standards §2.1): a
// missing object is KindNotFound, an S3 failure is KindUnavailable — the signal
// callers turn into degraded-mode reporting — and unreadable archive content is
// KindInternal. Nothing here knows about HTTP.
package cold

import (
	"context"
	"io"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Ops carried by the errors of this package. They read as a call path, so a log
// line names the operation without a stack trace.
const (
	opNew                  = "cold.New"
	opArchiveEpisodes      = "cold.ArchiveEpisodes"
	opFetchArchivedEpisode = "cold.FetchArchivedEpisode"
	opSnapshotKnowledge    = "cold.SnapshotKnowledge"
	opUploadBlob           = "cold.UploadBlob"
	opFetchBlob            = "cold.FetchBlob"
	opPut                  = "cold.objects.Put"
	opGet                  = "cold.objects.Get"
	opExists               = "cold.objects.Exists"
	opList                 = "cold.objects.List"
)

// Entities carried by the errors of this package.
const (
	entityConfig    = "config"
	entityEpisode   = "episode"
	entityArchive   = "episode_archive"
	entityKnowledge = "knowledge_snapshot"
	entityBlob      = "blob"
	entityObject    = "object"
)

// Client is the cold archive. Every method is context-first and returns
// semantic errors from internal/x/errs.
type Client interface {
	// ArchiveEpisodes merges recs into the project's {yyyy-mm}.json batch for
	// month (download-merge-upload, so repeated runs are idempotent by record
	// id) and returns the S3 key. Callers remove records from hot ONLY after
	// this returns nil (§4 step 3).
	ArchiveEpisodes(ctx context.Context, key projectkey.Key, month string, recs []episode.Record) (string, error)
	// FetchArchivedEpisode retrieves one archived episode by id, scanning the
	// project's monthly batches so provenance links stay resolvable (§2).
	FetchArchivedEpisode(ctx context.Context, key projectkey.Key, id string) (episode.Record, error)
	// SnapshotKnowledge uploads g as both latest.json and a timestamped
	// snapshot, returning both keys (§4 step 4).
	SnapshotKnowledge(ctx context.Context, key projectkey.Key, g knowledge.Graph, ts time.Time) (string, string, error)
	// UploadBlob streams a blob to its content-addressed key (idempotent for
	// the same sha) and returns the key (§6 step 2).
	UploadBlob(ctx context.Context, sha string, r io.Reader) (string, error)
	// FetchBlob opens a blob from cold; a missing blob is KindNotFound
	// (§6 step 6 cache miss).
	FetchBlob(ctx context.Context, sha string) (io.ReadCloser, error)
}

// objectStore is the raw object IO this package consumes: bucket-relative keys
// (no leading slash), no knowledge of the archive formats above it. The S3
// implementation lives in s3.go; unit tests substitute an in-memory one.
type objectStore interface {
	// Put uploads r to key, overwriting; bucket versioning is the backstop.
	Put(ctx context.Context, key string, r io.Reader) error
	// Get opens the object at key, or KindNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Exists reports whether key exists without downloading it.
	Exists(ctx context.Context, key string) (bool, error)
	// List returns every key under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
}

// Config is the single construction path for a Client.
type Config struct {
	// Bucket is the cold bucket (vms-memory-mcp).
	Bucket string
	// Region must be set explicitly: the AWS profile's default region differs
	// from the bucket's and inheriting it yields PermanentRedirect (§8).
	Region string
	// Profile is the shared-config profile; empty means the SDK default chain.
	Profile string
	// Username prefixes every key written by this client (§1).
	Username string
}

// client is the archive-format layer over an objectStore. It is stateless
// beyond its dependencies, so it is safe for concurrent use.
type client struct {
	objects  objectStore
	username string
}

// Compile-time contract check.
var _ Client = (*client)(nil)

// New returns a Client bound to cfg. It resolves the AWS shared config — local
// files only, no network call and no bucket check — so a missing profile is
// reported at wiring time; reachability is probed separately by the caller.
func New(cfg Config) (Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.Profile != "" {
		options = append(options, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}
	// LoadDefaultConfig only parses ~/.aws/{config,credentials}; the context is
	// demanded by the SDK signature and carries no network deadline here.
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), options...)
	if err != nil {
		return nil, errs.Unavailable(opNew, err)
	}
	api := s3.NewFromConfig(awsCfg)
	objects := &s3Objects{api: api, uploader: manager.NewUploader(api), bucket: cfg.Bucket}
	return newClient(objects, cfg.Username), nil
}

// newClient is the seam New and the unit tests share: the object store arrives
// as a dependency, so the archive formats are exercised without touching S3.
func newClient(objects objectStore, username string) *client {
	return &client{objects: objects, username: username}
}

// validate enforces the fields without which a key cannot be assembled.
func (c Config) validate() error {
	switch {
	case c.Bucket == "":
		return errs.Invalid(opNew, entityConfig, "s3 bucket must not be empty")
	case c.Region == "":
		return errs.Invalid(opNew, entityConfig, "s3 region must be set explicitly")
	case c.Username == "":
		return errs.Invalid(opNew, entityConfig, "username must not be empty")
	}
	return nil
}
