package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/auth"
	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

func (s *Server) handleLogin(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	token, expires, err := s.Admin.Login(req.Context(), body.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidPassword):
		slog.Warn("admin login failed", "remote", req.RemoteAddr)
		httpx.WriteEnvelopeError(w, req, http.StatusUnauthorized, 40102, "invalid password")
		return
	case err != nil:
		slog.Warn("admin login throttled or errored", "remote", req.RemoteAddr, "err", err)
		httpx.WriteEnvelopeError(w, req, http.StatusTooManyRequests, 42901, err.Error())
		return
	}
	httpx.WriteEnvelope(w, req, map[string]any{
		"token":      token,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, req *http.Request) {
	if err := s.Admin.Logout(req.Context(), sessionToken(req)); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, req *http.Request) {
	httpx.WriteEnvelope(w, req, map[string]bool{"authenticated": true})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if len(body.New) < 8 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "new password must be at least 8 characters")
		return
	}
	if err := s.Admin.ChangePassword(req.Context(), body.Old, body.New); err != nil {
		if errors.Is(err, auth.ErrInvalidPassword) {
			httpx.WriteEnvelopeError(w, req, http.StatusUnauthorized, 40102, "old password is incorrect")
			return
		}
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

// pathID parses a numeric {id} path parameter.
func pathID(req *http.Request) (int64, bool) {
	v := chi.URLParam(req, "id")
	if v == "" {
		return 0, false
	}
	var id int64
	for _, c := range []byte(v) {
		if c < '0' || c > '9' {
			return 0, false
		}
		id = id*10 + int64(c-'0')
	}
	return id, id > 0
}

var _ = store.ErrNotFound
