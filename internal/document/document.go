// Package document implements the document pipeline (architecture-v2.md §6):
// one document = blob (original bytes, cold-first) + a knowledge document node
// + document_chunk episodes. Extraction is deterministic only — no OCR, no
// summarization; unextractable inputs are reported honestly.
package document

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/drakejin/memory-mcp/internal/blob"
	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/config"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/ulid"
)

// Truncation reports the honesty contract of §6 step 4: when a document
// yields more than the chunk cap, the response states total vs indexed.
type Truncation struct {
	Total   int `json:"total"`
	Indexed int `json:"indexed"`
}

// IngestResult is the outcome of one document upload (§6).
type IngestResult struct {
	// SHA is the lowercase hex sha256 of the original bytes.
	SHA string `json:"sha"`
	// BlobKey is the S3 key the original was uploaded to (cold-first).
	BlobKey string `json:"blob_key"`
	// NodeID is the auto-created knowledge document node id.
	NodeID string `json:"node_id"`
	// Extractable is false when no deterministic text layer exists (e.g.
	// scanned PDF); the agent handles such documents itself.
	Extractable bool `json:"extractable"`
	// ChunkIDs are the created document_chunk episode ids, in sequence order.
	ChunkIDs []string `json:"chunk_ids"`
	// Truncated is non-nil when the 500-chunk cap cut the tail.
	Truncated *Truncation `json:"truncated,omitempty"`
}

// Service is the document contract the HTTP layer depends on.
type Service interface {
	// Ingest runs §6 steps 1-5: sha256, blob upload (idempotent per sha) +
	// local cache, deterministic extraction, chunking into document_chunk
	// episodes (indexed best-effort), and the knowledge document node.
	Ingest(ctx context.Context, key hotstore.ProjectKey, filename string, data io.Reader) (IngestResult, error)
	// Original opens the raw bytes by sha — local cache first, S3 rehydration
	// on miss (§6 step 6). hotstore.ErrNotFound when unknown everywhere.
	Original(ctx context.Context, sha string) (io.ReadCloser, error)
	// Chunks returns the document_chunk episodes for sha in chunk_seq order.
	Chunks(ctx context.Context, sha string) ([]episodic.Record, error)
}

// Chunk splits extracted text into ~chunkBytes chunks (UTF-8 safe: never
// splits a rune) capped at maxChunks. Returned Truncation always carries the
// true total; Truncated in IngestResult is only surfaced when total > indexed.
// maxChunks <= 0 means uncapped.
func Chunk(text string, chunkBytes, maxChunks int) ([]string, Truncation) {
	if text == "" {
		return nil, Truncation{}
	}
	if chunkBytes <= 0 {
		chunkBytes = config.DocumentChunkBytes
	}

	var chunks []string
	total := 0
	start := 0
	for i := 0; i < len(text); {
		_, size := utf8.DecodeRuneInString(text[i:])
		if i > start && (i-start)+size > chunkBytes {
			total++
			if maxChunks <= 0 || len(chunks) < maxChunks {
				chunks = append(chunks, text[start:i])
			}
			start = i
		}
		i += size
	}
	// Final partial chunk (start < len(text) always holds for non-empty text).
	total++
	if maxChunks <= 0 || len(chunks) < maxChunks {
		chunks = append(chunks, text[start:])
	}

	return chunks, Truncation{Total: total, Indexed: len(chunks)}
}

// Ingestor is the concrete Service wired from the storage layers.
type Ingestor struct {
	deps Deps
}

// Deps are the collaborators of Ingestor; every field is an interface so unit
// tests inject fakes.
type Deps struct {
	Store     hotstore.Store
	Cache     BlobCache
	Archiver  BlobArchiver
	Index     RecordIndexer
	Graph     NodeUpserter
	Extractor Extractor
	Clock     hotstore.Clock
}

// BlobCache is the subset of blob.Cache the pipeline needs (kept local to
// avoid a dependency knot; blob.FileCache satisfies it).
type BlobCache interface {
	Put(ctx context.Context, data io.Reader) (sha string, size int64, err error)
	Get(ctx context.Context, sha string) (io.ReadCloser, error)
}

// BlobArchiver is the subset of cold.Archiver the pipeline needs.
type BlobArchiver interface {
	UploadBlob(ctx context.Context, sha string, r io.Reader) (s3Key string, err error)
	FetchBlob(ctx context.Context, sha string) (io.ReadCloser, error)
	BlobExists(ctx context.Context, sha string) (bool, error)
}

// RecordIndexer is the subset of search.Index the pipeline needs (best-effort;
// failure marks manifest dirty, never fails the ingest).
type RecordIndexer interface {
	IndexRecords(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error
}

// NodeUpserter is the subset of graph.Store the pipeline needs to mirror the
// auto-created document node (best-effort).
type NodeUpserter interface {
	UpsertNodes(ctx context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error
}

// Compile-time contract check.
var _ Service = (*Ingestor)(nil)

// NewIngestor wires a concrete document Service.
func NewIngestor(deps Deps) *Ingestor {
	return &Ingestor{deps: deps}
}

// ErrColdUnavailable is returned by Ingest when the S3 layer is down: blobs
// are cold-first (§6 step 2), so an ingest without a confirmed cold copy would
// silently rely on the evictable local cache.
var ErrColdUnavailable = errors.New("document: cold storage unavailable")

// Ingest implements Service.
func (i *Ingestor) Ingest(ctx context.Context, key hotstore.ProjectKey, filename string, data io.Reader) (IngestResult, error) {
	d := i.deps

	// Step 1: sha256 — Cache.Put hashes while writing the local cache copy.
	sha, _, err := d.Cache.Put(ctx, data)
	if err != nil {
		return IngestResult{}, fmt.Errorf("document: cache original: %w", err)
	}
	res := IngestResult{SHA: sha, ChunkIDs: []string{}}

	// Step 2: cold-first blob upload (idempotent per sha).
	if d.Archiver == nil {
		return IngestResult{}, ErrColdUnavailable
	}
	blobKey, err := i.uploadBlob(ctx, sha)
	if err != nil {
		return IngestResult{}, err
	}
	res.BlobKey = blobKey

	// Step 3: deterministic extraction.
	text, extractable, err := i.extract(ctx, filename, sha)
	if err != nil {
		return IngestResult{}, err
	}
	res.Extractable = extractable

	now := d.Clock.Now()

	// Step 4: chunk into document_chunk episodes (skipped when unextractable).
	if extractable {
		chunkIDs, trunc, err := i.ensureChunks(ctx, key, sha, text, now)
		if err != nil {
			return IngestResult{}, err
		}
		res.ChunkIDs = chunkIDs
		if trunc.Total > trunc.Indexed {
			t := trunc
			res.Truncated = &t
		}
	}

	// Step 5: knowledge document node (filename + sha, no summary).
	nodeID, err := i.ensureDocumentNode(ctx, key, filename, sha, now)
	if err != nil {
		return IngestResult{}, err
	}
	res.NodeID = nodeID

	return res, nil
}

// uploadBlob streams the cached blob to cold. Same-sha uploads are idempotent
// at the Archiver level.
func (i *Ingestor) uploadBlob(ctx context.Context, sha string) (string, error) {
	rc, err := i.deps.Cache.Get(ctx, sha)
	if err != nil {
		return "", fmt.Errorf("document: reopen cached blob %s: %w", sha, err)
	}
	defer rc.Close()
	blobKey, err := i.deps.Archiver.UploadBlob(ctx, sha, rc)
	if err != nil {
		return "", fmt.Errorf("document: upload blob %s: %w", sha, err)
	}
	return blobKey, nil
}

// extract reopens the cached original and runs the deterministic extractor.
func (i *Ingestor) extract(ctx context.Context, filename, sha string) (string, bool, error) {
	rc, err := i.deps.Cache.Get(ctx, sha)
	if err != nil {
		return "", false, fmt.Errorf("document: reopen cached blob %s: %w", sha, err)
	}
	defer rc.Close()
	return i.deps.Extractor.Extract(ctx, filename, rc)
}

// ensureChunks appends document_chunk episodes for sha unless they already
// exist (same-sha re-ingest is idempotent). Indexing is best-effort: failure
// marks the manifest dirty and never fails the ingest (§1).
func (i *Ingestor) ensureChunks(ctx context.Context, key hotstore.ProjectKey, sha, text string, now time.Time) ([]string, Truncation, error) {
	d := i.deps

	existing, err := d.Store.ListEpisodes(ctx, key)
	if err != nil {
		return nil, Truncation{}, fmt.Errorf("document: list episodes for %s: %w", key.String(), err)
	}
	if ids := chunkIDsFor(existing, sha); len(ids) > 0 {
		// Re-ingest of a known document: report the truth about what is
		// indexed without appending duplicates.
		_, trunc := Chunk(text, config.DocumentChunkBytes, config.MaxDocumentChunks)
		trunc.Indexed = min(trunc.Indexed, len(ids))
		return ids, trunc, nil
	}

	chunks, trunc := Chunk(text, config.DocumentChunkBytes, config.MaxDocumentChunks)
	ids := make([]string, 0, len(chunks))
	recs := make([]episodic.Record, 0, len(chunks))
	for seq, chunk := range chunks {
		rec := episodic.Record{
			ID:         ulid.At(now.UnixMilli()),
			Kind:       episodic.KindDocumentChunk,
			OccurredAt: now,
			Actor:      episodic.ActorSystem,
			Text:       chunk,
			Entities:   []string{},
			Refs:       &episodic.Refs{DocSHA: sha, ChunkSeq: seq},
		}
		if err := d.Store.AppendEpisode(ctx, key, rec); err != nil {
			return nil, Truncation{}, fmt.Errorf("document: append chunk %d for %s: %w", seq, sha, err)
		}
		ids = append(ids, rec.ID)
		recs = append(recs, rec)
	}

	if len(recs) > 0 {
		if err := i.indexBestEffort(ctx, key, recs); err != nil {
			slog.Warn("document: chunk indexing degraded; manifest marked dirty",
				"project", key.String(), "sha", sha, "error", err)
			if derr := d.Store.MarkDirty(ctx, key, hotstore.PlaneEpisodic); derr != nil {
				slog.Warn("document: mark dirty failed", "project", key.String(), "error", derr)
			}
		}
	}
	return ids, trunc, nil
}

func (i *Ingestor) indexBestEffort(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error {
	if i.deps.Index == nil {
		return errors.New("search index unavailable")
	}
	return i.deps.Index.IndexRecords(ctx, key, recs)
}

// ensureDocumentNode finds or creates the auto document node for sha and
// mirrors a new node into the graph best-effort.
func (i *Ingestor) ensureDocumentNode(ctx context.Context, key hotstore.ProjectKey, filename, sha string, now time.Time) (string, error) {
	d := i.deps

	g, err := d.Store.ReadKnowledge(ctx, key)
	if err != nil {
		return "", fmt.Errorf("document: read knowledge for %s: %w", key.String(), err)
	}
	for _, n := range g.Nodes {
		if n.Kind == knowledge.KindDocument && slices.Contains(n.Aliases, sha) {
			return n.ID, nil // same-sha re-ingest: node already exists
		}
	}

	node := knowledge.Node{
		ID:         ulid.At(now.UnixMilli()),
		Kind:       knowledge.KindDocument,
		Name:       filename,
		Body:       "sha256:" + sha,
		Aliases:    []string{sha},
		State:      knowledge.StateActive,
		Trust:      knowledge.TrustImported,
		Supersedes: []string{},
		Provenance: []string{},
		Created:    now,
		Updated:    now,
	}
	next := knowledge.Graph{
		Nodes: append(slices.Clone(g.Nodes), node),
		Edges: slices.Clone(g.Edges),
	}
	if err := d.Store.WriteKnowledge(ctx, key, next); err != nil {
		return "", fmt.Errorf("document: write knowledge for %s: %w", key.String(), err)
	}

	if err := i.upsertNodeBestEffort(ctx, key, node); err != nil {
		slog.Warn("document: graph upsert degraded; manifest marked dirty",
			"project", key.String(), "sha", sha, "error", err)
		if derr := d.Store.MarkDirty(ctx, key, hotstore.PlaneKnowledge); derr != nil {
			slog.Warn("document: mark dirty failed", "project", key.String(), "error", derr)
		}
	}
	return node.ID, nil
}

func (i *Ingestor) upsertNodeBestEffort(ctx context.Context, key hotstore.ProjectKey, node knowledge.Node) error {
	if i.deps.Graph == nil {
		return errors.New("graph store unavailable")
	}
	return i.deps.Graph.UpsertNodes(ctx, key, []knowledge.Node{node})
}

// Original implements Service: local cache first, cold rehydration on miss
// (§6 step 6). The rehydrated copy is re-cached for the next read.
func (i *Ingestor) Original(ctx context.Context, sha string) (io.ReadCloser, error) {
	d := i.deps

	rc, err := d.Cache.Get(ctx, sha)
	if err == nil {
		return rc, nil
	}
	if !errors.Is(err, blob.ErrNotCached) {
		return nil, fmt.Errorf("document: cache read %s: %w", sha, err)
	}

	if d.Archiver == nil {
		return nil, fmt.Errorf("document: blob %s not cached and cold unavailable: %w", sha, hotstore.ErrNotFound)
	}
	cool, err := d.Archiver.FetchBlob(ctx, sha)
	if err != nil {
		if errors.Is(err, cold.ErrNotFound) {
			return nil, fmt.Errorf("document: blob %s: %w", sha, hotstore.ErrNotFound)
		}
		return nil, fmt.Errorf("document: fetch blob %s from cold: %w", sha, err)
	}

	cachedSHA, _, err := d.Cache.Put(ctx, cool)
	cool.Close()
	if err != nil {
		// Cache repopulation failed; stream directly from cold instead.
		slog.Warn("document: blob re-cache failed; streaming from cold", "sha", sha, "error", err)
		return d.Archiver.FetchBlob(ctx, sha)
	}
	if cachedSHA != sha {
		return nil, fmt.Errorf("document: cold blob %s hashed to %s — corrupt cold object", sha, cachedSHA)
	}
	return d.Cache.Get(ctx, sha)
}

// Chunks implements Service: scans all projects for document_chunk episodes
// referencing sha and returns them in chunk_seq order.
func (i *Ingestor) Chunks(ctx context.Context, sha string) ([]episodic.Record, error) {
	d := i.deps

	projects, err := d.Store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("document: list projects: %w", err)
	}
	out := []episodic.Record{}
	for _, key := range projects {
		recs, err := d.Store.ListEpisodes(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("document: list episodes for %s: %w", key.String(), err)
		}
		for _, rec := range recs {
			if rec.Kind == episodic.KindDocumentChunk && rec.Refs != nil && rec.Refs.DocSHA == sha {
				out = append(out, rec)
			}
		}
	}
	slices.SortFunc(out, func(x, y episodic.Record) int {
		if x.Refs.ChunkSeq != y.Refs.ChunkSeq {
			return x.Refs.ChunkSeq - y.Refs.ChunkSeq
		}
		if x.ID < y.ID {
			return -1
		}
		if x.ID > y.ID {
			return 1
		}
		return 0
	})
	return out, nil
}

// chunkIDsFor returns existing chunk episode ids for sha in chunk_seq order.
func chunkIDsFor(recs []episodic.Record, sha string) []string {
	type seqID struct {
		seq int
		id  string
	}
	var found []seqID
	for _, rec := range recs {
		if rec.Kind == episodic.KindDocumentChunk && rec.Refs != nil && rec.Refs.DocSHA == sha {
			found = append(found, seqID{seq: rec.Refs.ChunkSeq, id: rec.ID})
		}
	}
	slices.SortFunc(found, func(x, y seqID) int { return x.seq - y.seq })
	ids := make([]string, 0, len(found))
	for _, f := range found {
		ids = append(ids, f.id)
	}
	return ids
}
