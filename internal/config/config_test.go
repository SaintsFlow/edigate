package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv("EDIGATE_DB_URL", "")
	t.Setenv("EDIGATE_LISTEN", "")
	if _, err := Load(""); err == nil {
		t.Error("missing EDIGATE_DB_URL must fail")
	}

	t.Setenv("EDIGATE_DB_URL", "postgres://example/db")
	c, err := Load("")
	if err != nil || c.Listen != "127.0.0.1:8080" {
		t.Fatalf("defaults: %+v %v", c, err)
	}

	file := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(file, []byte("db_url: postgres://file/db\nlisten: 0.0.0.0:9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDIGATE_DB_URL", "")
	c, err = Load(file)
	if err != nil || c.DBURL != "postgres://file/db" || c.Listen != "0.0.0.0:9" {
		t.Fatalf("file: %+v %v", c, err)
	}
	t.Setenv("EDIGATE_LISTEN", "127.0.0.1:1")
	if c, _ = Load(file); c.Listen != "127.0.0.1:1" {
		t.Errorf("env must win over file, got %q", c.Listen)
	}

	t.Setenv("EDIGATE_LISTEN", "nonsense")
	if _, err := Load(file); err == nil {
		t.Error("bad listen address must fail")
	}
}
