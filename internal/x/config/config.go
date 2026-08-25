// Package config loads and validates server configuration from environment
// variables (architecture-v2.md §8) and owns the deterministic limits the rest
// of the system shares.
//
// Loading never touches the network and never panics: a malformed value is a
// KindInvalid *errs.Error, an unresolvable OS identity is KindInternal
// (code-standards §2.1).
package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// Ops carried by this package's errors; they read as a path through the
// system rather than a stack (code-standards §2.1).
const (
	opLoad     = "config.Load"
	opValidate = "config.Config.Validate"
)

// entityConfig is the single entity these errors address.
const entityConfig = "config"

// Environment variables read by Load (§8).
const (
	envHome            = "DJ_MEMORY_HOME"
	envUsername        = "DJ_MEMORY_USERNAME"
	envS3Bucket        = "DJ_MEMORY_S3_BUCKET"
	envS3Region        = "DJ_MEMORY_S3_REGION"
	envAWSProfile      = "AWS_PROFILE"
	envOpenSearchURL   = "DJ_MEMORY_OPENSEARCH_URL"
	envNeo4jURL        = "DJ_MEMORY_NEO4J_URL"
	envEpisodicTTLDays = "DJ_MEMORY_EPISODIC_TTL_DAYS"
	envListenAddr      = "DJ_MEMORY_LISTEN_ADDR"
)

// Deterministic limits from the spec. Referenced by consolidate (§3.1) and
// document (§6); they live here so every package agrees on one value.
const (
	// DefaultEpisodicTTLDays: consolidated episodes older than this sink to cold (§3.1).
	DefaultEpisodicTTLDays = 30
	// MaxProjectFileBytes: episodic pressure threshold per project file (§3.1).
	MaxProjectFileBytes = 5 << 20
	// MaxProjectRecords: episodic pressure threshold per project file (§3.1).
	MaxProjectRecords = 5000
	// DocumentChunkBytes: target chunk size for document_chunk episodes (§6).
	DocumentChunkBytes = 2048
	// MaxDocumentChunks: hard cap; overflow is truncated and reported honestly (§6).
	MaxDocumentChunks = 500
)

// Spec defaults applied when the matching environment variable is unset (§8).
const (
	// DefaultListenAddr is the only allowed bind address (§7: local tool, no auth).
	DefaultListenAddr = "127.0.0.1:8420"
	// DefaultS3Bucket is the cold-store bucket (§8).
	DefaultS3Bucket = "vms-memory-mcp"
	// DefaultS3Region is the region of bucket vms-memory-mcp (§8). It is set
	// explicitly rather than inherited from the AWS profile, whose default
	// region differs — inheriting it yields PermanentRedirect on every call.
	DefaultS3Region = "ap-northeast-2"
	// DefaultAWSProfile is the shared-config profile owning the bucket (§8).
	DefaultAWSProfile = "vms-holdings"
	// DefaultOpenSearchURL / DefaultNeo4jURL address the compose stack (§8).
	DefaultOpenSearchURL = "http://127.0.0.1:9200"
	DefaultNeo4jURL      = "bolt://127.0.0.1:7687"
	// DefaultNeo4jUser / DefaultNeo4jPassword are the fixed local credentials
	// of the volume-less container (§8). They are not secrets: the graph is a
	// disposable derivative bound to loopback, and the spec pins them so a
	// rebuilt container is reachable without configuration.
	DefaultNeo4jUser     = "neo4j"
	DefaultNeo4jPassword = "djmemory-local"
)

// Hot-store root layout: ~/{homeParentDir}/{homeDirName} (§1).
const (
	homeParentDir = ".local"
	homeDirName   = "dj-memory"
)

// Loopback prefixes accepted by Validate (§7: the server must never bind a
// routable interface).
const (
	loopbackIPPrefix   = "127.0.0.1:"
	loopbackHostPrefix = "localhost:"
)

// Config is the resolved runtime configuration. All fields are non-empty after
// a successful Load.
//
// It carries no json tags on purpose: the struct holds the Neo4j password and
// is never serialized into a response body or a log line.
type Config struct {
	// Home is the hot-store root, default ~/.local/dj-memory (env DJ_MEMORY_HOME).
	Home string
	// Username prefixes every S3 key, default OS user (env DJ_MEMORY_USERNAME).
	Username string
	// S3Bucket is the cold-store bucket (env DJ_MEMORY_S3_BUCKET).
	S3Bucket string
	// S3Region is the bucket region (env DJ_MEMORY_S3_REGION).
	S3Region string
	// AWSProfile is the shared-config profile used for S3 (env AWS_PROFILE).
	AWSProfile string
	// OpenSearchURL is the episodic index endpoint (env DJ_MEMORY_OPENSEARCH_URL).
	OpenSearchURL string
	// Neo4jURL is the bolt endpoint (env DJ_MEMORY_NEO4J_URL).
	Neo4jURL string
	// Neo4jUser / Neo4jPassword are the fixed local credentials (§8).
	Neo4jUser     string
	Neo4jPassword string
	// EpisodicTTLDays overrides DefaultEpisodicTTLDays (env DJ_MEMORY_EPISODIC_TTL_DAYS).
	EpisodicTTLDays int
	// ListenAddr is the HTTP bind address; must stay loopback (§7).
	ListenAddr string
}

// Load reads environment variables, applies spec defaults, and validates the
// result. It never touches the network.
func Load() (Config, error) {
	home, err := resolveHome()
	if err != nil {
		return Config{}, err
	}
	username, err := resolveUsername()
	if err != nil {
		return Config{}, err
	}
	ttl, err := resolveTTLDays()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Home:            home,
		Username:        username,
		S3Bucket:        envOr(envS3Bucket, DefaultS3Bucket),
		S3Region:        envOr(envS3Region, DefaultS3Region),
		AWSProfile:      envOr(envAWSProfile, DefaultAWSProfile),
		OpenSearchURL:   envOr(envOpenSearchURL, DefaultOpenSearchURL),
		Neo4jURL:        envOr(envNeo4jURL, DefaultNeo4jURL),
		Neo4jUser:       DefaultNeo4jUser,
		Neo4jPassword:   DefaultNeo4jPassword,
		EpisodicTTLDays: ttl,
		ListenAddr:      envOr(envListenAddr, DefaultListenAddr),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, errs.Wrap(opLoad, err)
	}
	return cfg, nil
}

// Validate enforces spec invariants, most importantly the loopback-only bind.
func (c Config) Validate() error {
	switch {
	case c.Home == "":
		return errs.Invalid(opValidate, entityConfig, "home must be non-empty")
	case c.Username == "":
		return errs.Invalid(opValidate, entityConfig, "username must be non-empty")
	case c.S3Bucket == "":
		return errs.Invalid(opValidate, entityConfig, "s3 bucket must be non-empty")
	}
	if !isLoopback(c.ListenAddr) {
		return errs.Invalid(opValidate, entityConfig, "listen addr must bind loopback only").
			WithField("listen_addr", c.ListenAddr)
	}
	if c.EpisodicTTLDays <= 0 {
		return errs.Invalid(opValidate, entityConfig, "episodic ttl days must be positive").
			WithField("episodic_ttl_days", c.EpisodicTTLDays)
	}
	return nil
}

// resolveHome returns DJ_MEMORY_HOME or the spec default under the OS home.
func resolveHome() (string, error) {
	if home := os.Getenv(envHome); home != "" {
		return home, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", errs.Internal(opLoad, err).WithField("env", envHome)
	}
	return filepath.Join(userHome, homeParentDir, homeDirName), nil
}

// resolveUsername returns DJ_MEMORY_USERNAME or the OS user name; it prefixes
// every cold key (§1).
func resolveUsername() (string, error) {
	if username := os.Getenv(envUsername); username != "" {
		return username, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", errs.Internal(opLoad, err).WithField("env", envUsername)
	}
	return u.Username, nil
}

// resolveTTLDays parses the episodic TTL override, defaulting per §3.1.
func resolveTTLDays() (int, error) {
	raw := os.Getenv(envEpisodicTTLDays)
	if raw == "" {
		return DefaultEpisodicTTLDays, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return 0, errs.Invalid(opLoad, entityConfig, envEpisodicTTLDays+" must be a positive integer").
			WithField("value", raw)
	}
	return parsed, nil
}

// isLoopback reports whether addr binds a loopback interface (§7).
func isLoopback(addr string) bool {
	return strings.HasPrefix(addr, loopbackIPPrefix) || strings.HasPrefix(addr, loopbackHostPrefix)
}

// envOr returns the environment value for key, or fallback when unset/empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
