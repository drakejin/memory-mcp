package hotstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// fakeClock is the injected Clock every test uses: no wall clock, no flake.
type fakeClock struct{ t time.Time }

func (f fakeClock) Now() time.Time { return f.t }

var testNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

// newTestClient returns a store rooted at a fresh temp home, plus that home.
func newTestClient(t *testing.T) (Client, string) {
	t.Helper()
	home := t.TempDir()
	c, err := New(Config{Home: home, Clock: fakeClock{t: testNow}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, home
}

func testKey() projectkey.Key {
	return projectkey.Key{Workspace: "ws", Team: "team", Project: "proj"}
}

func testRecord(id, text string) episode.Record {
	return episode.Record{
		ID:         id,
		Kind:       episode.KindEvent,
		OccurredAt: testNow,
		Actor:      episode.ActorAgent,
		Text:       text,
		Entities:   []string{"memory-mcp"},
	}
}

// episodicPath is the documented on-disk location of a project's episodic file.
func episodicPath(home string, key projectkey.Key) string {
	return filepath.Join(home, string(PlaneEpisodic), key.Workspace, key.Team, key.Project+jsonExt)
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{Home: t.TempDir(), Clock: fakeClock{t: testNow}}, false},
		{"missing home", Config{Clock: fakeClock{t: testNow}}, true},
		{"missing clock", Config{Home: t.TempDir()}, true},
		{"empty", Config{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg)
			if tt.wantErr {
				if !errors.Is(err, errs.ErrInvalid) {
					t.Fatalf("New = %v, want ErrInvalid", err)
				}
				if c != nil {
					t.Error("New returned a client alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if c == nil {
				t.Fatal("New returned nil client without error")
			}
		})
	}
}

func TestNewDoesNoIO(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-created-yet")
	if _, err := New(Config{Home: home, Clock: fakeClock{t: testNow}}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("New touched the disk: stat = %v", err)
	}
}

func TestSystemClockAdvances(t *testing.T) {
	clock := NewSystemClock()
	before := time.Now()
	got := clock.Now()
	if got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Errorf("Now() = %v, want a wall-clock time near %v", got, before)
	}
}

func TestOperationsRejectInvalidKey(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()
	bad := projectkey.Key{Workspace: "../etc", Team: "t", Project: "p"}

	tests := []struct {
		name string
		call func() error
	}{
		{"AppendEpisode", func() error { return store.AppendEpisode(ctx, bad, testRecord("01A", "x")) }},
		{"ListEpisodes", func() error { _, err := store.ListEpisodes(ctx, bad); return err }},
		{"GetEpisode", func() error { _, err := store.GetEpisode(ctx, bad, "01A"); return err }},
		{"UpdateEpisodes", func() error {
			return store.UpdateEpisodes(ctx, bad, []string{"01A"}, func(r episode.Record) episode.Record { return r })
		}},
		{"RemoveEpisodes", func() error { return store.RemoveEpisodes(ctx, bad, []string{"01A"}) }},
		{"ReadKnowledge", func() error { _, err := store.ReadKnowledge(ctx, bad); return err }},
		{"UpdateKnowledge", func() error { return setKnowledge(ctx, store, bad, knowledge.Graph{}) }},
		{"MarkDirty", func() error { return store.MarkDirty(ctx, bad, PlaneEpisodic) }},
		{"FileInfo", func() error { _, _, err := store.FileInfo(ctx, bad, PlaneEpisodic); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("%s with path-escaping key = %v, want ErrInvalid", tt.name, err)
			}
			// The op names the store entry point, not just the key validator.
			var domain *errs.Error
			if !errors.As(err, &domain) || !strings.HasPrefix(domain.Op, "hotstore.") {
				t.Errorf("op = %q, want a hotstore entry point", domain.Op)
			}
		})
	}
}

func TestCancelledContextRejected(t *testing.T) {
	store, _ := newTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := testKey()

	tests := []struct {
		name string
		call func() error
	}{
		{"AppendEpisode", func() error { return store.AppendEpisode(ctx, key, testRecord("01A", "x")) }},
		{"ListEpisodes", func() error { _, err := store.ListEpisodes(ctx, key); return err }},
		{"GetEpisode", func() error { _, err := store.GetEpisode(ctx, key, "01A"); return err }},
		{"UpdateEpisodes", func() error {
			return store.UpdateEpisodes(ctx, key, nil, func(r episode.Record) episode.Record { return r })
		}},
		{"RemoveEpisodes", func() error { return store.RemoveEpisodes(ctx, key, nil) }},
		{"ReadKnowledge", func() error { _, err := store.ReadKnowledge(ctx, key); return err }},
		{"UpdateKnowledge", func() error { return setKnowledge(ctx, store, key, knowledge.Graph{}) }},
		{"ListProjects", func() error { _, err := store.ListProjects(ctx); return err }},
		{"Manifest", func() error { _, err := store.Manifest(ctx); return err }},
		{"UpdateManifest", func() error {
			return store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) { return m, nil })
		}},
		{"MarkDirty", func() error { return store.MarkDirty(ctx, key, PlaneEpisodic) }},
		{"FileInfo", func() error { _, _, err := store.FileInfo(ctx, key, PlaneEpisodic); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s = %v, want context.Canceled in the chain", tt.name, err)
			}
			if !errors.Is(err, errs.ErrInternal) {
				t.Errorf("%s = %v, want KindInternal", tt.name, err)
			}
		})
	}
}

func TestCorruptFileSurfacesError(t *testing.T) {
	store, home := newTestClient(t)
	ctx := context.Background()
	key := testKey()

	path := episodicPath(home, key)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListEpisodes(ctx, key); !errors.Is(err, errs.ErrInternal) {
		t.Errorf("ListEpisodes on corrupt file = %v, want ErrInternal", err)
	}
	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); !errors.Is(err, errs.ErrInternal) {
		t.Errorf("append over corrupt file = %v, want ErrInternal (never clobber)", err)
	}
	// The failing path is a log-only field; it never becomes the client message.
	var domain *errs.Error
	_, err := store.ListEpisodes(ctx, key)
	if !errors.As(err, &domain) {
		t.Fatalf("err %v is not a domain error", err)
	}
	if domain.Msg != "internal error" {
		t.Errorf("Msg = %q, want the generic internal message", domain.Msg)
	}
	if domain.Fields["path"] != path {
		t.Errorf("Fields[path] = %v, want %q", domain.Fields["path"], path)
	}
}

func TestCorruptManifestSurfacesError(t *testing.T) {
	store, home := newTestClient(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(home, manifestName), []byte("not json"), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Manifest(ctx); !errors.Is(err, errs.ErrInternal) {
		t.Errorf("Manifest on corrupt file = %v, want ErrInternal", err)
	}
}

func TestUnwritableHomeIsInternal(t *testing.T) {
	if os.Geteuid() == 0 { // root ignores the mode bits this test relies on
		t.Skip("running as root")
	}
	store, home := newTestClient(t)
	ctx := context.Background()

	const readOnlyDir = 0o500
	if err := os.Chmod(home, readOnlyDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, dirPerm) }) // let TempDir clean up

	tests := []struct {
		name string
		call func() error
	}{
		{"AppendEpisode", func() error { return store.AppendEpisode(ctx, testKey(), testRecord("01AAAA", "x")) }},
		{"UpdateKnowledge", func() error { return setKnowledge(ctx, store, testKey(), knowledge.Graph{}) }},
		{"UpdateManifest", func() error {
			return store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) { return m, nil })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, errs.ErrInternal) {
				t.Fatalf("%s on unwritable home = %v, want ErrInternal", tt.name, err)
			}
		})
	}
}

func TestNoTempLitterAcrossOperations(t *testing.T) {
	store, home := newTestClient(t)
	ctx := context.Background()
	key := testKey()

	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); err != nil {
		t.Fatal(err)
	}
	if err := setKnowledge(ctx, store, key, knowledge.Graph{}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDirty(ctx, key, PlaneEpisodic); err != nil {
		t.Fatal(err)
	}
	assertNoTempFiles(t, home)
}

// assertNoTempFiles walks dir and fails on any leftover temp file.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	tmpPrefix := strings.TrimSuffix(tmpPattern, "*")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), tmpPrefix) {
			t.Errorf("temp file litter: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
