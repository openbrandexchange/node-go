// Package db wraps database/sql for SQLite (default, pure Go) and
// PostgreSQL. Queries are written with '?' placeholders and rebound for
// Postgres. All catalog writes go through WriteTx, which serialises writers
// (process mutex, plus a transaction-scoped advisory lock on Postgres) so
// change-feed sequence numbers are committed in order.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Dialect names.
const (
	SQLite   = "sqlite"
	Postgres = "postgres"
)

// TimeFormat is the fixed-width UTC format used for all stored timestamps,
// so they sort as text.
const TimeFormat = "2006-01-02T15:04:05.000000Z"

// ErrNoRows re-exports sql.ErrNoRows.
var ErrNoRows = sql.ErrNoRows

// DB is the database handle.
type DB struct {
	SQL     *sql.DB
	Dialect string
	writeMu sync.Mutex
}

// Open opens a database from a URL: "sqlite:///abs/path.db", "sqlite:rel.db",
// "postgres://..." or "postgresql://...".
func Open(url string) (*DB, error) {
	switch {
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		s, err := sql.Open("pgx", url)
		if err != nil {
			return nil, err
		}
		s.SetMaxOpenConns(20)
		s.SetConnMaxIdleTime(5 * time.Minute)
		d := &DB{SQL: s, Dialect: Postgres}
		return d, d.ping()
	case strings.HasPrefix(url, "sqlite:"):
		path := strings.TrimPrefix(url, "sqlite:")
		path = strings.TrimPrefix(path, "//")
		memory := path == ":memory:" || path == ""
		if !memory {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, err
			}
		}
		dsn := "file:" + path + "?_pragma=busy_timeout(15000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
		if memory {
			dsn = "file:obx-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "?mode=memory&cache=shared&_pragma=busy_timeout(15000)&_txlock=immediate"
		} else {
			dsn += "&_pragma=journal_mode(WAL)"
		}
		s, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		if memory {
			s.SetMaxOpenConns(1)
		} else {
			s.SetMaxOpenConns(8)
		}
		d := &DB{SQL: s, Dialect: SQLite}
		return d, d.ping()
	}
	return nil, fmt.Errorf("unsupported database URL (use sqlite:///path/obx.db or postgres://...)")
}

// SQLitePath returns the file of a sqlite: URL ("" for other databases and
// in-memory SQLite).
func SQLitePath(url string) string {
	if !strings.HasPrefix(url, "sqlite:") {
		return ""
	}
	path := strings.TrimPrefix(strings.TrimPrefix(url, "sqlite:"), "//")
	if path == ":memory:" {
		return ""
	}
	return path
}

func (d *DB) ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return d.SQL.PingContext(ctx)
}

// Close closes the pool.
func (d *DB) Close() error { return d.SQL.Close() }

// Rebind converts '?' placeholders to '$n' for Postgres.
func (d *DB) Rebind(q string) string {
	if d.Dialect != Postgres {
		return q
	}
	var b strings.Builder
	n := 0
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\'' {
			inStr = !inStr
		}
		if c == '?' && !inStr {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Querier is implemented by *DB and *Tx.
type Querier interface {
	Exec(ctx context.Context, q string, args ...any) (sql.Result, error)
	Query(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRow(ctx context.Context, q string, args ...any) *sql.Row
	DialectName() string
}

// Exec runs a statement.
func (d *DB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.SQL.ExecContext(ctx, d.Rebind(q), args...)
}

// Query runs a query.
func (d *DB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.SQL.QueryContext(ctx, d.Rebind(q), args...)
}

// QueryRow runs a single-row query.
func (d *DB) QueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return d.SQL.QueryRowContext(ctx, d.Rebind(q), args...)
}

// DialectName returns the dialect (Querier interface).
func (d *DB) DialectName() string { return d.Dialect }

// Tx is a transaction.
type Tx struct {
	tx *sql.Tx
	db *DB
	// AfterCommit callbacks run after a successful commit.
	after []func()
}

// Exec runs a statement.
func (t *Tx) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.db.Rebind(q), args...)
}

// Query runs a query.
func (t *Tx) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.db.Rebind(q), args...)
}

// QueryRow runs a single-row query.
func (t *Tx) QueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.db.Rebind(q), args...)
}

// DialectName returns the dialect.
func (t *Tx) DialectName() string { return t.db.Dialect }

// AfterCommit registers fn to run once the transaction committed.
func (t *Tx) AfterCommit(fn func()) { t.after = append(t.after, fn) }

// WriteTx runs fn in a serialised write transaction.
func (d *DB) WriteTx(ctx context.Context, fn func(tx *Tx) error) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.tx(ctx, true, fn)
}

func (d *DB) tx(ctx context.Context, write bool, fn func(tx *Tx) error) (err error) {
	sqlTx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t := &Tx{tx: sqlTx, db: d}
	defer func() {
		if r := recover(); r != nil {
			_ = sqlTx.Rollback()
			panic(r)
		}
	}()
	if write && d.Dialect == Postgres {
		if _, err := sqlTx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(4242424242)"); err != nil {
			_ = sqlTx.Rollback()
			return err
		}
	}
	if err := fn(t); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return err
	}
	for _, f := range t.after {
		f()
	}
	return nil
}

// Now returns the current time formatted for storage.
func Now() string { return time.Now().UTC().Format(TimeFormat) }

// Fmt formats t for storage.
func Fmt(t time.Time) string { return t.UTC().Format(TimeFormat) }

// Parse parses a stored timestamp.
func Parse(s string) time.Time {
	t, err := time.Parse(TimeFormat, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	return t.UTC()
}

// FmtPtr formats an optional time.
func FmtPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := Fmt(*t)
	return &s
}

// IsUniqueViolation reports whether err is a unique-constraint violation.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "duplicate key value") || strings.Contains(s, "SQLSTATE 23505")
}

// NotFound reports whether err is sql.ErrNoRows.
func NotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Bool converts a Go bool to the stored integer.
func Bool(b bool) int {
	if b {
		return 1
	}
	return 0
}

// In returns "(?,?,?)" for n parameters (n>0).
func In(n int) string {
	if n <= 0 {
		return "(NULL)"
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}
