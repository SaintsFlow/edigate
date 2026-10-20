package config

import (
	"os"
	"path/filepath"
	"testing"
)

const upstreamYAML = `db_url: postgres://file/db
upstreams:
  - name: rugpull-rates
    url: http://rugpull-rates:8090/mcp
    auth_header: {name: X-Gateway-Key, env: RUG_KEY}
    quarantine_results: true
  - name: shadow-carrier
    url: http://shadow-carrier:8090/mcp
upstream_allow:
  stand: [up.rugpull-rates.get_rates]
`

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestLoadUpstreams(t *testing.T) {
	t.Setenv("EDIGATE_DB_URL", "")
	t.Setenv("EDIGATE_LISTEN", "")
	t.Setenv("EDIGATE_MANIFEST_PUBKEY", "")
	file := writeConfig(t, upstreamYAML)

	// Without a key the gateway still starts; upstream.New then switches the servers off.
	if c, err := Load(file); err != nil || len(c.Upstreams) != 2 || c.ManifestPubKey != "" {
		t.Errorf("without a key: %+v %v", c, err)
	}
	t.Setenv("EDIGATE_MANIFEST_PUBKEY", "a2V5")
	c, err := Load(file)
	if err != nil || c.ManifestPubKey != "a2V5" {
		t.Fatal(c.ManifestPubKey, err)
	}
	if len(c.Upstreams) != 2 || c.Upstreams[0].AuthHeader.Env != "RUG_KEY" || !c.Upstreams[0].QuarantineResults || c.Upstreams[1].QuarantineResults {
		t.Errorf("upstreams = %+v", c.Upstreams)
	}
	if got := c.UpstreamAllow["stand"]; len(got) != 1 || got[0] != "up.rugpull-rates.get_rates" {
		t.Errorf("upstream_allow = %v", c.UpstreamAllow)
	}

	for name, text := range map[string]string{
		"a bad name":    "db_url: postgres://x/db\nupstreams:\n  - {name: Bad_Name, url: 'http://x/mcp'}\n",
		"a bad url":     "db_url: postgres://x/db\nupstreams:\n  - {name: rates, url: 'ftp://x'}\n",
		"a repeat":      "db_url: postgres://x/db\nupstreams:\n  - {name: rates, url: 'http://x/mcp'}\n  - {name: rates, url: 'http://y/mcp'}\n",
		"half a header": "db_url: postgres://x/db\nupstreams:\n  - {name: rates, url: 'http://x/mcp', auth_header: {name: X-Key}}\n",
	} {
		if _, err := Load(writeConfig(t, text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	servers, err := LoadUpstreams(file)
	if err != nil || len(servers) != 2 {
		t.Errorf("LoadUpstreams: %v %v", servers, err)
	}
}
