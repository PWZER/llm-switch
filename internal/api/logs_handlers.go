package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/payload"
	"github.com/PWZER/llm-switch/internal/store"
)

// handleListLogs serves GET /logs with filters and server-side pagination.
func (s *Server) handleListLogs(w http.ResponseWriter, req *http.Request) {
	f := store.LogFilter{}
	if v := req.URL.Query().Get("from"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
			if ms < 10_000_000_000 { // seconds given
				ms *= 1000
			}
			f.From = ms
		}
	}
	if v := req.URL.Query().Get("to"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
			if ms < 10_000_000_000 {
				ms *= 1000
			}
			f.To = ms
		}
	}
	f.Model = req.URL.Query().Get("model")
	f.APIKeyID = queryID(req, "key_id")
	f.ProviderID = queryID(req, "provider_id")
	f.AccountID = queryID(req, "account_id")
	f.ChannelID = queryID(req, "channel_id")
	f.Status = int(queryID(req, "status"))
	f.Page, _ = strconv.Atoi(req.URL.Query().Get("page"))
	f.PageSize, _ = strconv.Atoi(req.URL.Query().Get("page_size"))

	logs, total, err := s.St.Logs.QueryLogs(req.Context(), f)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]any{
		"items": logs, "total": total, "page": f.Page, "page_size": f.PageSize,
	})
}

func queryID(req *http.Request, name string) int64 {
	v := req.URL.Query().Get(name)
	if v == "" {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// segmentJSON renders one recorded segment for the envelope. Bodies are
// returned as text; encoding/json replaces invalid UTF-8, which is
// acceptable for a debugging view.
func segmentJSON(seg payload.Segment) map[string]any {
	return map[string]any{
		"headers":   seg.Headers,
		"body":      string(seg.Body),
		"truncated": seg.Truncated,
	}
}

// handleGetLogPayload serves GET /logs/{id}/payload: the three recorded
// segments (client request, upstream request, upstream response).
func (s *Server) handleGetLogPayload(w http.ResponseWriter, req *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	row, err := s.St.Logs.GetLogByID(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	if row.PayloadPath == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusNotFound, 40401, "no payload recorded for this log")
		return
	}
	rec, err := payload.Read(s.PayloadDir, row.PayloadPath)
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusNotFound, 40401, "payload expired or unavailable")
		return
	}
	out := map[string]any{
		"request_id":     rec.RequestID,
		"client_request": segmentJSON(rec.ClientReq),
	}
	if rec.UpstreamReq != nil {
		out["upstream_request"] = segmentJSON(*rec.UpstreamReq)
	} else {
		out["upstream_request"] = nil
	}
	if rec.RespStatus != 0 || rec.RespHeaders != nil || len(rec.RespBody) > 0 {
		out["upstream_response"] = map[string]any{
			"status":    rec.RespStatus,
			"headers":   rec.RespHeaders,
			"body":      string(rec.RespBody),
			"truncated": rec.RespTrunc,
		}
	} else {
		out["upstream_response"] = nil
	}
	httpx.WriteEnvelope(w, req, out)
}

// handleStatsOverview serves GET /stats/overview?from&to (default: last 24h).
func (s *Server) handleStatsOverview(w http.ResponseWriter, req *http.Request) {
	to := time.Now().UnixMilli()
	from := to - 24*3600*1000
	if v := queryID(req, "from"); v > 0 {
		from = v
	}
	if v := queryID(req, "to"); v > 0 {
		to = v
	}
	ov, err := s.St.Logs.OverviewStats(req.Context(), from, to)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	data := struct {
		store.Overview
		SuccessRate float64 `json:"success_rate"`
		DroppedLogs int64   `json:"dropped_logs"`
	}{Overview: ov}
	if ov.Requests > 0 {
		data.SuccessRate = float64(ov.Successes) / float64(ov.Requests)
	}
	data.DroppedLogs = s.Dropped()
	httpx.WriteEnvelope(w, req, data)
}

// handleStatsTimeseries serves GET /stats/timeseries?from&to&group_by=model|provider|account|key.
func (s *Server) handleStatsTimeseries(w http.ResponseWriter, req *http.Request) {
	to := time.Now().UnixMilli()
	from := to - 7*24*3600*1000
	if v := queryID(req, "from"); v > 0 {
		from = v
	}
	if v := queryID(req, "to"); v > 0 {
		to = v
	}
	groupBy := req.URL.Query().Get("group_by")
	rows, err := s.St.Logs.Timeseries(req.Context(), from, to, groupBy)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, rows)
}

// handleEngineStatus exposes snapshot version + account cooldown state (account
// id -> until) for the UI.
func (s *Server) handleEngineStatus(w http.ResponseWriter, req *http.Request) {
	var version int64
	if s.Snapshot != nil {
		version = s.Snapshot.Version()
	}
	httpx.WriteEnvelope(w, req, map[string]any{
		"snapshot_version": version,
		"cooldowns":        s.Cooldowns(),
	})
}
