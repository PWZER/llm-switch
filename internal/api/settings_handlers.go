package api

import (
	"net/http"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, req *http.Request) {
	all, err := s.St.Settings.GetAll(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	// Only expose the allowlisted, non-secret settings.
	out := map[string]string{}
	for k := range store.AdminSettingKeys {
		if v, ok := all[k]; ok {
			out[k] = v
		}
	}
	httpx.WriteEnvelope(w, req, out)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, req *http.Request) {
	var body map[string]string
	if !readJSON(w, req, &body) {
		return
	}
	for k := range body {
		if _, ok := store.AdminSettingKeys[k]; !ok {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40003, "unknown setting: "+k)
			return
		}
	}
	filtered, _, err := store.SanitizeAdminSettings(body)
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
		return
	}
	if len(filtered) == 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40001, "no settings provided")
		return
	}
	if err := s.St.Settings.SetMany(req.Context(), filtered); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	out, err := s.St.Settings.GetAll(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, out)
}
