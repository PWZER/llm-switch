package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/auth"
	"github.com/PWZER/llm-switch/internal/engine"
	"github.com/PWZER/llm-switch/internal/gateway"
	"github.com/PWZER/llm-switch/internal/stats"
	"github.com/PWZER/llm-switch/internal/store"
	"github.com/PWZER/llm-switch/internal/testutil"
)

// harness wires a real gateway against fake upstreams on a temp SQLite DB.
type harness struct {
	t      *testing.T
	st     *store.Store
	holder *engine.Holder
	srv    *httptest.Server
	client *http.Client
	openai *testutil.Fake
	anthro *testutil.Fake
	key    string // plaintext client key
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir, filepath.Join(dir, "gw.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.New(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	openai := testutil.NewOpenAI()
	anthro := testutil.NewAnthropic()
	t.Cleanup(openai.Server.Close)
	t.Cleanup(anthro.Server.Close)

	ctx := context.Background()
	pid, err := st.Providers.Create(ctx, "fake", nil)
	must(t, err)
	_, err = st.Accounts.Create(ctx, pid, "k1", "upstream-secret", 1, "")
	must(t, err)
	_, err = st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Name: "fake-openai", Protocol: "openai",
		BaseURL: openai.URL(), ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 10, Weight: 1,
		Passthrough: true,
	})
	must(t, err)
	_, err = st.Models.EnsureModel(ctx, &store.Model{
		ID: "test-model", ProviderID: pid, UpstreamModel: "fake-chat",
	})
	must(t, err)
	_, err = st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Name: "fake-anthropic", Protocol: "anthropic",
		BaseURL: anthro.URL(), ChatPath: "/v1/messages",
		AuthStyle: "x-api-key", ExtraHeaders: "{}", Enabled: true, Priority: 10, Weight: 1,
		Passthrough: true,
	})
	must(t, err)
	_, err = st.Models.EnsureModel(ctx, &store.Model{
		ID: "claude-test", ProviderID: pid, UpstreamModel: "fake-chat",
	})
	must(t, err)
	plain, hash, prefix := auth.GenerateClientKey()
	_, err = st.APIKeys.Create(ctx, "tester", hash, prefix, nil, nil)
	must(t, err)

	holder := engine.NewHolder()
	must(t, holder.Rebuild(ctx, st))
	logger := stats.New(st)
	pool := gateway.NewAccountPool()
	gw := gateway.New(holder, pool, gateway.NewUpstreamClient(), logger)

	r := chi.NewRouter()
	validateKey := func(key string) (gateway.ClientKeyRecord, bool) {
		rec, ok := holder.Load().ClientKeys[engine.HashKey(key)]
		return gateway.ClientKeyRecord{ID: rec.ID, Name: rec.Name}, ok
	}
	v1 := chi.NewRouter()
	v1.Use(gateway.ClientKeyAuth(validateKey))
	gw.Mount(v1)
	r.Mount("/v1", v1)
	// Prefixed mounts mirror main.go: the base_url prefix pins the /models
	// shape and the auth-failure error shape.
	anthroV1 := chi.NewRouter()
	anthroV1.Use(gateway.ClientKeyAuthFor("anthropic", validateKey))
	gw.MountAnthropic(anthroV1)
	r.Mount("/anthropic/v1", anthroV1)
	openaiV1 := chi.NewRouter()
	openaiV1.Use(gateway.ClientKeyAuthFor("openai", validateKey))
	gw.MountOpenAI(openaiV1)
	r.Mount("/openai/v1", openaiV1)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { logger.Close(context.Background()) })

	return &harness{
		t: t, st: st, holder: holder, srv: srv,
		client: srv.Client(), openai: openai, anthro: anthro, key: plain,
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func (h *harness) rebuild() {
	must(h.t, h.holder.Rebuild(context.Background(), h.st))
}

// disableHarnessProtocol disables every channel of the given protocol:
// provider-scoped rows then fall back to cross-protocol bridging, which is
// what the conversion matrix tests exercise.
func (h *harness) disableHarnessProtocol(protocol string) {
	h.t.Helper()
	ctx := context.Background()
	chs, err := h.st.Channels.List(ctx)
	must(h.t, err)
	for _, c := range chs {
		if c.Protocol == protocol {
			c.Enabled = false
			must(h.t, h.st.Channels.Update(ctx, &c))
		}
	}
	h.rebuild()
}

// addSingleProtocolModel registers model on a fresh provider whose only
// channel speaks the given protocol (backed by the matching fake): requests
// from the other client surface must cross-protocol bridge to reach it.
func (h *harness) addSingleProtocolModel(model, protocol string) {
	h.t.Helper()
	ctx := context.Background()
	pid, err := h.st.Providers.Create(ctx, "solo-"+model, nil)
	must(h.t, err)
	_, err = h.st.Accounts.Create(ctx, pid, "k", "upstream-secret", 1, "")
	must(h.t, err)
	ch := &store.Channel{
		ProviderID: pid, Name: "solo-" + protocol, Protocol: protocol,
		ExtraHeaders: "{}", Enabled: true, Priority: 10, Weight: 1, Passthrough: true,
	}
	if protocol == "anthropic" {
		ch.BaseURL, ch.ChatPath, ch.AuthStyle = h.anthro.URL(), "/v1/messages", "x-api-key"
	} else {
		ch.BaseURL, ch.ChatPath, ch.AuthStyle = h.openai.URL(), "/chat/completions", "bearer"
	}
	_, err = h.st.Channels.Create(ctx, ch)
	must(h.t, err)
	_, err = h.st.Models.EnsureModel(ctx, &store.Model{
		ID: model, ProviderID: pid, UpstreamModel: "fake-chat",
	})
	must(h.t, err)
	h.rebuild()
}

func (h *harness) post(path, body string, headers map[string]string) (*http.Response, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	must(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	must(h.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	must(h.t, err)
	return resp, raw
}

func TestOpenAIPassthroughNonStream(t *testing.T) {
	h := newHarness(t)
	resp, raw := h.post("/v1/chat/completions", `{"model":"test-model","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	must(t, json.Unmarshal(raw, &out))
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello from fake OpenAI" {
		t.Fatalf("unexpected body: %s", raw)
	}
	if out.Usage.PromptTokens != 12 {
		t.Fatalf("usage lost in passthrough: %+v", out.Usage)
	}

	// The log row attributes the serving account.
	time.Sleep(400 * time.Millisecond)
	logs, total, err := h.st.Logs.QueryLogs(context.Background(), store.LogFilter{Model: "test-model"})
	must(t, err)
	if total < 1 {
		t.Fatalf("expected log rows, got %d", total)
	}
	var attributed bool
	for _, l := range logs {
		if l.AccountID != nil && l.AccountName == "k1" && l.Success {
			attributed = true
		}
	}
	if !attributed {
		t.Fatalf("no log row carries account attribution: %+v", logs)
	}
}

// A model route target pinning an account must authenticate upstream with
// exactly that account's key.
func TestRoutePinnedAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The harness provider: look it up via its first channel.
	ch, err := h.st.Channels.Get(ctx, 1)
	must(t, err)
	pid := ch.ProviderID

	aid, err := h.st.Accounts.Create(ctx, pid, "pinned", "pinned-secret", 1, "")
	must(t, err)
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name: "pinned-route",
		Targets: []store.ModelRouteTarget{
			{ProviderID: pid, ChannelID: int64p(1), UpstreamModel: "fake-chat", AccountID: &aid},
		},
	}))
	h.rebuild()

	resp, raw := h.post("/v1/chat/completions", `{"model":"pinned-route","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if got := h.openai.LastAuthorization(); got != "Bearer pinned-secret" {
		t.Fatalf("pinned account key not used upstream: %q", got)
	}

	// Disabling the pinned account makes the route target unroutable: the
	// route chain is empty, and the bare name has no bindings -> 404.
	must(t, h.st.Accounts.Update(ctx, aid, nil, nil, boolp(false)))
	h.rebuild()
	resp, raw = h.post("/v1/chat/completions", `{"model":"pinned-route","stream":false}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled pinned account should unroutable the target, got %d: %s", resp.StatusCode, raw)
	}
}

func boolp(b bool) *bool { return &b }

func int64p(v int64) *int64 { return &v }

// TestLogModelCanonicalization: request_logs.model records the canonical
// resolved identity — the bare row name for models rows (claude-* mirrors
// and [1m] markers normalized away), the route's own name for route hits
// (literal claude-* routes keep their name) — while unroutable names log
// verbatim for debugging.
func TestLogModelCanonicalization(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A literal claude-* route and a plain route, both on the openai fake.
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "claude-special",
		Targets: []store.ModelRouteTarget{{ChannelID: int64p(1), UpstreamModel: "fake-chat"}},
	}))
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "my-route",
		Targets: []store.ModelRouteTarget{{ChannelID: int64p(1), UpstreamModel: "fake-chat"}},
	}))
	h.rebuild()

	anthroHeaders := map[string]string{"anthropic-version": "2023-06-01"}
	type req struct {
		path, body string
		headers    map[string]string
	}
	requests := []req{
		// Row hit, literal name.
		{"/v1/chat/completions", `{"model":"test-model","stream":false}`, nil},
		// Row hits via the claude-* mirror and via the [1m] marker.
		{"/v1/messages", `{"model":"claude-test-model","max_tokens":16,"stream":false}`, anthroHeaders},
		{"/v1/messages", `{"model":"test-model[1m]","max_tokens":16,"stream":false}`, anthroHeaders},
		// Literal claude-* route keeps its own name; the claude-<route>
		// mirror resolves to the bare route name.
		{"/v1/chat/completions", `{"model":"claude-special","stream":false}`, nil},
		{"/v1/chat/completions", `{"model":"claude-my-route","stream":false}`, nil},
		// Unroutable: the raw name is logged as sent.
		{"/v1/messages", `{"model":"claude-nonexistent","max_tokens":16,"stream":false}`, anthroHeaders},
	}
	for _, rq := range requests {
		wantStatus := 200
		if strings.Contains(rq.body, "nonexistent") {
			wantStatus = 404
		}
		resp, raw := h.post(rq.path, rq.body, rq.headers)
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s %s: status %d: %s", rq.path, rq.body, resp.StatusCode, raw)
		}
	}

	// Log writes flush on a 200ms batch — poll briefly for all six rows.
	var total int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		_, total, err = h.st.Logs.QueryLogs(ctx, store.LogFilter{})
		must(t, err)
		if total == len(requests) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if total != len(requests) {
		t.Fatalf("expected %d log rows, got %d", len(requests), total)
	}

	for _, want := range []struct {
		model         string
		count         int
		success       bool
		upstreamModel string
	}{
		{"test-model", 3, true, "fake-chat"},
		{"claude-special", 1, true, "fake-chat"},
		{"my-route", 1, true, "fake-chat"},
		{"claude-nonexistent", 1, false, ""},
	} {
		rows, _, err := h.st.Logs.QueryLogs(ctx, store.LogFilter{Model: want.model})
		must(t, err)
		if len(rows) != want.count {
			t.Fatalf("model %q: expected %d log rows, got %d", want.model, want.count, len(rows))
		}
		for _, l := range rows {
			if l.Model != want.model || l.Success != want.success || l.UpstreamModel != want.upstreamModel {
				t.Fatalf("model %q: log row wrong: %+v", want.model, l)
			}
		}
	}
}

func TestOpenAIPassthroughStream(t *testing.T) {
	h := newHarness(t)
	resp, raw := h.post("/v1/chat/completions", `{"model":"test-model","stream":true}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	s := string(raw)
	for _, want := range []string{"Hello", " stream", "[DONE]"} {
		if !strings.Contains(s, want) {
			t.Fatalf("stream missing %q:\n%s", want, s)
		}
	}
	// Byte fidelity: the tap must not alter frames.
	if !strings.Contains(s, `"prompt_tokens":12`) {
		t.Fatalf("usage frame missing:\n%s", s)
	}
}

func TestAnthropicPassthroughStream(t *testing.T) {
	h := newHarness(t)
	resp, raw := h.post("/v1/messages", `{"model":"claude-test","max_tokens":64,"stream":true}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	s := string(raw)
	for _, want := range []string{
		"event: message_start", "content_block_start", `"text_delta"`,
		`"stop_reason":"end_turn"`, "event: message_stop",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("stream missing %q:\n%s", want, s)
		}
	}
}

// TestSameProtocolPreference: a provider-scoped row expands to every live
// channel of its provider, but the client surface's protocol wins outright —
// the anthropic surface is served by the anthropic channel and the openai
// surface by the openai one, with no cross-protocol bridging.
func TestSameProtocolPreference(t *testing.T) {
	h := newHarness(t)

	_, raw := h.post("/v1/messages", `{"model":"test-model","max_tokens":16,"stream":false}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "Hello from fake Anthropic") {
		t.Fatalf("anthropic surface must prefer the anthropic channel: %s", raw)
	}

	_, raw = h.post("/v1/chat/completions", `{"model":"claude-test","stream":false}`, nil)
	if !strings.Contains(string(raw), "Hello from fake OpenAI") {
		t.Fatalf("openai surface must prefer the openai channel: %s", raw)
	}
}

func TestFailoverOn429(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Add a second provider with an openai channel at lower priority; fake #1
	// fails first 3 calls.
	pid, err := h.st.Providers.Create(ctx, "fake2", nil)
	must(t, err)
	_, err = h.st.Accounts.Create(ctx, pid, "k2", "upstream-secret-2", 1, "")
	must(t, err)
	_, err = h.st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Name: "fake2-openai", Protocol: "openai",
		BaseURL: h.openai.URL(), ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 5, Weight: 1,
		Passthrough: true,
	})
	must(t, err)
	_, err = h.st.Models.EnsureModel(ctx, &store.Model{
		ID: "test-model", ProviderID: pid, UpstreamModel: "fake-chat",
	})
	must(t, err)
	h.openai.FailFirstN(1)
	h.rebuild()

	resp, raw := h.post("/v1/chat/completions", `{"model":"test-model","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("failover failed, status %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "Hello from fake OpenAI") {
		t.Fatalf("unexpected body: %s", raw)
	}
	if h.openai.Requests() != 2 { // first 429'd, second succeeded (failN=1 => 2nd call OK)
		t.Fatalf("unexpected request count: %d", h.openai.Requests())
	}

	// The failure must be logged with attempts=2 once stats flush.
	time.Sleep(400 * time.Millisecond)
	logs, total, err := h.st.Logs.QueryLogs(ctx, store.LogFilter{Model: "test-model"})
	must(t, err)
	if total < 1 {
		t.Fatalf("expected log rows, got %d", total)
	}
	found := false
	for _, l := range logs {
		if l.Attempts == 2 && l.Success {
			found = true
		}
	}
	if !found {
		t.Fatalf("no log row with attempts=2: %+v", logs)
	}
}

func TestClientKeyAuthAndModelNotFound(t *testing.T) {
	h := newHarness(t)

	// Missing key -> 401.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model"}`))
	resp, err := h.client.Do(req)
	must(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}

	// Unknown model -> protocol-shaped 404.
	_, raw := h.post("/v1/messages", `{"model":"nope","max_tokens":8}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "not_found_error") {
		t.Fatalf("anthropic 404 shape missing: %s", raw)
	}
	_, raw = h.post("/v1/chat/completions", `{"model":"nope"}`, nil)
	if !strings.Contains(string(raw), "invalid_request_error") {
		t.Fatalf("openai 404 shape missing: %s", raw)
	}
	_, raw = h.post("/v1/responses", `{"model":"nope","input":"Hi"}`, nil)
	if !strings.Contains(string(raw), "invalid_request_error") {
		t.Fatalf("responses 404 shape missing: %s", raw)
	}
}

func TestModelsEndpointShapes(t *testing.T) {
	h := newHarness(t)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := h.client.Do(req)
	must(t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"object":"list"`) || !strings.Contains(string(raw), `"test-model"`) {
		t.Fatalf("openai shape wrong: %s", raw)
	}

	req2, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer "+h.key)
	req2.Header.Set("anthropic-version", "2023-06-01")
	resp2, err := h.client.Do(req2)
	must(t, err)
	raw2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(raw2), `"type":"model"`) || !strings.Contains(string(raw2), `"has_more":false`) {
		t.Fatalf("anthropic shape wrong: %s", raw2)
	}
}

func TestModelRouteHotSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Route pinned to the anthropic channel; model "main".
	ch, err := h.st.Channels.List(ctx)
	must(t, err)
	var anthroID int64
	for _, c := range ch {
		if c.Protocol == "anthropic" {
			anthroID = c.ID
		}
	}
	pid := ch[0].ProviderID
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{Name: "main", Targets: []store.ModelRouteTarget{
		{ProviderID: pid, ChannelID: &anthroID, UpstreamModel: "fake-chat"},
	}}))
	h.rebuild()

	// The route resolves and serves via the anthropic channel.
	_, raw := h.post("/v1/chat/completions", `{"model":"main","stream":false}`, nil)
	// OpenAI client -> anthropic channel: cross-protocol arrives in Phase 3;
	// for now the passthrough rewrite means the OpenAI client hits the
	// anthropic upstream and gets its non-stream JSON passed through.
	if !strings.Contains(string(raw), "fake") && !strings.Contains(string(raw), "Hello") {
		t.Fatalf("model route did not reach the anthropic fake: %s", raw)
	}

	// Hot-switch the route to the openai channel; no restart, next request follows.
	var openaiID int64
	for _, c := range ch {
		if c.Protocol == "openai" {
			openaiID = c.ID
		}
	}
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{Name: "main", Targets: []store.ModelRouteTarget{
		{ProviderID: pid, ChannelID: &openaiID, UpstreamModel: "fake-chat"},
	}}))
	h.rebuild()
	before := h.openai.Requests()
	_, raw = h.post("/v1/chat/completions", `{"model":"main","stream":false}`, nil)
	if h.openai.Requests() != before+1 {
		t.Fatalf("hot-switch did not take effect; body: %s", raw)
	}
	if !bytes.Contains(raw, []byte("Hello from fake OpenAI")) {
		t.Fatalf("unexpected body after hot-switch: %s", raw)
	}
}

func TestModelRouteMultiTargetFailover(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	chs, err := h.st.Channels.List(ctx)
	must(t, err)
	var anthroID, openaiID int64
	for _, c := range chs {
		if c.Protocol == "anthropic" {
			anthroID = c.ID
		} else {
			openaiID = c.ID
		}
	}
	pid := chs[0].ProviderID

	// Route chain: primary = anthropic channel, failover = openai channel.
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{Name: "chain", Targets: []store.ModelRouteTarget{
		{ProviderID: pid, ChannelID: &anthroID, UpstreamModel: "fake-chat"},
		{ProviderID: pid, ChannelID: &openaiID, UpstreamModel: "fake-chat"},
	}}))
	h.rebuild()

	// Anthropic client -> chain: passthrough on the anthropic endpoint works.
	_, raw := h.post("/v1/messages", `{"model":"chain","max_tokens":16,"stream":false}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "Hello from fake Anthropic") {
		t.Fatalf("primary target failed: %s", raw)
	}

	// Kill the primary endpoint; the chain must fail over to the openai one
	// (cross-protocol conversion path).
	h.anthro.FailFirstN(100)
	h.rebuild()

	_, raw = h.post("/v1/messages", `{"model":"chain","max_tokens":16,"stream":false}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "Hello from fake OpenAI") {
		t.Fatalf("failover target did not serve: %s", raw)
	}
}

// A provider-scoped route target (no channel pin) auto-selects among the
// provider's live endpoints: each client surface is served by its own
// protocol endpoint with no bridging.
func TestRouteProviderTargetAutoSelect(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ch, err := h.st.Channels.Get(ctx, 1)
	must(t, err)
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "auto",
		Targets: []store.ModelRouteTarget{{ProviderID: ch.ProviderID, UpstreamModel: "fake-chat"}},
	}))
	h.rebuild()

	_, raw := h.post("/v1/messages", `{"model":"auto","max_tokens":16,"stream":false}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "Hello from fake Anthropic") {
		t.Fatalf("anthropic surface must hit the anthropic endpoint: %s", raw)
	}
	_, raw = h.post("/v1/chat/completions", `{"model":"auto","stream":false}`, nil)
	if !strings.Contains(string(raw), "Hello from fake OpenAI") {
		t.Fatalf("openai surface must hit the openai endpoint: %s", raw)
	}
}

// Cross-protocol fallback: with the provider's anthropic endpoint disabled,
// the anthropic surface still serves the provider-scoped target via the
// openai endpoint (bridged).
func TestRouteProviderTargetCrossProtocolFallback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ch, err := h.st.Channels.Get(ctx, 1)
	must(t, err)
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "auto",
		Targets: []store.ModelRouteTarget{{ProviderID: ch.ProviderID, UpstreamModel: "fake-chat"}},
	}))
	h.disableHarnessProtocol("anthropic")

	_, raw := h.post("/v1/messages", `{"model":"auto","max_tokens":16,"stream":false}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "Hello from fake OpenAI") {
		t.Fatalf("anthropic surface must fall back to the openai endpoint: %s", raw)
	}
}

// In-segment failover: a provider-scoped target fails over across the
// provider's own endpoints (two openai endpoints; the anthropic one is
// protocol-filtered away for the openai surface).
func TestRouteProviderSegmentFailover(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ch, err := h.st.Channels.Get(ctx, 1)
	must(t, err)
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "auto",
		Targets: []store.ModelRouteTarget{{ProviderID: ch.ProviderID, UpstreamModel: "fake-chat"}},
	}))
	// A second openai endpoint of the same provider at lower priority.
	_, err = h.st.Channels.Create(ctx, &store.Channel{
		ProviderID: ch.ProviderID, Name: "fake-openai-2", Protocol: "openai",
		BaseURL: h.openai.URL(), ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 5, Weight: 1,
		Passthrough: true,
	})
	must(t, err)
	h.rebuild()

	// First openai call 429s; the segment's next candidate (the second openai
	// endpoint of the same provider) must take over.
	h.openai.FailFirstN(1)
	resp, raw := h.post("/v1/chat/completions", `{"model":"auto","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("segment failover failed, status %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "Hello from fake OpenAI") {
		t.Fatalf("unexpected body: %s", raw)
	}
	if h.openai.Requests() != 2 { // first 429'd, second succeeded
		t.Fatalf("unexpected request count: %d", h.openai.Requests())
	}
}

// A provider-scoped target with a pinned account authenticates upstream with
// exactly that key; disabling the account skips the whole segment (the route
// has nothing else to fall through to -> 404).
func TestRouteProviderTargetPinnedAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ch, err := h.st.Channels.Get(ctx, 1)
	must(t, err)
	aid, err := h.st.Accounts.Create(ctx, ch.ProviderID, "pinned", "pinned-secret", 1, "")
	must(t, err)
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name: "auto-pin",
		Targets: []store.ModelRouteTarget{
			{ProviderID: ch.ProviderID, UpstreamModel: "fake-chat", AccountID: &aid},
		},
	}))
	h.rebuild()

	resp, raw := h.post("/v1/chat/completions", `{"model":"auto-pin","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if got := h.openai.LastAuthorization(); got != "Bearer pinned-secret" {
		t.Fatalf("pinned account key not used upstream: %q", got)
	}

	must(t, h.st.Accounts.Update(ctx, aid, nil, nil, boolp(false)))
	h.rebuild()
	resp, raw = h.post("/v1/chat/completions", `{"model":"auto-pin","stream":false}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled pinned account should unroutable the segment, got %d: %s", resp.StatusCode, raw)
	}
}
