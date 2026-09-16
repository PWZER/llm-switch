package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

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
		Name string `json:"name"`
	}
	if !readJSON(w, req, &body) || body.Name == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "name is required")
		return
	}
	id, err := s.St.Providers.Create(req.Context(), body.Name)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
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
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.Name != nil {
		if err := s.St.Providers.Rename(req.Context(), id, *body.Name); err != nil {
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
	p, err := s.St.Providers.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, p)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	if err := s.St.Providers.Delete(req.Context(), id); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

// — provider keys ---------------------------------------------------------

func (s *Server) handleListProviderKeys(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	keys, err := s.St.Keys.List(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, keys)
}

func (s *Server) handleCreateProviderKey(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body struct {
		Label  string `json:"label"`
		APIKey string `json:"api_key"`
		Weight int    `json:"weight"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.APIKey == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "api_key is required")
		return
	}
	if body.Weight < 1 {
		body.Weight = 1
	}
	keyID, err := s.St.Keys.Create(req.Context(), id, body.Label, body.APIKey, body.Weight)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelopeStatus(w, req, http.StatusCreated, map[string]any{
		"id": keyID, "api_key_mask": maskForDisplay(body.APIKey),
	})
}

func (s *Server) handleUpdateProviderKey(w http.ResponseWriter, req *http.Request) {
	keyID, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil || keyID <= 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body struct {
		Label   *string `json:"label"`
		Weight  *int    `json:"weight"`
		Enabled *bool   `json:"enabled"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if err := s.St.Keys.Update(req.Context(), keyID, body.Label, body.Weight, body.Enabled); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteProviderKey(w http.ResponseWriter, req *http.Request) {
	keyID, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil || keyID <= 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	if err := s.St.Keys.Delete(req.Context(), keyID); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

// handleRefreshModels fetches the upstream model list through the provider's
// first enabled channel and upserts new entries (never overwrites existing).
func (s *Server) handleRefreshModels(w http.ResponseWriter, req *http.Request) {
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
	channels, err := s.St.Channels.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	var channel *store.Channel
	for i := range channels {
		if channels[i].ProviderID == id && channels[i].Enabled {
			channel = &channels[i]
			break
		}
	}
	if channel == nil {
		httpx.WriteEnvelopeError(w, req, http.StatusConflict, 40901,
			"provider has no enabled channel to fetch models through")
		return
	}
	keys, err := s.St.Keys.EnabledKeys(req.Context(), id)
	if err != nil || len(keys) == 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusConflict, 40902, "provider has no enabled api key")
		return
	}

	added, err := s.fetchUpstreamModels(req.Context(), channel, keys[0].APIKey, id)
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadGateway, 60001, "fetch models: "+err.Error())
		return
	}
	httpx.WriteEnvelope(w, req, map[string]any{"provider": p.Name, "models_added": added})
}

// fetchUpstreamModels GETs the model list (OpenAI and Anthropic shapes share
// the data[].id envelope) and inserts unknown entries.
func (s *Server) fetchUpstreamModels(ctx context.Context, ch *store.Channel, secret string, providerID int64) (int, error) {
	base := strings.TrimSuffix(ch.BaseURL, "/")
	modelsPath := orString(ch.ModelsURL, "/models")
	if ch.Protocol == "anthropic" && ch.ModelsURL == nil {
		modelsPath = "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+modelsPath, nil)
	if err != nil {
		return 0, err
	}
	if ch.AuthStyle == "x-api-key" {
		req.Header.Set("x-api-key", secret)
	} else {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := httpClientForRefresh.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		return 0, err
	}
	added := 0
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		inserted, err := s.St.Models.EnsureModel(ctx, m.ID, fmt.Sprintf("provider:%d", providerID))
		if err != nil {
			return added, err
		}
		if inserted {
			added++
		}
	}
	return added, nil
}

var httpClientForRefresh = &http.Client{Timeout: 30 * time.Second}

func orString(v *string, fb string) string {
	if v != nil && *v != "" {
		return *v
	}
	return fb
}

func maskForDisplay(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "…" + key[len(key)-4:]
}
