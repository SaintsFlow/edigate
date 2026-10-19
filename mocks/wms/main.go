// Command mock-wms runs mock-wms, the stand's stand-in for the warehouse system.
//
// Settings from the environment: MOCK_DB_URL (required; the mock writes only to its
// schema mock_wms), MOCK_LISTEN (default 127.0.0.1:8082), and the knobs
// MOCK_FAIL_AFTER_COMMIT_RATE (0 to 1), MOCK_LATENCY_MS, MOCK_REJECT_REF_PREFIX (shipments of
// orders with this prefix get 422) and MOCK_SHIP_QTY_FACTOR (a decimal multiplier of the
// shipped quantity, default 1: the fact of the warehouse differs from the order).
//
// "mock-wms healthcheck" calls /healthz on the local port and exits 0 or 1 (the
// container image has no curl).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SaintsFlow/edigate/mocks/wms/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("mock-wms stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	dbURL := os.Getenv("MOCK_DB_URL")
	if dbURL == "" {
		return errors.New("MOCK_DB_URL is required")
	}
	listen := os.Getenv("MOCK_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8082"
	}
	cfg := server.Config{RejectRefPrefix: os.Getenv("MOCK_REJECT_REF_PREFIX"), ShipQtyFactor: os.Getenv("MOCK_SHIP_QTY_FACTOR")}
	if v := os.Getenv("MOCK_FAIL_AFTER_COMMIT_RATE"); v != "" {
		rate, err := strconv.ParseFloat(v, 64)
		if err != nil || rate < 0 || rate > 1 {
			return fmt.Errorf("MOCK_FAIL_AFTER_COMMIT_RATE %q must be a number from 0 to 1", v)
		}
		cfg.FailAfterCommitRate = rate
	}
	if v := os.Getenv("MOCK_LATENCY_MS"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms < 0 {
			return fmt.Errorf("MOCK_LATENCY_MS %q must be a whole number of milliseconds", v)
		}
		cfg.Latency = time.Duration(ms) * time.Millisecond
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := server.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	srv := &http.Server{Addr: listen, Handler: server.New(pool, cfg).Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("mock-wms listening", "addr", listen)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func healthcheck() int {
	listen := os.Getenv("MOCK_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8082"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 1
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
