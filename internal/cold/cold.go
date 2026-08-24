// Package cold owns S3 archival: aged episodic batches, knowledge snapshots,
// and blob upload/restore (architecture-v2.md §1, §3, §4). The iron rule of
// aging: S3 put must be confirmed BEFORE anything is removed from hot.
package cold

import (
	"context"
	"errors"
	"fmt"
	"io"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrNotFound is returned when a cold object does not exist.
var ErrNotFound = errors.New("cold: object not found")

// Storage abstracts raw S3 object IO so unit tests can fake it. Keys are
// bucket-relative (no leading slash).
type Storage interface {
	// Put uploads r to key, overwriting; bucket versioning is the backstop.
	Put(ctx context.Context, key string, r io.Reader) error
	// Get opens the object at key, or ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Exists reports whether key exists without downloading it.
	Exists(ctx context.Context, key string) (bool, error)
	// List returns keys under prefix (used by /status sync reporting and
	// blackbox cleanup).
	List(ctx context.Context, prefix string) ([]string, error)
}

// S3Storage is the real AWS-backed Storage for bucket vms-memory-mcp.
type S3Storage struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// Compile-time contract check.
var _ Storage = (*S3Storage)(nil)

// NewS3 loads the shared AWS config for the given profile/region and returns
// an S3Storage. No network call is made until first use.
func NewS3(ctx context.Context, profile, region, bucket string) (*S3Storage, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithSharedConfigProfile(profile),
	)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg)
	return &S3Storage{client: client, uploader: manager.NewUploader(client), bucket: bucket}, nil
}

// Put implements Storage.
func (s *S3Storage) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: strptr(s.bucket),
		Key:    strptr(key),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("cold: put s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// Get implements Storage.
func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: strptr(s.bucket),
		Key:    strptr(key),
	})
	if err != nil {
		if isNotFoundErr(err) {
			return nil, fmt.Errorf("cold: get s3://%s/%s: %w", s.bucket, key, ErrNotFound)
		}
		return nil, fmt.Errorf("cold: get s3://%s/%s: %w", s.bucket, key, err)
	}
	return out.Body, nil
}

// Exists implements Storage.
func (s *S3Storage) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: strptr(s.bucket),
		Key:    strptr(key),
	})
	if err != nil {
		if isNotFoundErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("cold: head s3://%s/%s: %w", s.bucket, key, err)
	}
	return true, nil
}

// List implements Storage.
func (s *S3Storage) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: strptr(s.bucket),
		Prefix: strptr(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("cold: list s3://%s/%s: %w", s.bucket, prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	return keys, nil
}

// apiCoder matches smithy API errors without importing smithy-go directly.
type apiCoder interface {
	ErrorCode() string
}

// isNotFoundErr reports whether an S3 error means "object does not exist".
// HeadObject surfaces types.NotFound, GetObject surfaces types.NoSuchKey; the
// code check is a belt-and-braces fallback for either shape.
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
		code := coder.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound" || code == "404"
	}
	return false
}

func strptr(s string) *string { return &s }
