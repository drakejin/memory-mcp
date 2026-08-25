package rehydrate

import (
	"sync"
	"testing"
	"time"
)

// claimGate is the stampede guard of §5: it exists so "a burst of requests
// cannot stampede the derived stores". Every other test simulates the burst by
// advancing a fake clock between serial calls, which never exercises the mutex
// the guard is built on. This one issues the burst for real.
func TestClaimGateAdmitsExactlyOneOfABurst(t *testing.T) {
	// Arrange
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	svc := newService(t, newFakeStore(), newFakeIndex(), newFakeGraph(), &fakeClock{now: base})

	const callers = 32
	claims := make([]bool, callers)

	// Act: all callers hit the same key inside one debounce window.
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			<-start
			claims[i] = svc.claimGate(testKey)
		}()
	}
	close(start)
	wg.Wait()

	// Assert
	granted := 0
	for _, ok := range claims {
		if ok {
			granted++
		}
	}
	if granted != 1 {
		t.Fatalf("claimGate granted %d claims out of %d concurrent callers, want exactly 1", granted, callers)
	}

	// A different project is a different window: it must not be blocked by the
	// burst above.
	other := testKey
	other.Project = "other"
	if !svc.claimGate(other) {
		t.Error("claimGate denied a different project — the debounce window is not per-key")
	}
}
