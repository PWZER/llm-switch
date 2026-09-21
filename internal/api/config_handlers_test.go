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

// configTestServer mounts the config export/import routes on a bare mux
// (bypassing the session middleware).
func configTestServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	s := &Server{St: st}
	r := chi.NewRouter()
	r.Post("/config/export", s.handleConfigExport)
	r.Post("/config/import", s.handleConfigImport)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// seedConfigFixture builds one provider with account, channel, model, a
// route pinning the channel, a client key and a setting.
func seedConfigFixture(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	pid, err := st.Providers.Create(ctx, "ds", nil)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	aid, err := st.Accounts.Create(ctx, pid, "main", "sk-plaintext-key-0001", 2, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	cid, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Protocol: "openai",
		BaseURL: "http://127.0.0.1:1", ChatPath: "/chat/completions", AuthStyle: "bearer",
		ExtraHeaders: "{}", Enabled: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if err := st.Models.Create(ctx, &store.Model{ID: "m1", ProviderID: pid, Enabled: true}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if err := st.Routes.Upsert(ctx, &store.ModelRoute{Name: "main", Targets: []store.ModelRouteTarget{
		{ProviderID: pid, ChannelID: int64ptr(cid), AccountID: int64ptr(aid), UpstreamModel: "m1"},
	}}); err != nil {
		t.Fatalf("route: %v", err)
	}
	const keyHash64 = "beefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeef"
	if _, err := st.APIKeys.Create(ctx, "agent", keyHash64, "sk-lsw-ba…be", nil, nil); err != nil {
		t.Fatalf("api key: %v", err)
	}
	if err := st.Settings.Set(ctx, "retention_days", "21"); err != nil {
		t.Fatalf("setting: %v", err)
	}
}

func int64ptr(v int64) *int64 { return &v }

type configEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func TestConfigExportHandler(t *testing.T) {
	st := newRefreshTestStore(t)
	seedConfigFixture(t, st)
	srv := configTestServer(t, st)

	// Without account keys: valid envelope, no plaintext anywhere.
	rec := postJSON(t, srv, "/config/export",
		`{"sections":["providers","model_routes","api_keys","settings"],"include_account_keys":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	var env configEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Code != 0 {
		t.Fatalf("envelope: %v %+v", err, env)
	}
	if !strings.Contains(rec.Body.String(), `"retention_days":"21"`) ||
		!strings.Contains(rec.Body.String(), `"key_hash":"beefbeef`) {
		t.Fatalf("export must carry settings and key hashes: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-plaintext-key-0001") {
		t.Fatal("export leaked the account plaintext")
	}
	var doc store.ConfigExport
	if err := json.Unmarshal(env.Data, &doc); err != nil {
		t.Fatalf("decode doc: %v", err)
	}
	if doc.Version != store.ExportVersion || len(doc.Providers) != 1 ||
		len(doc.Providers[0].Channels) != 1 || len(doc.Providers[0].Models) != 1 ||
		len(doc.Providers[0].Accounts) != 1 {
		t.Fatalf("doc shape: %+v", doc)
	}
	if doc.Providers[0].Accounts[0].APIKey != "" {
		t.Fatalf("account key must be empty: %+v", doc.Providers[0].Accounts[0])
	}

	// With keys: plaintext present.
	rec = postJSON(t, srv, "/config/export",
		`{"sections":["providers"],"include_account_keys":true}`)
	if !strings.Contains(rec.Body.String(), "sk-plaintext-key-0001") {
		t.Fatal("include_account_keys=true must carry the plaintext")
	}

	// Validation errors.
	for _, bad := range []struct {
		body string
		want int
	}{
		{`{}`, http.StatusBadRequest},                         // empty sections -> 40002
		{`{"sections":["bogus"]}`, http.StatusBadRequest},     // unknown section -> 40002
		{`not json`, http.StatusBadRequest},                   // 40001
	} {
		rec = postJSON(t, srv, "/config/export", bad.body)
		if rec.Code != bad.want {
			t.Fatalf("export %s: want %d got %d", bad.body, bad.want, rec.Code)
		}
	}
	var verr configEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &verr)
	if verr.Code != 40001 && verr.Code != 40002 {
		t.Fatalf("error code must be 40001/40002, got %d", verr.Code)
	}
}

func TestConfigImportHandler(t *testing.T) {
	ctx := context.Background()
	src := newRefreshTestStore(t)
	seedConfigFixture(t, src)
	srcSrv := configTestServer(t, src)

	rec := postJSON(t, srcSrv, "/config/export",
		`{"sections":["providers","model_routes","api_keys","settings"],"include_account_keys":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	var exportEnv configEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &exportEnv); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	rawDoc := exportEnv.Data

	dst := newRefreshTestStore(t)
	dstSrv := configTestServer(t, dst)

	// Dry run first: 200 + plan, no rows written.
	rec = postJSON(t, dstSrv, "/config/import?dry_run=1", string(rawDoc))
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body.String())
	}
	var plan store.ImportResult
	if err := json.Unmarshal(exportEnvData(rec), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if plan.Providers.Created != 1 || plan.Channels.Created != 1 {
		t.Fatalf("plan counts: %+v", plan)
	}
	ps, _ := dst.Providers.List(ctx)
	if len(ps) != 0 {
		t.Fatalf("dry run must not write: %+v", ps)
	}

	// Real import.
	rec = postJSON(t, dstSrv, "/config/import", string(rawDoc))
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var res store.ImportResult
	if err := json.Unmarshal(exportEnvData(rec), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Providers != plan.Providers || res.Accounts != plan.Accounts ||
		res.Channels != plan.Channels || res.Models != plan.Models ||
		res.ModelRoutes != plan.ModelRoutes || res.APIKeys != plan.APIKeys ||
		res.Settings != plan.Settings ||
		strings.Join(res.Warnings, "|") != strings.Join(plan.Warnings, "|") {
		t.Fatalf("plan and import must agree: %+v vs %+v", plan, res)
	}

	// Idempotence: importing the same document again updates everything.
	rec = postJSON(t, dstSrv, "/config/import", string(rawDoc))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-import: %d %s", rec.Code, rec.Body.String())
	}
	var again store.ImportResult
	_ = json.Unmarshal(exportEnvData(rec), &again)
	if again.Providers.Updated != 1 || again.Providers.Created != 0 ||
		again.Channels.Updated != 1 || again.Channels.Created != 0 ||
		again.APIKeys.Updated != 1 || again.APIKeys.Created != 0 {
		t.Fatalf("second import must be all updates: %+v", again)
	}

	// Warnings surface through the envelope: a route pin naming a protocol the
	// provider has no channel of degrades to provider auto-select with a
	// warning.
	rec = postJSON(t, dstSrv, "/config/import",
		`{"version":`+strconv.Itoa(store.ExportVersion)+`,"model_routes":[{"name":"w","targets":[{"provider":"ds","channel_protocol":"nope","upstream_model":"m1"}]}]}`)
	var warnEnv configEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &warnEnv)
	if !strings.Contains(string(warnEnv.Data), "nope") {
		t.Fatalf("warnings must surface: %s", warnEnv.Data)
	}

	// Bad documents map to 40002, malformed body to 40001. The current
	// version alone carries no sections, so it is still a bad document.
	rec = postJSON(t, dstSrv, "/config/import", `{"version":`+strconv.Itoa(store.ExportVersion)+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sections-less document: %d", rec.Code)
	}
	var badEnv configEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &badEnv)
	if badEnv.Code != 40002 {
		t.Fatalf("bad document code: %d", badEnv.Code)
	}
	rec = postJSON(t, dstSrv, "/config/import", `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", rec.Code)
	}
}

// exportEnvData pulls the data field out of the last recorded envelope body.
func exportEnvData(rec *httptest.ResponseRecorder) []byte {
	var env configEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return env.Data
}
