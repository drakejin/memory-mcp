package cold

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// codedError stands in for a smithy API error without importing smithy-go
// (which is only an indirect dependency).
type codedError struct {
	code string
}

func (e codedError) Error() string     { return "api error " + e.code }
func (e codedError) ErrorCode() string { return e.code }

// TestIsNotFoundErr pins the classification that makes a missing monthly batch
// an empty slice instead of a hard failure (downloadBatch) and a missing blob
// an ErrNotFound the document layer can fall back on. Misclassifying either
// direction is a correctness bug, so both are covered.
func TestIsNotFoundErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "GetObject NoSuchKey", err: &types.NoSuchKey{}, want: true},
		{name: "HeadObject NotFound", err: &types.NotFound{}, want: true},
		{
			name: "wrapped NoSuchKey still classified",
			err:  fmt.Errorf("cold: get s3://b/k: %w", &types.NoSuchKey{}),
			want: true,
		},
		{
			name: "wrapped NotFound still classified",
			err:  fmt.Errorf("operation error S3: HeadObject: %w", &types.NotFound{}),
			want: true,
		},
		{name: "coded NoSuchKey", err: codedError{code: "NoSuchKey"}, want: true},
		{name: "coded NotFound", err: codedError{code: "NotFound"}, want: true},
		{name: "coded 404", err: codedError{code: "404"}, want: true},
		// Everything below must NOT be swallowed as "missing": treating an
		// auth/redirect/throttle failure as an empty batch would silently drop
		// already-archived records on the next merge-and-overwrite.
		{name: "access denied is not missing", err: codedError{code: "AccessDenied"}, want: false},
		{name: "permanent redirect is not missing", err: codedError{code: "PermanentRedirect"}, want: false},
		{name: "slow down is not missing", err: codedError{code: "SlowDown"}, want: false},
		{name: "no such bucket is not missing", err: codedError{code: "NoSuchBucket"}, want: false},
		{name: "plain error is not missing", err: errors.New("connection reset"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFoundErr(tt.err); got != tt.want {
				t.Errorf("isNotFoundErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
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
	key := hotstore.ProjectKey{Workspace: "vms", Team: "core", Project: "memory"}

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
			got:  EpisodeArchivePrefix("jin", key),
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
