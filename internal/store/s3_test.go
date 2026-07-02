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
func newTestS3(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv("GITMOTE_S3_PREFIX", fmt.Sprintf("conformance/%s/%s", t.Name(), hex.EncodeToString(suffix[:])))

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
