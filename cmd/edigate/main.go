// Command edigate runs the gateway: config -> database pool -> migrations ->
// edge + MCP server and the partner entrance -> HTTP server (/mcp,
// /edi/inbound, /healthz). It stops gracefully on SIGINT or SIGTERM.
//
// "edigate healthcheck" is the container healthcheck (the distroless image has
// no curl): it calls /healthz on the local listen port and exits 0 or 1.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/config"
	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/inbound"
	"github.com/SaintsFlow/edigate/internal/mcpserver"
	"github.com/SaintsFlow/edigate/internal/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("edigate stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to an optional YAML config file")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	mcpServer := mcpserver.New(st, log)
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	// One gate for both entrances: they share the bucket per client address.
	gate := edge.NewGate(ctx, st, log)
	mux := http.NewServeMux()
	mux.Handle("/mcp", gate.MCP(sdk))
	mux.Handle("/edi/inbound", inbound.New(gate, st, log))
	mux.HandleFunc("GET /healthz", healthz(st))

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("edigate listening", "addr", cfg.Listen)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthz answers 200 when the database responds and its migration version is
// the latest embedded one, 503 otherwise.
func healthz(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := st.CheckMigrated(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// healthcheck calls /healthz on this machine and returns the exit code.
func healthcheck() int {
	listen := os.Getenv("EDIGATE_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad EDIGATE_LISTEN")
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthz unreachable")
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthz status", resp.StatusCode)
		return 1
	}
	return 0
}
