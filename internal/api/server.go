// Package api implements the admin REST surface under /api/v1.
package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/PWZER/llm-switch/internal/auth"
	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

// Server bundles the dependencies of the admin API.
type Server struct {
	St    *store.Store
	Admin *auth.Admin
	// Reload rebuilds the routing snapshot; called after every successful
	// mutation so config changes take effect on the next request.
	Reload func(ctx context.Context) error
	// Snapshot exposes the current engine holder (version reporting).
	Snapshot interface{ Version() int64 }
	// Cooldowns reports accounts currently cooling down (account id -> until).
	Cooldowns func() map[int64]time.Time
	// Dropped reports rows dropped by the stats writer (backpressure counter).
	Dropped func() int64
}

// Router builds the /api/v1 chi router. Login and health are public;
// everything else requires a valid admin session.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Public
	r.Post("/auth/login", s.handleLogin)
	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteEnvelope(w, req, map[string]string{"status": "ok"})
	})

	// Authenticated
	r.Group(func(pr chi.Router) {
		pr.Use(s.requireSession)
		pr.Use(s.reloadAfterMutation)
		s.mountProtected(pr)
	})
	return r
}

// reloadAfterMutation triggers a snapshot rebuild after any mutating request
// that completed successfully (2xx/3xx), so the UI's next read reflects the
// new routing immediately.
func (s *Server) reloadAfterMutation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions || s.Reload == nil {
			next.ServeHTTP(w, req)
			return
		}
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, req)
		if rec.status < 400 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.Reload(ctx); err != nil {
				// Config was persisted; only the in-memory snapshot rebuild
				// failed. Log loudly: serving continues on the old snapshot.
				slog.Error("snapshot rebuild failed", "err", err)
			}
			cancel()
		}
	})
}

// statusWriter captures the response status for the reload hook.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type ctxKey string

const tokenKey ctxKey = "admin_token"

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token := bearer(req)
		if token == "" || !s.Admin.Valid(req.Context(), token) {
			httpx.WriteEnvelopeError(w, req, http.StatusUnauthorized, 40101, "unauthorized")
			return
		}
		ctx := context.WithValue(req.Context(), tokenKey, token)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// sessionToken returns the authenticated session token from context.
func sessionToken(req *http.Request) string {
	if v, ok := req.Context().Value(tokenKey).(string); ok {
		return v
	}
	return ""
}

func bearer(req *http.Request) string {
	h := req.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// mountProtected registers every session-guarded endpoint.
func (s *Server) mountProtected(pr chi.Router) {
	pr.Post("/auth/logout", s.handleLogout)
	pr.Get("/auth/me", s.handleMe)
	pr.Put("/auth/password", s.handleChangePassword)

	pr.Get("/settings", s.handleGetSettings)
	pr.Put("/settings", s.handlePutSettings)

	pr.Get("/providers", s.handleListProviders)
	pr.Post("/providers", s.handleCreateProvider)
	pr.Get("/providers/{id}", s.handleGetProvider)
	pr.Put("/providers/{id}", s.handleUpdateProvider)
	pr.Delete("/providers/{id}", s.handleDeleteProvider)

	pr.Get("/providers/{id}/accounts", s.handleListProviderAccounts)
	pr.Post("/providers/{id}/accounts", s.handleCreateAccount)
	pr.Get("/accounts", s.handleListAccounts)
	pr.Put("/accounts/{id}", s.handleUpdateAccount)
	pr.Delete("/accounts/{id}", s.handleDeleteAccount)
	pr.Post("/accounts/{id}/test", s.handleTestAccount)
	pr.Post("/accounts/{id}/usage", s.handleAccountUsage)
	pr.Post("/providers/{id}/refresh-models", s.handleRefreshModels)
	pr.Post("/providers/{id}/probe", s.handleProbeEndpoint)
	pr.Post("/providers/{id}/models-preview", s.handlePreviewModels)

	pr.Get("/channels", s.handleListChannels)
	pr.Post("/channels", s.handleCreateChannel)
	pr.Get("/channels/{id}", s.handleGetChannel)
	pr.Put("/channels/{id}", s.handleUpdateChannel)
	pr.Delete("/channels/{id}", s.handleDeleteChannel)
	pr.Post("/channels/{id}/test", s.handleTestChannel)

	pr.Get("/models", s.handleListModels)
	pr.Post("/models", s.handleCreateModel)
	pr.Put("/models/{providerID}/{id}", s.handleUpdateModel)
	pr.Delete("/models/{providerID}/{id}", s.handleDeleteModel)

	pr.Get("/model-routes", s.handleListRoutes)
	pr.Put("/model-routes/{name}", s.handleUpsertRoute)
	pr.Post("/model-routes/{name}/switch", s.handleUpsertRoute)
	pr.Delete("/model-routes/{name}", s.handleDeleteRoute)

	pr.Get("/client-keys", s.handleListClientKeys)
	pr.Post("/client-keys", s.handleCreateClientKey)
	pr.Put("/client-keys/{id}", s.handleUpdateClientKey)
	pr.Delete("/client-keys/{id}", s.handleDeleteClientKey)

	pr.Get("/logs", s.handleListLogs)
	pr.Get("/stats/overview", s.handleStatsOverview)
	pr.Get("/stats/timeseries", s.handleStatsTimeseries)
	pr.Get("/engine/status", s.handleEngineStatus)
}

// mapStoreErr translates repository errors into envelope errors.
func mapStoreErr(w http.ResponseWriter, req *http.Request, err error) {
	httpx.MapStoreErr(w, req, err)
}

// readJSON decodes a JSON request body into v with a sane size limit.
func readJSON(w http.ResponseWriter, req *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40001, "read body: "+err.Error())
		return false
	}
	if len(body) == 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40001, "empty request body")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40001, "invalid JSON: "+err.Error())
		return false
	}
	return true
}
