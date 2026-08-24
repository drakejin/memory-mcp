package document

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
)

// Original implements Service: local cache first, cold rehydration on miss
// (§6 step 6). The rehydrated copy is re-cached for the next read.
func (s *service) Original(ctx context.Context, sha string) (io.ReadCloser, error) {
	rc, err := s.cache.Get(ctx, sha)
	if err == nil {
		return rc, nil
	}
	if !s.cacheMissed(ctx, sha) {
		return nil, errs.Wrap(opOriginal, err)
	}

	if s.archiver == nil {
		return nil, errs.NotFound(opOriginal, entityDocument, sha).
			WithField("reason", "not cached and cold storage not configured")
	}
	cool, err := s.archiver.FetchBlob(ctx, sha)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return nil, errs.NotFound(opOriginal, entityDocument, sha)
		}
		return nil, errs.Wrap(opOriginal, err)
	}

	cachedSHA, _, err := s.cache.Put(ctx, cool)
	cool.Close()
	if err != nil {
		// Cache repopulation failed; cold is authoritative, so stream from it.
		s.log.WarnContext(ctx, "document blob re-cache failed; streaming from cold",
			"sha", sha, "error", err)
		return s.fetchFromCold(ctx, sha)
	}
	if cachedSHA != sha {
		return nil, errs.Internal(opOriginal, nil).
			WithField("sha", sha).
			WithField("cold_sha", cachedSHA).
			WithField("reason", "cold object does not match its content address")
	}
	rc, err = s.cache.Get(ctx, sha)
	if err != nil {
		return nil, errs.Wrap(opOriginal, err)
	}
	return rc, nil
}

// cacheMissed reports whether the cache simply does not hold sha, as opposed to
// having failed to read it. Asking the cache keeps this package independent of
// the cache's error vocabulary.
func (s *service) cacheMissed(ctx context.Context, sha string) bool {
	has, err := s.cache.Has(ctx, sha)
	return err == nil && !has
}

// fetchFromCold streams the blob straight from cold storage.
func (s *service) fetchFromCold(ctx context.Context, sha string) (io.ReadCloser, error) {
	rc, err := s.archiver.FetchBlob(ctx, sha)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return nil, errs.NotFound(opOriginal, entityDocument, sha)
		}
		return nil, errs.Wrap(opOriginal, err)
	}
	return rc, nil
}

// Chunks implements Service: scans all projects for document_chunk episodes
// referencing sha and returns them in chunk_seq order.
func (s *service) Chunks(ctx context.Context, sha string) ([]episodic.Record, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, errs.Wrap(opChunks, err)
	}

	out := []episodic.Record{}
	for _, key := range projects {
		recs, err := s.store.ListEpisodes(ctx, key)
		if err != nil {
			return nil, errs.Wrap(opChunks, err)
		}
		for _, rec := range recs {
			if isChunkOf(rec, sha) {
				out = append(out, rec)
			}
		}
	}
	slices.SortFunc(out, func(x, y episodic.Record) int {
		if x.Refs.ChunkSeq != y.Refs.ChunkSeq {
			return x.Refs.ChunkSeq - y.Refs.ChunkSeq
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out, nil
}
