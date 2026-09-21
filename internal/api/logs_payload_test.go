package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/payload"
	"github.com/PWZER/llm-switch/internal/store"
)

// writePayloadRecord plants one on-disk payload record and returns its
// relative path.
func writePayloadRecord(t *testing.T, dir string, rec *payload.Record) string {
	t.Helper()
	rel, err := payload.DirFor(dir, rec.TS, rec.RequestID)
	if err != nil {
		t.Fatalf("dir for: %v", err)
	}
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(abs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abs, "client_req.bin"), rec.ClientReq.Body, 0o600); err != nil {
		t.Fatal(err)
	}
	if rec.UpstreamReq != nil {
		if err := os.WriteFile(filepath.Join(abs, "upstream_req.bin"), rec.UpstreamReq.Body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(abs, "upstream_resp.bin"), rec.RespBody, 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abs, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	return rel
}

func TestGetLogPayload(t *testing.T) {
	st := newRefreshTestStore(t)
	dir := t.TempDir()
	s := &Server{St: st, PayloadDir: dir}
	r := chi.NewRouter()
	r.Get("/logs/{id}/payload", s.handleGetLogPayload)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	rec := &payload.Record{
		RequestID: "req-1",
		TS:        time.Now().UnixMilli(),
		ClientReq: payload.Segment{
			Headers: map[string][]string{"Content-Type": {"application/json"}},
			Body:    []byte(`{"model":"glm-4.6"}`),
		},
		UpstreamReq: &payload.Segment{
			Headers: map[string][]string{"Authorization": {"Bearer sk-s…cret"}},
			Body:    []byte(`{"model":"fake-chat"}`),
		},
		RespStatus: 200,
		RespBody:   []byte(`{"id":"chatcmpl-1"}`),
	}
	rel := writePayloadRecord(t, dir, rec)

	insert := func(path string) int64 {
		t.Helper()
		if err := st.Logs.InsertBatch(ctx, []store.RequestLog{
			{TS: time.Now().UnixMilli(), Model: "m", Success: true, PayloadPath: path},
		}); err != nil {
			t.Fatalf("insert log: %v", err)
		}
		logs, _, err := st.Logs.QueryLogs(ctx, store.LogFilter{Page: 1, PageSize: 1})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		return logs[0].ID
	}

	get := func(id int64) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/logs/"+strconv.FormatInt(id, 10)+"/payload", nil)
		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, req)
		return rec
	}

	// Happy path: all three segments.
	id := insert(rel)
	resp := get(id)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	var env struct {
		Data struct {
			ClientReq struct {
				Body string `json:"body"`
			} `json:"client_request"`
			UpstreamReq *struct {
				Body string `json:"body"`
			} `json:"upstream_request"`
			UpstreamResp struct {
				Status int    `json:"status"`
				Body   string `json:"body"`
			} `json:"upstream_response"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.ClientReq.Body != `{"model":"glm-4.6"}` {
		t.Fatalf("client body wrong: %q", env.Data.ClientReq.Body)
	}
	if env.Data.UpstreamReq == nil || env.Data.UpstreamReq.Body != `{"model":"fake-chat"}` {
		t.Fatalf("upstream request wrong: %+v", env.Data.UpstreamReq)
	}
	if env.Data.UpstreamResp.Status != 200 || env.Data.UpstreamResp.Body != `{"id":"chatcmpl-1"}` {
		t.Fatalf("upstream response wrong: %+v", env.Data.UpstreamResp)
	}

	// No payload recorded -> 404.
	if resp := get(insert("")); resp.Code != http.StatusNotFound {
		t.Fatalf("empty payload_path: want 404, got %d", resp.Code)
	}
	// Payload pruned/missing -> 404.
	if resp := get(insert("20260921/gone")); resp.Code != http.StatusNotFound {
		t.Fatalf("missing files: want 404, got %d", resp.Code)
	}
	// Unknown log id -> 404.
	if resp := get(999999); resp.Code != http.StatusNotFound {
		t.Fatalf("unknown id: want 404, got %d", resp.Code)
	}
}
