// Command llm-switch is the unified LLM API gateway: OpenAI- and
// Anthropic-compatible surfaces on one port, routed to multiple upstream
// providers with an embedded admin UI.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/spf13/cobra"

	"github.com/PWZER/llm-switch/internal/api"
	"github.com/PWZER/llm-switch/internal/auth"
	"github.com/PWZER/llm-switch/internal/config"
	"github.com/PWZER/llm-switch/internal/daemon"
	"github.com/PWZER/llm-switch/internal/engine"
	"github.com/PWZER/llm-switch/internal/gateway"
	"github.com/PWZER/llm-switch/internal/stats"
	"github.com/PWZER/llm-switch/internal/store"
	"github.com/PWZER/llm-switch/internal/web"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func main() {
	cfg := &config.Config{}
	if err := newRootCommand(cfg).Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCommand assembles the CLI. The root command boots the server;
// subcommands added here handle instance lifecycle (upgrade, status, stop).
func newRootCommand(cfg *config.Config) *cobra.Command {
	root := &cobra.Command{
		Use:     "llm-switch",
		Short:   "Unified LLM API gateway: OpenAI- and Anthropic-compatible surfaces on one port",
		Version: version,
		// Run failures are runtime errors, not usage mistakes — keep the
		// help text out of their output.
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg.Finalize(version)
			if cfg.Daemon && !daemon.IsChild() {
				os.Exit(daemon.Spawn(cfg.DataDir))
			}
			if err := run(cfg); err != nil {
				daemon.Notify(err)
				slog.Error("fatal", "err", err)
				os.Exit(1)
			}
			return nil
		},
	}
	root.SetVersionTemplate("llm-switch {{.Version}}\n")
	config.RegisterPersistentFlags(root.PersistentFlags(), cfg)
	config.RegisterFlags(root.Flags(), cfg)
	return root
}

func run(cfg *config.Config) error {
	logger := newLogger(cfg.LogFormat)
	slog.SetDefault(logger)

	// Single-instance lock, held for the process lifetime in every mode —
	// it protects the single-writer SQLite database.
	unlock, err := daemon.Lock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()

	logger.Info("starting llm-switch", "version", version, "addr", cfg.Addr, "data_dir", cfg.DataDir)

	db, err := store.Open(cfg.DataDir, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	st, err := store.New(db)
	if err != nil {
		return err
	}
	defer st.Close()

	// First boot only: seed the built-in vendor providers/channels so the
	// gateway works out of the box once an API key is added.
	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	seeded, err := st.SeedDefaultProviders(bootCtx)
	if err != nil {
		cancel()
		return err
	}
	if seeded {
		logger.Info("seeded default providers")
	}

	admin := auth.NewAdmin(st)
	generated, err := admin.Bootstrap(bootCtx, cfg.AdminPassword)
	cancel()
	if err != nil {
		return err
	}
	if generated != "" {
		logger.Warn("generated admin password (shown once; change it in Settings)",
			"password", generated)
	}

	// Routing snapshot: rebuilt at boot and after every admin mutation.
	holder := engine.NewHolder()
	if err := holder.Rebuild(context.Background(), st); err != nil {
		return err
	}
	pool := gateway.NewAccountPool()
	gw := gateway.New(holder, pool, gateway.NewUpstreamClient(), stats.New(st))
	defer gw.Log.Close(context.Background())

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Free health endpoints (unauthenticated).
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := db.Ping(); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	// Data plane: client-key auth (snapshot lookup, no DB on the hot path),
	// then the gateway routes. NOTE: never wrap /v1 in a Timeout middleware —
	// it kills long streams.
	validateKey := func(key string) (gateway.ClientKeyRecord, bool) {
		snap := holder.Load()
		if snap == nil {
			return gateway.ClientKeyRecord{}, false
		}
		rec, ok := snap.ClientKeys[engine.HashKey(key)]
		return gateway.ClientKeyRecord{ID: rec.ID, Name: rec.Name}, ok
	}
	v1 := chi.NewRouter()
	v1.Use(gateway.ClientKeyAuth(validateKey))
	gw.Mount(v1)
	r.Mount("/v1", v1)

	// Prefixed protocol surfaces: the base_url prefix — not request headers —
	// pins the /models listing shape and the auth-failure error shape. Claude
	// Code only exposes ANTHROPIC_BASE_URL, so its models URL is always
	// <base_url>/v1/models; /anthropic guarantees it the decorated Anthropic
	// listing. Bare /v1 stays for existing clients (header-sniffed shape).
	anthroV1 := chi.NewRouter()
	anthroV1.Use(gateway.ClientKeyAuthFor("anthropic", validateKey))
	gw.MountAnthropic(anthroV1)
	r.Mount("/anthropic/v1", anthroV1)

	openaiV1 := chi.NewRouter()
	openaiV1.Use(gateway.ClientKeyAuthFor("openai", validateKey))
	gw.MountOpenAI(openaiV1)
	r.Mount("/openai/v1", openaiV1)

	// Admin API with the post-mutation snapshot reload hook.
	apiSrv := &api.Server{
		St:        st,
		Admin:     admin,
		Reload:    func(ctx context.Context) error { return holder.Rebuild(ctx, st) },
		Snapshot:  holder,
		Cooldowns: pool.Cooldowns,
		Dropped:   gw.Log.Dropped,
	}
	r.Mount("/api/v1", apiSrv.Router())

	// Admin UI.
	if !cfg.WebDev {
		r.Handle("/*", web.Handler())
	}

	// Base context for every request: canceled at the start of shutdown so
	// in-flight streams (which derive their upstream calls and idle watchdogs
	// from the request context) unblock immediately instead of holding
	// srv.Shutdown for its full timeout.
	baseCtx, stopRequests := context.WithCancel(context.Background())
	defer stopRequests()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		// No WriteTimeout: streaming responses must live as long as needed.
	}

	errCh := make(chan error, 1)
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	// The socket is bound: report startup success to a daemon parent and
	// drop the pid file used to stop the daemon.
	daemon.Notify(nil)
	if pidPath := daemon.WritePid(cfg.DataDir); pidPath != "" {
		defer os.Remove(pidPath)
	}
	go func() {
		logger.Info("http server listening", "addr", cfg.Addr, "web_dev", cfg.WebDev)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		logger.Info("shutting down", "signal", sig.String())
		// A second Ctrl+C restores the default behavior: exit immediately.
		signal.Stop(stop)
		stopRequests()
	}

	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	closeCtx, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel3()
	if err := gw.Log.Close(closeCtx); err != nil {
		logger.Error("stats drain failed", "err", err)
	}
	logger.Info("bye")
	return nil
}

func newLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
