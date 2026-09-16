package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/auth"
	"github.com/PWZER/llm-switch/internal/httpx"
)

func (s *Server) handleListClientKeys(w http.ResponseWriter, req *http.Request) {
	keys, err := s.St.APIKeys.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, keys)
}

// handleCreateClientKey issues a gateway key. The plaintext is returned exactly
// once in this response; only its sha256 hash is stored.
func (s *Server) handleCreateClientKey(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name       string `json:"name"`
		TokenLimit *int64 `json:"token_limit"`
		ExpiresAt  *int64 `json:"expires_at"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.Name == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "name is required")
		return
	}
	plain, hash, prefix := auth.GenerateClientKey()
	id, err := s.St.APIKeys.Create(req.Context(), body.Name, hash, prefix, body.TokenLimit, body.ExpiresAt)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelopeStatus(w, req, http.StatusCreated, map[string]any{
		"id":     id,
		"prefix": prefix,
		"key":    plain, // shown exactly once
	})
}

func (s *Server) handleUpdateClientKey(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if err := s.St.APIKeys.Update(req.Context(), id, body.Name, body.Enabled); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteClientKey(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	if err := s.St.APIKeys.Delete(req.Context(), id); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}
