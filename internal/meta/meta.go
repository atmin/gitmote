// Package meta is the metadata layer: all mutable forge state — repos, refs,
// users, tokens, ssh keys, ACLs — in an s3lite-backed SQLite database, per the
// schema in docs/architecture/storage.md. Refs are the source of truth, and a
// ref update is a single-transaction compare-and-swap (see CASRef); that CAS is
// the linearization point behind the single-writer, content-before-pointer
// model in docs/architecture/safety.md.
package meta

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"log/slog"
	"time"

	"github.com/atmin/s3lite"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotFound is returned by the Get* lookups when no row matches.
var ErrNotFound = errors.New("meta: not found")

// Config opens the metadata database. It mirrors the subset of s3lite.Config
// the forge needs; the schema is supplied internally, so callers never manage
// migrations.
type Config struct {
	// LocalPath is the on-disk path for the SQLite file. Required.
	LocalPath string
	// RestoreFrom is a replica URL to restore from on cold start; empty for a
	// local-only database (tests).
	RestoreFrom string
	// BackupTo is a replica URL to replicate to continuously; empty disables
	// replication (tests).
	BackupTo string
	// S3 configures s3:// replicas.
	S3 s3lite.S3Config
	// Logger receives litestream's log records; nil suppresses INFO.
	Logger *slog.Logger
}

// Metadata is the query layer over the s3lite-backed database.
type Metadata struct {
	db *s3lite.DB
}

// Open opens (restoring and replicating per cfg) the metadata database and
// applies the embedded schema. The schema is idempotent, so Open on an
// existing database leaves it unchanged.
func Open(ctx context.Context, cfg Config) (*Metadata, error) {
	db, err := s3lite.Open(ctx, s3lite.Config{
		LocalPath:   cfg.LocalPath,
		RestoreFrom: cfg.RestoreFrom,
		BackupTo:    cfg.BackupTo,
		S3:          cfg.S3,
		Logger:      cfg.Logger,
		Migrations:  []string{schemaSQL},
	})
	if err != nil {
		return nil, err
	}
	return &Metadata{db: db}, nil
}

// Close durably flushes pending replication and closes the database; the flush
// is bounded by s3lite's ShutdownSyncTimeout. A no-op flush without replication.
func (m *Metadata) Close() error { return m.db.Close() }

// CloseContext is Close with the final replication flush bounded by ctx instead
// of the default timeout, for wiring into a graceful-shutdown deadline.
func (m *Metadata) CloseContext(ctx context.Context) error { return m.db.CloseContext(ctx) }

// now is the timestamp format for the TEXT *_at columns: RFC 3339, UTC.
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// isNoRows reports whether err is the "no rows" sentinel.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// boolToInt maps a Go bool to the 0/1 SQLite stores for it.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
