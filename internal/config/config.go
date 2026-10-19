// Package config loads the gateway settings: an optional YAML file (flag
// --config) overridden by environment variables, checked at start.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/SaintsFlow/edigate/internal/upstream"
)

// Config is everything the gateway needs to start in this version.
type Config struct {
	DBURL  string `yaml:"db_url"`
	Listen string `yaml:"listen"`

	// The quarantined reader (an OpenAI-compatible API). An empty model switches
	// edi.extract_note off: it answers quarantine_invalid and the rest works.
	QuarantineBaseURL string `yaml:"quarantine_base_url"`
	QuarantineModel   string `yaml:"quarantine_model"`
	QuarantineAPIKey  string `yaml:"quarantine_api_key"`

	// The relay that delivers the outbox to the ERP. An empty OnecURL switches it
	// off: orders are reserved and stay queued. TimeScale divides the retry pauses.
	OnecURL      string  `yaml:"onec_url"`
	RelayWorkers int     `yaml:"relay_workers"`
	TimeScale    float64 `yaml:"time_scale"`

	// The warehouse and the trading partner the sagas deliver to. An empty WMSURL leaves the
	// warehouse effects queued; an empty PartnerURL leaves the documents for the partner queued.
	WMSURL     string `yaml:"wms_url"`
	PartnerURL string `yaml:"partner_url"`

	// The upstream MCP servers the gateway may pass tools of on (flow P8), the public key
	// (base64, ed25519) that verifies their pinned descriptions, and the up.* tools each
	// tenant may call (data of the policy; empty means none). Without servers there are no
	// up.* tools. Servers without a key are switched off at start (see upstream.New).
	Upstreams      []upstream.Server   `yaml:"upstreams"`
	ManifestPubKey string              `yaml:"manifest_pubkey"`
	UpstreamAllow  map[string][]string `yaml:"upstream_allow"`

	// Fault injection for tests of the guarantee only (see .env.example). Off by
	// default; read from the environment only.
	ChaosExitBeforeSend float64       `yaml:"-"`
	ChaosExitAfterApply float64       `yaml:"-"`
	ChaosCrashAfter     time.Duration `yaml:"-"`
	ChaosSeed           *uint64       `yaml:"-"`
}

const (
	defaultListen       = "127.0.0.1:8080"
	defaultRelayWorkers = 2
)

// Load reads the YAML file when path is not empty, applies the EDIGATE_*
// variables over it, fills defaults and checks the result.
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
	if v := os.Getenv("EDIGATE_QUARANTINE_BASE_URL"); v != "" {
		c.QuarantineBaseURL = v
	}
	if v := os.Getenv("EDIGATE_QUARANTINE_MODEL"); v != "" {
		c.QuarantineModel = v
	}
	if v := os.Getenv("EDIGATE_QUARANTINE_API_KEY"); v != "" {
		c.QuarantineAPIKey = v
	}
	if v := os.Getenv("EDIGATE_ONEC_URL"); v != "" {
		c.OnecURL = v
	}
	if v := os.Getenv("EDIGATE_WMS_URL"); v != "" {
		c.WMSURL = v
	}
	if v := os.Getenv("EDIGATE_PARTNER_URL"); v != "" {
		c.PartnerURL = v
	}
	if v := os.Getenv("EDIGATE_MANIFEST_PUBKEY"); v != "" {
		c.ManifestPubKey = v
	}
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.RelayWorkers == 0 {
		c.RelayWorkers = defaultRelayWorkers
	}
	if c.TimeScale == 0 {
		c.TimeScale = 1
	}
	if err := c.loadEnvNumbers(); err != nil {
		return Config{}, err
	}
	return c, c.check()
}

// LoadUpstreams reads only the list of upstream servers from the YAML file. edigatectl
// uses it to pin the tools of a server without the rest of the gateway settings.
func LoadUpstreams(path string) ([]upstream.Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c struct {
		Upstreams []upstream.Server `yaml:"upstreams"`
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return c.Upstreams, upstream.CheckAll(c.Upstreams)
}

// loadEnvNumbers reads the numeric settings of the relay and of fault injection.
func (c *Config) loadEnvNumbers() error {
	var err error
	if c.RelayWorkers, err = envInt("EDIGATE_RELAY_WORKERS", c.RelayWorkers); err != nil {
		return err
	}
	if c.TimeScale, err = envFloat("EDIGATE_TIME_SCALE", c.TimeScale); err != nil {
		return err
	}
	if c.ChaosExitBeforeSend, err = envFloat("EDIGATE_CHAOS_EXIT_BEFORE_SEND", 0); err != nil {
		return err
	}
	if c.ChaosExitAfterApply, err = envFloat("EDIGATE_CHAOS_EXIT_AFTER_APPLY", 0); err != nil {
		return err
	}
	if v := os.Getenv("EDIGATE_CHAOS_CRASH_AFTER"); v != "" {
		if c.ChaosCrashAfter, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("EDIGATE_CHAOS_CRASH_AFTER %q: %w", v, err)
		}
	}
	if v := os.Getenv("EDIGATE_CHAOS_SEED"); v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return fmt.Errorf("EDIGATE_CHAOS_SEED %q: %w", v, err)
		}
		c.ChaosSeed = &seed
	}
	return nil
}

func envInt(name string, fallback int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", name, v, err)
	}
	return n, nil
}

func envFloat(name string, fallback float64) (float64, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", name, v, err)
	}
	return f, nil
}

func (c Config) check() error {
	if c.DBURL == "" {
		return errors.New("EDIGATE_DB_URL is required")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("EDIGATE_LISTEN %q: %w", c.Listen, err)
	}
	if c.QuarantineModel != "" && c.QuarantineBaseURL == "" {
		return errors.New("EDIGATE_QUARANTINE_BASE_URL is required when EDIGATE_QUARANTINE_MODEL is set")
	}
	if err := upstream.CheckAll(c.Upstreams); err != nil {
		return err
	}
	if c.RelayWorkers < 1 || c.TimeScale <= 0 {
		return errors.New("EDIGATE_RELAY_WORKERS must be at least 1 and EDIGATE_TIME_SCALE above 0")
	}
	for _, p := range []float64{c.ChaosExitBeforeSend, c.ChaosExitAfterApply} {
		if p < 0 || p > 1 {
			return errors.New("EDIGATE_CHAOS_EXIT_* must be between 0 and 1")
		}
	}
	return nil
}
