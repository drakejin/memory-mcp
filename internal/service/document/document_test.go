package document

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// TestNewValidatesConfig pins which collaborators are required (the canonical
// plane and the clock) and which are optional (every derived store, whose
// absence is degradation, not a construction error).
func TestNewValidatesConfig(t *testing.T) {
	full := func() Config {
		return Config{
			Store:    newFakeStore(),
			Cache:    newFakeCache(),
			Clock:    fixedClock{t: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)},
			IDs:      &fakeIDs{},
			Archiver: newFakeArchiver(),
			Index:    &fakeIndexer{},
			Graph:    &fakeGraph{},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "complete config", mutate: func(*Config) {}},
		{name: "no archiver is allowed", mutate: func(c *Config) { c.Archiver = nil }},
		{name: "no index is allowed", mutate: func(c *Config) { c.Index = nil }},
		{name: "no graph is allowed", mutate: func(c *Config) { c.Graph = nil }},
		{name: "missing store", mutate: func(c *Config) { c.Store = nil }, wantErr: true},
		{name: "missing cache", mutate: func(c *Config) { c.Cache = nil }, wantErr: true},
		{name: "missing clock", mutate: func(c *Config) { c.Clock = nil }, wantErr: true},
		{name: "missing id generator", mutate: func(c *Config) { c.IDs = nil }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full()
			tt.mutate(&cfg)

			got, err := New(cfg)
			if tt.wantErr {
				if !errors.Is(err, errs.ErrInvalid) {
					t.Fatalf("err = %v, want invalid", err)
				}
				if got != nil {
					t.Error("no Service may be returned alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got == nil {
				t.Fatal("New returned a nil Service")
			}
		})
	}
}

// TestNewDefaultsExtractorAndLogger proves the optional Config fields resolve to
// working defaults instead of nil panics.
func TestNewDefaultsExtractorAndLogger(t *testing.T) {
	svc, err := New(Config{
		Store: newFakeStore(),
		Cache: newFakeCache(),
		Clock: fixedClock{t: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)},
		IDs:   &fakeIDs{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := svc.(*service)
	if !ok {
		t.Fatalf("New returned %T, want *service", svc)
	}
	if _, ok := impl.extractor.(textExtractor); !ok {
		t.Errorf("extractor = %T, want the built-in textExtractor", impl.extractor)
	}
	if impl.log == nil {
		t.Error("logger must default instead of staying nil")
	}
}

// TestIngestInjectedExtractor proves Config.Extractor really replaces the
// built-in one — the seam agents rely on for non-default formats.
func TestIngestInjectedExtractor(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, func(c *Config) { c.Extractor = stubExtractor{text: "extracted by stub"} })

	res, err := r.svc.Ingest(ctx, docKey, "anything.bin", strings.NewReader("raw bytes"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !res.Extractable || len(res.ChunkIDs) != 1 {
		t.Fatalf("result = %+v, want one extracted chunk", res)
	}
	if got := r.store.episodes[docKey.String()][0].Text; got != "extracted by stub" {
		t.Errorf("chunk text = %q, want the injected extractor's output", got)
	}
}

// TestIngestExtractorFailure keeps extraction failures fatal to the ingest: a
// half-ingested document must not be reported as success.
func TestIngestExtractorFailure(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, func(c *Config) {
		c.Extractor = stubExtractor{err: errs.Internal("document.Extract", errors.New("corrupt"))}
	})

	_, err := r.svc.Ingest(ctx, docKey, "broken.pdf", strings.NewReader("%PDF-nope"))
	if !errors.Is(err, errs.ErrInternal) {
		t.Fatalf("err = %v, want internal", err)
	}
	if len(r.store.episodes[docKey.String()]) != 0 {
		t.Error("no chunks may be written when extraction failed")
	}
}
