// Command llm-switch is the unified LLM API gateway: OpenAI- and
// Anthropic-compatible surfaces on one port, routed to multiple upstream
// providers with an embedded admin UI.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/PWZER/llm-switch/internal/api"
	"github.com/PWZER/llm-switch/internal/auth"
	"github.com/PWZER/llm-switch/internal/config"
	"github.com/PWZER/llm-switch/internal/engine"
	"github.com/PWZER/llm-switch/internal/gateway"
	"github.com/PWZER/llm-switch/internal/stats"
	"github.com/PWZER/llm-switch/internal/store"
	"github.com/PWZER/llm-switch/internal/web"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load(version)
	logger := newLogger(cfg.LogFormat)
	slog.SetDefault(logger)
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

	admin := auth.NewAdmin(st)
	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	pool := gateway.NewKeyPool()
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
	v1 := chi.NewRouter()
	v1.Use(gateway.ClientKeyAuth(func(key string) (gateway.ClientKeyRecord, bool) {
		snap := holder.Load()
		if snap == nil {
			return gateway.ClientKeyRecord{}, false
		}
		rec, ok := snap.ClientKeys[engine.HashKey(key)]
		return gateway.ClientKeyRecord{ID: rec.ID, Name: rec.Name}, ok
	}))
	gw.Mount(v1)
	r.Mount("/v1", v1)

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

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		// No WriteTimeout: streaming responses must live as long as needed.
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.Addr, "web_dev", cfg.WebDev)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
