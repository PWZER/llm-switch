package api

import (
	"errors"
	"net/http"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

// configImportMaxBody caps the import document (a whole-gateway config stays
// far below this; the cap only guards abuse).
const configImportMaxBody int64 = 16 << 20

type configExportRequest struct {
	Sections           []string `json:"sections"`
	IncludeAccountKeys bool     `json:"include_account_keys"`
}

// handleConfigExport assembles the requested config sections into a JSON
// document. It is read-only, but being a POST it still triggers the
// post-mutation snapshot rebuild — the same tolerated cost as the probe
// actions.
func (s *Server) handleConfigExport(w http.ResponseWriter, req *http.Request) {
	var body configExportRequest
	if !readJSON(w, req, &body) {
		return
	}
	if len(body.Sections) == 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "sections is required")
		return
	}
	known := map[string]bool{}
	for _, sec := range store.ExportSections {
		known[sec] = true
	}
	for _, sec := range body.Sections {
		if !known[sec] {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "unknown section: "+sec)
			return
		}
	}
	doc, err := s.St.ExportConfig(req.Context(), body.Sections, body.IncludeAccountKeys)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, doc)
}

// handleConfigImport applies a config document as an upsert merge. The body
// is the export document itself; `?dry_run=1` runs the identical transaction
// and rolls it back, returning the plan (counts + warnings) without writing.
// A 200 response triggers the automatic snapshot reload.
func (s *Server) handleConfigImport(w http.ResponseWriter, req *http.Request) {
	var doc store.ConfigExport
	if !readJSONLimit(w, req, &doc, configImportMaxBody) {
		return
	}
	var (
		res *store.ImportResult
		err error
	)
	if req.URL.Query().Get("dry_run") == "1" {
		res, err = s.St.PlanImport(req.Context(), &doc)
	} else {
		res, err = s.St.ImportConfig(req.Context(), &doc)
	}
	if err != nil {
		if errors.Is(err, store.ErrBadDocument) {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
			return
		}
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, res)
}
