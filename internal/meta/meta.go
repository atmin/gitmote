// Package meta is the metadata layer. It keeps all mutable forge state in a
// SQLite database that s3lite backs:
//   - repos and their refs
//   - users
//   - tokens and ssh keys
//   - ACLs
//
// The schema follows docs/architecture/storage.md. Refs are the source of
// truth. A ref update is a compare-and-swap in a single transaction (see
// CASRef). That compare-and-swap is the linearization point of the
// single-writer, content-before-pointer model in docs/architecture/safety.md.
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

// Config holds the settings that open the metadata database. It mirrors the
// subset of s3lite.Config that the forge needs. The package supplies the
// schema internally, so callers never manage migrations.
type Config struct {
	// LocalPath is the path of the SQLite file on local disk. It is required.
	LocalPath string
	// RestoreFrom is a replica URL to restore from on a cold start. It is
	// empty for a local-only database, as in tests.
	RestoreFrom string
	// BackupTo is a replica URL to replicate to continuously. Empty disables
	// replication, as in tests.
	BackupTo string
	// S3 configures s3:// replicas.
	S3 s3lite.S3Config
	// Logger receives the lifecycle events of s3lite. s3lite gates the
	// per-sync messages of litestream to WARN. A nil Logger uses a stderr
	// logger that logs WARN and higher.
	Logger *slog.Logger
	// Role selects how the s3lite lease coordinates the single writer. The
	// zero value is RoleAuto. It coordinates by lease when BackupTo is an
	// s3:// replica. Otherwise the instance is the sole writer, as in tests
	// and in local dev without replication. Set a coordinating role only when
	// BackupTo is an s3:// replica that the instances share.
	Role s3lite.Role
	// LeaseTTL is the duration of the writer lease for leased roles. Zero uses
	// the s3lite default of 30s. The holder renews the lease at TTL/3 and
	// stops on any renewal failure.
	LeaseTTL time.Duration
	// Owner labels the lease holder in the lock file for diagnostics. When
	// Owner is empty, s3lite generates a label (hostname:pid).
	Owner string
	// OnDemandPromotion makes a follower promote only through TryPromote,
	// never in the background. It works together with YieldLease. Without
	// it, this instance or an idle peer takes a yielded lease back within one
	// tick.
	OnDemandPromotion bool
}

// Metadata is the query layer over the database that s3lite backs.
type Metadata struct {
	db *s3lite.DB
}

// Open opens the metadata database and applies the embedded schema. It
// restores and replicates the database as cfg sets. The schema is idempotent,
// so Open on an existing database leaves it unchanged.
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

		OnDemandPromotion: cfg.OnDemandPromotion,
	})
	if err != nil {
		return nil, err
	}
	m := &Metadata{db: db}
	if err := m.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return m, nil
}

// migrate applies additive schema changes to tables that already hold rows.
// A CREATE TABLE IF NOT EXISTS in schema.sql cannot express these changes.
// s3lite has no version table, so every migration must be idempotent. migrate
// checks pragma_table_info and runs ALTER only for missing columns. On a
// fresh database, schema.sql already creates the columns, so migrate does
// nothing. This is the established pattern for adding a column to an
// existing table.
func (m *Metadata) migrate(ctx context.Context) error {
	return m.addColumns(ctx, "tokens", []columnDef{
		{"expires_at", "TEXT"},
		{"repo_scope", "INTEGER REFERENCES repos(id)"},
		{"read_only", "INTEGER NOT NULL DEFAULT 0"},
	})
}

// columnDef is one column to add. It holds the name, and the DDL for the type
// and constraints that follows "ADD COLUMN <name>". An added NOT NULL column
// must carry a DEFAULT.
type columnDef struct {
	name string
	ddl  string
}

// addColumns adds each column that table lacks. The table name and the column
// definitions are internal constants, never user input. So the DDL that is
// built from strings is safe.
func (m *Metadata) addColumns(ctx context.Context, table string, cols []columnDef) error {
	have := make(map[string]bool)
	rows, err := m.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, c := range cols {
		if have[c.name] {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+c.name+` `+c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// Close durably flushes pending replication and closes the database. The
// s3lite ShutdownSyncTimeout bounds the flush. Without replication, the flush
// does nothing.
func (m *Metadata) Close() error { return m.db.Close() }

// CloseContext is Close, but ctx bounds the final replication flush instead of
// the default timeout. Use it to apply a graceful-shutdown deadline.
func (m *Metadata) CloseContext(ctx context.Context) error { return m.db.CloseContext(ctx) }

// IsLeader reports whether this instance currently holds the writer lease and
// may serve writes. It is always true for a sole writer without a lease. Gate
// the write path on it. A follower must refuse pushes (see
// docs/architecture/safety.md).
func (m *Metadata) IsLeader() bool { return m.db.IsLeader() }

// Generation returns the lease fencing token. The token is monotonic, and
// each takeover increments it. It is 0 when no lease is held.
func (m *Metadata) Generation() int64 { return m.db.Generation() }

// TryPromote tries to acquire the writer lease at once. It does not wait for
// the background lease poll. This lets a follower that receives a request
// during a deploy handoff promote immediately. It returns true if this
// instance is the writer or has just become the writer. It returns false if
// another instance still holds the lease. It blocks for the restore while it
// promotes, so bound ctx.
func (m *Metadata) TryPromote(ctx context.Context) (bool, error) { return m.db.TryPromote(ctx) }

// YieldLease hands the writer lease back while this instance stays up as a
// follower. Another instance on the same storage root can then write next.
// The final sync runs before the lock is released, so the next writer sees
// every acknowledged write. It returns s3lite.ErrNotLeader when there is no
// lease to yield. Bound ctx.
func (m *Metadata) YieldLease(ctx context.Context) error { return m.db.YieldLease(ctx) }

// OnPromote registers a callback. The callback runs after this instance becomes
// the writer. That is after it acquires the lease and starts replicating.
func (m *Metadata) OnPromote(fn func()) { m.db.OnPromote(fn) }

// OnDemote registers a callback that runs after this instance loses the lease
// and stops writing.
func (m *Metadata) OnDemote(fn func(error)) { m.db.OnDemote(fn) }

// now returns the current time in the format of the TEXT *_at columns: RFC
// 3339 in UTC.
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// isNoRows reports whether err is the "no rows" sentinel.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// boolToInt maps a Go bool to the 0 or 1 that SQLite stores for it.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
