package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/httpx"
)

// — provider accounts -------------------------------------------------------
//
// An account is one upstream credential set; it always belongs to exactly one
// provider (created via the nested route, provider_id immutable afterwards).

func (s *Server) handleListProviderAccounts(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	accounts, err := s.St.Accounts.List(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, accounts)
}

// handleListAccounts serves the cross-provider account pool listing.
func (s *Server) handleListAccounts(w http.ResponseWriter, req *http.Request) {
	accounts, err := s.St.Accounts.ListAll(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, accounts)
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body struct {
		Label       string          `json:"label"`
		APIKey      string          `json:"api_key"`
		Weight      int             `json:"weight"`
		UsageProbes json.RawMessage `json:"usage_probes"`
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
	probes, perr := validateUsageProbes(body.UsageProbes)
	if perr != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, perr.Error())
		return
	}
	accountID, err := s.St.Accounts.Create(req.Context(), id, body.Label, body.APIKey, body.Weight, probes)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelopeStatus(w, req, http.StatusCreated, map[string]any{
		"id": accountID, "api_key_mask": maskForDisplay(body.APIKey),
	})
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, req *http.Request) {
	accountID, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil || accountID <= 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body struct {
		Label       *string          `json:"label"`
		Weight      *int             `json:"weight"`
		Enabled     *bool            `json:"enabled"`
		UsageProbes *json.RawMessage `json:"usage_probes"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.Label != nil || body.Weight != nil || body.Enabled != nil {
		if err := s.St.Accounts.Update(req.Context(), accountID, body.Label, body.Weight, body.Enabled); err != nil {
			mapStoreErr(w, req, err)
			return
		}
	}
	if body.UsageProbes != nil {
		probes, perr := validateUsageProbes(*body.UsageProbes)
		if perr != nil {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, perr.Error())
			return
		}
		if err := s.St.Accounts.SetUsageProbes(req.Context(), accountID, probes); err != nil {
			mapStoreErr(w, req, err)
			return
		}
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, req *http.Request) {
	accountID, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil || accountID <= 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	if err := s.St.Accounts.Delete(req.Context(), accountID); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}
