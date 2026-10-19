// Command edigatectl is the operator tool of the gateway.
//
//	edigatectl tenant add <id> <name>
//	edigatectl token issue --tenant T --kind agent|operator|admin|partner --id P [--partner ISA05:ISA06]
//	edigatectl token revoke --id P
//	edigatectl call --url URL --token TOKEN <tool> [JSON-arguments]
//
// tenant and token commands work on the database directly (EDIGATE_DB_URL):
// they are setup done on the gateway machine, not a network API. "token issue"
// prints the token to stdout once; nothing else ever shows it.
// "call" is a debugging MCP client. Exit codes: 0 ok, 1 failed (for call: the
// server answered with a JSON-RPC error), 2 wrong command line.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/store"
)

const usage = `usage:
  edigatectl tenant add <id> <name>
  edigatectl token issue --tenant T --kind agent|operator|admin|partner --id P [--partner ISA05:ISA06]
  edigatectl token revoke --id P
  edigatectl call --url URL --token TOKEN <tool> [JSON-arguments]
`

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func main() {
	err := run(context.Background(), os.Args[1:], os.Stdout)
	var ue usageError
	switch {
	case err == nil:
	case errors.As(err, &ue):
		fmt.Fprintln(os.Stderr, ue.msg)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError{"no command"}
	}
	switch args[0] {
	case "tenant":
		if len(args) == 4 && args[1] == "add" {
			return withStore(ctx, func(st *store.Store) error { return tenantAdd(ctx, st, args[2], args[3], stdout) })
		}
		return usageError{"tenant: expected: add <id> <name>"}
	case "token":
		if len(args) < 2 {
			return usageError{"token: expected issue or revoke"}
		}
		switch args[1] {
		case "issue":
			return tokenIssue(ctx, args[2:], stdout)
		case "revoke":
			return tokenRevoke(ctx, args[2:], stdout)
		}
		return usageError{"token: unknown subcommand " + args[1]}
	case "call":
		return call(ctx, args[1:], stdout)
	}
	return usageError{"unknown command " + args[0]}
}

// withStore opens the database from EDIGATE_DB_URL and requires that the
// gateway has already applied the migrations.
func withStore(ctx context.Context, f func(*store.Store) error) error {
	url := os.Getenv("EDIGATE_DB_URL")
	if url == "" {
		return errors.New("EDIGATE_DB_URL is not set")
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.CheckMigrated(ctx); err != nil {
		return fmt.Errorf("%w: start edigate once first", err)
	}
	return f(st)
}

func tenantAdd(ctx context.Context, st *store.Store, id, name string, out io.Writer) error {
	if err := st.AddTenant(ctx, id, name); err != nil {
		return err
	}
	fmt.Fprintln(out, "tenant ready:", id)
	return nil
}

func tokenIssue(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("token issue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "")
	kind := fs.String("kind", "", "")
	id := fs.String("id", "", "")
	partner := fs.String("partner", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError{"token issue: bad flags"}
	}
	switch *kind {
	case "agent", "operator", "admin":
		if *partner != "" {
			return usageError{"token issue: --partner is only for kind partner"}
		}
	case "partner":
		if *partner == "" {
			return usageError{"token issue: kind partner needs --partner ISA05:ISA06"}
		}
	default:
		return usageError{"token issue: --kind must be agent, operator, admin or partner"}
	}
	if *tenant == "" || *id == "" {
		return usageError{"token issue: --tenant and --id are required"}
	}
	return withStore(ctx, func(st *store.Store) error {
		token, sum, err := store.NewToken()
		if err != nil {
			return err
		}
		p := store.Principal{ID: *id, TenantID: *tenant, Kind: *kind, PartnerID: *partner}
		if err := st.IssuePrincipal(ctx, p, sum); err != nil {
			return fmt.Errorf("issue token: %w", err)
		}
		fmt.Fprintln(out, token)
		return nil
	})
}

func tokenRevoke(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.String("id", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *id == "" {
		return usageError{"token revoke: --id is required"}
	}
	return withStore(ctx, func(st *store.Store) error {
		if err := st.RevokePrincipal(ctx, *id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errors.New("no active principal with this id")
			}
			return err
		}
		fmt.Fprintln(out, "revoked:", *id)
		return nil
	})
}

// call runs one tool through the MCP client and prints structuredContent as
// one JSON line. A JSON-RPC error is printed as {"error":{...}} and fails.
func call(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", "", "")
	token := fs.String("token", "", "")
	if err := fs.Parse(args); err != nil || *url == "" || *token == "" || fs.NArg() < 1 || fs.NArg() > 2 {
		return usageError{"call: --url, --token and a tool name are required"}
	}
	tool := fs.Arg(0)
	arguments := map[string]any{}
	if fs.NArg() == 2 {
		if err := json.Unmarshal([]byte(fs.Arg(1)), &arguments); err != nil {
			return usageError{"call: arguments must be a JSON object"}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             *url,
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: *token, next: http.DefaultTransport}},
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "edigatectl", Version: "0.1.0-dev"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return printRPCError(out, err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: arguments})
	if err != nil {
		return printRPCError(out, err)
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(structured))
	return nil
}

// printRPCError prints a JSON-RPC error from the server as {"error":{...}} and
// returns a plain error so the exit code is 1; other errors are returned as is.
func printRPCError(out io.Writer, err error) error {
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		return err
	}
	body, mErr := json.Marshal(map[string]any{"error": wire})
	if mErr != nil {
		return mErr
	}
	fmt.Fprintln(out, string(body))
	return errors.New("server returned an error")
}

// bearerTransport adds the Authorization header to every request.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}
