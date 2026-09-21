package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

func (s *Server) handleListProviders(w http.ResponseWriter, req *http.Request) {
	providers, err := s.St.Providers.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, providers)
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name           string   `json:"name"`
		ModelsURL      *string  `json:"models_url"`
		RegisterModels []string `json:"register_models"`
	}
	if !readJSON(w, req, &body) || body.Name == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "name is required")
		return
	}
	if err := validateModelsURL(body.ModelsURL); err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
		return
	}
	id, err := s.St.Providers.Create(req.Context(), body.Name, body.ModelsURL)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	s.registerModels(req.Context(), id, body.RegisterModels)
	httpx.WriteEnvelopeStatus(w, req, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleGetProvider(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	p, err := s.St.Providers.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, p)
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body struct {
		Name           *string  `json:"name"`
		ModelsURL      *string  `json:"models_url"` // non-nil = set (empty string clears)
		Enabled        *bool    `json:"enabled"`
		RegisterModels []string `json:"register_models"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.Name != nil || body.ModelsURL != nil {
		if err := validateModelsURL(body.ModelsURL); err != nil {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
			return
		}
		p, err := s.St.Providers.Get(req.Context(), id)
		if err != nil {
			mapStoreErr(w, req, err)
			return
		}
		if body.Name != nil {
			p.Name = *body.Name
		}
		if body.ModelsURL != nil {
			if *body.ModelsURL == "" {
				p.ModelsURL = nil
			} else {
				p.ModelsURL = body.ModelsURL
			}
		}
		if err := s.St.Providers.Update(req.Context(), &p); err != nil {
			mapStoreErr(w, req, err)
			return
		}
	}
	if body.Enabled != nil {
		if err := s.St.Providers.SetEnabled(req.Context(), id, *body.Enabled); err != nil {
			mapStoreErr(w, req, err)
			return
		}
	}
	s.registerModels(req.Context(), id, body.RegisterModels)
	p, err := s.St.Providers.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, p)
}

// validateModelsURL accepts nil/empty (unset) or an absolute http(s) URL —
// the fetch URL is configured explicitly at the provider level, never
// derived from endpoint base URLs.
func validateModelsURL(u *string) error {
	if u == nil || *u == "" {
		return nil
	}
	parsed, err := url.Parse(*u)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errText("models_url must be an absolute http(s) URL")
	}
	return nil
}

// registerModels inserts provider-scoped identity rows for the given ids
// (insert-if-missing; existing rows are never touched). Failures are logged,
// not fatal: the provider itself is already saved.
func (s *Server) registerModels(ctx context.Context, providerID int64, ids []string) {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := s.St.Models.EnsureModel(ctx, &store.Model{
			ID: id, ProviderID: providerID, UpstreamModel: id,
		}); err != nil {
			slog.Warn("register model failed", "provider_id", providerID, "model", id, "err", err)
		}
	}
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	if err := s.St.DeleteProvider(req.Context(), id); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

// handleRefreshModels fetches the upstream model list from the provider's
// configured models_url (provider-level, absolute — never derived from
// endpoint URLs) and returns it WITHOUT registering anything: the admin UI
// presents the list for selection and commits the chosen subset through
// sync-models. The account to authenticate with is chosen explicitly:
// account_id is required and must name an enabled account of this provider —
// there is no silent fallback.
func (s *Server) handleRefreshModels(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body struct {
		AccountID int64 `json:"account_id"`
	}
	// Tolerate an empty body so the 40002 below carries the actionable message.
	_ = json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&body)
	if body.AccountID <= 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "account_id is required")
		return
	}
	p, err := s.St.Providers.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	account, err := s.St.Accounts.Get(req.Context(), body.AccountID)
	if err != nil || account.ProviderID != id || !account.Enabled {
		httpx.WriteEnvelopeError(w, req, http.StatusNotFound, 40401, "account not found or disabled")
		return
	}
	if p.ModelsURL == nil || *p.ModelsURL == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusConflict, 40901,
			"models_url is not configured for this provider — set it on the provider first")
		return
	}

	// The auth header style comes from the provider's endpoints (OpenAI
	// protocol first — the models list is an OpenAI-style endpoint); with no
	// channels at all, bearer is the safe default.
	authStyle := "bearer"
	if channels, err := s.St.Channels.List(req.Context()); err == nil {
		authStyle = pickModelsAuthStyle(channels, id)
	}

	models, err := getModelsURL(req.Context(), *p.ModelsURL, authStyle, account.APIKey)
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadGateway, 60001, "fetch models: "+err.Error())
		return
	}
	httpx.WriteEnvelope(w, req, map[string]any{
		"provider": p.Name, "account_id": account.ID, "models": models,
	})
}

// handleSyncModels applies an explicit add/remove diff of model rows for one
// provider — the commit step behind the admin UI's model picker. register
// entries EnsureModel provider-scoped identity rows (insert-if-missing, NULL
// limits backfilled — manual edits survive); remove entries delete rows by
// (provider_id, id), including historical rows the upstream list no longer
// carries. Both lists are explicit from the client, so removal is not
// limited to ids seen in the last fetch.
func (s *Server) handleSyncModels(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	if _, err := s.St.Providers.Get(req.Context(), id); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	var body struct {
		Register []upstreamModel `json:"register"`
		Remove   []string        `json:"remove"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	added := 0
	for _, m := range body.Register {
		if m.ID == "" {
			continue
		}
		inserted, err := s.St.Models.EnsureModel(req.Context(), &store.Model{
			ID:              m.ID,
			ProviderID:      id,
			UpstreamModel:   m.ID,
			DisplayName:     m.ID,
			ContextWindow:   m.ContextLength,
			MaxOutputTokens: m.MaxOutputTokens,
		})
		if err != nil {
			mapStoreErr(w, req, err)
			return
		}
		if inserted {
			added++
		}
	}
	removed := 0
	for _, modelID := range body.Remove {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		if err := s.St.Models.Delete(req.Context(), id, modelID); err != nil {
			mapStoreErr(w, req, err)
			return
		}
		removed++
	}
	httpx.WriteEnvelope(w, req, map[string]int{
		"models_added": added, "models_removed": removed,
	})
}

// upstreamHTTPError is a non-2xx models-list response.
type upstreamHTTPError struct {
	status int
	msg    string
}

func (e *upstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.status, e.msg)
}

// upstreamModel is one entry of a models-list response. Vendor models lists
// (OpenAI and Anthropic shapes) carry only ids; aggregators like OpenRouter
// add top-level context_length / max_output_tokens — parsed when present,
// else NULL so the registry stays untouched.
type upstreamModel struct {
	ID              string `json:"id"`
	ContextLength   *int64 `json:"context_length"`
	MaxOutputTokens *int64 `json:"max_output_tokens"`
}

func getModelsURL(ctx context.Context, url, authStyle, secret string) ([]upstreamModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authStyle == "x-api-key" {
		req.Header.Set("x-api-key", secret)
	} else {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := httpClientForRefresh.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &upstreamHTTPError{status: resp.StatusCode, msg: strings.TrimSpace(string(body))}
	}
	var list struct {
		Data []upstreamModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		return nil, err
	}
	models := make([]upstreamModel, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			models = append(models, m)
		}
	}
	return models, nil
}

var httpClientForRefresh = &http.Client{Timeout: 30 * time.Second}

func maskForDisplay(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "…" + key[len(key)-4:]
}
