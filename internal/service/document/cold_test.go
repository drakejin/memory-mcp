package document

// Tests for the cold-store ops surface (ColdConfigured / ColdReachable) and
// the content-address format this package owns (ValidSHA).

import (
	"context"
	"strings"
	"testing"
)

func TestColdConfigured(t *testing.T) {
	t.Run("wired archiver", func(t *testing.T) {
		r := newRig(t)
		if !r.svc.ColdConfigured() {
			t.Error("ColdConfigured() = false with a wired archiver")
		}
	})
	t.Run("no archiver", func(t *testing.T) {
		r := newRig(t, func(c *Config) { c.Archiver = nil })
		if r.svc.ColdConfigured() {
			t.Error("ColdConfigured() = true with no archiver — §6 step 2 cannot hold")
		}
	})
}

// TestColdReachable pins the §0-principle-3 honesty probe. It must be a GET
// (FetchBlob): S3 answers HeadObject against a non-existent bucket with a bare
// 404 indistinguishable from a missing key, so only a hit or an explicit
// not-found proves the bucket exists.
func TestColdReachable(t *testing.T) {
	ctx := context.Background()

	t.Run("bucket live, probe key absent", func(t *testing.T) {
		// The fake answers not-found for any sha it does not hold.
		r := newRig(t)
		if !r.svc.ColdReachable(ctx) {
			t.Error("a live bucket without the probe key must read as reachable")
		}
		if r.archiver.fetches != 1 {
			t.Errorf("fetches = %d, want exactly one GET probe", r.archiver.fetches)
		}
	})

	t.Run("probe key present", func(t *testing.T) {
		r := newRig(t)
		r.archiver.blobs[probeSHA] = []byte{}
		if !r.svc.ColdReachable(ctx) {
			t.Error("a probe hit must read as reachable")
		}
	})

	t.Run("bucket-level failure", func(t *testing.T) {
		r := newRig(t)
		r.archiver.failFetch = true
		if r.svc.ColdReachable(ctx) {
			t.Error("a failing bucket must read as unreachable")
		}
		if !strings.Contains(r.logs.String(), "cold store unreachable") {
			t.Errorf("unreachability must be logged; got %s", r.logs.String())
		}
	})

	t.Run("no archiver", func(t *testing.T) {
		r := newRig(t, func(c *Config) { c.Archiver = nil })
		if r.svc.ColdReachable(ctx) {
			t.Error("an unconfigured archiver is not reachable")
		}
	})
}

func TestValidSHA(t *testing.T) {
	tests := []struct {
		name string
		sha  string
		want bool
	}{
		{"well-formed", probeSHA, true},
		{"empty", "", false},
		{"too short", "abc123", false},
		{"uppercase hex", strings.ToUpper(probeSHA), false},
		{"non-hex characters", strings.Repeat("z", 64), false},
		{"too long", probeSHA + "aa", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidSHA(tt.sha); got != tt.want {
				t.Errorf("ValidSHA(%q) = %v, want %v", tt.sha, got, tt.want)
			}
		})
	}
}
