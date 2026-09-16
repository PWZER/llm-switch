package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

// — model registry --------------------------------------------------------

func (s *Server) handleListModels(w http.ResponseWriter, req *http.Request) {
	models, err := s.St.Models.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, models)
}

func (s *Server) handleCreateModel(w http.ResponseWriter, req *http.Request) {
	var body store.Model
	if !readJSON(w, req, &body) {
		return
	}
	if body.ID == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "id is required")
		return
	}
	if body.Source == "" {
		body.Source = "manual"
	}
	body.Enabled = true
	if err := s.St.Models.Upsert(req.Context(), &body); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	out, err := s.St.Models.Get(req.Context(), body.ID)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelopeStatus(w, req, http.StatusCreated, out)
}

func (s *Server) handleUpdateModel(w http.ResponseWriter, req *http.Request) {
	id := chiModelID(req)
	current, err := s.St.Models.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	var body struct {
		DisplayName     *string `json:"display_name"`
		Enabled         *bool   `json:"enabled"`
		ContextWindow   *int64  `json:"context_window"`
		MaxOutputTokens *int64  `json:"max_output_tokens"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.DisplayName != nil {
		current.DisplayName = *body.DisplayName
	}
	if body.Enabled != nil {
		current.Enabled = *body.Enabled
	}
	if body.ContextWindow != nil {
		current.ContextWindow = body.ContextWindow
	}
	if body.MaxOutputTokens != nil {
		current.MaxOutputTokens = body.MaxOutputTokens
	}
	if err := s.St.Models.Update(req.Context(), &current); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	out, err := s.St.Models.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, out)
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, req *http.Request) {
	if err := s.St.Models.Delete(req.Context(), chiModelID(req)); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

func chiModelID(req *http.Request) string {
	return chi.URLParam(req, "id")
}

// — aliases (hot-switch layer) --------------------------------------------

func (s *Server) handleListAliases(w http.ResponseWriter, req *http.Request) {
	aliases, err := s.St.Aliases.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, aliases)
}

func (s *Server) handleUpsertAlias(w http.ResponseWriter, req *http.Request) {
	name := aliasName(req)
	var body struct {
		ChannelID     int64  `json:"channel_id"`
		UpstreamModel string `json:"upstream_model"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if name == "" || body.ChannelID <= 0 || body.UpstreamModel == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
			"name, channel_id and upstream_model are required")
		return
	}
	// The channel must exist; alias targets are validated eagerly so a typo
	// cannot silently black-hole agent traffic.
	if _, err := s.St.Channels.Get(req.Context(), body.ChannelID); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	a := store.Alias{Name: name, ChannelID: body.ChannelID, UpstreamModel: body.UpstreamModel}
	if err := s.St.Aliases.Upsert(req.Context(), &a); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	out, err := s.St.Aliases.Get(req.Context(), name)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, out)
}

func (s *Server) handleDeleteAlias(w http.ResponseWriter, req *http.Request) {
	if err := s.St.Aliases.Delete(req.Context(), aliasName(req)); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

func aliasName(req *http.Request) string {
	return strings.TrimSpace(chi.URLParam(req, "name"))
}
