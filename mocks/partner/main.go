// Command partner-emulator runs the stand's trading partner (see mocks/partner/server).
//
// Settings from the environment: PARTNER_DB_URL (required; it writes only to its schema
// mock_partner), PARTNER_LISTEN (default 127.0.0.1:8083), GATEWAY_URL (where the 997 goes),
// PARTNER_TOKENS_FILE (JSON {"<partner id>": "<token>"}, read again at every send), and the
// knobs PARTNER_ACK_DELAY_MS, PARTNER_ACK_STATUS (A, E, R, P or none, default A),
// PARTNER_MDN_DELAY_MS and PARTNER_MDN_DROP (0 to 1). POST /control changes the knobs at
// run time ({"ack_delay_ms", "ack_status", "mdn_delay_ms", "mdn_drop"}).
//
// "partner-emulator healthcheck" calls /healthz on the local port and exits 0 or 1 (the
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

	"github.com/SaintsFlow/edigate/mocks/partner/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("partner-emulator stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	dbURL := os.Getenv("PARTNER_DB_URL")
	if dbURL == "" {
		return errors.New("PARTNER_DB_URL is required")
	}
	listen := os.Getenv("PARTNER_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8083"
	}
	cfg := server.Config{
		Knobs:      server.Knobs{AckStatus: envOr("PARTNER_ACK_STATUS", "A")},
		GatewayURL: os.Getenv("GATEWAY_URL"), TokensFile: os.Getenv("PARTNER_TOKENS_FILE"),
		Log: slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
	var err error
	if cfg.AckDelayMS, err = envInt("PARTNER_ACK_DELAY_MS"); err != nil {
		return err
	}
	if cfg.MDNDelayMS, err = envInt("PARTNER_MDN_DELAY_MS"); err != nil {
		return err
	}
	if v := os.Getenv("PARTNER_MDN_DROP"); v != "" {
		if cfg.MDNDropRate, err = strconv.ParseFloat(v, 64); err != nil {
			return fmt.Errorf("PARTNER_MDN_DROP %q must be a number from 0 to 1", v)
		}
	}
	if !cfg.Knobs.Valid() {
		return errors.New("PARTNER_ACK_STATUS must be A, E, R, P or none; the delays at least 0; PARTNER_MDN_DROP from 0 to 1")
	}
	if cfg.GatewayURL == "" || cfg.TokensFile == "" {
		return errors.New("GATEWAY_URL and PARTNER_TOKENS_FILE are required")
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

	emulator := server.New(pool, cfg)
	srv := &http.Server{Addr: listen, Handler: emulator.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("partner-emulator listening", "addr", listen)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdown)
	emulator.Close()
	return err
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q must be a whole number", name, v)
	}
	return n, nil
}

func healthcheck() int {
	listen := os.Getenv("PARTNER_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8083"
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
