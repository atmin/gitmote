package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

// testConformance is the shared Store contract suite. Every implementation
// must pass it — mem and s3 stay in lockstep by construction. newStore must
// return an empty store with an isolated keyspace.
func testConformance(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	put := func(t *testing.T, s Store, key string, content []byte) {
		t.Helper()
		if err := s.Put(ctx, key, bytes.NewReader(content)); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
	}
	get := func(t *testing.T, s Store, key string) []byte {
		t.Helper()
		rc, err := s.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get(%s): %v", key, err)
		}
		defer rc.Close()
		content, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read Get(%s) body: %v", key, err)
		}
		return content
	}

	t.Run("GetMissing", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Get(ctx, "repo/objects/ab/cdef")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ExistsMissing", func(t *testing.T) {
		s := newStore(t)
		ok, err := s.Exists(ctx, "repo/objects/ab/cdef")
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if ok {
			t.Error("Exists(missing) = true, want false")
		}
	})

	t.Run("PutGetRoundtrip", func(t *testing.T) {
		s := newStore(t)
		// Git objects are binary: zlib streams with NUL and high bytes.
		content := []byte("blob 12\x00\x00\x01\xff\xfebinary data")
		put(t, s, "repo/objects/ab/cdef", content)

		if got := get(t, s, "repo/objects/ab/cdef"); !bytes.Equal(got, content) {
			t.Errorf("Get returned %q, want %q", got, content)
		}
		ok, err := s.Exists(ctx, "repo/objects/ab/cdef")
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if !ok {
			t.Error("Exists(present) = false, want true")
		}
	})

	t.Run("RePutIsNoOp", func(t *testing.T) {
		s := newStore(t)
		content := []byte("same content, same hash")
		put(t, s, "repo/objects/ab/cdef", content)
		put(t, s, "repo/objects/ab/cdef", content)

		if got := get(t, s, "repo/objects/ab/cdef"); !bytes.Equal(got, content) {
			t.Errorf("Get after re-Put returned %q, want %q", got, content)
		}
		keys, err := s.List(ctx, "repo/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if want := []string{"repo/objects/ab/cdef"}; !slices.Equal(keys, want) {
			t.Errorf("List after re-Put = %v, want %v", keys, want)
		}
	})

	t.Run("ExistsIsNotPrefixMatch", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "repo/objects/ab/cdef", []byte("x"))

		ok, err := s.Exists(ctx, "repo/objects/ab")
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if ok {
			t.Error(`Exists("repo/objects/ab") = true, want false — a key is not its prefix`)
		}
	})

	t.Run("ListPrefixBoundary", func(t *testing.T) {
		s := newStore(t)
		for _, key := range []string{
			"repo1/objects/pack/pack-1.pack",
			"repo1/objects/aa/111",
			"repo10/objects/bb/222",
		} {
			put(t, s, key, []byte(key))
		}

		// "repo1/" must not match "repo10/…": the slash is the boundary.
		keys, err := s.List(ctx, "repo1/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := []string{"repo1/objects/aa/111", "repo1/objects/pack/pack-1.pack"}
		if !slices.Equal(keys, want) {
			t.Errorf("List(repo1/) = %v, want %v (sorted)", keys, want)
		}

		keys, err = s.List(ctx, "repo1/objects/pack/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if want := []string{"repo1/objects/pack/pack-1.pack"}; !slices.Equal(keys, want) {
			t.Errorf("List(repo1/objects/pack/) = %v, want %v", keys, want)
		}
	})

	t.Run("ListNoMatch", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "repo/objects/ab/cdef", []byte("x"))

		keys, err := s.List(ctx, "other/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(keys) != 0 {
			t.Errorf("List(other/) = %v, want empty", keys)
		}
	})
}

func TestMemConformance(t *testing.T) {
	testConformance(t, func(t *testing.T) Store { return NewMem() })
}
