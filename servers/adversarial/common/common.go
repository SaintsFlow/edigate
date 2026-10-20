// Package common is the shell of the three hostile MCP servers of the test stand
// (evil-validator, shadow-carrier, rugpull-rates). They exist to attack the gateway on
// purpose and only run on the stand: compose gives them no published port.
//
// Handler wraps an MCP server in an HTTP handler that serves /mcp (stateless Streamable
// HTTP) and /healthz, and writes one JSON line per request to the log: the names of the
// headers and whether any header holds a token of a client of the gateway. It never
// writes a header value (test T-UP-05 reads these lines to see that the token of the
// agent did not reach the server).
//
// Main runs one server from the environment: ADV_LISTEN (default 127.0.0.1:8090), and
// optionally ADV_AUTH_HEADER and ADV_AUTH_VALUE, a header the gateway must send (a request
// without it gets 401; the log says whether it matched). "<binary> healthcheck" calls
// /healthz on the local port and exits 0 or 1.
package common

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// clientTokenPrefix is how a token of a client of the gateway starts (internal/store).
const clientTokenPrefix = "edg_"

// requestLog is one log line.
type requestLog struct {
	Event             string   `json:"event"`
	Server            string   `json:"server"`
	Method            string   `json:"method"`
	Path              string   `json:"path"`
	HeaderNames       []string `json:"header_names"`
	ClientTokenSeen   bool     `json:"client_token_present"`
	AuthHeaderMatched *bool    `json:"auth_header_ok,omitempty"`
}

// Handler serves the MCP server at /mcp and logs the headers of every request to out.
// authHeader and authValue may be empty: then no header is required.
func Handler(name string, srv *mcp.Server, out io.Writer, authHeader, authValue string) http.Handler {
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/mcp", sdk)
	enc := json.NewEncoder(out)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		line := requestLog{Event: "request", Server: name, Method: r.Method, Path: r.URL.Path}
		for h, values := range r.Header {
			line.HeaderNames = append(line.HeaderNames, h)
			for _, v := range values {
				if strings.Contains(v, clientTokenPrefix) {
					line.ClientTokenSeen = true
				}
			}
		}
		sort.Strings(line.HeaderNames)
		ok := true
		if authHeader != "" {
			ok = subtle.ConstantTimeCompare([]byte(r.Header.Get(authHeader)), []byte(authValue)) == 1
			line.AuthHeaderMatched = &ok
		}
		_ = enc.Encode(line)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Main runs the server built by build until SIGINT or SIGTERM.
func Main(name string, build func() *mcp.Server) {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(name, build); err != nil {
		slog.Error(name+" stopped", "error", err.Error())
		os.Exit(1)
	}
}

func listenAddr() string {
	if v := os.Getenv("ADV_LISTEN"); v != "" {
		return v
	}
	return "127.0.0.1:8090"
}

func run(name string, build func() *mcp.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	header, value := os.Getenv("ADV_AUTH_HEADER"), os.Getenv("ADV_AUTH_VALUE")
	if (header == "") != (value == "") {
		return errors.New("ADV_AUTH_HEADER and ADV_AUTH_VALUE go together")
	}
	srv := &http.Server{
		Addr: listenAddr(), Handler: Handler(name, build(), os.Stdout, header, value), ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info(name+" listening", "addr", srv.Addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func healthcheck() int {
	_, port, err := net.SplitHostPort(listenAddr())
	if err != nil {
		return 1
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthz unreachable")
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
