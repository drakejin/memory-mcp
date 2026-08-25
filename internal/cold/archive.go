package cold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// ArchiveEpisodes implements Client. The batch object is a plain JSON array of
// episodic records sorted by id (ULIDs, so time-ordered). Merging is
// download-merge-upload keyed on record id, so re-running after a partial
// failure never duplicates records.
func (c *client) ArchiveEpisodes(ctx context.Context, key hotstore.ProjectKey, month string, recs []episodic.Record) (string, error) {
	s3Key := EpisodeArchiveKey(c.username, key, month)
	if len(recs) == 0 {
		return s3Key, nil
	}

	existing, err := c.downloadBatch(ctx, s3Key)
	if err != nil {
		return "", errs.Wrap(opArchiveEpisodes, err)
	}

	seen := make(map[string]bool, len(existing))
	merged := make([]episodic.Record, 0, len(existing)+len(recs))
	for _, rec := range existing {
		seen[rec.ID] = true
		merged = append(merged, rec)
	}
	for _, rec := range recs {
		if seen[rec.ID] {
			continue
		}
		seen[rec.ID] = true
		merged = append(merged, rec)
	}
	slices.SortFunc(merged, func(x, y episodic.Record) int {
		return strings.Compare(x.ID, y.ID)
	})

	payload, err := json.Marshal(merged)
	if err != nil {
		return "", errs.Internal(opArchiveEpisodes, err).WithField("key", s3Key)
	}
	if err := c.objects.Put(ctx, s3Key, bytes.NewReader(payload)); err != nil {
		return "", errs.Wrap(opArchiveEpisodes, err)
	}
	return s3Key, nil
}

// downloadBatch fetches and decodes a monthly batch; a missing object yields an
// empty slice, not an error — that is what makes the first archive of a month
// and a retry after a failed put behave identically.
func (c *client) downloadBatch(ctx context.Context, s3Key string) ([]episodic.Record, error) {
	rc, err := c.objects.Get(ctx, s3Key)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer rc.Close()

	var recs []episodic.Record
	if err := json.NewDecoder(rc).Decode(&recs); err != nil {
		return nil, errs.Internal(opArchiveEpisodes, err).
			WithField("entity", entityArchive).
			WithField("key", s3Key)
	}
	return recs, nil
}

// FetchArchivedEpisode implements Client. It scans the project's monthly
// batches newest-first: recent episodes are the likelier lookups.
func (c *client) FetchArchivedEpisode(ctx context.Context, key hotstore.ProjectKey, id string) (episodic.Record, error) {
	prefix := episodeArchivePrefix(c.username, key)
	keys, err := c.objects.List(ctx, prefix)
	if err != nil {
		return episodic.Record{}, errs.Wrap(opFetchArchivedEpisode, err)
	}
	slices.Sort(keys)
	slices.Reverse(keys)

	for _, s3Key := range keys {
		if !strings.HasSuffix(s3Key, jsonExt) {
			continue
		}
		recs, err := c.downloadBatch(ctx, s3Key)
		if err != nil {
			return episodic.Record{}, errs.Wrap(opFetchArchivedEpisode, err)
		}
		for _, rec := range recs {
			if rec.ID == id {
				return rec, nil
			}
		}
	}
	return episodic.Record{}, errs.NotFound(opFetchArchivedEpisode, entityEpisode, id).
		WithField("prefix", prefix)
}

// SnapshotKnowledge implements Client. latest.json is written first so it is
// never older than the timestamped snapshot it accompanies.
func (c *client) SnapshotKnowledge(ctx context.Context, key hotstore.ProjectKey, g knowledge.Graph, ts time.Time) (string, string, error) {
	payload, err := json.Marshal(g)
	if err != nil {
		return "", "", errs.Internal(opSnapshotKnowledge, err).
			WithField("entity", entityKnowledge).
			WithField("project", key.String())
	}
	latestKey := KnowledgeLatestKey(c.username, key)
	snapshotKey := KnowledgeSnapshotKey(c.username, key, ts)
	if err := c.objects.Put(ctx, latestKey, bytes.NewReader(payload)); err != nil {
		return "", "", errs.Wrap(opSnapshotKnowledge, err)
	}
	if err := c.objects.Put(ctx, snapshotKey, bytes.NewReader(payload)); err != nil {
		return "", "", errs.Wrap(opSnapshotKnowledge, err)
	}
	return latestKey, snapshotKey, nil
}

// UploadBlob implements Client. Blobs are content-addressed, so an object that
// already exists under the same sha is already the correct bytes — skip the put
// instead of paying for a redundant upload.
func (c *client) UploadBlob(ctx context.Context, sha string, r io.Reader) (string, error) {
	s3Key := BlobKey(c.username, sha)
	exists, err := c.objects.Exists(ctx, s3Key)
	if err != nil {
		return "", errs.Wrap(opUploadBlob, err)
	}
	if exists {
		return s3Key, nil
	}
	if err := c.objects.Put(ctx, s3Key, r); err != nil {
		return "", errs.Wrap(opUploadBlob, err)
	}
	return s3Key, nil
}

// FetchBlob implements Client.
func (c *client) FetchBlob(ctx context.Context, sha string) (io.ReadCloser, error) {
	rc, err := c.objects.Get(ctx, BlobKey(c.username, sha))
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			// Re-address the miss to the blob the caller asked for; the object
			// key is an implementation detail of this package.
			return nil, errs.NotFound(opFetchBlob, entityBlob, sha)
		}
		return nil, errs.Wrap(opFetchBlob, err)
	}
	return rc, nil
}
