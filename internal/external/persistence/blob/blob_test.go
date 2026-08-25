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

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// newTestClient returns a cache rooted at a fresh temp dir, plus that dir.
func newTestClient(t *testing.T) (Client, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "blobs")
	c, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, dir
}

func shaOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{Dir: t.TempDir()}, false},
		{"missing dir", Config{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg)
			if tt.wantErr {
				if !errors.Is(err, errs.ErrInvalid) {
					t.Fatalf("New = %v, want ErrInvalid", err)
				}
				if c != nil {
					t.Error("New returned a client alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if c == nil {
				t.Fatal("New returned nil client without error")
			}
		})
	}
}

func TestNewDoesNoIO(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created-yet")
	if _, err := New(Config{Dir: dir}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("New touched the disk: stat = %v", err)
	}
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
			cache, dir := newTestClient(t)
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
	cache, dir := newTestClient(t)
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
	cache, _ := newTestClient(t)
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

func TestGetMissingIsNotFound(t *testing.T) {
	cache, _ := newTestClient(t)
	missing := shaOf([]byte("never stored"))

	_, err := cache.Get(context.Background(), missing)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("err %v is not a domain error", err)
	}
	if domain.Entity != entityBlob || domain.ID != missing {
		t.Errorf("entity/id = %q/%q, want %q/%q", domain.Entity, domain.ID, entityBlob, missing)
	}
}

func TestInvalidSHARejected(t *testing.T) {
	cache, _ := newTestClient(t)
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
			calls := []struct {
				name string
				call func() error
			}{
				{"Get", func() error { _, err := cache.Get(ctx, tt.sha); return err }},
				{"Has", func() error { _, err := cache.Has(ctx, tt.sha); return err }},
				{"Evict", func() error { return cache.Evict(ctx, tt.sha) }},
			}
			for _, c := range calls {
				err := c.call()
				// Invalid, never "not found": a malformed address is caller
				// input, so callers must not fall back to cold storage.
				if !errors.Is(err, errs.ErrInvalid) {
					t.Errorf("%s(%q) = %v, want ErrInvalid", c.name, tt.sha, err)
				}
				if errors.Is(err, errs.ErrNotFound) {
					t.Errorf("%s(%q) reported not found for a malformed sha", c.name, tt.sha)
				}
				if got := publicMsg(err); got != invalidSHAMsg {
					t.Errorf("%s(%q) client message = %q, want the rule statement", c.name, tt.sha, got)
				}
			}
		})
	}
}

func TestHas(t *testing.T) {
	cache, _ := newTestClient(t)
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
	cache, _ := newTestClient(t)
	ctx := context.Background()

	sha, _, err := cache.Put(ctx, bytes.NewReader([]byte("evict me")))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Evict(ctx, sha); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := cache.Get(ctx, sha); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("post-evict Get = %v, want ErrNotFound", err)
	}
	// Evicting an absent sha is a no-op (cold backstop remains).
	if err := cache.Evict(ctx, sha); err != nil {
		t.Errorf("second Evict = %v, want nil", err)
	}
}

func TestPutFailingReaderLeavesNoLitter(t *testing.T) {
	cache, dir := newTestClient(t)
	ctx := context.Background()

	_, _, err := cache.Put(ctx, io.MultiReader(
		bytes.NewReader([]byte("partial")),
		failingReader{},
	))
	if !errors.Is(err, errs.ErrInternal) {
		t.Fatalf("Put with broken reader = %v, want ErrInternal", err)
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

func TestUnwritableDirIsInternal(t *testing.T) {
	if os.Geteuid() == 0 { // root ignores the mode bits this test relies on
		t.Skip("running as root")
	}
	parent := t.TempDir()
	const readOnlyDir = 0o500
	if err := os.Chmod(parent, readOnlyDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, dirPerm) }) // let TempDir clean up

	cache, err := New(Config{Dir: filepath.Join(parent, "blobs")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cache.Put(context.Background(), bytes.NewReader([]byte("x")))
	if !errors.Is(err, errs.ErrInternal) {
		t.Fatalf("Put into unwritable dir = %v, want ErrInternal", err)
	}
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("err %v is not a domain error", err)
	}
	// The failing path is a log-only field, never the client message.
	if domain.Msg != "internal error" {
		t.Errorf("Msg = %q, want the generic internal message", domain.Msg)
	}
	if domain.Fields["path"] == nil {
		t.Error("path field missing from the log payload")
	}
}

func TestCancelledContextRejected(t *testing.T) {
	cache, _ := newTestClient(t)
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
			err := tt.call()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with cancelled ctx = %v, want context.Canceled", tt.name, err)
			}
			if !errors.Is(err, errs.ErrInternal) {
				t.Errorf("%s = %v, want KindInternal", tt.name, err)
			}
		})
	}
}

// publicMsg is the message a client would see: the innermost authored Msg in
// the chain, which is the rule apierr.From applies at the transport boundary.
func publicMsg(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if domain, ok := e.(*errs.Error); ok && domain.Msg != "" {
			return domain.Msg
		}
	}
	return ""
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
	tmpPrefix := strings.TrimSuffix(tmpPattern, "*")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Errorf("temp file litter: %s", e.Name())
		}
	}
}
