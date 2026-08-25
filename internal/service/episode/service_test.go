package episode

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		// wantErr is a substring of the KindInvalid message; empty = success.
		wantErr string
	}{
		{
			name: "full config",
		},
		{
			name: "optional collaborators may all be nil",
			mutate: func(c *Config) {
				c.Index = nil
				c.Archive = nil
				c.Gate = nil
				c.Logger = nil
			},
		},
		{
			name:    "missing store",
			mutate:  func(c *Config) { c.Store = nil },
			wantErr: "store must be set",
		},
		{
			name:    "missing bookkeeper",
			mutate:  func(c *Config) { c.Bookkeeper = nil },
			wantErr: "bookkeeper must be set",
		},
		{
			name:    "missing clock",
			mutate:  func(c *Config) { c.Clock = nil },
			wantErr: "clock must be set",
		},
		{
			name:    "missing id generator",
			mutate:  func(c *Config) { c.IDs = nil },
			wantErr: "id generator must be set",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newFixture().config()
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			svc, err := New(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if svc == nil {
					t.Fatal("New returned a nil Service without an error")
				}
				return
			}
			if err == nil {
				t.Fatal("New accepted an unservable config")
			}
			if !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("error kind = %v, want errs.ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name the missing field %q", err, tc.wantErr)
			}
			if svc != nil {
				t.Error("New must not hand out a Service alongside an error")
			}
		})
	}
}

// TestNilLoggerDiscardsBestEffortFailures proves the degraded paths stay total
// without a logger: the failure is discarded, the result still reports it.
func TestNilLoggerDiscardsBestEffortFailures(t *testing.T) {
	f := newFixture()
	f.books.dirtyErr = errBoom
	svc := f.service(t, func(c *Config) {
		c.Logger = nil
		c.Index = nil
	})

	res, err := svc.Append(context.Background(), testKey, AppendRequest{
		Kind: KindEvent, Actor: ActorAgent, Text: "t",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(res.Degraded) != 1 || res.Degraded[0] != DegradedSearch {
		t.Fatalf("degraded = %v, want [%s]", res.Degraded, DegradedSearch)
	}
}
