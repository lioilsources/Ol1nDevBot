// Package store owns the SQLite database: migrations and sqlc-generated queries.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite"

	"github.com/lioilsources/Ol1nDevBot/internal/store/db"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the database handle and the generated queries.
type Store struct {
	*db.Queries
	DB *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies migrations.
// Use ":memory:" for tests.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single process, tiny data: one connection avoids SQLITE_BUSY and keeps
	// ":memory:" databases consistent across queries.
	sqldb.SetMaxOpenConns(1)
	if err := sqldb.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrateUp(sqldb); err != nil {
		return nil, err
	}
	return &Store{Queries: db.New(sqldb), DB: sqldb}, nil
}

func migrateUp(sqldb *sql.DB) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrations source: %w", err)
	}
	drv, err := migratesqlite.WithInstance(sqldb, &migratesqlite.Config{})
	if err != nil {
		return fmt.Errorf("migrations driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", drv)
	if err != nil {
		return fmt.Errorf("migrate init: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

// Now is the clock used for persisted timestamps; overridable in tests.
var Now = func() time.Time { return time.Now() }

// Unix returns the current unix time in seconds.
func Unix() int64 { return Now().Unix() }

// Baseline marks (scope, key) as initialised. It reports true when the
// baseline was created by this call, i.e. this is the first observation.
func (s *Store) Baseline(ctx context.Context, scope, key string) (bool, error) {
	n, err := s.InsertBaseline(ctx, db.InsertBaselineParams{Scope: scope, Key: key, CreatedAt: Unix()})
	return n > 0, err
}

// IsNotFound reports whether err is sql.ErrNoRows.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
