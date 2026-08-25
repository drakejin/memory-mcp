package httpserver_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	app "github.com/drakejin/memory-mcp/internal/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// TestNewRejectsUnservableConfig pins the constructor contract: the intrinsic
// dependencies must be present, and the §7 trust boundary ("no auth is safe
// because it is not reachable") is enforced here too, not only in config.Load.
// A derived store is deliberately not required — §5 says a dead one degrades
// the server rather than stopping it.
func TestNewRejectsUnservableConfig(t *testing.T) {
	base := func() app.Config {
		return app.Config{
			ListenAddr:      testListenAddr,
			S3Bucket:        testS3Bucket,
			EpisodicTTLDays: testTTLDays,
			Clock:           fakeClock{now: fixedNow},
			IDs:             &fakeIDs{},
			Logger:          discardLogger(),
		}
	}

	tests := []struct {
		name    string
		mutate  func(*app.Config)
		wantErr bool
	}{
		{"loopback ip", nil, false},
		{"localhost name", func(c *app.Config) { c.ListenAddr = "localhost:8420" }, false},
		{"every derived store absent is legal", func(c *app.Config) { c.Store, c.Index, c.Graph = nil, nil, nil }, false},
		{"all interfaces bound", func(c *app.Config) {
			c.Store, c.Index, c.Graph = newFakeStore(), newFakeIndex(), newFakeGraph()
		}, false},
		{"empty listen addr", func(c *app.Config) { c.ListenAddr = "" }, true},
		{"wildcard bind", func(c *app.Config) { c.ListenAddr = "0.0.0.0:8420" }, true},
		{"routable bind", func(c *app.Config) { c.ListenAddr = "192.168.0.10:8420" }, true},
		{"zero ttl", func(c *app.Config) { c.EpisodicTTLDays = 0 }, true},
		{"negative ttl", func(c *app.Config) { c.EpisodicTTLDays = -1 }, true},
		{"no clock", func(c *app.Config) { c.Clock = nil }, true},
		{"no id generator", func(c *app.Config) { c.IDs = nil }, true},
		{"no logger", func(c *app.Config) { c.Logger = nil }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			srv, err := app.New(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if srv != nil {
					t.Error("a rejected config must not yield a server")
				}
				// A constructor is below the transport boundary, so it speaks
				// the domain vocabulary (§2.1), not HTTP.
				if !errors.Is(err, errs.ErrInvalid) {
					t.Errorf("error = %v, want errs.ErrInvalid", err)
				}
				return
			}
			if srv == nil {
				t.Fatal("New returned no server and no error")
			}
		})
	}
}

// specPaths fetches the served OpenAPI document and returns its paths object.
func specPaths(t *testing.T) map[string]map[string]any {
	t.Helper()
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/swagger/doc.json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("swagger doc.json status = %d, want 200 (run `make swagger`)", rec.Code)
	}
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("swagger doc.json is not JSON: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("swagger spec declares no paths")
	}
	return spec.Paths
}

// TestEveryRouteIsAnnotated pins the §7 endpoint table to the generated spec:
// a route added to Router without a swaggo block fails here.
func TestEveryRouteIsAnnotated(t *testing.T) {
	want := []struct{ path, method string }{
		{"/healthz", "get"},
		{"/v1/status", "get"},
		{"/v1/consolidate", "post"},
		{"/v1/reindex", "post"},
		{"/v1/documents/{sha}", "get"},
		{"/v1/documents/{sha}/chunks", "get"},
		{"/v1/{ws}/{team}/{proj}/episodes", "post"},
		{"/v1/{ws}/{team}/{proj}/episodes/search", "get"},
		{"/v1/{ws}/{team}/{proj}/episodes/{id}", "get"},
		{"/v1/{ws}/{team}/{proj}/knowledge/nodes", "post"},
		{"/v1/{ws}/{team}/{proj}/knowledge/nodes/{id}", "patch"},
		{"/v1/{ws}/{team}/{proj}/knowledge/nodes/{id}", "delete"},
		{"/v1/{ws}/{team}/{proj}/knowledge/edges", "post"},
		{"/v1/{ws}/{team}/{proj}/knowledge/search", "get"},
		{"/v1/{ws}/{team}/{proj}/knowledge/graph", "get"},
		{"/v1/{ws}/{team}/{proj}/documents", "post"},
	}

	paths := specPaths(t)
	for _, tc := range want {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			ops, ok := paths[tc.path]
			if !ok {
				t.Fatalf("%s is missing from the OpenAPI spec", tc.path)
			}
			if _, ok := ops[tc.method]; !ok {
				t.Fatalf("%s %s is missing from the OpenAPI spec", tc.method, tc.path)
			}
		})
	}
	if len(paths) != 15 {
		t.Errorf("spec declares %d paths, want the 15 of §7 — update this test when the API changes", len(paths))
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
