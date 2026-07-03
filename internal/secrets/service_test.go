package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/atmin/gitmote/internal/meta"
)

// newSvc builds a service over a keyring (one key) and a real temp meta DB with
// one repo, returning the service and the repo id.
func newSvc(t *testing.T) (*Service, int64) {
	t.Helper()
	t.Setenv(keyEnvPrefix+"1", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, masterKeyLen)))
	kr, err := NewKeyringFromEnv()
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	md, err := meta.Open(context.Background(), meta.Config{LocalPath: filepath.Join(t.TempDir(), "meta.sqlite3")})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = md.Close() })
	r, err := md.CreateRepo(context.Background(), "atmin/app", "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return NewService(kr, md), r.ID
}

func TestServiceSetGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, repoID := newSvc(t)

	if err := svc.SetSecret(ctx, repoID, "API_TOKEN", "s3cr3t"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	got, err := svc.Secrets(ctx, repoID)
	if err != nil {
		t.Fatalf("Secrets: %v", err)
	}
	if got["API_TOKEN"] != "s3cr3t" {
		t.Errorf("decrypted secret = %q, want s3cr3t", got["API_TOKEN"])
	}
}

func TestServiceUpsertAndDelete(t *testing.T) {
	ctx := context.Background()
	svc, repoID := newSvc(t)

	_ = svc.SetSecret(ctx, repoID, "K", "one")
	_ = svc.SetSecret(ctx, repoID, "K", "two") // overwrite
	got, _ := svc.Secrets(ctx, repoID)
	if got["K"] != "two" {
		t.Errorf("after upsert K = %q, want two", got["K"])
	}

	if err := svc.DeleteSecret(ctx, repoID, "K"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	names, _ := svc.ListSecretNames(ctx, repoID)
	if len(names) != 0 {
		t.Errorf("names after delete = %v, want empty", names)
	}
	// Deleting again is a no-op, not an error.
	if err := svc.DeleteSecret(ctx, repoID, "K"); err != nil {
		t.Errorf("second delete = %v, want nil (idempotent)", err)
	}
}

func TestServiceListNamesOnly(t *testing.T) {
	ctx := context.Background()
	svc, repoID := newSvc(t)
	_ = svc.SetSecret(ctx, repoID, "BETA", "vb")
	_ = svc.SetSecret(ctx, repoID, "ALPHA", "va")

	names, err := svc.ListSecretNames(ctx, repoID)
	if err != nil {
		t.Fatalf("ListSecretNames: %v", err)
	}
	if len(names) != 2 || names[0] != "ALPHA" || names[1] != "BETA" {
		t.Errorf("names = %v, want [ALPHA BETA] (sorted, names only)", names)
	}
}

func TestServiceRejectsBadAndReservedNames(t *testing.T) {
	ctx := context.Background()
	svc, repoID := newSvc(t)
	for _, name := range []string{"1leading", "has-dash", "has space", "GITMOTE_URL", "WORKER_SECRET"} {
		if err := svc.SetSecret(ctx, repoID, name, "v"); err == nil {
			t.Errorf("SetSecret(%q) succeeded, want rejection", name)
		}
	}
}

// TestSecretValueNotStoredInPlaintext is the at-rest leak invariant: the stored
// envelope's ciphertext must not contain the plaintext bytes.
func TestSecretValueNotStoredInPlaintext(t *testing.T) {
	ctx := context.Background()
	svc, repoID := newSvc(t)
	const value = "PLAINTEXT-NEEDLE-9f3a"
	if err := svc.SetSecret(ctx, repoID, "K", value); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	stored, err := svc.store.GetSecrets(ctx, repoID)
	if err != nil {
		t.Fatalf("GetSecrets: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored = %d envelopes, want 1", len(stored))
	}
	if bytes.Contains(stored[0].CT, []byte(value)) {
		t.Error("stored ciphertext contains the plaintext value")
	}
}
