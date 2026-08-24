package cold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// Archiver is the archive-format layer above Storage: it knows the JSON batch
// formats and key layout, and is what consolidate/document/rehydrate depend
// on. Unit tests fake Storage or Archiver as convenient.
type Archiver interface {
	// ArchiveEpisodes merges recs into the project's {yyyy-mm}.json batch for
	// month (download-merge-upload so repeated runs are idempotent by record
	// id) and returns the S3 key. Callers remove from hot ONLY after this
	// returns nil (§4 step 3).
	ArchiveEpisodes(ctx context.Context, key hotstore.ProjectKey, month string, recs []episodic.Record) (s3Key string, err error)
	// FetchArchivedEpisode retrieves one archived episode by id, scanning the
	// project's monthly batches (provenance links stay resolvable, §2).
	FetchArchivedEpisode(ctx context.Context, key hotstore.ProjectKey, id string) (episodic.Record, error)
	// SnapshotKnowledge uploads g as both latest.json and a timestamped
	// snapshot, returning both keys (§4 step 4).
	SnapshotKnowledge(ctx context.Context, key hotstore.ProjectKey, g knowledge.Graph, ts time.Time) (latestKey, snapshotKey string, err error)
	// UploadBlob streams a blob to its content-addressed key (idempotent for
	// the same sha) and returns the key (§6 step 2).
	UploadBlob(ctx context.Context, sha string, r io.Reader) (s3Key string, err error)
	// FetchBlob opens a blob from cold, or ErrNotFound (§6 step 6 cache miss).
	FetchBlob(ctx context.Context, sha string) (io.ReadCloser, error)
	// BlobExists reports whether the blob is already in cold.
	BlobExists(ctx context.Context, sha string) (bool, error)
}

// S3Archiver implements Archiver over a Storage, prefixing every key with the
// configured username.
type S3Archiver struct {
	storage  Storage
	username string
}

// Compile-time contract check.
var _ Archiver = (*S3Archiver)(nil)

// NewArchiver returns an S3Archiver writing under {username}/... (§1).
func NewArchiver(storage Storage, username string) *S3Archiver {
	return &S3Archiver{storage: storage, username: username}
}

// ArchiveEpisodes implements Archiver. The batch object is a plain JSON array
// of episodic records sorted by id (ULIDs, so time-ordered). Merging is
// download-merge-upload keyed on record id, so re-running after a partial
// failure never duplicates records.
func (a *S3Archiver) ArchiveEpisodes(ctx context.Context, key hotstore.ProjectKey, month string, recs []episodic.Record) (string, error) {
	s3Key := EpisodeArchiveKey(a.username, key, month)
	if len(recs) == 0 {
		return s3Key, nil
	}

	existing, err := a.downloadBatch(ctx, s3Key)
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("cold: marshal archive %s: %w", s3Key, err)
	}
	if err := a.storage.Put(ctx, s3Key, bytes.NewReader(payload)); err != nil {
		return "", err
	}
	return s3Key, nil
}

// downloadBatch fetches and decodes a monthly batch; a missing object yields
// an empty slice, not an error.
func (a *S3Archiver) downloadBatch(ctx context.Context, s3Key string) ([]episodic.Record, error) {
	rc, err := a.storage.Get(ctx, s3Key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer rc.Close()
	var recs []episodic.Record
	if err := json.NewDecoder(rc).Decode(&recs); err != nil {
		return nil, fmt.Errorf("cold: decode archive %s: %w", s3Key, err)
	}
	return recs, nil
}

// FetchArchivedEpisode implements Archiver. It scans the project's monthly
// batches newest-first until the id is found.
func (a *S3Archiver) FetchArchivedEpisode(ctx context.Context, key hotstore.ProjectKey, id string) (episodic.Record, error) {
	prefix := EpisodeArchivePrefix(a.username, key)
	keys, err := a.storage.List(ctx, prefix)
	if err != nil {
		return episodic.Record{}, err
	}
	// Newest month first: recent episodes are the likelier lookups.
	slices.Sort(keys)
	slices.Reverse(keys)
	for _, s3Key := range keys {
		if !strings.HasSuffix(s3Key, ".json") {
			continue
		}
		recs, err := a.downloadBatch(ctx, s3Key)
		if err != nil {
			return episodic.Record{}, err
		}
		for _, rec := range recs {
			if rec.ID == id {
				return rec, nil
			}
		}
	}
	return episodic.Record{}, fmt.Errorf("cold: archived episode %s under %s: %w", id, prefix, ErrNotFound)
}

// SnapshotKnowledge implements Archiver. latest.json is written first so it is
// never older than the timestamped snapshot it accompanies.
func (a *S3Archiver) SnapshotKnowledge(ctx context.Context, key hotstore.ProjectKey, g knowledge.Graph, ts time.Time) (string, string, error) {
	payload, err := json.Marshal(g)
	if err != nil {
		return "", "", fmt.Errorf("cold: marshal knowledge snapshot for %s: %w", key.String(), err)
	}
	latestKey := KnowledgeLatestKey(a.username, key)
	snapshotKey := KnowledgeSnapshotKey(a.username, key, ts)
	if err := a.storage.Put(ctx, latestKey, bytes.NewReader(payload)); err != nil {
		return "", "", err
	}
	if err := a.storage.Put(ctx, snapshotKey, bytes.NewReader(payload)); err != nil {
		return "", "", err
	}
	return latestKey, snapshotKey, nil
}

// UploadBlob implements Archiver. Blobs are content-addressed, so an existing
// object with the same sha is already the correct bytes — skip the upload.
func (a *S3Archiver) UploadBlob(ctx context.Context, sha string, r io.Reader) (string, error) {
	s3Key := BlobKey(a.username, sha)
	exists, err := a.storage.Exists(ctx, s3Key)
	if err != nil {
		return "", err
	}
	if exists {
		return s3Key, nil
	}
	if err := a.storage.Put(ctx, s3Key, r); err != nil {
		return "", err
	}
	return s3Key, nil
}

// FetchBlob implements Archiver.
func (a *S3Archiver) FetchBlob(ctx context.Context, sha string) (io.ReadCloser, error) {
	return a.storage.Get(ctx, BlobKey(a.username, sha))
}

// BlobExists implements Archiver.
func (a *S3Archiver) BlobExists(ctx context.Context, sha string) (bool, error) {
	return a.storage.Exists(ctx, BlobKey(a.username, sha))
}
