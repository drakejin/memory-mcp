package hotstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
)

func TestAppendListGetEpisodes(t *testing.T) {
	store, home := newTestClient(t)
	ctx := context.Background()
	key := testKey()

	// Missing file lists empty, not error.
	recs, err := store.ListEpisodes(ctx, key)
	if err != nil {
		t.Fatalf("ListEpisodes on missing file: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected empty list, got %d", len(recs))
	}

	ids := []string{"01AAAA", "01BBBB", "01CCCC"}
	for i, id := range ids {
		if err := store.AppendEpisode(ctx, key, testRecord(id, "text "+id)); err != nil {
			t.Fatalf("AppendEpisode %d: %v", i, err)
		}
	}

	recs, err = store.ListEpisodes(ctx, key)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	for i, id := range ids {
		if recs[i].ID != id {
			t.Errorf("record %d id = %q, want %q (append order)", i, recs[i].ID, id)
		}
	}

	got, err := store.GetEpisode(ctx, key, "01BBBB")
	if err != nil {
		t.Fatalf("GetEpisode: %v", err)
	}
	if got.Text != "text 01BBBB" {
		t.Errorf("GetEpisode text = %q", got.Text)
	}

	// File lives at the documented layout and no temp litter remains.
	path := episodicPath(home, key)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected episodic file at %s: %v", path, err)
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestGetEpisodeMissingIsNotFound(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()
	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); err != nil {
		t.Fatal(err)
	}

	_, err := store.GetEpisode(ctx, key, "01ZZZZ")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("GetEpisode missing = %v, want ErrNotFound", err)
	}
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("err %v is not a domain error", err)
	}
	if domain.Entity != entityEpisode || domain.ID != "01ZZZZ" {
		t.Errorf("entity/id = %q/%q, want %q/%q", domain.Entity, domain.ID, entityEpisode, "01ZZZZ")
	}
	if domain.Op != opGetEpisode {
		t.Errorf("Op = %q, want %q", domain.Op, opGetEpisode)
	}
}

func TestAppendDuplicateIsConflict(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()

	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "first")); err != nil {
		t.Fatal(err)
	}
	err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "dupe"))
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate append = %v, want ErrConflict", err)
	}

	// The original record survives untouched.
	recs, err := store.ListEpisodes(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Text != "first" {
		t.Errorf("duplicate append mutated the file: %+v", recs)
	}
}

func TestUpdateEpisodes(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()
	for _, id := range []string{"01AAAA", "01BBBB"} {
		if err := store.AppendEpisode(ctx, key, testRecord(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	err := store.UpdateEpisodes(ctx, key, []string{"01BBBB"}, func(r episodic.Record) episodic.Record {
		r.Consolidated = true
		return r
	})
	if err != nil {
		t.Fatalf("UpdateEpisodes: %v", err)
	}

	recs, err := store.ListEpisodes(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].Consolidated {
		t.Error("untouched record was mutated")
	}
	if !recs[1].Consolidated {
		t.Error("targeted record was not updated")
	}
}

func TestUpdateEpisodesRejects(t *testing.T) {
	identity := func(r episodic.Record) episodic.Record { return r }
	rewriteID := func(r episodic.Record) episodic.Record {
		r.ID = "01XXXX"
		return r
	}
	tests := []struct {
		name     string
		ids      []string
		fn       func(episodic.Record) episodic.Record
		wantKind error
	}{
		// A missing id is caller input: not found.
		{"missing id", []string{"01ZZZZ"}, identity, errs.ErrNotFound},
		// An id-rewriting fn is a caller bug, not client input: internal.
		{"id rewrite", []string{"01AAAA"}, rewriteID, errs.ErrInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := newTestClient(t)
			ctx := context.Background()
			key := testKey()
			if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "keep")); err != nil {
				t.Fatal(err)
			}

			if err := store.UpdateEpisodes(ctx, key, tt.ids, tt.fn); !errors.Is(err, tt.wantKind) {
				t.Fatalf("UpdateEpisodes = %v, want %v", err, tt.wantKind)
			}
			// Neither rejection may reach disk.
			recs, err := store.ListEpisodes(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 1 || recs[0].ID != "01AAAA" || recs[0].Text != "keep" {
				t.Errorf("rejected update leaked to disk: %+v", recs)
			}
		})
	}
}

func TestUpdateEpisodesReportsEveryMissingID(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()
	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); err != nil {
		t.Fatal(err)
	}

	err := store.UpdateEpisodes(ctx, key, []string{"01ZZZZ", "01YYYY"}, func(r episodic.Record) episodic.Record { return r })
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("err %v is not a domain error", err)
	}
	if domain.ID != "01YYYY,01ZZZZ" {
		t.Errorf("ID = %q, want the sorted missing ids", domain.ID)
	}
}

func TestRemoveEpisodes(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()
	for _, id := range []string{"01AAAA", "01BBBB", "01CCCC"} {
		if err := store.AppendEpisode(ctx, key, testRecord(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	// Absent ids are skipped (idempotent aging retry).
	if err := store.RemoveEpisodes(ctx, key, []string{"01BBBB", "01ZZZZ"}); err != nil {
		t.Fatalf("RemoveEpisodes: %v", err)
	}
	recs, err := store.ListEpisodes(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].ID != "01AAAA" || recs[1].ID != "01CCCC" {
		t.Fatalf("unexpected survivors: %+v", recs)
	}

	// Retry with the same ids is a no-op.
	if err := store.RemoveEpisodes(ctx, key, []string{"01BBBB"}); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}

	// Removal from a project that never existed is a no-op too.
	other := ProjectKey{Workspace: "ws", Team: "team", Project: "ghost"}
	if err := store.RemoveEpisodes(ctx, other, []string{"01AAAA"}); err != nil {
		t.Fatalf("remove on missing file: %v", err)
	}

	// Manifest record count tracked the removal.
	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Files[ManifestFileKey(PlaneEpisodic, key)].RecordCount; got != 2 {
		t.Errorf("manifest record_count = %d, want 2", got)
	}
}
