package cold

import (
	"path"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// S3 key layout (§1). These helpers are the single source of truth for cold
// paths — nothing, inside this package or outside it, assembles a cold key by
// hand.
const (
	// Path segments of the §1 layout.
	segEpisodic  = "episodic"
	segKnowledge = "knowledge"
	segBlobs     = "blobs"
	segSnapshots = "snapshots"

	// jsonExt suffixes every archive object; blobs carry raw bytes instead.
	jsonExt = ".json"
	// latestObject is the always-current knowledge snapshot (§4 step 4).
	latestObject = "latest" + jsonExt

	// snapshotStamp is the compact UTC form used in snapshot keys, chosen so
	// lexical order equals chronological order.
	snapshotStamp = "20060102T150405Z"
	// monthStamp is the {yyyy-mm} batch bucket of §3.1.
	monthStamp = "2006-01"
	// blobFanout is how many leading sha characters form the shard directory.
	blobFanout = 2
)

// EpisodeArchiveKey returns
// {username}/episodic/{ws}/{team}/{proj}/{yyyy-mm}.json.
func EpisodeArchiveKey(username string, k projectkey.Key, month string) string {
	return path.Join(username, segEpisodic, k.Workspace, k.Team, k.Project, month+jsonExt)
}

// episodeArchivePrefix returns the listing prefix for a project's monthly
// batches: {username}/episodic/{ws}/{team}/{proj}/.
func episodeArchivePrefix(username string, k projectkey.Key) string {
	return path.Join(username, segEpisodic, k.Workspace, k.Team, k.Project) + "/"
}

// KnowledgeLatestKey returns
// {username}/knowledge/{ws}/{team}/{proj}/latest.json.
func KnowledgeLatestKey(username string, k projectkey.Key) string {
	return path.Join(username, segKnowledge, k.Workspace, k.Team, k.Project, latestObject)
}

// KnowledgeSnapshotKey returns
// {username}/knowledge/{ws}/{team}/{proj}/snapshots/{ts}.json with ts in the
// compact UTC form.
func KnowledgeSnapshotKey(username string, k projectkey.Key, ts time.Time) string {
	stamp := ts.UTC().Format(snapshotStamp)
	return path.Join(username, segKnowledge, k.Workspace, k.Team, k.Project, segSnapshots, stamp+jsonExt)
}

// BlobKey returns {username}/blobs/{sha[:2]}/{sha}.
func BlobKey(username, sha string) string {
	shard := sha
	if len(sha) >= blobFanout {
		shard = sha[:blobFanout]
	}
	return path.Join(username, segBlobs, shard, sha)
}

// ArchiveMonth formats occurredAt as the {yyyy-mm} batch bucket in UTC.
func ArchiveMonth(occurredAt time.Time) string {
	return occurredAt.UTC().Format(monthStamp)
}
