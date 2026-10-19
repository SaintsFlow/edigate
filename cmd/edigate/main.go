// Command edigate runs the gateway: config -> database pool -> migrations ->
// edge + MCP server, the partner entrance and the operator entrance -> HTTP
// server (/mcp, /edi/inbound, /ops/, /healthz), the relay that delivers the outbox
// (to the ERP, the warehouse and, as AS2 transmissions, to the partner) and the saga
// runner. It stops gracefully on SIGINT or SIGTERM.
//
// The flag --mode (or EDIGATE_MODE) makes the same binary play one configuration of the
// benchmark: full (the default), plain, reqhash, no-sanitize, no-policy, no-bizkey or
// no-contracts. An unknown value stops the start with exit code 2.
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
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/approval"
	"github.com/SaintsFlow/edigate/internal/config"
	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/inbound"
	"github.com/SaintsFlow/edigate/internal/mcpserver"
	"github.com/SaintsFlow/edigate/internal/mode"
	"github.com/SaintsFlow/edigate/internal/ops"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/quarantine"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/tools"
	"github.com/SaintsFlow/edigate/internal/txn"
	"github.com/SaintsFlow/edigate/internal/txn/saga"
	"github.com/SaintsFlow/edigate/internal/upstream"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("edigate stopped", "error", err.Error())
		var bad badMode
		if errors.As(err, &bad) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// badMode is an unknown value of --mode or EDIGATE_MODE: the gateway does not start (exit code 2).
type badMode struct{ error }

func run() error {
	configPath := flag.String("config", "", "path to an optional YAML config file")
	modeText := flag.String("mode", "", "configuration to play: "+modeNames()+" (default full; EDIGATE_MODE)")
	flag.Parse()
	if *modeText == "" {
		*modeText = os.Getenv("EDIGATE_MODE")
	}
	m, err := mode.Parse(*modeText)
	if err != nil {
		return badMode{err}
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	log.Info("edigate mode", "mode", string(m))

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

	reader := quarantine.NewReader(quarantine.NewHTTPCompleter(cfg.QuarantineBaseURL, cfg.QuarantineAPIKey), cfg.QuarantineModel, st)
	engine, err := policy.New(ctx, policy.WithUpstreamAllow(cfg.UpstreamAllow))
	if err != nil {
		return err
	}
	var decider tools.Decider = engine
	if m == mode.NoPolicy {
		decider = policy.AllowAll{}
	}
	st.SetApprovalTTL(approval.TTL(cfg.TimeScale))
	go approval.Run(ctx, st, log)
	up, err := upstream.New(cfg.Upstreams, cfg.ManifestPubKey, st, log, os.Getenv)
	if err != nil {
		return err
	}
	// At start every upstream is read once: a tool without a valid pin is named in the log. It
	// runs apart, so that a server that does not answer cannot hold the start back.
	go up.Refresh(ctx, nil)
	// The clients of the other systems. The relay and the sagas (modes with an outbox) and the
	// open tools (plain and reqhash) use the same ones. The partner clients send the public
	// address of this process, so that the 997 of a document comes back to it (a benchmark slot).
	var env tools.PlainEnv
	effects := adapters.Mux{}
	if cfg.OnecURL != "" {
		onec := adapters.NewOnec(cfg.OnecURL)
		effects.Onec, env.Onec = onec, onec
	}
	var as2 *adapters.AS2
	if cfg.PartnerURL != "" {
		mail := adapters.NewMail(cfg.PartnerURL).WithReplyTo(cfg.PublicURL) // the letters go to the same stand service
		as2 = adapters.NewAS2(cfg.PartnerURL).WithReplyTo(cfg.PublicURL)
		effects.Mail, env.Mail, env.AS2 = mail, mail, as2
	}
	var facts saga.Facts // nil without a warehouse: shipment sagas then wait
	if cfg.WMSURL != "" {
		wms := adapters.NewWMS(cfg.WMSURL)
		effects.WMS, env.WMS, facts = wms, wms, wms
	}

	mcpServer := mcpserver.New(st, reader, decider, log, mcpserver.WithUpstream(up), mcpserver.WithMode(m), mcpserver.WithPlain(env))
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	// One gate for both entrances: they share the bucket per client address.
	gate := edge.NewGate(ctx, st, log)
	var mcpHandler http.Handler = gate.MCP(sdk)
	if cfg.TransportLog != "" {
		tlog, err := edge.OpenTransportLog(cfg.TransportLog)
		if err != nil {
			return err
		}
		defer tlog.Close()
		mcpHandler = tlog.Wrap(mcpHandler)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/edi/inbound", inbound.New(gate, st, log))
	mux.Handle("/ops/", ops.New(gate, st, log))
	mux.HandleFunc("GET /healthz", healthz(st, m))

	// The outbox, the relay and the sagas belong to the modes that have them: the open tools of
	// plain and reqhash do their effects at once and leave nothing in the outbox.
	chaos := txn.NewChaos(cfg.ChaosExitBeforeSend, cfg.ChaosExitAfterApply, cfg.ChaosCrashAfter, cfg.ChaosSeed)
	chaos.Start()
	switch {
	case m.Open():
		log.Info("the relay and the sagas are off in this mode", "mode", string(m))
	case cfg.OnecURL != "":
		if cfg.PartnerURL == "" {
			log.Warn("EDIGATE_PARTNER_URL is not set: documents for partners and letters stay queued")
		}
		if cfg.WMSURL == "" {
			log.Warn("EDIGATE_WMS_URL is not set: reservations and shipments stay queued")
		}
		runner := &saga.Runner{Store: st, Adapter: effects, Facts: facts, Policy: decider, Scale: cfg.TimeScale, Log: log}
		relay := &txn.Relay{
			Store: st, Adapter: effects, Workers: cfg.RelayWorkers,
			Scale: cfg.TimeScale, Log: log, Chaos: chaos,
		}
		if as2 != nil {
			relay.AS2 = as2
		}
		go relay.Run(ctx)
		go runner.Run(ctx)
	default:
		log.Warn("EDIGATE_ONEC_URL is not set: the relay is off and orders stay queued")
	}

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

// modeNames lists the modes for the help text of --mode.
func modeNames() string {
	names := make([]string, len(mode.All))
	for i, m := range mode.All {
		names[i] = string(m)
	}
	return strings.Join(names, ", ")
}

// healthz answers 200 when the database responds and its migration version is
// the latest embedded one, 503 otherwise. The answer names the mode the process plays.
func healthz(st *store.Store, m mode.Mode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		status := "ok"
		if err := st.CheckMigrated(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			status = "unavailable"
		}
		_, _ = fmt.Fprintf(w, `{"status":%q,"mode":%q}`, status, string(m))
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
