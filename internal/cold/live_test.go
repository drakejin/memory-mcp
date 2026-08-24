package cold

// Live tests exercise the real bucket s3://vms-memory-mcp (ap-northeast-2).
// Guard: they skip unless DJ_MEMORY_LIVE_TEST=1 — the single project-wide live
// gate, shared with internal/search and internal/graph — so `go test ./...`
// stays hermetic and credential-free. Run with:
//
//	make live
//	# or: DJ_MEMORY_LIVE_TEST=1 go test ./internal/cold/ -run Live -v
//
// Everything written lands under {username}/... with a per-run unique username
// beginning jin/livetest/, and t.Cleanup deletes it again.
//
// Why this exists: every other test in this package drives the s3API/uploader
// fakes, so nothing verified that the constructed client reaches the bucket at
// all. The specific hazard architecture-v2.md §8 calls out is that profile
// vms-holdings defaults to ap-southeast-1 while the bucket lives in
// ap-northeast-2, so inheriting the profile region yields PermanentRedirect. A
// regression dropping awsconfig.WithRegion keeps unit coverage green and
// breaks every archive; TestLiveWrongRegionFailsLoudly pins it.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// liveEnvVar is the one opt-in gate for every live test in this repo; the same
// name and the same exact-"1" comparison appear in internal/search and
// internal/graph, and `make live` sets it (code-standards §3).
const (
	liveEnvVar   = "DJ_MEMORY_LIVE_TEST"
	liveEnvValue = "1"
)

// The real cold store of architecture-v2.md §8.
const (
	liveBucket = "vms-memory-mcp"
	liveRegion = "ap-northeast-2"
	// liveWrongRegion is the vms-holdings profile default — the region the
	// client must NOT inherit.
	liveWrongRegion = "ap-southeast-1"
	liveProfile     = "vms-holdings"
	// livePrefix is the reserved scratch namespace; §10 keeps blackbox and
	// live-test objects out of real data.
	livePrefix = "jin/livetest"
)

// liveUsername returns a per-run key namespace so concurrent or interrupted
// runs never collide. It doubles as the cleanup prefix.
func liveUsername(t *testing.T) string {
	t.Helper()
	return livePrefix + "/" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().UTC().Format("20060102T150405.000000000")
}

// awsRM deletes an S3 path through the aws CLI. Cleanup deliberately does not
// go through the code under test: a delete implemented here would be
// production API surface no shipped code calls, which the dead-code policy
// (code-standards §4) forbids, and an independent channel is the more honest
// verifier anyway.
func awsRM(target string, recursive bool) (string, error) {
	args := []string{"--profile", liveProfile, "--region", liveRegion, "s3", "rm"}
	if recursive {
		args = append(args, "--recursive")
	}
	args = append(args, "s3://"+liveBucket+"/"+target)
	out, err := exec.Command("aws", args...).CombinedOutput()
	return string(out), err
}

// newLiveClient builds a Client on the real bucket in the correct region and
// registers prefix cleanup. It skips unless the live gate is set.
func newLiveClient(t *testing.T) (Client, string, context.Context) {
	t.Helper()
	if os.Getenv(liveEnvVar) != liveEnvValue {
		t.Skip("live S3 test: set " + liveEnvVar + "=" + liveEnvValue + " with AWS profile " + liveProfile + " available (make live)")
	}
	username := liveUsername(t)
	c, err := New(Config{Bucket: liveBucket, Region: liveRegion, Profile: liveProfile, Username: username})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	objects := c.(*client).objects
	if _, err := objects.List(ctx, username+"/"); err != nil {
		t.Fatalf("bucket %s unreachable in %s with profile %s: %v", liveBucket, liveRegion, liveProfile, err)
	}
	t.Cleanup(func() {
		if out, err := awsRM(username+"/", true); err != nil {
			t.Errorf("cleanup s3://%s/%s: %v\n%s", liveBucket, username, err, out)
		}
	})
	return c, username, ctx
}

func liveRecord(id, text string, at time.Time) episodic.Record {
	return episodic.Record{
		ID: id, Kind: episodic.KindEvent, OccurredAt: at, Actor: episodic.ActorAgent,
		Text: text, Entities: []string{"livetest"}, Consolidated: true,
	}
}

func liveKey() hotstore.ProjectKey {
	return hotstore.ProjectKey{Workspace: "livetest", Team: "cold", Project: "roundtrip"}
}

// TestLiveArchiveRoundTrip is the end-to-end claim no fake can make: the bytes
// really landed in the bucket and really come back. §4 deletes canonical hot
// records after a claimed-successful put, so "the put succeeded" must be true.
func TestLiveArchiveRoundTrip(t *testing.T) {
	// Arrange
	c, username, ctx := newLiveClient(t)
	key := liveKey()
	at := time.Date(2026, 3, 14, 1, 2, 3, 0, time.UTC)
	month := ArchiveMonth(at)
	recs := []episodic.Record{
		liveRecord("01LIVE0000000000000000001", "첫 번째 라이브 레코드", at),
		liveRecord("01LIVE0000000000000000002", "second live record", at.Add(time.Hour)),
	}

	// Act
	gotKey, err := c.ArchiveEpisodes(ctx, key, month, recs)
	if err != nil {
		t.Fatalf("ArchiveEpisodes: %v", err)
	}

	// Assert
	if want := EpisodeArchiveKey(username, key, month); gotKey != want {
		t.Fatalf("archive key = %q, want %q", gotKey, want)
	}
	for _, rec := range recs {
		got, err := c.FetchArchivedEpisode(ctx, key, rec.ID)
		if err != nil {
			t.Fatalf("FetchArchivedEpisode(%s): %v", rec.ID, err)
		}
		if got.Text != rec.Text {
			t.Errorf("archived %s text = %q, want %q", rec.ID, got.Text, rec.Text)
		}
	}

	// Re-archiving the same month must merge by id, not duplicate (§4 retries).
	if _, err := c.ArchiveEpisodes(ctx, key, month, recs[:1]); err != nil {
		t.Fatalf("ArchiveEpisodes (rerun): %v", err)
	}
	if got, err := c.FetchArchivedEpisode(ctx, key, recs[1].ID); err != nil {
		t.Errorf("record %s lost by an idempotent re-archive: %v", recs[1].ID, err)
	} else if got.Text != recs[1].Text {
		t.Errorf("re-archive corrupted %s: %q", recs[1].ID, got.Text)
	}

	// A missing id is KindNotFound, not a transport failure.
	if _, err := c.FetchArchivedEpisode(ctx, key, "01LIVE000000000000000ABSENT"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("FetchArchivedEpisode(absent) = %v, want ErrNotFound", err)
	}
}

// TestLiveBlobRoundTrip covers §6 step 2: the blob is cold-first, so the upload
// must be real and the fetch must return identical bytes.
func TestLiveBlobRoundTrip(t *testing.T) {
	// Arrange
	c, username, ctx := newLiveClient(t)
	payload := []byte("live blob payload — 한글 포함\x00\x01binary")
	sha := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// Act
	gotKey, err := c.UploadBlob(ctx, sha, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}

	// Assert
	if want := BlobKey(username, sha); gotKey != want {
		t.Fatalf("blob key = %q, want %q", gotKey, want)
	}
	rc, err := c.FetchBlob(ctx, sha)
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("blob round-trip differs: got %d bytes, want %d", len(got), len(payload))
	}

	if _, err := c.FetchBlob(ctx, "0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("FetchBlob(absent) = %v, want ErrNotFound", err)
	}
}

// TestLiveSnapshotKnowledge covers §4 step 4: latest.json plus a timestamped
// snapshot, both really present.
func TestLiveSnapshotKnowledge(t *testing.T) {
	// Arrange
	c, username, ctx := newLiveClient(t)
	key := liveKey()
	ts := time.Date(2026, 8, 25, 4, 5, 6, 0, time.UTC)
	g := knowledge.Graph{
		Nodes: []knowledge.Node{{
			ID: "01LIVENODE00000000000001", Kind: knowledge.KindFact, Name: "live-fact",
			State: knowledge.StateActive, Trust: knowledge.TrustUserStated,
		}},
		Edges: []knowledge.Edge{},
	}

	// Act
	latest, snapshot, err := c.SnapshotKnowledge(ctx, key, g, ts)
	if err != nil {
		t.Fatalf("SnapshotKnowledge: %v", err)
	}

	// Assert
	if want := KnowledgeLatestKey(username, key); latest != want {
		t.Errorf("latest key = %q, want %q", latest, want)
	}
	if want := KnowledgeSnapshotKey(username, key, ts); snapshot != want {
		t.Errorf("snapshot key = %q, want %q", snapshot, want)
	}
	objects := c.(*client).objects
	for _, k := range []string{latest, snapshot} {
		ok, err := objects.Exists(ctx, k)
		if err != nil {
			t.Fatalf("Exists(%s): %v", k, err)
		}
		if !ok {
			t.Errorf("snapshot object %s is not in the bucket", k)
		}
	}
}

// TestLiveWrongRegionFailsLoudly is the regression pin for architecture-v2.md
// §8: the vms-holdings profile defaults to ap-southeast-1 while the bucket is
// in ap-northeast-2. A client built on the wrong region must fail — silently
// writing somewhere else would be far worse, since §4 deletes hot records once
// the put "succeeds".
func TestLiveWrongRegionFailsLoudly(t *testing.T) {
	// Arrange
	if os.Getenv(liveEnvVar) != liveEnvValue {
		t.Skip("live S3 test: set " + liveEnvVar + "=" + liveEnvValue + " (make live)")
	}
	username := liveUsername(t)
	c, err := New(Config{Bucket: liveBucket, Region: liveWrongRegion, Profile: liveProfile, Username: username})
	if err != nil {
		t.Fatalf("New(wrong region): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Act
	_, err = c.UploadBlob(ctx, "1111111111111111111111111111111111111111111111111111111111111111",
		strings.NewReader("must not land"))

	// Assert
	if err == nil {
		// It somehow succeeded: scrub the object so a stray copy is not left in
		// the bucket, then fail — succeeding here means the region no longer
		// protects anything.
		if out, rmErr := awsRM(username+"/", true); rmErr != nil {
			t.Errorf("cleanup stray objects under %s: %v\n%s", username, rmErr, out)
		}
		t.Fatal("upload with the profile's default region succeeded; the §8 region hazard is no longer pinned by this test")
	}
	if !errors.Is(err, errs.ErrUnavailable) {
		t.Errorf("wrong-region upload = %v, want KindUnavailable (the degraded-mode signal)", err)
	}
}
