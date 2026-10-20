package main

// Commands for the slots of the benchmark (protocol section 6): "db create <name>" and
// "db drop <name>" make and remove the database of one gateway process on the PostgreSQL
// server of EDIGATE_DB_URL. The gateway applies its migrations itself when it starts.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"syscall"

	"github.com/jackc/pgx/v5"
)

// reDBName keeps the name of a slot database to a plain identifier: it is quoted anyway, but
// a name from a command line should not be able to reach a system database by accident.
var reDBName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// dbCommand is "db create <name>" and "db drop <name>".
func dbCommand(ctx context.Context, args []string, out io.Writer) error {
	const form = "db: expected: create|drop <name> (a name of lower case letters, digits and underscores)"
	if len(args) != 2 || (args[0] != "create" && args[0] != "drop") || !reDBName.MatchString(args[1]) {
		return usageError{form}
	}
	action, name := args[0], args[1]
	url := os.Getenv("EDIGATE_DB_URL")
	if url == "" {
		return errors.New("EDIGATE_DB_URL is not set")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect to the server: %w", err)
	}
	defer conn.Close(ctx)
	if action == "drop" {
		// The server of the URL must keep the database the command is connected to.
		var current string
		if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&current); err != nil {
			return err
		}
		if current == name {
			return errors.New("db drop: this is the database of EDIGATE_DB_URL itself")
		}
	}
	ident := pgx.Identifier{name}.Sanitize()
	if action == "create" {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
			return fmt.Errorf("db create: %w", err)
		}
		fmt.Fprintln(out, "database created:", name)
		return nil
	}
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("db drop: %w", err)
	}
	fmt.Fprintln(out, "database dropped:", name)
	return nil
}

// lockFile takes an exclusive lock on a file (created if missing) and returns the function
// that gives it back. Writers of one tokens file use it so that two slots set up at the same
// time do not erase each other's entries.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
