// Package storetest gives tests a throwaway migrated database.
//
// New reads EDIGATE_TEST_DB_URL; when it is not set the test is skipped. It
// creates a database edigate_test_<random>, applies the migrations and drops
// the database in t.Cleanup.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SaintsFlow/edigate/internal/store"
)

// New returns a store on a fresh migrated database and its pool.
func New(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	adminURL := os.Getenv("EDIGATE_TEST_DB_URL")
	if adminURL == "" {
		t.Skip("EDIGATE_TEST_DB_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "edigate_test_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	s := store.New(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, pool
}
