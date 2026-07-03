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
	// Role selects single-writer coordination via the s3lite lease. The zero
	// value (RoleOff) keeps the always-writer behaviour used by tests and local
	// unreplicated dev; it only makes sense to set a leased role when BackupTo is
	// a shared replica the instances coordinate on.
	Role s3lite.Role
	// LeaseTTL is the writer-lease duration for leased roles; zero uses s3lite's
	// default (30s). The holder renews at TTL/3 and stops on any renew failure.
	LeaseTTL time.Duration
	// Owner labels the lease holder in the lock file for diagnostics; empty lets
	// s3lite generate one (hostname:pid).
	Owner string
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
		Role:        cfg.Role,
		LeaseTTL:    cfg.LeaseTTL,
		Owner:       cfg.Owner,
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

// IsLeader reports whether this instance currently holds the writer lease and
// may serve writes. Always true under RoleOff (sole writer). Gate the write
// path on it: a follower must refuse pushes (see docs/architecture/safety.md).
func (m *Metadata) IsLeader() bool { return m.db.IsLeader() }

// Generation returns the lease fencing token — monotonic, bumped on each
// takeover; 0 when no lease is held.
func (m *Metadata) Generation() int64 { return m.db.Generation() }

// OnPromote registers a callback fired after this instance becomes the writer
// (acquired the lease and started replicating).
func (m *Metadata) OnPromote(fn func()) { m.db.OnPromote(fn) }

// OnDemote registers a callback fired after this instance loses the lease and
// stops writing.
func (m *Metadata) OnDemote(fn func(error)) { m.db.OnDemote(fn) }

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
