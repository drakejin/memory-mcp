package document

// The cold-store ops surface of Service: presence and reachability of the
// archive this pipeline is cold-first against (P12). /v1/status reads both
// through here so the transport layer never touches the archiver itself.

import (
	"context"
	"errors"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// probeSHA is sha256("") — a cheap, always-valid probe key for cold-store
// reachability.
const probeSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// ColdConfigured implements Service.
func (s *service) ColdConfigured() bool {
	return s.archiver != nil
}

// ColdReachable implements Service.
//
// It deliberately uses a GET (FetchBlob) rather than a HEAD: S3 answers
// HeadObject against a *non-existent bucket* with a bare 404 that is
// byte-for-byte indistinguishable from a missing key, so a HEAD-based probe
// reports a bucket that does not exist as "reachable". GetObject returns
// NoSuchBucket, so only a hit or an explicit not-found proves the bucket is
// really there.
func (s *service) ColdReachable(ctx context.Context) bool {
	if s.archiver == nil {
		return false
	}
	body, err := s.archiver.FetchBlob(ctx, probeSHA)
	if err == nil {
		body.Close()
		return true
	}
	if errors.Is(err, errs.ErrNotFound) {
		// Bucket answered; the probe key simply is not stored.
		return true
	}
	s.log.Warn("cold store unreachable", "bucket", s.bucket, "error", err)
	return false
}
