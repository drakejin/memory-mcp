package cold

import (
	"context"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// S3 error codes that mean "the object is not there". GetObject reports
// NoSuchKey, HeadObject reports a bare NotFound; the string codes are the
// belt-and-braces fallback for either shape.
const (
	codeNoSuchKey = "NoSuchKey"
	codeNotFound  = "NotFound"
	codeHTTP404   = "404"
)

// s3API is the slice of the S3 API this package calls directly. Declaring it
// here (rather than depending on *s3.Client) keeps the object store testable
// and satisfies s3.ListObjectsV2APIClient for the paginator.
type s3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// objectUploader is manager.Uploader's Upload method: it streams a body of
// unknown length (switching to multipart when large), which a bare PutObject
// cannot do for an io.Reader.
type objectUploader interface {
	Upload(ctx context.Context, in *s3.PutObjectInput, opts ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

// s3Objects is the AWS-backed objectStore for one bucket.
type s3Objects struct {
	api      s3API
	uploader objectUploader
	bucket   string
}

// Compile-time contract checks.
var (
	_ objectStore               = (*s3Objects)(nil)
	_ s3.ListObjectsV2APIClient = (s3API)(nil)
)

// Put implements objectStore.
func (s *s3Objects) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
		Body:   r,
	})
	if err != nil {
		return s.unavailable(opPut, key, err)
	}
	return nil
}

// Get implements objectStore.
func (s *s3Objects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		if isNotFoundErr(err) {
			return nil, s.notFound(opGet, key)
		}
		return nil, s.unavailable(opGet, key, err)
	}
	return out.Body, nil
}

// Exists implements objectStore.
func (s *s3Objects) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		if isNotFoundErr(err) {
			return false, nil
		}
		return false, s.unavailable(opExists, key, err)
	}
	return true, nil
}

// List implements objectStore.
func (s *s3Objects) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	pages := s3.NewListObjectsV2Paginator(s.api, &s3.ListObjectsV2Input{
		Bucket: &s.bucket,
		Prefix: &prefix,
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, s.unavailable(opList, prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	return keys, nil
}

// unavailable classifies every S3 transport failure as the degraded-mode
// signal, tagging the bucket and key for the log without exposing them to a
// client (Fields never reach a response body).
func (s *s3Objects) unavailable(op, key string, cause error) error {
	return errs.Unavailable(op, cause).
		WithField("bucket", s.bucket).
		WithField("key", key)
}

// notFound reports a missing object; the key is the identifier callers above
// translate into a blob sha or an episode id.
func (s *s3Objects) notFound(op, key string) error {
	return errs.NotFound(op, entityObject, key).WithField("bucket", s.bucket)
}

// apiCoder matches smithy API errors without importing smithy-go directly.
type apiCoder interface {
	ErrorCode() string
}

// isNotFoundErr reports whether an S3 error means "object does not exist".
// Misclassifying here is a correctness bug in both directions: an auth or
// throttle failure read as "missing" would silently drop already-archived
// records on the next merge-and-overwrite.
func isNotFoundErr(err error) bool {
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var coder apiCoder
	if errors.As(err, &coder) {
		switch coder.ErrorCode() {
		case codeNoSuchKey, codeNotFound, codeHTTP404:
			return true
		}
	}
	return false
}
