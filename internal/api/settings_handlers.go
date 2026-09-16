package api

import (
	"net/http"
	"strconv"

	"github.com/PWZER/llm-switch/internal/httpx"
)

// settingKeys is the editable allowlist. admin password and anything secret
// deliberately live outside this table.
var settingKeys = map[string]string{
	"retention_days":         "int",
	"max_failover_attempts":  "int",
	"default_max_tokens":     "int",
	"auto_bind_new_models":   "bool",
	"stream_idle_timeout_s":  "int",
	"log_bodies":             "bool",
}

func (s *Server) handleGetSettings(w http.ResponseWriter, req *http.Request) {
	all, err := s.St.Settings.GetAll(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	// Only expose the allowlisted, non-secret settings.
	out := map[string]string{}
	for k := range settingKeys {
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
	filtered := map[string]string{}
	for k, v := range body {
		kind, ok := settingKeys[k]
		if !ok {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40003, "unknown setting: "+k)
			return
		}
		switch kind {
		case "int":
			if _, err := strconv.Atoi(v); err != nil {
				httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, k+" must be an integer")
				return
			}
		case "bool":
			if _, err := strconv.ParseBool(v); err != nil {
				httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, k+" must be a boolean")
				return
			}
		}
		filtered[k] = v
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
