package search

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

// schemaMu exists so "concurrent upserts cannot race index creation". Every
// other schema test drives it serially, which never exercises the mutex. These
// issue the burst for real: whatever the interleaving, the expensive
// convergence must happen exactly once.

// burst runs fn(i) for i in [0,n) behind a shared start barrier and waits.
func burst(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}

// countMethod counts recorded calls with the given HTTP method.
func countMethod(calls []fakeCall, method string) int {
	n := 0
	for _, c := range calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func TestConcurrentEnsureIndexCreatesOnce(t *testing.T) {
	// Arrange: the index is absent on the first HEAD and present afterwards,
	// which is what a real cluster reports once one caller has created it.
	var mu sync.Mutex
	created := false
	ft := &fakeTransport{handler: func(call fakeCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		switch call.Method {
		case http.MethodHead:
			if created {
				return http.StatusOK, `{}`
			}
			return http.StatusNotFound, `{}`
		case http.MethodPut:
			created = true
			return http.StatusOK, `{"acknowledged":true}`
		default: // GET /_settings
			return http.StatusOK, settingsMapped
		}
	}}
	c := newTestClient(t, ft)

	// Act
	const callers = 16
	errsOut := make([]error, callers)
	burst(callers, func(i int) { errsOut[i] = c.EnsureIndex(context.Background()) })

	// Assert
	for i, err := range errsOut {
		if err != nil {
			t.Fatalf("EnsureIndex %d: %v", i, err)
		}
	}
	if puts := countMethod(ft.recorded(), http.MethodPut); puts != 1 {
		t.Fatalf("index was PUT %d times under %d concurrent callers, want exactly 1 — schemaMu did not serialise creation", puts, callers)
	}
	if !c.schemaReady {
		t.Error("schemaReady not latched after a successful concurrent EnsureIndex")
	}
}

func TestConcurrentEnsureSchemaConvergesOnce(t *testing.T) {
	// Arrange: an unlatched client whose index already exists with the nori
	// mapping. The settings read is the expensive step the latch protects.
	ft := &fakeTransport{handler: func(call fakeCall) (int, string) {
		if call.Method == http.MethodGet {
			return http.StatusOK, settingsMapped
		}
		return http.StatusOK, `{}`
	}}
	c := newTestClient(t, ft)

	// Act
	const callers = 16
	errsOut := make([]error, callers)
	burst(callers, func(i int) { errsOut[i] = c.ensureSchema(context.Background(), opEnsureIndex) })

	// Assert
	for i, err := range errsOut {
		if err != nil {
			t.Fatalf("ensureSchema %d: %v", i, err)
		}
	}
	calls := ft.recorded()
	if settings := countMethod(calls, http.MethodGet); settings != 1 {
		t.Fatalf("settings read %d times under %d concurrent callers, want exactly 1 — the latch is not serialised", settings, callers)
	}
	if puts := countMethod(calls, http.MethodPut); puts != 0 {
		t.Errorf("an already-mapped index was recreated %d times", puts)
	}
	// Every latched caller still re-checks existence, by design: the derived
	// container can be swapped under a running server (§1).
	if heads := countMethod(calls, http.MethodHead); heads != callers {
		t.Errorf("HEAD probes = %d, want %d (one per call — the latch must not skip the existence check)", heads, callers)
	}
}
