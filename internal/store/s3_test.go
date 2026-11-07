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
// endpoint, such as Silo (a MinIO fork) in dev. The environment gates it: it
// skips unless GITMOTE_TEST_S3 is set. GITMOTE_S3_BUCKET, GITMOTE_S3_ENDPOINT and the
// standard AWS_* variables configure the target.
func TestS3Conformance(t *testing.T) {
	if os.Getenv("GITMOTE_TEST_S3") == "" {
		t.Skip("set GITMOTE_TEST_S3=1 (plus GITMOTE_S3_BUCKET, GITMOTE_S3_ENDPOINT, AWS_*) to run against MinIO/S3")
	}
	testConformance(t, newTestS3)
}

// TestParseBucket checks that ParseBucket splits a storage-root spec into the
// bucket and the prefix. It also checks that ParseBucket trims leading and
// trailing slashes.
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

// newTestS3 builds an S3 store scoped to a unique prefix, so concurrent test
// runs can share a bucket. On cleanup, it deletes everything under the prefix.
func newTestS3(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// Extend the storage-root spec to scope this run to a unique sub-path of the
	// operator's bucket. Concurrent runs then share a bucket without colliding.
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
