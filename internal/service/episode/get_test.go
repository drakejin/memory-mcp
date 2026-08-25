package episode

import (
	"context"
	"errors"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

func TestGet(t *testing.T) {
	hotRec := Record{ID: ulidA, Kind: KindEvent, Actor: ActorAgent, Text: "hot", OccurredAt: fixedNow}
	coldRec := Record{ID: ulidB, Kind: KindEvent, Actor: ActorAgent, Text: "aged to S3", OccurredAt: fixedNow}

	tests := []struct {
		name   string
		id     string
		setup  func(*fixture)
		mutate func(*Config)
		// wantErr is the errs sentinel; nil = success.
		wantErr error
		// wantMsg pins the outermost public message when it matters.
		wantMsg   string
		wantCause error
		wantText  string
		// wantArchiveCalls proves hot-first: a hot hit never consults cold.
		wantArchiveCalls int
	}{
		{
			name: "hot hit never consults the archive",
			id:   ulidA,
			setup: func(f *fixture) {
				f.store.episodes[testKey.String()] = []Record{hotRec}
			},
			wantText: "hot",
		},
		{
			name: "cold fallback keeps provenance resolvable (P11)",
			id:   ulidB,
			setup: func(f *fixture) {
				f.archive.archived[ulidB] = coldRec
			},
			wantText:         "aged to S3",
			wantArchiveCalls: 1,
		},
		{
			name:             "absent everywhere is not found",
			id:               ulidC,
			wantErr:          errs.ErrNotFound,
			wantArchiveCalls: 1,
		},
		{
			name:    "no archive wired names the gap in the not-found",
			id:      ulidC,
			mutate:  func(c *Config) { c.Archive = nil },
			wantErr: errs.ErrNotFound,
			wantMsg: "episode not in hot store; cold archive unavailable",
		},
		{
			name: "archive lookup failure stays internal",
			id:   ulidC,
			setup: func(f *fixture) {
				f.archive.fetchErr = errBoom
			},
			wantErr:          errs.ErrInternal,
			wantCause:        errBoom,
			wantArchiveCalls: 1,
		},
		{
			name: "hot lookup failure stays internal and skips the archive",
			id:   ulidA,
			setup: func(f *fixture) {
				f.store.getErr = errBoom
			},
			wantErr:   errs.ErrInternal,
			wantCause: errBoom,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.setup != nil {
				tc.setup(f)
			}
			svc := f.service(t, tc.mutate)

			res, err := svc.Get(context.Background(), testKey, tc.id)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				if tc.wantMsg != "" {
					if domain := asDomain(t, err); domain.Msg != tc.wantMsg {
						t.Errorf("public message = %q, want %q", domain.Msg, tc.wantMsg)
					}
				}
				if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
					t.Errorf("cause %v lost from chain %v", tc.wantCause, err)
				}
			} else {
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if res.Record.Text != tc.wantText {
					t.Errorf("text = %q, want %q", res.Record.Text, tc.wantText)
				}
				if len(res.Degraded) != 0 {
					t.Errorf("degraded = %v, want none", res.Degraded)
				}
			}
			if f.archive.fetchCalls != tc.wantArchiveCalls {
				t.Errorf("archive calls = %d, want %d (hot-first discipline)", f.archive.fetchCalls, tc.wantArchiveCalls)
			}
		})
	}
}
