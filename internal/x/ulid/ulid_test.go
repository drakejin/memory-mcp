package ulid

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// Note: v1 (test/) shipped no ulid.test.ts; these table-driven tests encode the
// contract of src/ulid.ts directly.

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// failingReader is an entropy source that is always broken, so the generator
// must report an error instead of panicking (code-standards §3).
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy device gone") }

// newTestClient builds a Client over a fixed clock and, unless overridden, the
// real entropy source.
func newTestClient(t *testing.T, clock Clock, entropy io.Reader) Client {
	t.Helper()
	c, err := New(Config{Clock: clock, Entropy: entropy})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return c
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	c, err := New(Config{})
	if err == nil {
		t.Fatal("New() = nil error, want invalid config")
	}
	if !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("New() error = %v, want kind invalid", err)
	}
	if c != nil {
		t.Error("New() must not return a client alongside an error")
	}
}

func TestNewDefaultsToCryptoRand(t *testing.T) {
	c := newTestClient(t, &fakeClock{now: time.Unix(0, 0)}, nil)
	id, err := c.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v", err)
	}
	if !Valid(id) {
		t.Errorf("Generate() = %q, not a valid ULID", id)
	}
}

func TestGenerateAtShape(t *testing.T) {
	tests := []struct {
		name   string
		millis int64
	}{
		{name: "epoch", millis: 0},
		{name: "recent", millis: 1_750_000_000_000},
		{name: "far future", millis: 4_000_000_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, &fakeClock{}, nil)
			id, err := c.GenerateAt(tt.millis)
			if err != nil {
				t.Fatalf("GenerateAt(%d) = %v", tt.millis, err)
			}
			if len(id) != encodedLen {
				t.Fatalf("GenerateAt(%d) length = %d, want %d", tt.millis, len(id), encodedLen)
			}
			if !Valid(id) {
				t.Fatalf("GenerateAt(%d) = %q, not a valid ULID", tt.millis, id)
			}
		})
	}
}

func TestGenerateUsesTheInjectedClock(t *testing.T) {
	clock := &fakeClock{now: time.UnixMilli(1_750_000_000_000).UTC()}
	c := newTestClient(t, clock, nil)

	earlier, err := c.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v", err)
	}
	clock.now = clock.now.Add(time.Second)
	later, err := c.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v", err)
	}
	if earlier >= later {
		t.Fatalf("clock advance must sort forward: %q >= %q", earlier, later)
	}
	if earlier[:timeLen] != encodeTime(1_750_000_000_000) {
		t.Errorf("time half = %q, want the injected clock's stamp", earlier[:timeLen])
	}
}

func TestGenerateAtRejectsPreEpochTimestamps(t *testing.T) {
	c := newTestClient(t, &fakeClock{}, nil)
	// A negative stamp used to index the alphabet out of range and panic;
	// library code returns an error instead (code-standards §3).
	if _, err := c.GenerateAt(-1); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("GenerateAt(-1) error = %v, want kind invalid", err)
	}
}

func TestGenerateAtReportsEntropyFailure(t *testing.T) {
	c := newTestClient(t, &fakeClock{}, failingReader{})

	id, err := c.GenerateAt(1)
	if err == nil {
		t.Fatal("GenerateAt() = nil error, want the entropy failure surfaced")
	}
	if !errors.Is(err, errs.ErrInternal) {
		t.Errorf("GenerateAt() error = %v, want kind internal", err)
	}
	if id != "" {
		t.Errorf("GenerateAt() = %q, want no id alongside an error", id)
	}
}

func TestEncodeTimeKnownValues(t *testing.T) {
	tests := []struct {
		name   string
		millis int64
		want   string
	}{
		{name: "zero", millis: 0, want: "0000000000"},
		{name: "one", millis: 1, want: "0000000001"},
		{name: "thirty-two", millis: 32, want: "0000000010"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encodeTime(tt.millis); got != tt.want {
				t.Fatalf("encodeTime(%d) = %q, want %q", tt.millis, got, tt.want)
			}
		})
	}
}

func TestMonotonicWithinSameMillisecond(t *testing.T) {
	const n = 100
	const millis = int64(1_750_000_000_123)
	c := newTestClient(t, &fakeClock{}, nil)

	ids := make([]string, 0, n)
	for range n {
		id, err := c.GenerateAt(millis)
		if err != nil {
			t.Fatalf("GenerateAt(%d) = %v", millis, err)
		}
		ids = append(ids, id)
	}

	if !slices.IsSorted(ids) {
		t.Fatalf("ids not monotonic within one millisecond: %v", ids)
	}
	seen := make(map[string]struct{}, n)
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ulid %q within same millisecond", id)
		}
		seen[id] = struct{}{}
	}
}

func TestMonotonicUnderConcurrentUse(t *testing.T) {
	const workers = 8
	const perWorker = 50
	const millis = int64(1_750_000_000_321)
	c := newTestClient(t, &fakeClock{}, nil)

	var mu sync.Mutex
	ids := make([]string, 0, workers*perWorker)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				id, err := c.GenerateAt(millis)
				if err != nil {
					t.Errorf("GenerateAt() = %v", err)
					return
				}
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	slices.Sort(ids)
	if len(slices.Compact(slices.Clone(ids))) != len(ids) {
		t.Fatalf("concurrent minting produced duplicates among %d ids", len(ids))
	}
}

func TestTimeOrdering(t *testing.T) {
	c := newTestClient(t, &fakeClock{}, nil)
	earlier, err := c.GenerateAt(1_750_000_000_000)
	if err != nil {
		t.Fatalf("GenerateAt() = %v", err)
	}
	later, err := c.GenerateAt(1_750_000_000_001)
	if err != nil {
		t.Fatalf("GenerateAt() = %v", err)
	}
	if earlier >= later {
		t.Fatalf("ulid at earlier millis %q should sort before %q", earlier, later)
	}
}

func TestRandomHalfIsRedrawnOnANewMillisecond(t *testing.T) {
	// A fixed entropy stream makes the random half deterministic, so the test
	// can assert that a new millisecond redraws rather than increments.
	entropy := bytes.NewReader(bytes.Repeat([]byte{0}, 2*randomLen))
	c := newTestClient(t, &fakeClock{}, entropy)

	first, err := c.GenerateAt(1)
	if err != nil {
		t.Fatalf("GenerateAt() = %v", err)
	}
	second, err := c.GenerateAt(2)
	if err != nil {
		t.Fatalf("GenerateAt() = %v", err)
	}
	if first[timeLen:] != second[timeLen:] {
		t.Errorf("random halves = %q / %q, want both redrawn from the same stream", first[timeLen:], second[timeLen:])
	}

	// The stream is now exhausted: the next new millisecond must fail rather
	// than silently reuse the previous randomness.
	if _, err := c.GenerateAt(3); err == nil {
		t.Error("GenerateAt() = nil error, want the exhausted entropy reported")
	}
}

func TestIncrementRandomCarries(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  []byte
	}{
		{name: "low digit", input: []byte{0, 0, 0}, want: []byte{0, 0, 1}},
		{name: "single carry", input: []byte{0, 0, radix - 1}, want: []byte{0, 1, 0}},
		{name: "overflow wraps", input: []byte{radix - 1, radix - 1}, want: []byte{0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := slices.Clone(tt.input)
			got := incrementRandom(input)
			if !slices.Equal(got, tt.want) {
				t.Errorf("incrementRandom(%v) = %v, want %v", tt.input, got, tt.want)
			}
			if !slices.Equal(input, tt.input) {
				t.Errorf("incrementRandom mutated its input: %v", input)
			}
		})
	}
}

func TestValid(t *testing.T) {
	c := newTestClient(t, &fakeClock{now: time.UnixMilli(1_750_000_000_000)}, nil)
	generated, err := c.Generate()
	if err != nil {
		t.Fatalf("Generate() = %v", err)
	}

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "valid generated", value: generated, want: true},
		{name: "too short", value: "01ARZ3NDEKTSV4RRFFQ69G5FA", want: false},
		{name: "too long", value: "01ARZ3NDEKTSV4RRFFQ69G5FAVX", want: false},
		{name: "lowercase rejected", value: "01arz3ndektsv4rrffq69g5fav", want: false},
		{name: "excluded letter I", value: "01ARZ3NDEKTSV4RRFFQ69G5FIV", want: false},
		{name: "excluded letter L", value: "01ARZ3NDEKTSV4RRFFQ69G5FLV", want: false},
		{name: "excluded letter O", value: "01ARZ3NDEKTSV4RRFFQ69G5FOV", want: false},
		{name: "excluded letter U", value: "01ARZ3NDEKTSV4RRFFQ69G5FUV", want: false},
		{name: "empty", value: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Valid(tt.value); got != tt.want {
				t.Fatalf("Valid(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
