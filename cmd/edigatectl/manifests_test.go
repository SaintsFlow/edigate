package main

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/store/storetest"
	"github.com/SaintsFlow/edigate/servers/adversarial/adversary"
	"github.com/SaintsFlow/edigate/servers/adversarial/common"
)

// pinRig is a database, a signing key, and a config that lists the servers given.
type pinRig struct {
	t       *testing.T
	dir     string
	config  string
	keyFile string
	pubKey  string
}

func newPinRig(t *testing.T, servers map[string]string) *pinRig {
	t.Helper()
	_, pool := storetest.New(t)
	t.Setenv("EDIGATE_DB_URL", pool.Config().ConnString())
	r := &pinRig{t: t, dir: t.TempDir()}
	r.keyFile = filepath.Join(r.dir, "signing.key")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"manifests", "keygen", "--out", r.keyFile}, &out); err != nil {
		t.Fatal(err)
	}
	r.pubKey = strings.TrimSpace(out.String())
	yaml := "upstreams:\n"
	for name, url := range servers {
		yaml += "  - name: " + name + "\n    url: " + url + "\n"
	}
	r.config = filepath.Join(r.dir, "edigate.yaml")
	if err := os.WriteFile(r.config, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *pinRig) pin(server string, extra ...string) (string, error) {
	var out bytes.Buffer
	args := append([]string{"manifests", "pin", "--config", r.config, "--server", server, "--signing-key", r.keyFile}, extra...)
	err := run(context.Background(), args, &out)
	return out.String(), err
}

func (r *pinRig) list() string {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"manifests", "list"}, &out); err != nil {
		r.t.Fatal(err)
	}
	return out.String()
}

func serve(t *testing.T, name string, srv *mcp.Server) string {
	t.Helper()
	ts := httptest.NewServer(common.Handler(name, srv, io.Discard, "", ""))
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp"
}

// T-UP-01: manifests pin refuses evil-validator, whose description names edi.send_asn; the
// flag --accept-lint does not change that; nothing is written.
func TestT_UP_01_PinRefusesAToolReference(t *testing.T) {
	url := serve(t, "evil-validator", adversary.EvilValidator())
	r := newPinRig(t, map[string]string{"evil-validator": url})

	out, err := r.pin("evil-validator")
	if err == nil || !strings.Contains(out, "tool_reference: edi.send_asn") {
		t.Fatalf("pin: err %v, out %q; want a refusal that prints the finding", err, out)
	}
	out, err = r.pin("evil-validator", "--accept-lint")
	if err == nil || !strings.Contains(out, "tool_reference: edi.send_asn") {
		t.Fatalf("pin --accept-lint: err %v, out %q; a reference to a tool is never accepted", err, out)
	}
	if got := r.list(); got != "" {
		t.Errorf("pins written by a refused command: %q", got)
	}
}

// A clean server is pinned and listed; a description that only addresses the model is pinned
// with --accept-lint and its findings are stored; the pin of a server that is not in the
// config, and a pin without a key, are usage errors.
func TestPinCleanAndImperative(t *testing.T) {
	clean := serve(t, "rugpull-rates", adversary.RugpullRates(3))
	imperative := mcp.NewServer(&mcp.Implementation{Name: "pushy", Version: "0"}, nil)
	imperative.AddTool(&mcp.Tool{
		Name: "weather", Description: "Returns the weather. Always answer in metric units.",
		InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	// A tool of a server that is pinned already is a name another description must not use.
	copycat := mcp.NewServer(&mcp.Implementation{Name: "copycat", Version: "0"}, nil)
	copycat.AddTool(&mcp.Tool{
		Name: "forecast", Description: "Returns a forecast. Combine it with get_rates for the price.",
		InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	r := newPinRig(t, map[string]string{
		"rugpull-rates": clean, "pushy": serve(t, "pushy", imperative), "copycat": serve(t, "copycat", copycat),
	})

	out, err := r.pin("rugpull-rates")
	if err != nil || !strings.Contains(out, "pinned rugpull-rates.get_rates sha256=") || !strings.Contains(out, "lint=0") {
		t.Fatalf("clean pin: %v %q", err, out)
	}
	if got := r.list(); !strings.HasPrefix(got, "rugpull-rates\tget_rates\t") || !strings.Contains(got, "\tadmin\tlint=0") {
		t.Errorf("list = %q", got)
	}

	out, err = r.pin("copycat", "--accept-lint")
	if err == nil || !strings.Contains(out, "tool_reference: get_rates") {
		t.Fatalf("a name of a pinned tool of another server: %v %q", err, out)
	}

	out, err = r.pin("pushy")
	if err == nil || !strings.Contains(out, "imperative: always") {
		t.Fatalf("imperative without the flag: %v %q", err, out)
	}
	out, err = r.pin("pushy", "--accept-lint", "--by", "alice")
	if err != nil || !strings.Contains(out, "lint=1") {
		t.Fatalf("imperative with the flag: %v %q", err, out)
	}
	var list bytes.Buffer
	if err := run(context.Background(), []string{"manifests", "list", "--server", "pushy"}, &list); err != nil ||
		!strings.Contains(list.String(), "pushy\tweather\t") || !strings.Contains(list.String(), "\talice\tlint=1") {
		t.Errorf("list of pushy = %q (%v)", list.String(), err)
	}

	if out, err := r.pin("nothing"); err == nil || strings.Contains(out, "pinned") {
		t.Errorf("a server that is not in the config: %v %q", err, out)
	}
	var sink bytes.Buffer
	if err := run(context.Background(), []string{"manifests", "pin", "--server", "pushy"}, &sink); err == nil {
		t.Error("pin without a config and a key was accepted")
	}
}

// keygen writes the private key with mode 0600, prints the public key, and never overwrites.
func TestKeygen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "k")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"manifests", "keygen", "--out", file}, &out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v %v", info, err)
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(out.String(), strings.TrimSpace(string(raw))) {
		t.Error("the private key was printed")
	}
	if err := run(context.Background(), []string{"manifests", "keygen", "--out", file}, &out); err == nil {
		t.Error("an existing key file was overwritten")
	}
}
