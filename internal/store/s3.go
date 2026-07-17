package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3 is the durable Store implementation backed by an S3-compatible bucket
// (AWS S3, MinIO, …). Puts overwrite unconditionally — keys are
// content-addressed, so rewriting the same content is a no-op.
type S3 struct {
	client   *s3.Client
	uploader *transfermanager.Client
	bucket   string
	prefix   string // optional key prefix inside the bucket; "" or ends with "/"
}

var _ Store = (*S3)(nil)

// NewS3 returns a Store over the given bucket. prefix, if non-empty, is
// prepended to every key (a "/" separator is added if missing) — it scopes
// the store to a corner of a shared bucket.
func NewS3(client *s3.Client, bucket, prefix string) *S3 {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &S3{
		client:   client,
		uploader: transfermanager.New(client),
		bucket:   bucket,
		prefix:   prefix,
	}
}

// ParseBucket splits the storage-root spec "bucket[/base/path]" into the S3
// bucket name and an optional key prefix. S3 bucket names cannot contain "/", so
// the first segment is the bucket and the remainder is a prefix prepended to
// every key — scoping a whole forge (objects, CI logs, and, via the caller, the
// metadata replica) under one root of a bucket it may share.
func ParseBucket(raw string) (bucket, prefix string) {
	raw = strings.Trim(raw, "/")
	bucket, prefix, _ = strings.Cut(raw, "/")
	return bucket, prefix
}

// NewS3FromEnv builds an S3 store from the environment:
//
//   - GITMOTE_S3_BUCKET   — storage root, "bucket" or "bucket/base/path" (required)
//   - GITMOTE_S3_ENDPOINT — custom endpoint, e.g. a local MinIO (optional;
//     implies path-style addressing)
//
// Region and credentials come from the standard AWS environment
// (AWS_REGION, AWS_ACCESS_KEY_ID, …) and config files; the region defaults to
// us-east-1 when unset (ignored by most S3-compatible stores).
func NewS3FromEnv(ctx context.Context) (*S3, error) {
	bucket, prefix := ParseBucket(os.Getenv("GITMOTE_S3_BUCKET"))
	if bucket == "" {
		return nil, errors.New("store: GITMOTE_S3_BUCKET is not set")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithDefaultRegion("us-east-1"))
	if err != nil {
		return nil, fmt.Errorf("store: load aws config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint := os.Getenv("GITMOTE_S3_ENDPOINT"); endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			// Virtual-host addressing needs DNS per bucket; local
			// S3-compatible stores like MinIO want path-style.
			o.UsePathStyle = true
		}
	})
	return NewS3(client, bucket, prefix), nil
}

// Put implements Store. The uploader streams r, switching to multipart for
// large bodies (packfiles), so r need not be seekable.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.prefix + key),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("store: put %s: %w", key, err)
	}
	return nil
}

// Get implements Store.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.prefix + key),
	})
	if err != nil {
		var noKey *types.NoSuchKey
		if errors.As(err, &noKey) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get %s: %w", key, err)
	}
	return out.Body, nil
}

// Exists implements Store.
func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.prefix + key),
	})
	if err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("store: exists %s: %w", key, err)
	}
	return true, nil
}

// List implements Store.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefix + prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("store: list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, strings.TrimPrefix(aws.ToString(obj.Key), s.prefix))
		}
	}
	return keys, nil
}
