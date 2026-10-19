// Package config loads the gateway settings: an optional YAML file (flag
// --config) overridden by environment variables, checked at start.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is everything the gateway needs to start in this version.
type Config struct {
	DBURL  string `yaml:"db_url"`
	Listen string `yaml:"listen"`
}

const defaultListen = "127.0.0.1:8080"

// Load reads the YAML file when path is not empty, applies EDIGATE_DB_URL and
// EDIGATE_LISTEN over it, fills defaults and checks the result.
func Load(path string) (Config, error) {
	var c Config
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(data, &c); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}
	if v := os.Getenv("EDIGATE_DB_URL"); v != "" {
		c.DBURL = v
	}
	if v := os.Getenv("EDIGATE_LISTEN"); v != "" {
		c.Listen = v
	}
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	return c, c.check()
}

func (c Config) check() error {
	if c.DBURL == "" {
		return errors.New("EDIGATE_DB_URL is required")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("EDIGATE_LISTEN %q: %w", c.Listen, err)
	}
	return nil
}
