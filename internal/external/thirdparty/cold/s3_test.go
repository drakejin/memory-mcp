package cold

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// codedError stands in for a smithy API error without importing smithy-go
// (which is only an indirect dependency).
type codedError struct {
	code string
}

func (e codedError) Error() string     { return "api error " + e.code }
func (e codedError) ErrorCode() string { return e.code }

// fakeS3API is an in-memory stand-in for the three S3 calls the object store
// makes. Errors are injected per operation.
type fakeS3API struct {
	objects  map[string][]byte
	pageSize int // 0 means a single page
	getErr   error
	headErr  error
	listErr  error
	lastKeys []string // keys requested via GetObject/HeadObject, in order
}

func newFakeS3API() *fakeS3API {
	return &fakeS3API{objects: map[string][]byte{}}
}

func (f *fakeS3API) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.lastKeys = append(f.lastKeys, *in.Key)
	if f.getErr != nil {
		return nil, f.getErr
	}
	data, ok := f.objects[*in.Key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (f *fakeS3API) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.lastKeys = append(f.lastKeys, *in.Key)
	if f.headErr != nil {
		return nil, f.headErr
	}
	if _, ok := f.objects[*in.Key]; !ok {
		return nil, &types.NotFound{}
	}
	return &s3.HeadObjectOutput{}, nil
}

func (f *fakeS3API) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var matched []string
	for k := range f.objects {
		if in.Prefix == nil || strings.HasPrefix(k, *in.Prefix) {
			matched = append(matched, k)
		}
	}
	slices.Sort(matched)

	start := 0
	if in.ContinuationToken != nil {
		idx := slices.Index(matched, *in.ContinuationToken)
		if idx < 0 {
			return nil, errors.New("fake: unknown continuation token")
		}
		start = idx
	}
	end := len(matched)
	if f.pageSize > 0 && start+f.pageSize < end {
		end = start + f.pageSize
	}

	out := &s3.ListObjectsV2Output{}
	for _, k := range matched[start:end] {
		key := k
		out.Contents = append(out.Contents, types.Object{Key: &key})
	}
	if end < len(matched) {
		truncated := true
		next := matched[end]
		out.IsTruncated = &truncated
		out.NextContinuationToken = &next
	}
	return out, nil
}

// fakeUploader records uploads; *manager.Uploader satisfies the same seam.
type fakeUploader struct {
	api *fakeS3API
	err error
}

func (f *fakeUploader) Upload(_ context.Context, in *s3.PutObjectInput, _ ...func(*manager.Uploader)) (*manager.UploadOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.api.objects[*in.Key] = data
	return &manager.UploadOutput{}, nil
}

func newTestS3Objects() (*s3Objects, *fakeS3API, *fakeUploader) {
	api := newFakeS3API()
	up := &fakeUploader{api: api}
	return &s3Objects{api: api, uploader: up, bucket: "vms-memory-mcp"}, api, up
}

func TestS3ObjectsRoundTrip(t *testing.T) {
	ctx := context.Background()
	objects, api, _ := newTestS3Objects()

	if err := objects.Put(ctx, "jin/blobs/ab/abc", strings.NewReader("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if string(api.objects["jin/blobs/ab/abc"]) != "payload" {
		t.Errorf("stored object = %q", api.objects["jin/blobs/ab/abc"])
	}

	rc, err := objects.Get(ctx, "jin/blobs/ab/abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "payload" {
		t.Errorf("Get = %q, want %q", got, "payload")
	}

	exists, err := objects.Exists(ctx, "jin/blobs/ab/abc")
	if err != nil || !exists {
		t.Errorf("Exists = %v, %v; want true, nil", exists, err)
	}
	missing, err := objects.Exists(ctx, "jin/blobs/zz/zzz")
	if err != nil || missing {
		t.Errorf("Exists(absent) = %v, %v; want false, nil", missing, err)
	}
}

// TestS3ObjectsErrorKinds pins the classification the whole degraded-mode story
// rests on: a missing object is not-found, every transport failure is
// unavailable — never the other way round.
func TestS3ObjectsErrorKinds(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		arrange func(*fakeS3API, *fakeUploader)
		call    func(*s3Objects) error
		want    error
	}{
		{
			name:    "get of a missing key is not found",
			arrange: func(*fakeS3API, *fakeUploader) {},
			call: func(o *s3Objects) error {
				_, err := o.Get(ctx, "jin/episodic/absent.json")
				return err
			},
			want: errs.ErrNotFound,
		},
		{
			name:    "get failure is unavailable",
			arrange: func(a *fakeS3API, _ *fakeUploader) { a.getErr = codedError{code: "AccessDenied"} },
			call: func(o *s3Objects) error {
				_, err := o.Get(ctx, "jin/episodic/x.json")
				return err
			},
			want: errs.ErrUnavailable,
		},
		{
			name:    "head failure is unavailable",
			arrange: func(a *fakeS3API, _ *fakeUploader) { a.headErr = codedError{code: "SlowDown"} },
			call: func(o *s3Objects) error {
				_, err := o.Exists(ctx, "jin/blobs/ab/abc")
				return err
			},
			want: errs.ErrUnavailable,
		},
		{
			name:    "list failure is unavailable",
			arrange: func(a *fakeS3API, _ *fakeUploader) { a.listErr = codedError{code: "NoSuchBucket"} },
			call: func(o *s3Objects) error {
				_, err := o.List(ctx, "jin/episodic/")
				return err
			},
			want: errs.ErrUnavailable,
		},
		{
			name:    "upload failure is unavailable",
			arrange: func(_ *fakeS3API, u *fakeUploader) { u.err = errors.New("connection reset") },
			call: func(o *s3Objects) error {
				return o.Put(ctx, "jin/blobs/ab/abc", strings.NewReader("x"))
			},
			want: errs.ErrUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects, api, up := newTestS3Objects()
			tt.arrange(api, up)

			err := tt.call(objects)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want kind %v", err, tt.want)
			}
			var domain *errs.Error
			if !errors.As(err, &domain) {
				t.Fatalf("err = %v, want *errs.Error", err)
			}
			if domain.Op == "" {
				t.Error("error must carry an Op")
			}
			if domain.Fields["bucket"] != "vms-memory-mcp" {
				t.Errorf("fields = %v, want the bucket recorded", domain.Fields)
			}
		})
	}
}

func TestS3ObjectsListPaginates(t *testing.T) {
	ctx := context.Background()
	objects, api, _ := newTestS3Objects()
	api.pageSize = 2
	for _, k := range []string{
		"jin/episodic/vms/core/memory/2026-06.json",
		"jin/episodic/vms/core/memory/2026-07.json",
		"jin/episodic/vms/core/memory/2026-08.json",
		"jin/knowledge/vms/core/memory/latest.json",
	} {
		api.objects[k] = []byte("{}")
	}

	keys, err := objects.List(ctx, "jin/episodic/vms/core/memory/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{
		"jin/episodic/vms/core/memory/2026-06.json",
		"jin/episodic/vms/core/memory/2026-07.json",
		"jin/episodic/vms/core/memory/2026-08.json",
	}
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want %v (all pages, prefix honoured)", keys, want)
	}
}

// TestIsNotFoundErr pins the classification that makes a missing monthly batch
// an empty slice instead of a hard failure (downloadBatch) and a missing blob a
// not-found the document layer can fall back on.
func TestIsNotFoundErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "GetObject NoSuchKey", err: &types.NoSuchKey{}, want: true},
		{name: "HeadObject NotFound", err: &types.NotFound{}, want: true},
		{
			name: "wrapped NoSuchKey still classified",
			err:  fmt.Errorf("get s3://b/k: %w", &types.NoSuchKey{}),
			want: true,
		},
		{
			name: "wrapped NotFound still classified",
			err:  fmt.Errorf("operation error S3: HeadObject: %w", &types.NotFound{}),
			want: true,
		},
		{name: "coded NoSuchKey", err: codedError{code: codeNoSuchKey}, want: true},
		{name: "coded NotFound", err: codedError{code: codeNotFound}, want: true},
		{name: "coded 404", err: codedError{code: codeHTTP404}, want: true},
		// Everything below must NOT be swallowed as "missing": treating an
		// auth/redirect/throttle failure as an empty batch would silently drop
		// already-archived records on the next merge-and-overwrite.
		{name: "access denied is not missing", err: codedError{code: "AccessDenied"}, want: false},
		{name: "permanent redirect is not missing", err: codedError{code: "PermanentRedirect"}, want: false},
		{name: "slow down is not missing", err: codedError{code: "SlowDown"}, want: false},
		{name: "no such bucket is not missing", err: codedError{code: "NoSuchBucket"}, want: false},
		{name: "plain error is not missing", err: errors.New("connection reset"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFoundErr(tt.err); got != tt.want {
				t.Errorf("isNotFoundErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
