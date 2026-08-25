package knowledge

// Construction and bookkeeping rules of the service layer.

import (
	"context"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

func TestNewValidatesConfig(t *testing.T) {
	valid := func() Config {
		return Config{
			Store: &fakeHot{log: &callLog{}},
			IDs:   &fakeIDs{},
			Clock: fixedClock{now: testNow},
		}
	}

	tests := []struct {
		name   string
		mutate func(c Config) Config
		wantOK bool
	}{
		{"minimal valid config (nil graph, nil logger)", func(c Config) Config { return c }, true},
		{"nil store", func(c Config) Config { c.Store = nil; return c }, false},
		{"nil id generator", func(c Config) Config { c.IDs = nil; return c }, false},
		{"nil clock", func(c Config) Config { c.Clock = nil; return c }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, err := New(tt.mutate(valid()))
			if tt.wantOK {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if svc == nil {
					t.Fatal("New returned a nil Service")
				}
				return
			}
			assertDomainErr(t, err, errs.ErrInvalid, opNew, entityConfig)
			if svc != nil {
				t.Fatalf("rejected config must return a nil Service, got %T", svc)
			}
		})
	}
}

// TestBookkeepingFailuresAreLogOnly pins the §5 rule: manifest bookkeeping
// must never fail a request whose hot write already succeeded — neither the
// dirty mark of a degraded mirror nor the freshness refresh of a healthy one.
func TestBookkeepingFailuresAreLogOnly(t *testing.T) {
	ctx := context.Background()

	t.Run("mark indexed failure keeps the write green", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		r.hot.markIndexedErr = errBoom

		res, err := r.svc.CreateNode(ctx, testKey, validCreateInput())
		if err != nil {
			t.Fatalf("CreateNode: %v", err)
		}
		if len(res.Degraded) != 0 {
			t.Fatalf("degraded = %v, want none — bookkeeping is not the mirror", res.Degraded)
		}
	})

	t.Run("mark dirty failure keeps the degraded write green", func(t *testing.T) {
		r := newRig(t, Graph{}, false)
		r.hot.markDirtyErr = errBoom

		res, err := r.svc.CreateNode(ctx, testKey, validCreateInput())
		if err != nil {
			t.Fatalf("CreateNode: %v", err)
		}
		if len(res.Degraded) != 1 || res.Degraded[0] != degradedGraph {
			t.Fatalf("degraded = %v, want [%q]", res.Degraded, degradedGraph)
		}
	})
}
