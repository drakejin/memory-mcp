package document

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// shaBodyPrefix labels the sha inside the document node body; the node carries
// no summary by design (§6 step 5).
const shaBodyPrefix = "sha256:"

// mirror describes one best-effort derived mirror of an ingest: the manifest
// plane that goes dirty when it fails, the degraded note the response carries,
// and the entity the log line names. The three always travel together, so
// binding them in one value keeps the chunk and node paths from drifting apart.
type mirror struct {
	plane  rehydrate.Plane
	note   string
	entity string
}

// errNodeExists aborts the UpdateKnowledge write when a document node for the
// same sha is already present. Re-ingesting identical bytes must leave the
// graph file untouched: rewriting it would bump the mtime the stat gate reads
// (§5) and schedule a rehydration that has nothing to converge.
var errNodeExists = errors.New("document node already exists")

// Ingest implements Service.
func (s *service) Ingest(ctx context.Context, key projectkey.Key, filename string, data io.Reader) (IngestResult, error) {
	// Step 1: sha256 — Put hashes while writing the local cache copy.
	sha, _, err := s.cache.Put(ctx, data)
	if err != nil {
		return IngestResult{}, errs.Wrap(opIngest, err)
	}
	res := IngestResult{SHA: sha, ChunkIDs: []string{}}

	// Step 2: cold-first blob upload (idempotent per sha). Without a confirmed
	// cold copy the ingest would silently rely on the evictable local cache.
	if s.archiver == nil {
		return IngestResult{}, errs.Unavailable(opIngest, nil).
			WithField("reason", "cold storage not configured")
	}
	blobKey, err := s.uploadBlob(ctx, sha)
	if err != nil {
		return IngestResult{}, err
	}
	res.BlobKey = blobKey

	// Step 3: deterministic extraction.
	text, extractable, err := s.extract(ctx, filename, sha)
	if err != nil {
		return IngestResult{}, err
	}
	res.Extractable = extractable

	now := s.clock.Now()

	// Step 4: chunk into document_chunk episodes (skipped when unextractable).
	// Steps 4 and 5 fill their own part of res, including the degraded notes
	// of a derived mirror that failed — the caller is told what really landed.
	if extractable {
		if err := s.ensureChunks(ctx, &res, key, sha, text, now); err != nil {
			return IngestResult{}, err
		}
	}

	// Step 5: knowledge document node (filename + sha, no summary).
	if err := s.ensureDocumentNode(ctx, &res, key, filename, sha, now); err != nil {
		return IngestResult{}, err
	}

	return res, nil
}

// uploadBlob streams the cached blob to cold. Same-sha uploads are idempotent
// at the archiver level.
func (s *service) uploadBlob(ctx context.Context, sha string) (string, error) {
	rc, err := s.cache.Get(ctx, sha)
	if err != nil {
		return "", errs.Wrap(opIngest, err)
	}
	defer rc.Close()

	blobKey, err := s.archiver.UploadBlob(ctx, sha, rc)
	if err != nil {
		return "", errs.Wrap(opIngest, err)
	}
	return blobKey, nil
}

// extract reopens the cached original and runs the deterministic extractor.
func (s *service) extract(ctx context.Context, filename, sha string) (string, bool, error) {
	rc, err := s.cache.Get(ctx, sha)
	if err != nil {
		return "", false, errs.Wrap(opIngest, err)
	}
	defer rc.Close()

	text, extractable, err := s.extractor.Extract(ctx, filename, rc)
	if err != nil {
		return "", false, errs.Wrap(opIngest, err)
	}
	return text, extractable, nil
}

// ensureChunks appends document_chunk episodes for sha unless they already
// exist (same-sha re-ingest is idempotent) and records the chunk ids plus the
// honest truncation report on res. Indexing is best-effort: a failure marks the
// manifest dirty and degrades res instead of failing the ingest (§1).
func (s *service) ensureChunks(ctx context.Context, res *IngestResult, key projectkey.Key, sha, text string, now time.Time) error {
	existing, err := s.store.ListEpisodes(ctx, key)
	if err != nil {
		return errs.Wrap(opIngest, err)
	}
	if ids := chunkIDsFor(existing, sha); len(ids) > 0 {
		// Re-ingest of a known document: report the truth about what is
		// indexed without appending duplicates.
		_, trunc := chunkDocument(text)
		trunc.Indexed = min(trunc.Indexed, len(ids))
		res.setChunks(ids, trunc)
		return nil
	}

	chunks, trunc := chunkDocument(text)
	ids := make([]string, 0, len(chunks))
	recs := make([]episode.Record, 0, len(chunks))
	for seq, chunk := range chunks {
		id, err := s.ids.GenerateAt(now.UnixMilli())
		if err != nil {
			return errs.Wrap(opIngest, err)
		}
		rec := episode.Record{
			ID:         id,
			Kind:       episode.KindDocumentChunk,
			OccurredAt: now,
			Actor:      episode.ActorSystem,
			Text:       chunk,
			Entities:   []string{},
			Refs:       &episode.Refs{DocSHA: sha, ChunkSeq: seq},
		}
		if err := s.store.AppendEpisode(ctx, key, rec); err != nil {
			return errs.Wrap(opIngest, err)
		}
		ids = append(ids, rec.ID)
		recs = append(recs, rec)
	}
	res.setChunks(ids, trunc)

	if len(recs) > 0 {
		if err := s.indexChunks(ctx, key, recs); err != nil {
			s.reportDegraded(ctx, res, key, mirror{
				plane:  rehydrate.PlaneEpisodic,
				note:   degradedSearch,
				entity: entityChunk,
			}, sha, err)
		}
	}
	return nil
}

// setChunks records what step 4 produced: the chunk ids and, only when the cap
// actually cut the tail, the truncation report (§6 step 4).
func (r *IngestResult) setChunks(ids []string, trunc Truncation) {
	r.ChunkIDs = ids
	if trunc.Total > trunc.Indexed {
		truncated := trunc
		r.Truncated = &truncated
	}
}

// indexChunks mirrors chunk episodes into search when an index is configured.
func (s *service) indexChunks(ctx context.Context, key projectkey.Key, recs []episode.Record) error {
	if s.index == nil {
		return errs.Unavailable(opIngest, nil).WithField("reason", "search index not configured")
	}
	return errs.Wrap(opIngest, s.index.IndexRecords(ctx, key, recs))
}

// ensureDocumentNode finds or creates the auto document node for sha, records
// its id on res, and mirrors a new node into the graph best-effort — a failed
// mirror degrades res instead of failing the ingest. The lookup and the append
// run inside one UpdateKnowledge closure so two concurrent ingests of different
// documents cannot overwrite each other's node.
func (s *service) ensureDocumentNode(ctx context.Context, res *IngestResult, key projectkey.Key, filename, sha string, now time.Time) error {
	var created knowledge.Node
	err := s.store.UpdateKnowledge(ctx, key, func(g knowledge.Graph) (knowledge.Graph, error) {
		for _, n := range g.Nodes {
			if n.Kind == knowledge.KindDocument && slices.Contains(n.Aliases, sha) {
				created = n
				return g, errNodeExists // same-sha re-ingest: nothing to write
			}
		}
		id, err := s.ids.GenerateAt(now.UnixMilli())
		if err != nil {
			return g, err
		}
		created = knowledge.Node{
			ID:         id,
			Kind:       knowledge.KindDocument,
			Name:       filename,
			Body:       shaBodyPrefix + sha,
			Aliases:    []string{sha},
			State:      knowledge.StateActive,
			Trust:      knowledge.TrustImported,
			Supersedes: []string{},
			Provenance: []string{},
			Created:    now,
			Updated:    now,
		}
		return knowledge.Graph{
			Nodes: append(slices.Clone(g.Nodes), created),
			Edges: slices.Clone(g.Edges),
		}, nil
	})
	switch {
	case errors.Is(err, errNodeExists):
		res.NodeID = created.ID
		return nil
	case err != nil:
		return errs.Wrap(opIngest, err)
	}
	res.NodeID = created.ID

	if err := s.upsertNode(ctx, key, created); err != nil {
		s.reportDegraded(ctx, res, key, mirror{
			plane:  rehydrate.PlaneKnowledge,
			note:   degradedGraph,
			entity: entityGraph,
		}, sha, err)
	}
	return nil
}

// upsertNode mirrors the document node into the graph when one is configured.
func (s *service) upsertNode(ctx context.Context, key projectkey.Key, node knowledge.Node) error {
	if s.graph == nil {
		return errs.Unavailable(opIngest, nil).WithField("reason", "knowledge graph not configured")
	}
	return errs.Wrap(opIngest, s.graph.UpsertNodes(ctx, key, []knowledge.Node{node}))
}

// reportDegraded records a failed derived-store mirror: the hot write already
// succeeded, so this is degradation, not failure (§1, §5). The note goes on the
// response so the caller learns the mirror is missing instead of being told the
// ingest fully succeeded; the manifest dirty mark is what the next rehydration
// converges on.
func (s *service) reportDegraded(ctx context.Context, res *IngestResult, key projectkey.Key, m mirror, sha string, cause error) {
	res.Degraded = append(res.Degraded, m.note)
	s.log.WarnContext(ctx, "document mirror degraded; marking manifest dirty",
		"project", key.String(), "plane", string(m.plane), "entity", m.entity, "sha", sha, "error", cause)
	if err := s.store.MarkDirty(ctx, key, m.plane); err != nil {
		s.log.WarnContext(ctx, "document mark dirty failed",
			"project", key.String(), "plane", string(m.plane), "error", err)
	}
}

// chunkIDsFor returns existing chunk episode ids for sha in chunk_seq order.
func chunkIDsFor(recs []episode.Record, sha string) []string {
	type seqID struct {
		seq int
		id  string
	}
	var found []seqID
	for _, rec := range recs {
		if isChunkOf(rec, sha) {
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

// isChunkOf reports whether rec is a document_chunk of the document sha.
func isChunkOf(rec episode.Record, sha string) bool {
	return rec.Kind == episode.KindDocumentChunk && rec.Refs != nil && rec.Refs.DocSHA == sha
}
