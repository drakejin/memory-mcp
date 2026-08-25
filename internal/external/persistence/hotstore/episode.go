package hotstore

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// missingIDSep joins the ids reported by a KindNotFound covering several
// records, so the id segment stays a single machine-readable token.
const missingIDSep = ","

// errImmutableID is the log-only cause behind the KindInternal returned when an
// update fn rewrites a record id. Ids are the immutable provenance anchor (§2),
// so this is a bug in the calling package, never client input.
var errImmutableID = errors.New("update fn changed an immutable episode id")

// AppendEpisode implements Client. Duplicate ids are rejected: episodic is
// append-only and ids are the immutable provenance anchor (§2).
func (c *client) AppendEpisode(ctx context.Context, key projectkey.Key, rec episode.Record) error {
	if err := guard(ctx, opAppendEpisode, key); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	recs, err := c.readEpisodesLocked(opAppendEpisode, key)
	if err != nil {
		return err
	}
	for _, existing := range recs {
		if existing.ID == rec.ID {
			return errs.Conflict(opAppendEpisode, entityEpisode, rec.ID, "episode already exists").
				WithField("project", key.String())
		}
	}
	return c.writeEpisodesLocked(opAppendEpisode, key, append(recs, rec))
}

// ListEpisodes implements Client.
func (c *client) ListEpisodes(ctx context.Context, key projectkey.Key) ([]episode.Record, error) {
	if err := guard(ctx, opListEpisodes, key); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readEpisodesLocked(opListEpisodes, key)
}

// GetEpisode implements Client.
func (c *client) GetEpisode(ctx context.Context, key projectkey.Key, id string) (episode.Record, error) {
	if err := guard(ctx, opGetEpisode, key); err != nil {
		return episode.Record{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	recs, err := c.readEpisodesLocked(opGetEpisode, key)
	if err != nil {
		return episode.Record{}, err
	}
	for _, rec := range recs {
		if rec.ID == id {
			return rec, nil
		}
	}
	return episode.Record{}, errs.NotFound(opGetEpisode, entityEpisode, id).
		WithField("project", key.String())
}

// UpdateEpisodes implements Client. Every id must exist (KindNotFound
// otherwise) and fn must not change record ids — ids are immutable (§2), so a
// fn that rewrites one is a caller bug, not caller input.
func (c *client) UpdateEpisodes(ctx context.Context, key projectkey.Key, ids []string, fn func(episode.Record) episode.Record) error {
	if err := guard(ctx, opUpdateEpisodes, key); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	recs, err := c.readEpisodesLocked(opUpdateEpisodes, key)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	next := make([]episode.Record, len(recs))
	for i, rec := range recs {
		if !wanted[rec.ID] {
			next[i] = rec
			continue
		}
		replacement := fn(rec)
		if replacement.ID != rec.ID {
			return errs.Internal(opUpdateEpisodes, errImmutableID).
				WithField("id", rec.ID).
				WithField("rewritten_id", replacement.ID)
		}
		next[i] = replacement
		delete(wanted, rec.ID)
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for id := range wanted {
			missing = append(missing, id)
		}
		slices.Sort(missing)
		return errs.NotFound(opUpdateEpisodes, entityEpisode, strings.Join(missing, missingIDSep)).
			WithField("project", key.String())
	}
	return c.writeEpisodesLocked(opUpdateEpisodes, key, next)
}

// RemoveEpisodes implements Client. Absent ids (and an absent file) are
// skipped so aging retries stay idempotent (§4).
func (c *client) RemoveEpisodes(ctx context.Context, key projectkey.Key, ids []string) error {
	if err := guard(ctx, opRemoveEpisodes, key); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	recs, err := c.readEpisodesLocked(opRemoveEpisodes, key)
	if err != nil {
		return err
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	next := make([]episode.Record, 0, len(recs))
	for _, rec := range recs {
		if !drop[rec.ID] {
			next = append(next, rec)
		}
	}
	if len(next) == len(recs) {
		return nil // nothing removed — no rewrite, no manifest churn
	}
	return c.writeEpisodesLocked(opRemoveEpisodes, key, next)
}

// readEpisodesLocked loads the project's episodic array; missing file yields
// an empty slice. Caller holds c.mu.
func (c *client) readEpisodesLocked(op string, key projectkey.Key) ([]episode.Record, error) {
	path := c.planePath(key, PlaneEpisodic)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []episode.Record{}, nil
	}
	if err != nil {
		return nil, errs.IO(op, path, err)
	}
	var recs []episode.Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, errs.IO(op, path, err)
	}
	return recs, nil
}

// writeEpisodesLocked atomically persists the record array and updates the
// manifest file state (sha256 + record count; IndexedAt/Dirty preserved).
// Caller holds c.mu.
func (c *client) writeEpisodesLocked(op string, key projectkey.Key, recs []episode.Record) error {
	path := c.planePath(key, PlaneEpisodic)
	data, err := marshalCanonical(recs)
	if err != nil {
		return errs.IO(op, path, err)
	}
	if err := writeFileAtomic(op, path, data); err != nil {
		return err
	}
	return c.updateFileStateLocked(op, PlaneEpisodic, key, data, len(recs))
}
