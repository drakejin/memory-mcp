//go:build e2e

// P12 — blob cold-first (feature-inventory.md §2 P12): a document's original
// bytes are already in S3 when ingest answers — observed with the aws CLI, not
// through the server — so the local blob cache is genuinely disposable:
// deleting the cached file and reading the original back must restore it from
// cold byte-identically and repopulate the cache.
package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cold "github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
)

// p12Token marks this file's fixture content.
const p12Token = "p12blobtoken"

// p12BlobCacheDir is the content-addressed cache directory under the home
// ({home}/blobs/{sha} — internal/external/persistence/blob).
const p12BlobCacheDir = "blobs"

// TestP12_BlobColdFirst_EvictAndRestore runs the full cold-first round trip:
// ingest → S3 holds the original before ANY read → evict the local cache file
// → GET the original → byte-identical restore → cache repopulated.
func TestP12_BlobColdFirst_EvictAndRestore(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p12-blob")

	// Mixed-width UTF-8 plus a bulk tail so byte identity is a real claim, and
	// an independent sha256 so the server's content addressing is checked
	// against something the server did not compute.
	content := []byte("# p12 cold-first fixture\n\n" +
		p12Token + " 원본 바이트 왕복 — byte identity matters: ①②③ 😀\n\n" +
		strings.Repeat("payload line for byte-identity checks across evict and restore\n", 40))
	sum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(sum[:])

	res := h.ingestDoc(t, key, "p12-coldfirst.md", content)
	if res.SHA != wantSHA {
		failf(t, "server content address %s != independently computed sha256 %s", res.SHA, wantSHA)
	}
	if len(res.Degraded) != 0 {
		failf(t, "ingest: unexpected degraded notes with every store up: %v", res.Degraded)
	}
	blobKey := cold.BlobKey(s3Username, res.SHA)
	if res.BlobKey != blobKey {
		failf(t, "ingest blob_key %q != cold key layout %q", res.BlobKey, blobKey)
	}
	// Own keys under the e2e prefix are cleaned even before TestMain's sweep.
	t.Cleanup(func() {
		if out, err := runCmd("", "aws", awsArgs("s3", "rm", "s3://"+s3Bucket+"/"+blobKey)...); err != nil {
			t.Logf("p12 cleanup: aws s3 rm %s: %v\n%s", blobKey, err, out)
		}
	})

	// (1) Cold-first: S3 already holds the byte-identical original — verified
	// with the aws CLI BEFORE any read of the document has happened.
	if ok, ev := h.s3Exists(blobKey); !ok {
		failf(t, "s3 inspector: blob missing right after ingest — cold-first violated: %s", ev)
	}
	if got := h.s3Cat(t, blobKey); !bytes.Equal(got, content) {
		failf(t, "s3 inspector: cold bytes differ from the original: got %d bytes, want %d", len(got), len(content))
	}
	pass(t, "cold-first: s3://%s/%s holds the byte-identical original before any read", s3Bucket, blobKey)

	// (2) The ingest wrote through the local cache too.
	cachePath := filepath.Join(h.home, p12BlobCacheDir, res.SHA)
	if got := readFileRaw(t, cachePath); !bytes.Equal(got, content) {
		failf(t, "blob cache %s differs from the original after ingest: %d vs %d bytes", cachePath, len(got), len(content))
	}

	// (3) Evict the cache file directly — the cache is disposable by design.
	if err := os.Remove(cachePath); err != nil {
		failf(t, "evict blob cache file %s: %v", cachePath, err)
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, fs.ErrNotExist) {
		failf(t, "blob cache file %s still present after evict (stat err=%v)", cachePath, err)
	}
	pass(t, "local cache evicted: %s gone", cachePath)

	// (4) GET the original: with the cache empty this can only be served from
	// cold, and it must be byte-identical.
	status, got := h.api(t).getRawBytes(t, "/v1/documents/"+res.SHA)
	if status != http.StatusOK {
		failf(t, "GET /v1/documents/%s after evict: want 200, got HTTP %d: %.300s", res.SHA, status, got)
	}
	if !bytes.Equal(got, content) {
		failf(t, "GET after evict is not byte-identical: got %d bytes, want %d", len(got), len(content))
	}

	// (5) The read repopulated the cache with identical bytes (Original
	// re-caches synchronously before answering).
	if got := readFileRaw(t, cachePath); !bytes.Equal(got, content) {
		failf(t, "blob cache %s not repopulated byte-identically after the cold read: %d vs %d bytes", cachePath, len(got), len(content))
	}
	pass(t, "cold restore: GET returned %d identical bytes and repopulated %s", len(got), cachePath)

	// (6) A second GET — now a cache hit — must serve the same bytes.
	status2, got2 := h.api(t).getRawBytes(t, "/v1/documents/"+res.SHA)
	if status2 != http.StatusOK || !bytes.Equal(got2, content) {
		failf(t, "second GET (cache hit): want 200 with %d identical bytes, got HTTP %d with %d bytes", len(content), status2, len(got2))
	}
	pass(t, "cache hit after restore serves the same %d bytes — evict/restore round trip complete", len(got2))
}
