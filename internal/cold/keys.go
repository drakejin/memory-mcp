package cold

import (
	"path"
	"time"

	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// S3 key layout (§1). These helpers are the single source of truth for cold
// paths; fully implemented by the scaffold — do not duplicate elsewhere.

// EpisodeArchiveKey returns
// {username}/episodic/{ws}/{team}/{proj}/{yyyy-mm}.json.
func EpisodeArchiveKey(username string, k hotstore.ProjectKey, month string) string {
	return path.Join(username, "episodic", k.Workspace, k.Team, k.Project, month+".json")
}

// EpisodeArchivePrefix returns the listing prefix for a project's monthly
// batches: {username}/episodic/{ws}/{team}/{proj}/. (Additive helper: needed
// by FetchArchivedEpisode to scan batches.)
func EpisodeArchivePrefix(username string, k hotstore.ProjectKey) string {
	return path.Join(username, "episodic", k.Workspace, k.Team, k.Project) + "/"
}

// KnowledgeLatestKey returns
// {username}/knowledge/{ws}/{team}/{proj}/latest.json.
func KnowledgeLatestKey(username string, k hotstore.ProjectKey) string {
	return path.Join(username, "knowledge", k.Workspace, k.Team, k.Project, "latest.json")
}

// KnowledgeSnapshotKey returns
// {username}/knowledge/{ws}/{team}/{proj}/snapshots/{ts}.json with ts in UTC
// RFC3339 compact form (20060102T150405Z).
func KnowledgeSnapshotKey(username string, k hotstore.ProjectKey, ts time.Time) string {
	stamp := ts.UTC().Format("20060102T150405Z")
	return path.Join(username, "knowledge", k.Workspace, k.Team, k.Project, "snapshots", stamp+".json")
}

// BlobKey returns {username}/blobs/{sha[:2]}/{sha}.
func BlobKey(username, sha string) string {
	prefix := sha
	if len(sha) >= 2 {
		prefix = sha[:2]
	}
	return path.Join(username, "blobs", prefix, sha)
}

// ArchiveMonth formats occurredAt as the {yyyy-mm} batch bucket in UTC.
func ArchiveMonth(occurredAt time.Time) string {
	return occurredAt.UTC().Format("2006-01")
}
