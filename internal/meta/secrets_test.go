package meta

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestGetOrCreateSecretCreatesOnceAndReuses(t *testing.T) {
	ctx := context.Background()
	m := open(t)

	calls := 0
	gen := func() ([]byte, error) {
		calls++
		return []byte("generated-value"), nil
	}

	// First call generates and persists.
	v1, err := m.GetOrCreateSecret(ctx, "cookie_key", gen)
	if err != nil {
		t.Fatalf("GetOrCreateSecret (first): %v", err)
	}
	if string(v1) != "generated-value" {
		t.Errorf("value = %q, want the generated value", v1)
	}

	// Second call reuses the stored value — gen must not run again.
	v2, err := m.GetOrCreateSecret(ctx, "cookie_key", func() ([]byte, error) {
		t.Fatal("gen called on an existing secret")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("GetOrCreateSecret (second): %v", err)
	}
	if string(v2) != "generated-value" {
		t.Errorf("reused value = %q, want the stored value", v2)
	}
	if calls != 1 {
		t.Errorf("gen calls = %d, want 1 (generate once)", calls)
	}

	// A different name gets its own value.
	other, err := m.GetOrCreateSecret(ctx, "worker_secret", func() ([]byte, error) {
		return []byte("other-value"), nil
	})
	if err != nil {
		t.Fatalf("GetOrCreateSecret (other name): %v", err)
	}
	if string(other) != "other-value" {
		t.Errorf("other value = %q, want its own generated value", other)
	}
}

func TestGetOrCreateSecretPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.sqlite3")

	m := openAt(t, path)
	v1, err := m.GetOrCreateSecret(ctx, "cookie_key", func() ([]byte, error) {
		return []byte("stable-key"), nil
	})
	if err != nil {
		t.Fatalf("GetOrCreateSecret: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open the same file (a restart / scale-to-zero restore): the value is the
	// one restored, so gen must not run.
	m2 := openAt(t, path)
	v2, err := m2.GetOrCreateSecret(ctx, "cookie_key", func() ([]byte, error) {
		t.Fatal("gen called after reopen; the persisted value should be reused")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("GetOrCreateSecret after reopen: %v", err)
	}
	if string(v2) != string(v1) {
		t.Errorf("value after reopen = %q, want the persisted %q", v2, v1)
	}
}

func TestGetOrCreateSecretGenErrorPropagates(t *testing.T) {
	ctx := context.Background()
	m := open(t)

	want := errors.New("no entropy")
	if _, err := m.GetOrCreateSecret(ctx, "cookie_key", func() ([]byte, error) {
		return nil, want
	}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	// Nothing was persisted, so a later successful gen still creates the value.
	v, err := m.GetOrCreateSecret(ctx, "cookie_key", func() ([]byte, error) {
		return []byte("recovered"), nil
	})
	if err != nil {
		t.Fatalf("GetOrCreateSecret after gen error: %v", err)
	}
	if string(v) != "recovered" {
		t.Errorf("value = %q, want the value from the successful gen", v)
	}
}
