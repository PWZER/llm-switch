package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

// — model registry --------------------------------------------------------

// handleListModels returns every models row with its provider context
// (provider name, provider enabled flag). Rows are the routing table itself,
// so this page is a complete inventory — no synthesized entries.
func (s *Server) handleListModels(w http.ResponseWriter, req *http.Request) {
	models, err := s.St.Models.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	providers, err := s.St.Providers.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	type providerInfo struct {
		Name    string
		Enabled bool
	}
	byID := make(map[int64]providerInfo, len(providers))
	for _, p := range providers {
		byID[p.ID] = providerInfo{p.Name, p.Enabled}
	}

	type modelRow struct {
		store.Model
		ProviderName    string `json:"provider_name,omitempty"`
		ProviderEnabled bool   `json:"provider_enabled"`
	}
	out := make([]modelRow, 0, len(models))
	for _, m := range models {
		row := modelRow{Model: m}
		if pi, ok := byID[m.ProviderID]; ok {
			row.ProviderName = pi.Name
			row.ProviderEnabled = pi.Enabled
		}
		out = append(out, row)
	}
	httpx.WriteEnvelope(w, req, out)
}

// handleCreateModel registers one client-facing name on one or more providers
// (one row per provider; the alias defaults to identity per row).
func (s *Server) handleCreateModel(w http.ResponseWriter, req *http.Request) {
	var body struct {
		ID              string  `json:"id"`
		ProviderIDs     []int64 `json:"provider_ids"`
		UpstreamModel   string  `json:"upstream_model"`
		DisplayName     string  `json:"display_name"`
		ContextWindow   *int64  `json:"context_window"`
		MaxOutputTokens *int64  `json:"max_output_tokens"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.ID == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "id is required")
		return
	}
	if len(body.ProviderIDs) == 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "provider_ids is required")
		return
	}
	for _, pid := range body.ProviderIDs {
		if _, err := s.St.Providers.Get(req.Context(), pid); err != nil {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
				fmt.Sprintf("provider %d not found", pid))
			return
		}
	}
	created := make([]store.Model, 0, len(body.ProviderIDs))
	for _, pid := range body.ProviderIDs {
		m := store.Model{
			ID: body.ID, ProviderID: pid, UpstreamModel: body.UpstreamModel,
			DisplayName: body.DisplayName, ContextWindow: body.ContextWindow,
			MaxOutputTokens: body.MaxOutputTokens, Enabled: true,
		}
		if err := s.St.Models.Create(req.Context(), &m); err != nil {
			mapStoreErr(w, req, err)
			return
		}
		row, err := s.St.Models.Get(req.Context(), pid, body.ID)
		if err != nil {
			mapStoreErr(w, req, err)
			return
		}
		created = append(created, row)
	}
	httpx.WriteEnvelopeStatus(w, req, http.StatusCreated, created)
}

func (s *Server) handleUpdateModel(w http.ResponseWriter, req *http.Request) {
	providerID, id, ok := chiModelKey(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid model key")
		return
	}
	current, err := s.St.Models.Get(req.Context(), providerID, id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	var body struct {
		UpstreamModel   *string `json:"upstream_model"`
		DisplayName     *string `json:"display_name"`
		Enabled         *bool   `json:"enabled"`
		ContextWindow   *int64  `json:"context_window"`
		MaxOutputTokens *int64  `json:"max_output_tokens"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.UpstreamModel != nil {
		current.UpstreamModel = *body.UpstreamModel
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
	out, err := s.St.Models.Get(req.Context(), providerID, id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, out)
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, req *http.Request) {
	providerID, id, ok := chiModelKey(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid model key")
		return
	}
	if err := s.St.Models.Delete(req.Context(), providerID, id); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

// chiModelKey decodes the {providerID}/{id} path pair. chi routes on the raw
// (percent-encoded) path when one is present (mux matches r.URL.RawPath), so
// escapes like %2F inside model ids such as "vendor/large" arrive encoded
// here; unescape so both the plain and encoded wire forms work.
func chiModelKey(req *http.Request) (providerID int64, id string, ok bool) {
	n, err := strconv.ParseInt(chi.URLParam(req, "providerID"), 10, 64)
	if err != nil || n <= 0 {
		return 0, "", false
	}
	id = chi.URLParam(req, "id")
	if unescaped, uerr := url.PathUnescape(id); uerr == nil {
		id = unescaped
	} else {
		return 0, "", false
	}
	return n, id, true
}

// — model routes (hot-switch layer) -----------------------------------------

func (s *Server) handleListRoutes(w http.ResponseWriter, req *http.Request) {
	routes, err := s.St.Routes.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, routes)
}

func (s *Server) handleUpsertRoute(w http.ResponseWriter, req *http.Request) {
	name := routeName(req)
	if name == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "model route name is required")
		return
	}
	// Names are canonical: "[1m]" is reserved for the GET /v1/models
	// 1M-context marker and is never part of a routing identity.
	if strings.HasSuffix(name, "[1m]") {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
			`model route names are canonical: the "[1m]" suffix is reserved for the 1M-context listing marker`)
		return
	}
	// targets: ordered failover chain. Legacy single-target bodies
	// ({channel_id, upstream_model}) are accepted and wrapped into one target.
	var body struct {
		Targets       *[]store.ModelRouteTarget `json:"targets"`
		ChannelID     *int64                    `json:"channel_id"`
		UpstreamModel *string                   `json:"upstream_model"`
	}
	if !readJSON(w, req, &body) {
		return
	}

	var targets []store.ModelRouteTarget
	switch {
	case body.Targets != nil:
		targets = *body.Targets
	case body.ChannelID != nil && body.UpstreamModel != nil:
		targets = []store.ModelRouteTarget{{ChannelID: body.ChannelID, UpstreamModel: *body.UpstreamModel}}
	default:
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
			`provide "targets" (ordered list) or legacy "channel_id"+"upstream_model"`)
		return
	}
	if len(targets) == 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "at least one target is required")
		return
	}
	// Validate every target eagerly so a typo cannot silently black-hole
	// agent traffic. provider_id owns the target; channel_id optionally pins
	// one endpoint (null = auto-select among the provider's enabled
	// channels). A pinned account must belong to the target's provider.
	// Legacy payloads without provider_id are backfilled from the channel,
	// so stored targets always carry it.
	for i := range targets {
		tgt := &targets[i]
		if tgt.ProviderID <= 0 && (tgt.ChannelID == nil || *tgt.ChannelID <= 0) {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
				fmt.Sprintf("target %d needs provider_id (or channel_id)", i+1))
			return
		}
		if tgt.ChannelID != nil && *tgt.ChannelID <= 0 {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
				fmt.Sprintf("target %d: invalid channel_id %d", i+1, *tgt.ChannelID))
			return
		}
		if tgt.UpstreamModel == "" {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
				fmt.Sprintf("target %d needs upstream_model", i+1))
			return
		}
		if tgt.ProviderID <= 0 {
			// Legacy target: derive the provider from the pinned channel.
			ch, err := s.St.Channels.Get(req.Context(), *tgt.ChannelID)
			if err != nil {
				mapStoreErr(w, req, err)
				return
			}
			tgt.ProviderID = ch.ProviderID
		} else {
			if _, err := s.St.Providers.Get(req.Context(), tgt.ProviderID); err != nil {
				mapStoreErr(w, req, err)
				return
			}
			if tgt.ChannelID != nil {
				ch, err := s.St.Channels.Get(req.Context(), *tgt.ChannelID)
				if err != nil {
					mapStoreErr(w, req, err)
					return
				}
				if ch.ProviderID != tgt.ProviderID {
					httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
						fmt.Sprintf("target %d: channel %d does not belong to provider %d", i+1, *tgt.ChannelID, tgt.ProviderID))
					return
				}
			}
		}
		if tgt.AccountID != nil && *tgt.AccountID > 0 {
			account, err := s.St.Accounts.Get(req.Context(), *tgt.AccountID)
			if err != nil {
				mapStoreErr(w, req, err)
				return
			}
			if account.ProviderID != tgt.ProviderID {
				httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
					fmt.Sprintf("target %d: account %d does not belong to provider %d", i+1, *tgt.AccountID, tgt.ProviderID))
				return
			}
		}
	}

	a := store.ModelRoute{Name: name, Targets: targets}
	if err := s.St.Routes.Upsert(req.Context(), &a); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	out, err := s.St.Routes.Get(req.Context(), name)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, out)
}

func (s *Server) handleDeleteRoute(w http.ResponseWriter, req *http.Request) {
	if err := s.St.Routes.Delete(req.Context(), routeName(req)); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

func routeName(req *http.Request) string {
	return strings.TrimSpace(chi.URLParam(req, "name"))
}
