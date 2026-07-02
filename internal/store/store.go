// Package store provides durable, content-addressed storage for git objects
// and packfiles, per the S3 layout in docs/architecture/storage.md. Keys are
// content-addressed ({repo}/objects/… and {repo}/objects/pack/…), so values
// are immutable: a re-Put of an existing key is a no-op. This idempotence is
// the substrate for the content-before-pointer invariant in
// docs/architecture/safety.md.
package store

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Get for a key that does not exist.
var ErrNotFound = errors.New("store: key not found")

// Store is the storage contract for immutable git objects and packs.
type Store interface {
	// Put stores the content read from r under key. Keys are
	// content-addressed, so a re-Put of an existing key is a no-op.
	Put(ctx context.Context, key string, r io.Reader) error

	// Get returns the content stored under key. The caller must close the
	// returned reader. Returns ErrNotFound if the key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Exists reports whether key exists.
	Exists(ctx context.Context, key string) (bool, error)

	// List returns, in lexicographic order, all keys that start with
	// prefix — plain string-prefix matching, as in S3. Callers listing a
	// "directory" must include the trailing slash.
	List(ctx context.Context, prefix string) ([]string, error)
}
