package ulid

import (
	"sort"
	"testing"
)

// Note: v1 (test/) shipped no ulid.test.ts; these table-driven tests encode the
// contract of src/ulid.ts directly.

func TestAtShape(t *testing.T) {
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
			id := At(tt.millis)
			if len(id) != 26 {
				t.Fatalf("At(%d) length = %d, want 26", tt.millis, len(id))
			}
			if !IsULID(id) {
				t.Fatalf("At(%d) = %q, not a valid ULID", tt.millis, id)
			}
		})
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
	millis := int64(1_750_000_000_123)
	ids := make([]string, 0, n)
	for range n {
		ids = append(ids, At(millis))
	}
	sorted := make([]string, n)
	copy(sorted, ids)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ids not monotonic at %d: generation order %q, sorted order %q", i, ids[i], sorted[i])
		}
	}
	seen := make(map[string]struct{}, n)
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ulid %q within same millisecond", id)
		}
		seen[id] = struct{}{}
	}
}

func TestTimeOrdering(t *testing.T) {
	earlier := At(1_750_000_000_000)
	later := At(1_750_000_000_001)
	if !(earlier < later) {
		t.Fatalf("ulid at earlier millis %q should sort before %q", earlier, later)
	}
}

func TestIsULID(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "valid generated", value: New(), want: true},
		{name: "too short", value: "01ARZ3NDEKTSV4RRFFQ69G5FA", want: false},
		{name: "lowercase rejected", value: "01arz3ndektsv4rrffq69g5fav", want: false},
		{name: "excluded letter I", value: "01ARZ3NDEKTSV4RRFFQ69G5FIV"[:26], want: false},
		{name: "excluded letter L", value: "01ARZ3NDEKTSV4RRFFQ69G5FL", want: false},
		{name: "excluded letter O", value: "01ARZ3NDEKTSV4RRFFQ69G5FO", want: false},
		{name: "excluded letter U", value: "01ARZ3NDEKTSV4RRFFQ69G5FU", want: false},
		{name: "empty", value: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsULID(tt.value); got != tt.want {
				t.Fatalf("IsULID(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
