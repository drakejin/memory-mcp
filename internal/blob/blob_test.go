package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestCache(t *testing.T) (*FileCache, string) {
	t.Helper()
	dir := t.TempDir()
	return New(filepath.Join(dir, "blobs")), filepath.Join(dir, "blobs")
}

func shaOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestPut(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"ascii", []byte("hello world")},
		{"empty", []byte{}},
		{"binary", []byte{0x00, 0xff, 0x10, 0x80}},
		{"korean", []byte("보안을 끄고 테스트한다")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, dir := newTestCache(t)
			ctx := context.Background()

			sha, size, err := cache.Put(ctx, bytes.NewReader(tt.data))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if want := shaOf(tt.data); sha != want {
				t.Errorf("sha = %q, want %q", sha, want)
			}
			if size != int64(len(tt.data)) {
				t.Errorf("size = %d, want %d", size, len(tt.data))
			}
			// Stored flat at {dir}/{sha} (§1 layout).
			if _, err := os.Stat(filepath.Join(dir, sha)); err != nil {
				t.Errorf("blob not at %s/%s: %v", dir, sha, err)
			}
			assertNoTempFiles(t, dir)
		})
	}
}

func TestPutIdempotent(t *testing.T) {
	cache, dir := newTestCache(t)
	ctx := context.Background()
	data := []byte("same content twice")

	sha1, size1, err := cache.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sha2, size2, err := cache.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("re-put: %v", err)
	}
	if sha1 != sha2 || size1 != size2 {
		t.Errorf("re-put mismatch: (%s,%d) vs (%s,%d)", sha1, size1, sha2, size2)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly one file after re-put, got %d", len(entries))
	}
}

func TestGetRoundTrip(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()
	data := []byte("round trip payload")

	sha, _, err := cache.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := cache.Get(ctx, sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get returned %q, want %q", got, data)
	}
}

func TestGetMissing(t *testing.T) {
	cache, _ := newTestCache(t)
	missing := shaOf([]byte("never stored"))
	_, err := cache.Get(context.Background(), missing)
	if !errors.Is(err, ErrNotCached) {
		t.Errorf("Get missing = %v, want ErrNotCached", err)
	}
}

func TestInvalidSHARejected(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()
	invalid := []struct {
		name string
		sha  string
	}{
		{"empty", ""},
		{"short", "abc123"},
		{"uppercase", strings.ToUpper(shaOf([]byte("x")))},
		{"traversal", "../../etc/passwd"},
		{"non-hex", strings.Repeat("zz", 32)},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := cache.Get(ctx, tt.sha); err == nil || errors.Is(err, ErrNotCached) {
				t.Errorf("Get(%q) = %v, want validation error", tt.sha, err)
			}
			if _, err := cache.Has(ctx, tt.sha); err == nil {
				t.Errorf("Has(%q) accepted invalid sha", tt.sha)
			}
			if err := cache.Evict(ctx, tt.sha); err == nil {
				t.Errorf("Evict(%q) accepted invalid sha", tt.sha)
			}
		})
	}
}

func TestHas(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()

	sha, _, err := cache.Put(ctx, bytes.NewReader([]byte("present")))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := cache.Has(ctx, sha)
	if err != nil || !ok {
		t.Errorf("Has(present) = %v, %v; want true, nil", ok, err)
	}
	ok, err = cache.Has(ctx, shaOf([]byte("absent")))
	if err != nil || ok {
		t.Errorf("Has(absent) = %v, %v; want false, nil", ok, err)
	}
}

func TestEvict(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()

	sha, _, err := cache.Put(ctx, bytes.NewReader([]byte("evict me")))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Evict(ctx, sha); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := cache.Get(ctx, sha); !errors.Is(err, ErrNotCached) {
		t.Errorf("post-evict Get = %v, want ErrNotCached", err)
	}
	// Evicting an absent sha is a no-op (cold backstop remains).
	if err := cache.Evict(ctx, sha); err != nil {
		t.Errorf("second Evict = %v, want nil", err)
	}
}

func TestPutFailingReaderLeavesNoLitter(t *testing.T) {
	cache, dir := newTestCache(t)
	ctx := context.Background()

	_, _, err := cache.Put(ctx, io.MultiReader(
		bytes.NewReader([]byte("partial")),
		failingReader{},
	))
	if err == nil {
		t.Fatal("expected error from failing reader")
	}
	assertNoTempFiles(t, dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("partial write became visible: %v", entries)
	}
}

func TestCancelledContextRejected(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sha := shaOf([]byte("x"))

	tests := []struct {
		name string
		call func() error
	}{
		{"Put", func() error { _, _, err := cache.Put(ctx, bytes.NewReader([]byte("x"))); return err }},
		{"Get", func() error { _, err := cache.Get(ctx, sha); return err }},
		{"Has", func() error { _, err := cache.Has(ctx, sha); return err }},
		{"Evict", func() error { return cache.Evict(ctx, sha) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, context.Canceled) {
				t.Errorf("%s with cancelled ctx = %v, want context.Canceled", tt.name, err)
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stream broke") }

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file litter: %s", e.Name())
		}
	}
}
