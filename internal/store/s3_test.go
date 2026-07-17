package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestS3Conformance runs the shared suite against a real S3-compatible
// endpoint (MinIO in dev). Gated by env: skipped unless GITMOTE_TEST_S3 is
// set; GITMOTE_S3_BUCKET, GITMOTE_S3_ENDPOINT, and the standard AWS_* vars
// configure the target.
func TestS3Conformance(t *testing.T) {
	if os.Getenv("GITMOTE_TEST_S3") == "" {
		t.Skip("set GITMOTE_TEST_S3=1 (plus GITMOTE_S3_BUCKET, GITMOTE_S3_ENDPOINT, AWS_*) to run against MinIO/S3")
	}
	testConformance(t, newTestS3)
}

// newTestS3 builds an S3 store scoped to a unique prefix so concurrent test
// runs can share a bucket, and deletes everything under it on cleanup.
func TestParseBucket(t *testing.T) {
	tests := []struct {
		raw, bucket, prefix string
	}{
		{"gitmote", "gitmote", ""},
		{"shared/gitmote", "shared", "gitmote"},
		{"shared/a/b", "shared", "a/b"},
		{"/gitmote/", "gitmote", ""},
		{"shared/gitmote/", "shared", "gitmote"},
		{"", "", ""},
	}
	for _, tt := range tests {
		b, p := ParseBucket(tt.raw)
		if b != tt.bucket || p != tt.prefix {
			t.Errorf("ParseBucket(%q) = (%q, %q), want (%q, %q)", tt.raw, b, p, tt.bucket, tt.prefix)
		}
	}
}

func newTestS3(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// Scope this run to a unique sub-path of the operator's bucket by extending the
	// storage-root spec — concurrent runs share a bucket without colliding.
	base := os.Getenv("GITMOTE_S3_BUCKET")
	t.Setenv("GITMOTE_S3_BUCKET", fmt.Sprintf("%s/conformance/%s/%s", base, t.Name(), hex.EncodeToString(suffix[:])))

	s, err := NewS3FromEnv(ctx)
	if err != nil {
		t.Fatalf("NewS3FromEnv: %v", err)
	}
	t.Cleanup(func() {
		keys, err := s.List(ctx, "")
		if err != nil {
			t.Errorf("cleanup list: %v", err)
			return
		}
		for _, key := range keys {
			_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(s.prefix + key),
			})
			if err != nil {
				t.Errorf("cleanup delete %s: %v", key, err)
			}
		}
	})
	return s
}
