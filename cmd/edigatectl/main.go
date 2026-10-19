// Command edigatectl is the operator tool of the gateway.
//
//	edigatectl tenant add <id> <name> [--interchange-id Q:ID]
//	edigatectl partner add --tenant T --partner Q:ID --role customer|supplier --name N
//	edigatectl token issue --tenant T --kind agent|operator|admin|partner --id P [--partner ISA05:ISA06]
//	edigatectl token revoke --id P
//	edigatectl call --url URL --token TOKEN <tool> [JSON-arguments]
//	edigatectl parse <file>
//	edigatectl send --url URL --token T --from Q:ID --to Q:ID --file F [--message-id ID]
//	edigatectl fixtures load <fixtures.json> [--partner-tokens-out <file> [--reply-url <url>]]
//	edigatectl db create|drop <name>
//	edigatectl txn reopen --url URL --token OPERATOR_TOKEN <key-hex>
//	edigatectl approvals list|approve|reject --url URL --token OPERATOR_TOKEN [--status S] [id]
//
// tenant, partner, token and fixtures commands work on the database directly
// (EDIGATE_DB_URL): they are setup done on the gateway machine, not a network
// API. "token issue" prints the token to stdout once; nothing else ever shows it.
// "call" is a debugging MCP client. "parse" reads a file with the strict X12
// parser and needs no database. "send" posts an interchange to /edi/inbound.
// "fixtures load" reads the file of the corpus generator (tenant, partners with
// limits, price list) and is safe to run again. With --partner-tokens-out it also issues
// a token to each customer partner that has none and adds it to the JSON file
// partner-emulator reads; with --reply-url (the public address of one gateway process, a
// slot of the benchmark) the keys of that slot are "<url> <partner id>" and the entries of
// other slots stay. "db create|drop" makes and removes the database of a slot on the server
// of EDIGATE_DB_URL. "txn reopen" is an operator
// action and goes over HTTP to /ops/ of the gateway at URL (for example
// http://127.0.0.1:18080), so the audit log knows the operator. "approvals" lists
// the approval requests of the operator's tenant and approves or rejects one; a
// request that needs two operators stays pending until a second, different
// operator token approves it.
//
//	edigatectl manifests keygen --out FILE
//	edigatectl manifests pin --config FILE --server S --signing-key FILE [--accept-lint] [--by NAME]
//	edigatectl manifests list [--server S]
//
// "manifests" pins the descriptions of the tools of an upstream MCP server (see
// manifests.go): keygen makes the signing key, pin reads the live tool list of the server
// named in the config, lints it and stores a signed pin per tool, list shows the pins.
// Exit codes: 0 ok, 1 failed (for call: the server answered with a JSON-RPC
// error; for parse: something was rejected; for send and txn reopen: the answer
// was not 200), 2 wrong command line.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
	"github.com/SaintsFlow/edigate/internal/store"
)

const usage = `usage:
  edigatectl tenant add <id> <name> [--interchange-id Q:ID]
  edigatectl partner add --tenant T --partner Q:ID --role customer|supplier --name N
  edigatectl token issue --tenant T --kind agent|operator|admin|partner --id P [--partner ISA05:ISA06]
  edigatectl token revoke --id P
  edigatectl call --url URL --token TOKEN <tool> [JSON-arguments]
  edigatectl parse <file>
  edigatectl send --url URL --token T --from Q:ID --to Q:ID --file F [--message-id ID]
  edigatectl fixtures load <fixtures.json> [--partner-tokens-out <file> [--reply-url <url>]]
  edigatectl db create|drop <name>
  edigatectl txn reopen --url URL --token OPERATOR_TOKEN <key-hex>
  edigatectl approvals list|approve|reject --url URL --token OPERATOR_TOKEN [--status S] [id]
  edigatectl manifests keygen --out FILE
  edigatectl manifests pin --config FILE --server S --signing-key FILE [--accept-lint] [--by NAME]
  edigatectl manifests list [--server S]
`

// qualifiedID is ISA05:ISA06 or ISA07:ISA08: a two-character qualifier and an
// identifier of up to 15 characters.
var qualifiedID = regexp.MustCompile(`^[A-Za-z0-9]{2}:[A-Za-z0-9._-]{1,15}$`)

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
		if len(args) >= 4 && args[1] == "add" {
			return tenantAdd(ctx, args[2], args[3], args[4:], stdout)
		}
		return usageError{"tenant: expected: add <id> <name> [--interchange-id Q:ID]"}
	case "partner":
		if len(args) >= 2 && args[1] == "add" {
			return partnerAdd(ctx, args[2:], stdout)
		}
		return usageError{"partner: expected: add --tenant T --partner Q:ID --role customer|supplier --name N"}
	case "parse":
		return parseFile(args[1:], stdout)
	case "send":
		return send(ctx, args[1:], stdout)
	case "fixtures":
		if len(args) >= 3 && args[1] == "load" {
			return fixturesLoad(ctx, args[2:], stdout)
		}
		return usageError{"fixtures: expected: load <fixtures.json> [--partner-tokens-out <file> [--reply-url <url>]]"}
	case "db":
		return dbCommand(ctx, args[1:], stdout)
	case "txn":
		if len(args) >= 2 && args[1] == "reopen" {
			return txnReopen(ctx, args[2:], stdout)
		}
		return usageError{"txn: expected: reopen --url URL --token T <key-hex>"}
	case "approvals":
		return approvals(ctx, args[1:], stdout)
	case "manifests":
		return manifests(ctx, args[1:], stdout)
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

// tenantAdd creates a tenant, or sets the interchange id of an existing one.
func tenantAdd(ctx context.Context, id, name string, flags []string, out io.Writer) error {
	fs := flag.NewFlagSet("tenant add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	interchangeID := fs.String("interchange-id", "", "")
	if err := fs.Parse(flags); err != nil || fs.NArg() != 0 {
		return usageError{"tenant add: bad flags"}
	}
	if *interchangeID != "" && !qualifiedID.MatchString(*interchangeID) {
		return usageError{"tenant add: --interchange-id must look like ZZ:ZZTESTSUPPL"}
	}
	return withStore(ctx, func(st *store.Store) error {
		if err := st.AddTenant(ctx, id, name, *interchangeID); err != nil {
			return err
		}
		fmt.Fprintln(out, "tenant ready:", id)
		return nil
	})
}

func partnerAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("partner add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "")
	partner := fs.String("partner", "", "")
	role := fs.String("role", "", "")
	name := fs.String("name", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError{"partner add: bad flags"}
	}
	if *tenant == "" || *name == "" || !qualifiedID.MatchString(*partner) {
		return usageError{"partner add: --tenant, --name and --partner ZZ:ID are required"}
	}
	if *role != "customer" && *role != "supplier" {
		return usageError{"partner add: --role must be customer or supplier"}
	}
	return withStore(ctx, func(st *store.Store) error {
		if err := st.AddPartner(ctx, store.Partner{TenantID: *tenant, PartnerID: *partner, Role: *role, Name: *name}); err != nil {
			return fmt.Errorf("add partner: %w", err)
		}
		fmt.Fprintln(out, "partner ready:", *partner)
		return nil
	})
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
		if *kind == "partner" {
			if _, err := st.PartnerAccess(ctx, *tenant, *partner); errors.Is(err, store.ErrNotFound) {
				return errors.New("this partner is not registered for the tenant: run partner add first")
			} else if err != nil {
				return err
			}
		}
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

// parseSummary is what "parse" prints: one JSON line about the file.
type parseSummary struct {
	Envelope string         `json:"envelope"` // accepted or rejected
	Reason   string         `json:"reason,omitempty"`
	Detail   string         `json:"detail,omitempty"`
	Sender   string         `json:"sender,omitempty"`
	Receiver string         `json:"receiver,omitempty"`
	ISA13    string         `json:"isa13,omitempty"`
	Groups   []groupSummary `json:"groups,omitempty"`
}

type groupSummary struct {
	GS01 string       `json:"gs01"`
	GS06 string       `json:"gs06"`
	Sets []setSummary `json:"sets"`
}

type setSummary struct {
	ST01   string      `json:"st01"`
	ST02   string      `json:"st02"`
	Status string      `json:"status"` // accepted or rejected
	Errors []x12.Error `json:"errors,omitempty"`
}

// parseFile reads a file with the strict parser and prints a JSON summary. It
// fails (exit 1) when the envelope or any set is rejected.
func parseFile(args []string, out io.Writer) error {
	if len(args) != 1 {
		return usageError{"parse: expected one file"}
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	ic, err := x12.Parse(raw)
	var env *x12.EnvelopeError
	if errors.As(err, &env) {
		if err := printJSON(out, parseSummary{Envelope: "rejected", Reason: env.Reason, Detail: env.Detail}); err != nil {
			return err
		}
		return errors.New("the envelope is rejected")
	}
	if err != nil {
		return err
	}

	sum := parseSummary{Envelope: "accepted", Sender: ic.SenderID(), Receiver: ic.ReceiverID(), ISA13: ic.ControlNumber()}
	allAccepted := true
	for _, g := range ic.Groups {
		gs := groupSummary{GS01: g.GS.Value(1), GS06: g.GS.Value(6), Sets: []setSummary{}}
		for _, s := range g.Sets {
			set := setSummary{ST01: x12.Printable(s.ST.Value(1)), ST02: x12.Printable(s.ST.Value(2)), Status: "accepted", Errors: s.Errors}
			if len(s.Errors) > 0 {
				set.Status, allAccepted = "rejected", false
			}
			gs.Sets = append(gs.Sets, set)
		}
		sum.Groups = append(sum.Groups, gs)
	}
	if err := printJSON(out, sum); err != nil {
		return err
	}
	if !allAccepted {
		return errors.New("some sets are rejected")
	}
	return nil
}

func printJSON(out io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(b))
	return nil
}

// send posts one interchange to /edi/inbound and prints the HTTP status and the
// body of the answer (an MDN or an error). Only 200 is success.
func send(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", "", "")
	token := fs.String("token", "", "")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	file := fs.String("file", "", "")
	messageID := fs.String("message-id", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *url == "" || *token == "" || *from == "" || *to == "" || *file == "" {
		return usageError{"send: --url, --token, --from, --to and --file are required"}
	}
	body, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	if *messageID == "" {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		*messageID = "<" + hex.EncodeToString(b) + "@edigatectl>"
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*token)
	req.Header.Set("Content-Type", "application/edi-x12")
	req.Header.Set("Message-Id", *messageID)
	req.Header.Set("AS2-From", *from)
	req.Header.Set("AS2-To", *to)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "HTTP %d\n%s\n", resp.StatusCode, answer)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
	}
	return nil
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
