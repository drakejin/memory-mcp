// Package config loads and validates server configuration from environment
// variables (architecture-v2.md §8). It is fully implemented by the scaffold;
// module agents must not modify it.
package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Deterministic limits from the spec. Referenced by consolidate (§3.1) and
// document (§6); keep them here so every package agrees on one value.
const (
	// DefaultListenAddr is the only allowed bind address (§7: local tool, no auth).
	DefaultListenAddr = "127.0.0.1:8420"
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
	// DefaultS3Region is the region of bucket vms-memory-mcp (§8). It is set
	// explicitly rather than inherited from the AWS profile, whose default
	// region differs — inheriting it yields PermanentRedirect on every call.
	DefaultS3Region = "ap-northeast-2"
)

// Config is the resolved runtime configuration. All fields are non-empty after
// a successful Load.
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
	home := os.Getenv("DJ_MEMORY_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("config: resolve home dir: %w", err)
		}
		home = filepath.Join(userHome, ".local", "dj-memory")
	}

	username := os.Getenv("DJ_MEMORY_USERNAME")
	if username == "" {
		u, err := user.Current()
		if err != nil {
			return Config{}, fmt.Errorf("config: resolve OS user: %w", err)
		}
		username = u.Username
	}

	ttl := DefaultEpisodicTTLDays
	if raw := os.Getenv("DJ_MEMORY_EPISODIC_TTL_DAYS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("config: DJ_MEMORY_EPISODIC_TTL_DAYS must be a positive integer, got %q", raw)
		}
		ttl = parsed
	}

	cfg := Config{
		Home:            home,
		Username:        username,
		S3Bucket:        envOr("DJ_MEMORY_S3_BUCKET", "vms-memory-mcp"),
		S3Region:        envOr("DJ_MEMORY_S3_REGION", DefaultS3Region),
		AWSProfile:      envOr("AWS_PROFILE", "vms-holdings"),
		OpenSearchURL:   envOr("DJ_MEMORY_OPENSEARCH_URL", "http://127.0.0.1:9200"),
		Neo4jURL:        envOr("DJ_MEMORY_NEO4J_URL", "bolt://127.0.0.1:7687"),
		Neo4jUser:       "neo4j",
		Neo4jPassword:   "djmemory-local",
		EpisodicTTLDays: ttl,
		ListenAddr:      envOr("DJ_MEMORY_LISTEN_ADDR", DefaultListenAddr),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces spec invariants, most importantly the loopback-only bind.
func (c Config) Validate() error {
	if c.Home == "" || c.Username == "" || c.S3Bucket == "" {
		return fmt.Errorf("config: home, username and s3 bucket must be non-empty")
	}
	if !strings.HasPrefix(c.ListenAddr, "127.0.0.1:") && !strings.HasPrefix(c.ListenAddr, "localhost:") {
		return fmt.Errorf("config: listen addr %q must bind loopback only (§7)", c.ListenAddr)
	}
	if c.EpisodicTTLDays <= 0 {
		return fmt.Errorf("config: episodic TTL days must be positive, got %d", c.EpisodicTTLDays)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
