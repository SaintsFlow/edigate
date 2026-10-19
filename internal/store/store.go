// Package store owns the PostgreSQL pool, the embedded goose migrations and
// every SQL query of the gateway. All queries are parameterized.
//
// Input: a database URL (Open) or an existing pool (New, used by tests).
// Output: typed values and the sentinel errors ErrNotFound and ErrNotMigrated.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound means no row matched. A row of another tenant must look the same
// as a missing one, so callers answer both with not_found.
var ErrNotFound = errors.New("not found")

// ErrNotMigrated means the database schema is not at the latest migration.
var ErrNotMigrated = errors.New("database migrations are not applied")

// Store is the gateway's access to PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database at url.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// New wraps an existing pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

func (s *Store) provider() (*goose.Provider, *sql.DB, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, nil, err
	}
	db := stdlib.OpenDBFromPool(s.pool)
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return p, db, nil
}

// Migrate applies all embedded migrations (forward only).
func (s *Store) Migrate(ctx context.Context) error {
	p, db, err := s.provider()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// CheckMigrated reports ErrNotMigrated unless the database version equals the
// latest embedded migration. It also fails when the database does not answer.
func (s *Store) CheckMigrated(ctx context.Context) error {
	p, db, err := s.provider()
	if err != nil {
		return err
	}
	defer db.Close()
	current, err := p.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	sources := p.ListSources()
	if len(sources) == 0 || current != sources[len(sources)-1].Version {
		return ErrNotMigrated
	}
	return nil
}

// AddTenant creates a tenant; an existing id is left as it is.
func (s *Store) AddTenant(ctx context.Context, id, name string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, id, name)
	return err
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
