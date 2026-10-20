package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

// dbExists asks the server whether a database is there.
func dbExists(t *testing.T, adminURL, name string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// db create makes a database on the server of EDIGATE_DB_URL, db drop removes it; a bad name,
// the database of the URL itself and an unknown command are refused.
func TestDBCreateAndDrop(t *testing.T) {
	adminURL := os.Getenv("EDIGATE_TEST_DB_URL")
	if adminURL == "" {
		t.Skip("EDIGATE_TEST_DB_URL is not set")
	}
	t.Setenv("EDIGATE_DB_URL", adminURL)
	ctx := context.Background()
	var out bytes.Buffer
	const name = "edigate_slot_test_ctl"
	_ = dbCommand(ctx, []string{"drop", name}, &out) // a leftover of a broken run

	if err := dbCommand(ctx, []string{"create", name}, &out); err != nil {
		t.Fatal(err)
	}
	if !dbExists(t, adminURL, name) {
		t.Fatal("the database was not created")
	}
	if err := dbCommand(ctx, []string{"create", name}, &out); err == nil {
		t.Error("creating a database that is there must fail")
	}
	if err := dbCommand(ctx, []string{"drop", name}, &out); err != nil {
		t.Fatal(err)
	}
	if dbExists(t, adminURL, name) {
		t.Fatal("the database was not dropped")
	}
	if err := dbCommand(ctx, []string{"drop", name}, &out); err != nil {
		t.Errorf("dropping a database that is not there must not fail: %v", err)
	}

	u, _ := url.Parse(adminURL)
	own := u.Path[1:]
	for _, args := range [][]string{{"drop", own}, {"create", "Bad-Name"}, {"create", "x; DROP DATABASE " + own}, {"rename", name}, {"create"}, {"create", ""}} {
		err := dbCommand(ctx, args, &out)
		if err == nil {
			t.Errorf("%v must be refused", args)
		}
	}
	var ue usageError
	if err := dbCommand(ctx, []string{"create", "Bad-Name"}, &out); !errors.As(err, &ue) {
		t.Errorf("a bad name is a usage error (exit code 2), got %v", err)
	}
	if !dbExists(t, adminURL, own) {
		t.Fatal("the database of the URL is gone")
	}
}

// With --reply-url the tokens of the slot are written under "<url> <partner>" and the entries of
// other slots stay; the address must be a base address and needs --partner-tokens-out.
func TestFixturesLoadReplyURL(t *testing.T) {
	_, pool := storetest.New(t)
	t.Setenv("EDIGATE_DB_URL", pool.Config().ConnString())
	dir := t.TempDir()
	file, tokens := filepath.Join(dir, "fixtures.json"), filepath.Join(dir, "partner_tokens.json")
	if err := os.WriteFile(file, []byte(fixtures), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokens, []byte(`{"http://127.0.0.1:18191 ZZ:ZZTESTBUYER": "other-slot"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := fixturesLoad(context.Background(), []string{file, "--partner-tokens-out", tokens, "--reply-url", "http://127.0.0.1:18190/"}, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(tokens)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["http://127.0.0.1:18191 ZZ:ZZTESTBUYER"] != "other-slot" ||
		got["http://127.0.0.1:18190 ZZ:ZZTESTBUYER"] == "" || got["http://127.0.0.1:18190 ZZ:ZZTESTOTHER"] == "" {
		t.Fatalf("tokens file: %v", got)
	}
	for _, args := range [][]string{
		{file, "--reply-url", "http://127.0.0.1:18190"},
		{file, "--partner-tokens-out", tokens, "--reply-url", "http://127.0.0.1:18190/path"},
		{file, "--partner-tokens-out", tokens, "--reply-url", "ftp://x"},
	} {
		var ue usageError
		if err := fixturesLoad(context.Background(), args, &out); !errors.As(err, &ue) {
			t.Errorf("%v must be a usage error, got %v", args, err)
		}
	}
}
