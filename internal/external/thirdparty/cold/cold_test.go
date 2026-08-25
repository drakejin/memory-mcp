package cold

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// isolateAWSEnv points the SDK at an empty shared-config file so New is
// hermetic: no ambient profile, no credentials file, no network.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty aws config: %v", err)
	}
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func validConfig() Config {
	return Config{Bucket: "vms-memory-mcp", Region: "ap-northeast-2", Username: "jin"}
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr error
	}{
		{name: "complete config", mutate: func(*Config) {}},
		{
			name:    "missing bucket",
			mutate:  func(c *Config) { c.Bucket = "" },
			wantErr: errs.ErrInvalid,
		},
		{
			// The profile's default region differs from the bucket's; inheriting
			// it silently yields PermanentRedirect on every call (§8).
			name:    "missing region",
			mutate:  func(c *Config) { c.Region = "" },
			wantErr: errs.ErrInvalid,
		},
		{
			name:    "missing username",
			mutate:  func(c *Config) { c.Username = "" },
			wantErr: errs.ErrInvalid,
		},
		{
			name:    "unknown shared-config profile",
			mutate:  func(c *Config) { c.Profile = "no-such-profile-for-tests" },
			wantErr: errs.ErrUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateAWSEnv(t)
			cfg := validConfig()
			tt.mutate(&cfg)

			got, err := New(cfg)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if got != nil {
					t.Error("no Client may be returned alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got == nil {
				t.Fatal("New returned a nil Client")
			}
		})
	}
}

// TestNewBindsUsernameAndBucket proves New wires the config through to the key
// layout without dialling S3 (the failure below is the fake, not the network).
func TestNewBindsUsernameAndBucket(t *testing.T) {
	isolateAWSEnv(t)

	got, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := got.(*client)
	if !ok {
		t.Fatalf("New returned %T, want *client", got)
	}
	if c.username != "jin" {
		t.Errorf("username = %q, want %q", c.username, "jin")
	}
	objects, ok := c.objects.(*s3Objects)
	if !ok {
		t.Fatalf("object store is %T, want *s3Objects", c.objects)
	}
	if objects.bucket != "vms-memory-mcp" {
		t.Errorf("bucket = %q, want %q", objects.bucket, "vms-memory-mcp")
	}
}

// TestArchiveMonth pins the {yyyy-mm} batch bucket, including the UTC
// normalization that decides which batch a near-midnight episode lands in.
func TestArchiveMonth(t *testing.T) {
	tests := []struct {
		name     string
		occurred time.Time
		want     string
	}{
		{
			name:     "utc timestamp",
			occurred: time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
			want:     "2026-08",
		},
		{
			name:     "single digit month is zero padded",
			occurred: time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC),
			want:     "2026-01",
		},
		{
			name:     "non-utc zone is normalized to utc",
			occurred: time.Date(2026, 9, 1, 5, 0, 0, 0, time.FixedZone("KST", 9*3600)),
			want:     "2026-08", // 2026-09-01T05:00+09:00 == 2026-08-31T20:00Z
		},
		{
			name:     "december stays in its own year",
			occurred: time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
			want:     "2026-12",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ArchiveMonth(tt.occurred); got != tt.want {
				t.Errorf("ArchiveMonth(%s) = %q, want %q", tt.occurred, got, tt.want)
			}
		})
	}
}

// TestKeyLayout pins the §1 S3 paths every other package and the blackbox
// cleanup prefix depend on.
func TestKeyLayout(t *testing.T) {
	key := projectkey.Key{Workspace: "vms", Team: "core", Project: "memory"}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "episode archive",
			got:  EpisodeArchiveKey("jin", key, "2026-08"),
			want: "jin/episodic/vms/core/memory/2026-08.json",
		},
		{
			name: "episode archive prefix keeps trailing slash",
			got:  episodeArchivePrefix("jin", key),
			want: "jin/episodic/vms/core/memory/",
		},
		{
			name: "knowledge latest",
			got:  KnowledgeLatestKey("jin", key),
			want: "jin/knowledge/vms/core/memory/latest.json",
		},
		{
			name: "knowledge snapshot",
			got:  KnowledgeSnapshotKey("jin", key, time.Date(2026, 8, 25, 12, 30, 45, 0, time.UTC)),
			want: "jin/knowledge/vms/core/memory/snapshots/20260825T123045Z.json",
		},
		{
			name: "knowledge snapshot normalizes zone to utc",
			got:  KnowledgeSnapshotKey("jin", key, time.Date(2026, 8, 25, 21, 30, 45, 0, time.FixedZone("KST", 9*3600))),
			want: "jin/knowledge/vms/core/memory/snapshots/20260825T123045Z.json",
		},
		{
			name: "blob fans out on the first two sha chars",
			got:  BlobKey("jin", "abcdef0123456789"),
			want: "jin/blobs/ab/abcdef0123456789",
		},
		{
			name: "blob key tolerates a short sha without panicking",
			got:  BlobKey("jin", "a"),
			want: "jin/blobs/a/a",
		},
		{
			name: "blob key tolerates an empty sha without panicking",
			got:  BlobKey("jin", ""),
			want: "jin/blobs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}
