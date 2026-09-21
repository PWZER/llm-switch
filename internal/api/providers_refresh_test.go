package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/store"
)

func newRefreshTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.New(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestGetModelsURLParsesOptionalLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"vendor/large","context_length":131072,"max_output_tokens":8192},
			{"id":"vendor/plain"}
		]}`))
	}))
	defer srv.Close()

	models, err := getModelsURL(context.Background(), srv.URL, "bearer", "sk-test")
	if err != nil {
		t.Fatalf("getModelsURL: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	if models[0].ContextLength == nil || *models[0].ContextLength != 131072 ||
		models[0].MaxOutputTokens == nil || *models[0].MaxOutputTokens != 8192 {
		t.Fatalf("limits not parsed: %+v", models[0])
	}
	if models[1].ContextLength != nil || models[1].MaxOutputTokens != nil {
		t.Fatalf("plain entry must keep nil limits: %+v", models[1])
	}
}

// refresh-models only fetches and returns the upstream list (with optional
// aggregator limits) — nothing is registered. Registration and cleanup go
// through sync-models: register entries EnsureModel (identity alias, limits
// backfilled, manual edits survive), remove entries delete provider-scoped
// rows — including rows that never appeared in any fetch.
func TestRefreshAndSyncModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"model-a","context_length":65536},
			{"id":"model-b","context_length":32768,"max_output_tokens":4096}
		]}`))
	}))
	defer srv.Close()

	st := newRefreshTestStore(t)
	ctx := context.Background()
	pid, err := st.Providers.Create(ctx, "p", &srv.URL)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Protocol: "openai",
		BaseURL: srv.URL, ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true,
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}
	aid, err := st.Accounts.Create(ctx, pid, "main", "sk-test", 1, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	s := &Server{St: st}
	r := chi.NewRouter()
	r.Post("/providers/{id}/refresh-models", s.handleRefreshModels)
	r.Post("/providers/{id}/sync-models", s.handleSyncModels)

	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost,
			"/providers/"+strconv.FormatInt(pid, 10)+"/"+path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", path, rec.Code, rec.Body.String())
		}
		return rec
	}

	// Refresh returns the fetched list with limits and registers nothing.
	rec := post("refresh-models", `{"account_id":`+strconv.FormatInt(aid, 10)+`}`)
	var env struct {
		Data struct {
			Provider  string `json:"provider"`
			AccountID int64  `json:"account_id"`
			Models    []struct {
				ID              string `json:"id"`
				ContextLength   *int64 `json:"context_length"`
				MaxOutputTokens *int64 `json:"max_output_tokens"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Provider != "p" || env.Data.AccountID != aid || len(env.Data.Models) != 2 {
		t.Fatalf("refresh payload wrong: %s", rec.Body.String())
	}
	if env.Data.Models[0].ID != "model-a" || env.Data.Models[0].ContextLength == nil ||
		*env.Data.Models[0].ContextLength != 65536 || env.Data.Models[0].MaxOutputTokens != nil {
		t.Fatalf("model-a entry wrong: %+v", env.Data.Models[0])
	}
	if env.Data.Models[1].ID != "model-b" || env.Data.Models[1].ContextLength == nil ||
		*env.Data.Models[1].ContextLength != 32768 ||
		env.Data.Models[1].MaxOutputTokens == nil || *env.Data.Models[1].MaxOutputTokens != 4096 {
		t.Fatalf("model-b entry wrong: %+v", env.Data.Models[1])
	}
	if rows, err := st.Models.List(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("refresh must not register rows: %v %v", rows, err)
	}

	// Sync registers the selected subset with backfilled limits.
	rec = post("sync-models", `{"register":[
		{"id":"model-a","context_length":65536},
		{"id":"model-b","context_length":32768,"max_output_tokens":4096}
	]}`)
	var syncEnv struct {
		Data struct {
			ModelsAdded   int `json:"models_added"`
			ModelsRemoved int `json:"models_removed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &syncEnv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if syncEnv.Data.ModelsAdded != 2 || syncEnv.Data.ModelsRemoved != 0 {
		t.Fatalf("sync counts wrong: %s", rec.Body.String())
	}
	models, err := st.Models.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]store.Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if a := byID["model-a"]; a.ProviderID != pid || a.UpstreamModel != "model-a" ||
		a.ContextWindow == nil || *a.ContextWindow != 65536 {
		t.Fatalf("model-a not stored with limits: %+v", a)
	}
	if b := byID["model-b"]; b.ContextWindow == nil || *b.ContextWindow != 32768 ||
		b.MaxOutputTokens == nil || *b.MaxOutputTokens != 4096 {
		t.Fatalf("model-b not stored with limits: %+v", b)
	}

	// A manual context_window survives re-registering the same id; nothing
	// new is added (insert-if-missing).
	ma := byID["model-a"]
	manual := int64(123456)
	ma.ContextWindow = &manual
	if err := st.Models.Update(ctx, &ma); err != nil {
		t.Fatalf("manual edit: %v", err)
	}
	rec = post("sync-models", `{"register":[{"id":"model-a","context_length":65536}]}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &syncEnv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if syncEnv.Data.ModelsAdded != 0 {
		t.Fatalf("re-sync must not re-add: %s", rec.Body.String())
	}
	models, _ = st.Models.List(ctx)
	for _, m := range models {
		if m.ID == "model-a" && (m.ContextWindow == nil || *m.ContextWindow != 123456) {
			t.Fatalf("manual context_window clobbered: %+v", m)
		}
	}

	// Removal deletes rows by (provider, id) — including a row that never
	// appeared in any fetch (historical entry the upstream no longer lists).
	if _, err := st.Models.EnsureModel(ctx, &store.Model{
		ID: "manual-only", ProviderID: pid, UpstreamModel: "manual-only",
	}); err != nil {
		t.Fatalf("seed manual row: %v", err)
	}
	rec = post("sync-models", `{"remove":["model-b","manual-only"]}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &syncEnv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if syncEnv.Data.ModelsRemoved != 2 || syncEnv.Data.ModelsAdded != 0 {
		t.Fatalf("remove counts wrong: %s", rec.Body.String())
	}
	models, _ = st.Models.List(ctx)
	if len(models) != 1 || models[0].ID != "model-a" {
		t.Fatalf("only model-a must remain: %+v", models)
	}
}

// Refresh without a configured provider models_url fails with an actionable
// 40901 — the fetch URL is provider-level config, never derived from
// endpoint URLs.
func TestRefreshModelsRequiresModelsURL(t *testing.T) {
	st := newRefreshTestStore(t)
	ctx := context.Background()
	pid, err := st.Providers.Create(ctx, "p", nil)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	aid, err := st.Accounts.Create(ctx, pid, "main", "sk-test", 1, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	s := &Server{St: st}
	r := chi.NewRouter()
	r.Post("/providers/{id}/refresh-models", s.handleRefreshModels)

	req := httptest.NewRequest(http.MethodPost,
		"/providers/"+strconv.FormatInt(pid, 10)+"/refresh-models",
		strings.NewReader(`{"account_id":`+strconv.FormatInt(aid, 10)+`}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != 40901 || !strings.Contains(env.Msg, "models_url") {
		t.Fatalf("error must name models_url: %s", rec.Body.String())
	}
}

func TestListModelsResolvesProviderContext(t *testing.T) {
	st := newRefreshTestStore(t)
	ctx := context.Background()
	pid, err := st.Providers.Create(ctx, "Zhipu GLM", nil)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := st.Models.EnsureModel(ctx, &store.Model{
		ID: "glm-4.6", ProviderID: pid, ContextWindow: func() *int64 { v := int64(204800); return &v }(),
	}); err != nil {
		t.Fatalf("ensure model: %v", err)
	}

	s := &Server{St: st}
	rec := httptest.NewRecorder()
	s.handleListModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int `json:"code"`
		Data []struct {
			ID              string `json:"id"`
			ProviderID      int64  `json:"provider_id"`
			ProviderName    string `json:"provider_name"`
			ProviderEnabled bool   `json:"provider_enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != 0 || len(env.Data) != 1 {
		t.Fatalf("unexpected envelope: %s", rec.Body.String())
	}
	row := env.Data[0]
	if row.ID != "glm-4.6" || row.ProviderID != pid || row.ProviderName != "Zhipu GLM" || !row.ProviderEnabled {
		t.Fatalf("provider context not resolved: %+v", row)
	}
}

func TestListModelsProviderScopedRows(t *testing.T) {
	st := newRefreshTestStore(t)
	ctx := context.Background()

	bind := func(name string) {
		t.Helper()
		pid, err := st.Providers.Create(ctx, name, nil)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		for _, id := range []string{"shared-model", "only-" + name} {
			if _, err := st.Models.EnsureModel(ctx, &store.Model{
				ID: id, ProviderID: pid, UpstreamModel: id,
			}); err != nil {
				t.Fatalf("register %s on %s: %v", id, name, err)
			}
		}
	}
	bind("ProvA")
	bind("ProvB")

	s := &Server{St: st}
	rec := httptest.NewRecorder()
	s.handleListModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int `json:"code"`
		Data []struct {
			ID           string `json:"id"`
			ProviderName string `json:"provider_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	type key struct {
		id       string
		provider string
	}
	rows := map[key]bool{}
	for _, m := range env.Data {
		rows[key{m.ID, m.ProviderName}] = true
	}
	// The same name on two providers is two independent rows, each carrying its
	// own provider context — no cross-provider merge.
	if len(rows) != 4 {
		t.Fatalf("want 4 provider-scoped rows, got %d: %s", len(rows), rec.Body.String())
	}
	for _, want := range []key{
		{"shared-model", "ProvA"}, {"shared-model", "ProvB"},
		{"only-ProvA", "ProvA"}, {"only-ProvB", "ProvB"},
	} {
		if !rows[want] {
			t.Fatalf("missing row %v: %s", want, rec.Body.String())
		}
	}
}
