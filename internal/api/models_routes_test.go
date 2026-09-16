package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/store"
)

// The {providerID}/{id} route pair: ids containing "/" (OpenRouter-style
// "vendor/large") must survive percent-encoding, and the key must scope rows
// per provider. Mounted on a bare mux to bypass the session middleware.
func TestModelRoutesProviderKey(t *testing.T) {
	st := newRefreshTestStore(t)
	ctx := context.Background()
	var pids [2]int64
	for i, name := range []string{"p1", "p2"} {
		pid, err := st.Providers.Create(ctx, name, nil)
		if err != nil {
			t.Fatalf("provider %s: %v", name, err)
		}
		pids[i] = pid
		if _, err := st.Models.EnsureModel(ctx, &store.Model{
			ID: "vendor/large", ProviderID: pid, UpstreamModel: "vendor/large",
		}); err != nil {
			t.Fatalf("seed model on %s: %v", name, err)
		}
	}

	s := &Server{St: st}
	r := chi.NewRouter()
	r.Put("/models/{providerID}/{id}", s.handleUpdateModel)
	r.Delete("/models/{providerID}/{id}", s.handleDeleteModel)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// Encoded id scopes to provider 1's row only.
	if rec := call(http.MethodPut, "/models/"+strconv.FormatInt(pids[0], 10)+"/vendor%2Flarge", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("put status = %d: %s", rec.Code, rec.Body.String())
	}
	row1, err := st.Models.Get(ctx, pids[0], "vendor/large")
	if err != nil || row1.Enabled {
		t.Fatalf("provider 1 row not disabled: %+v err=%v", row1, err)
	}
	row2, err := st.Models.Get(ctx, pids[1], "vendor/large")
	if err != nil || !row2.Enabled {
		t.Fatalf("provider 2 row must stay enabled: %+v err=%v", row2, err)
	}

	// Delete removes only the addressed row.
	if rec := call(http.MethodDelete, "/models/"+strconv.FormatInt(pids[0], 10)+"/vendor%2Flarge", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := st.Models.Get(ctx, pids[0], "vendor/large"); err == nil {
		t.Fatalf("provider 1 row should be gone")
	}
	if _, err := st.Models.Get(ctx, pids[1], "vendor/large"); err != nil {
		t.Fatalf("provider 2 row should survive: %v", err)
	}

	// Negative provider id: 400, not a silent lookup.
	if rec := call(http.MethodPut, "/models/-1/x", `{"enabled":false}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative providerID status = %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(call(http.MethodPut, "/models/-1/x", `{"enabled":false}`).Body.Bytes(), &env); err != nil || env.Code == 0 {
		t.Fatalf("negative providerID must return an error envelope, got %s", call(http.MethodPut, "/models/-1/x", `{"enabled":false}`).Body.String())
	}

	// Missing row: 404 envelope.
	if rec := call(http.MethodPut, "/models/"+strconv.FormatInt(pids[1], 10)+"/missing", `{"enabled":false}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing row status = %d: %s", rec.Code, rec.Body.String())
	}
}

// Model route names are canonical: the "[1m]" suffix is reserved for the
// GET /v1/models 1M-context marker and is rejected at the admin API.
func TestModelRouteNameCanonical(t *testing.T) {
	st := newRefreshTestStore(t)
	ctx := context.Background()
	pid, err := st.Providers.Create(ctx, "p", nil)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	cid, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Name: "c", Protocol: "openai",
		BaseURL: "http://127.0.0.1:1", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 1, Weight: 1,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}

	s := &Server{St: st}
	r := chi.NewRouter()
	r.Put("/model-routes/{name}", s.handleUpsertRoute)
	call := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	targets := `{"targets":[{"channel_id":` + strconv.FormatInt(cid, 10) + `,"upstream_model":"m"}]}`

	// Reserved suffix: 400 envelope, nothing stored.
	rec := call("/model-routes/kimi-k3[1m]", targets)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("[1m] name status = %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Code == 0 {
		t.Fatalf("reserved-suffix name must return an error envelope: %s", rec.Body.String())
	}
	if _, err := st.Routes.Get(ctx, "kimi-k3[1m]"); err == nil {
		t.Fatalf("reserved-suffix route must not be stored")
	}

	// Plain canonical name: created.
	if rec := call("/model-routes/kimi-k3", targets); rec.Code != http.StatusOK {
		t.Fatalf("plain name status = %d: %s", rec.Code, rec.Body.String())
	}
}
