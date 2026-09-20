package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/store"
)

// accountsTestServer mounts the account/channel/provider action routes on a
// bare mux (bypassing the session middleware).
func accountsTestServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	s := &Server{St: st}
	r := chi.NewRouter()
	r.Post("/providers/{id}/accounts", s.handleCreateAccount)
	r.Get("/providers/{id}/accounts", s.handleListProviderAccounts)
	r.Put("/accounts/{id}", s.handleUpdateAccount)
	r.Post("/accounts/{id}/test", s.handleTestAccount)
	r.Post("/accounts/{id}/usage", s.handleAccountUsage)
	r.Post("/providers/{id}/refresh-models", s.handleRefreshModels)
	r.Post("/providers/{id}/models-preview", s.handlePreviewModels)
	r.Post("/providers/{id}/probe", s.handleProbeEndpoint)
	r.Post("/channels/{id}/test", s.handleTestChannel)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, srv *httptest.Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	return rec
}

func seedProviderChannel(t *testing.T, st *store.Store, upstreamURL string) (pid, cid int64) {
	t.Helper()
	ctx := context.Background()
	modelsURL := upstreamURL + "/models" // provider-level fetch URL (absolute)
	pid, err := st.Providers.Create(ctx, "p", &modelsURL)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	cid, err = st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Name: "ch", Protocol: "openai",
		BaseURL: upstreamURL, ChatPath: "/chat/completions", AuthStyle: "bearer",
		ExtraHeaders: "{}", Enabled: true, Priority: 10, Weight: 1,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	return pid, cid
}

func TestAccountCRUD(t *testing.T) {
	st := newRefreshTestStore(t)
	pid, _ := seedProviderChannel(t, st, "http://127.0.0.1:1")
	srv := accountsTestServer(t, st)

	rec := postJSON(t, srv, "/providers/"+strconv.FormatInt(pid, 10)+"/accounts",
		`{"label":"main","api_key":"sk-test-12345678","weight":3,"usage_probes":[{"type":"balance","path":"/user/balance"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			ID   int64  `json:"id"`
			Mask string `json:"api_key_mask"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Data.ID == 0 || env.Data.Mask == "" {
		t.Fatalf("create response: %s", rec.Body.String())
	}
	accounts, err := st.Accounts.List(context.Background(), pid)
	if err != nil || len(accounts) != 1 || string(accounts[0].UsageProbes) == "" {
		t.Fatalf("account not persisted with probes: %+v err=%v", accounts, err)
	}

	// api_key required.
	rec = postJSON(t, srv, "/providers/"+strconv.FormatInt(pid, 10)+"/accounts", `{"label":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing api_key status = %d", rec.Code)
	}

	// Update toggles enabled; usage_probes editable.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/accounts/"+strconv.FormatInt(env.Data.ID, 10),
		strings.NewReader(`{"enabled":false,"usage_probes":[]}`))
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := st.Accounts.Get(context.Background(), env.Data.ID)
	if err != nil || got.Enabled || string(got.UsageProbes) != "[]" {
		t.Fatalf("update lost: %+v err=%v", got, err)
	}
}

func TestRefreshModelsRequiresExplicitAccount(t *testing.T) {
	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer upstream.Close()

	st := newRefreshTestStore(t)
	pid, _ := seedProviderChannel(t, st, upstream.URL)
	srv := accountsTestServer(t, st)
	ctx := context.Background()
	aid, err := st.Accounts.Create(ctx, pid, "main", "sk-chosen-account", 1, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	pp := "/providers/" + strconv.FormatInt(pid, 10) + "/refresh-models"

	// Missing account_id: 400.
	if rec := postJSON(t, srv, pp, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no account_id status = %d: %s", rec.Code, rec.Body.String())
	}
	// Foreign/disabled account: 404.
	other, err := st.Providers.Create(ctx, "other", nil)
	if err != nil {
		t.Fatalf("other provider: %v", err)
	}
	foreignID, err := st.Accounts.Create(ctx, other, "x", "sk-foreign", 1, "")
	if err != nil {
		t.Fatalf("foreign account: %v", err)
	}
	if rec := postJSON(t, srv, pp, `{"account_id":`+strconv.FormatInt(foreignID, 10)+`}`); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign account status = %d: %s", rec.Code, rec.Body.String())
	}
	if err := st.Accounts.Update(ctx, aid, nil, nil, boolp(false)); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	if rec := postJSON(t, srv, pp, `{"account_id":`+strconv.FormatInt(aid, 10)+`}`); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled account status = %d: %s", rec.Code, rec.Body.String())
	}
	if err := st.Accounts.Update(ctx, aid, nil, nil, boolp(true)); err != nil {
		t.Fatalf("re-enable account: %v", err)
	}

	// Success: the chosen account's key authenticates upstream; the fetched
	// list comes back in the response and nothing is registered.
	rec := postJSON(t, srv, pp, `{"account_id":`+strconv.FormatInt(aid, 10)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d: %s", rec.Code, rec.Body.String())
	}
	if auth, _ := gotAuth.Load().(string); auth != "Bearer sk-chosen-account" {
		t.Fatalf("upstream saw auth %q, want the chosen account's key", auth)
	}
	var env struct {
		Data struct {
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Models) != 1 || env.Data.Models[0].ID != "m1" {
		t.Fatalf("refresh must return the fetched list: %s", rec.Body.String())
	}
	if _, err := st.Models.Get(ctx, pid, "m1"); err == nil {
		t.Fatalf("refresh must not register rows")
	}
}

func TestChannelTestConnectivityOnly(t *testing.T) {
	// 401 without any auth header still means reachable.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("connectivity probe must not send credentials")
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer upstream.Close()

	st := newRefreshTestStore(t)
	// No accounts at all: connectivity testing must not require one.
	_, cid := seedProviderChannel(t, st, upstream.URL)
	srv := accountsTestServer(t, st)

	rec := postJSON(t, srv, "/channels/"+strconv.FormatInt(cid, 10)+"/test", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			OK    bool   `json:"ok"`
			Class string `json:"class"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.Data.OK || env.Data.Class != "auth" {
		t.Fatalf("401 without key must be reachable: %+v", env.Data)
	}
}

func TestAccountTestQuickAndDeep(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
		case "/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	st := newRefreshTestStore(t)
	pid, _ := seedProviderChannel(t, st, upstream.URL)
	srv := accountsTestServer(t, st)
	ctx := context.Background()
	good, err := st.Accounts.Create(ctx, pid, "good", "sk-good", 1, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	bad, err := st.Accounts.Create(ctx, pid, "bad", "sk-bad", 1, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}

	type testData struct {
		OK         bool   `json:"ok"`
		Class      string `json:"class"`
		Depth      string `json:"depth"`
		ModelCount int    `json:"model_count"`
	}
	call := func(accountID int64, body string) testData {
		t.Helper()
		rec := postJSON(t, srv, "/accounts/"+strconv.FormatInt(accountID, 10)+"/test", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data testData `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Data
	}

	quick := call(good, `{"depth":"quick"}`)
	if !quick.OK || quick.Depth != "quick" || quick.ModelCount != 2 {
		t.Fatalf("quick test: %+v", quick)
	}
	badQuick := call(bad, `{"depth":"quick"}`)
	if badQuick.OK || badQuick.Class != "auth" {
		t.Fatalf("quick test with bad key must fail as auth: %+v", badQuick)
	}
	deep := call(good, `{"depth":"deep","model":"m1"}`)
	if !deep.OK || deep.Depth != "deep" || deep.Class != "ok" {
		t.Fatalf("deep test: %+v", deep)
	}
}

func TestModelsPreviewExplicitKeyOrAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-any" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer upstream.Close()

	st := newRefreshTestStore(t)
	pid, _ := seedProviderChannel(t, st, upstream.URL)
	srv := accountsTestServer(t, st)
	pp := "/providers/" + strconv.FormatInt(pid, 10) + "/models-preview"
	// models-preview fetches an explicit absolute URL — the endpoint's
	// base_url is no longer involved.
	form := `,"models_url":"` + upstream.URL + `/models"}`

	// No account_id and no api_key: 400 (no silent fallback even though an
	// enabled account exists).
	if _, err := st.Accounts.Create(context.Background(), pid, "main", "sk-any", 1, ""); err != nil {
		t.Fatalf("account: %v", err)
	}
	if rec := postJSON(t, srv, pp, `{`+form[1:]); rec.Code != http.StatusBadRequest {
		t.Fatalf("no credential status = %d: %s", rec.Code, rec.Body.String())
	}
	// Pasted key works for an unsaved account.
	if rec := postJSON(t, srv, pp, `{"api_key":"sk-any"`+form); rec.Code != http.StatusOK {
		t.Fatalf("pasted key status = %d: %s", rec.Code, rec.Body.String())
	}
}

// The form probe mirrors the channel test: connectivity-only, no credential
// required or sent.
func TestProbeEndpointConnectivityOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("connectivity probe must not send credentials")
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer upstream.Close()

	st := newRefreshTestStore(t)
	pid, _ := seedProviderChannel(t, st, upstream.URL)
	srv := accountsTestServer(t, st)

	rec := postJSON(t, srv, "/providers/"+strconv.FormatInt(pid, 10)+"/probe",
		`{"protocol":"openai","base_url":"`+upstream.URL+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			OK    bool   `json:"ok"`
			Class string `json:"class"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.Data.OK || env.Data.Class != "auth" {
		t.Fatalf("401 without credential must be reachable: %+v", env.Data)
	}
}

func boolp(b bool) *bool { return &b }
