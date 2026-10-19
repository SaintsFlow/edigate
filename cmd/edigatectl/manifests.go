package main

// The commands that pin the descriptions of upstream tools (flow P8):
//
//	manifests keygen --out <file>
//	manifests pin    --config <file> --server S --signing-key <file> [--accept-lint] [--by NAME]
//	manifests list   [--server S]
//
// keygen makes an ed25519 key pair: the private key goes to the file (mode 0600, never
// overwritten), the public key is printed for EDIGATE_MANIFEST_PUBKEY. The gateway needs
// only the public key. pin reads the live tools/list of the server straight from the
// config, lints every description, and signs and stores a pin per tool. It refuses, and
// writes nothing, when a description names another tool (tool_reference), and when it
// addresses the model (imperative) unless --accept-lint is given; the findings are then
// stored with the pin.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SaintsFlow/edigate/internal/config"
	"github.com/SaintsFlow/edigate/internal/contracts"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/upstream"
)

func manifests(ctx context.Context, args []string, out io.Writer) error {
	const form = "manifests: expected: keygen --out FILE | pin --config FILE --server S --signing-key FILE [--accept-lint] | list [--server S]"
	if len(args) == 0 {
		return usageError{form}
	}
	switch args[0] {
	case "keygen":
		return manifestsKeygen(args[1:], out)
	case "pin":
		return manifestsPin(ctx, args[1:], out)
	case "list":
		return manifestsList(ctx, args[1:], out)
	}
	return usageError{form}
}

func manifestsKeygen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("manifests keygen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("out", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *path == "" {
		return usageError{"manifests keygen: --out FILE is required"}
	}
	pub, priv, err := upstream.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("the key file: %w", err)
	}
	if _, err := f.WriteString(priv + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintln(out, pub)
	return nil
}

func manifestsPin(ctx context.Context, args []string, out io.Writer) error {
	const form = "manifests pin: --config FILE --server S --signing-key FILE [--accept-lint] [--by NAME]"
	fs := flag.NewFlagSet("manifests pin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", os.Getenv("EDIGATE_CONFIG"), "")
	server := fs.String("server", "", "")
	keyPath := fs.String("signing-key", "", "")
	acceptLint := fs.Bool("accept-lint", false, "")
	by := fs.String("by", "admin", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *cfgPath == "" || *server == "" || *keyPath == "" {
		return usageError{form}
	}
	servers, err := config.LoadUpstreams(*cfgPath)
	if err != nil {
		return err
	}
	var srv *upstream.Server
	for i := range servers {
		if servers[i].Name == *server {
			srv = &servers[i]
		}
	}
	if srv == nil {
		return fmt.Errorf("server %q is not in the config", *server)
	}
	rawKey, err := os.ReadFile(*keyPath)
	if err != nil {
		return fmt.Errorf("signing key: %w", err)
	}
	priv, err := upstream.ParsePrivateKey(strings.TrimSpace(string(rawKey)))
	if err != nil {
		return err
	}
	secret := ""
	if srv.AuthHeader != nil {
		if secret = os.Getenv(srv.AuthHeader.Env); secret == "" {
			return fmt.Errorf("the variable %s is empty", srv.AuthHeader.Env)
		}
	}
	live, err := upstream.FetchTools(ctx, *srv, secret)
	if err != nil {
		return fmt.Errorf("read tools/list of %s: %w", srv.Name, err)
	}
	return withStore(ctx, func(st *store.Store) error {
		names, err := knownNames(ctx, st, srv.Name)
		if err != nil {
			return err
		}
		type plan struct {
			pin      store.Manifest
			findings []upstream.Finding
		}
		var plans []plan
		refused := false
		for _, t := range live {
			findings := upstream.Lint(t.Description, names)
			for _, f := range findings {
				fmt.Fprintf(out, "lint %s.%s: %s: %s\n", srv.Name, t.Name, f.Kind, f.Detail)
			}
			if upstream.HasKind(findings, upstream.ToolReference) || (upstream.HasKind(findings, upstream.Imperative) && !*acceptLint) {
				refused = true
			}
			hash, err := upstream.Hash(t)
			if err != nil {
				return fmt.Errorf("hash %s: %w", t.Name, err)
			}
			lint, err := json.Marshal(findings)
			if err != nil {
				return err
			}
			if findings == nil {
				lint = []byte("[]")
			}
			plans = append(plans, plan{
				pin:      store.Manifest{Server: srv.Name, Tool: t.Name, SHA256: hash[:], Signature: upstream.Sign(priv, srv.Name, hash), Lint: lint, PinnedBy: *by},
				findings: findings,
			})
		}
		if refused {
			return errors.New("not pinned: a description names another tool, or addresses the model (imperative: --accept-lint pins it)")
		}
		for _, p := range plans {
			if err := st.PinManifest(ctx, p.pin); err != nil {
				return err
			}
			fmt.Fprintf(out, "pinned %s.%s sha256=%x lint=%d\n", p.pin.Server, p.pin.Tool, p.pin.SHA256, len(p.findings))
		}
		return nil
	})
}

// knownNames are the tool names a description of this server must not mention: the tools
// of the gateway, and the pinned tools of the other servers, bare and with their prefix.
func knownNames(ctx context.Context, st *store.Store, self string) ([]string, error) {
	names := contracts.ToolNames()
	pins, err := st.ListManifests(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, p := range pins {
		if p.Server != self {
			names = append(names, p.Tool, upstream.Prefix+p.Server+"."+p.Tool)
		}
	}
	return names, nil
}

func manifestsList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("manifests list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError{"manifests list: [--server S]"}
	}
	return withStore(ctx, func(st *store.Store) error {
		pins, err := st.ListManifests(ctx, *server)
		if err != nil {
			return err
		}
		for _, p := range pins {
			var findings []upstream.Finding
			_ = json.Unmarshal(p.Lint, &findings)
			fmt.Fprintf(out, "%s\t%s\t%x\t%s\t%s\tlint=%d\n", p.Server, p.Tool, p.SHA256, p.PinnedAt, p.PinnedBy, len(findings))
		}
		return nil
	})
}
