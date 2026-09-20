package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PWZER/llm-switch/internal/store"
)

// seedRegistry registers model rows with context limits on the harness
// provider (rows expand to every live channel of the provider, so the names
// are anthropic-served and the claude-*/[1m] listing decorations apply).
func (h *harness) seedRegistry() {
	h.t.Helper()
	ctx := context.Background()
	var pid int64
	chs, err := h.st.Channels.List(ctx)
	must(h.t, err)
	for _, c := range chs {
		if c.Protocol == "anthropic" {
			pid = c.ProviderID
		}
	}
	seed := func(id string, cw, mot int64) {
		must(h.t, h.st.Models.Create(ctx, &store.Model{
			ID: id, ProviderID: pid, UpstreamModel: "fake-chat",
			DisplayName: id, Enabled: true,
			ContextWindow: strPtrI64(cw), MaxOutputTokens: strPtrI64(mot),
		}))
	}
	seed("aaa-model", 32000, 4096)
	seed("zzz-model", 128000, 8192)
	h.rebuild()
}

func strPtrI64(v int64) *int64 { return &v }

func (h *harness) getModels(t *testing.T, query string, anthropic bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models"+query, nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key)
	if anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := h.client.Do(req)
	must(t, err)
	defer resp.Body.Close()
	raw := make([]byte, 1<<20)
	n, _ := resp.Body.Read(raw)
	return resp, raw[:n]
}

func TestModelsPaginationAndContext(t *testing.T) {
	h := newHarness(t)
	h.seedRegistry()

	// Anthropic shape: page through with limit=1 + after_id cursor.
	_, raw := h.getModels(t, "?limit=1", true)
	var p1 struct {
		Data []struct {
			Type          string `json:"type"`
			ID            string `json:"id"`
			DisplayName   string `json:"display_name"`
			ContextLength *int64 `json:"context_length"`
		} `json:"data"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
		HasMore bool   `json:"has_more"`
	}
	must(t, json.Unmarshal(raw, &p1))
	if len(p1.Data) != 1 || p1.Data[0].ID != "aaa-model" || p1.Data[0].Type != "model" {
		t.Fatalf("page 1 wrong: %s", raw)
	}
	if p1.FirstID != "aaa-model" || p1.LastID != "aaa-model" || !p1.HasMore {
		t.Fatalf("page 1 envelope wrong: %s", raw)
	}
	if p1.Data[0].ContextLength == nil || *p1.Data[0].ContextLength != 32000 {
		t.Fatalf("context_length missing: %s", raw)
	}

	_, raw = h.getModels(t, "?limit=1&after_id=aaa-model", true)
	var p2 struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		FirstID string `json:"first_id"`
		HasMore bool   `json:"has_more"`
	}
	must(t, json.Unmarshal(raw, &p2))
	// The Anthropic surface lists discovery variants, so claude-aaa-model
	// (variant of aaa-model) sorts right after aaa-model.
	if len(p2.Data) != 1 || p2.Data[0].ID != "claude-aaa-model" || !p2.HasMore {
		t.Fatalf("page 2 wrong: %s", raw)
	}

	// Cursor past the last entry: empty page, has_more false.
	_, raw = h.getModels(t, "?after_id=zzz-model", true)
	var p3 struct {
		Data    []json.RawMessage `json:"data"`
		HasMore bool              `json:"has_more"`
	}
	must(t, json.Unmarshal(raw, &p3))
	if len(p3.Data) != 0 || p3.HasMore {
		t.Fatalf("tail page wrong: %s", raw)
	}

	// OpenAI shape carries context fields and the provider description, but
	// never the claude-* discovery variants.
	_, raw = h.getModels(t, "", false)
	if !strings.Contains(string(raw), `"context_length":128000`) ||
		!strings.Contains(string(raw), `"max_output_tokens":8192`) {
		t.Fatalf("openai shape missing context fields: %s", raw)
	}
	// The description marks the source ("[model]" for registry rows) and
	// names the account that would serve first: "[model] {provider}/{account}"
	// (the channel name is omitted — any live channel can serve the name).
	if !strings.Contains(string(raw), `"description":"[model] fake/k1"`) {
		t.Fatalf("openai shape missing provider/account description: %s", raw)
	}
	if strings.Contains(string(raw), `"claude-zzz-model"`) {
		t.Fatalf("openai shape must not list discovery variants: %s", raw)
	}
	if !strings.Contains(string(raw), `"has_more":false`) {
		t.Fatalf("openai full list should not have more: %s", raw)
	}

	// The Anthropic surface lists the variants alongside the plain names.
	_, raw = h.getModels(t, "", true)
	if !strings.Contains(string(raw), `"claude-zzz-model"`) {
		t.Fatalf("anthropic shape missing discovery variant: %s", raw)
	}
}

// TestModelsDescriptionSourceMarker: the description marks where a listed
// name comes from — "[model] " for provider registry rows, "[route] " for
// model routes. A name backed by both (a route wins at resolve time) is
// marked as a route; the claude-* discovery mirrors inherit the marker.
func TestModelsDescriptionSourceMarker(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Dual existence: a models row AND a route on the same name.
	ch, err := h.st.Channels.Get(ctx, 1)
	must(t, err)
	pid := ch.ProviderID
	_, err = h.st.Models.EnsureModel(ctx, &store.Model{
		ID: "dual-model", ProviderID: pid, UpstreamModel: "fake-chat",
	})
	must(t, err)
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "dual-model",
		Targets: []store.ModelRouteTarget{{ChannelID: int64p(1), UpstreamModel: "fake-chat"}},
	}))
	// Pure route: no models row backs the name.
	must(t, h.st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "route-only",
		Targets: []store.ModelRouteTarget{{ChannelID: int64p(1), UpstreamModel: "fake-chat"}},
	}))
	h.rebuild()

	// OpenAI shape: rows carry "[model] ", route names (pure or dual) "[route] ".
	_, raw := h.getModels(t, "", false)
	for _, want := range []string{
		`"description":"[model] fake/k1"`,
		`"description":"[route] fake/k1"`,
		`"dual-model"`, `"route-only"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("openai shape missing %s: %s", want, raw)
		}
	}

	// Anthropic shape: the claude-* discovery mirrors inherit the marker.
	_, raw = h.getModels(t, "", true)
	if !strings.Contains(string(raw), `"description":"[route] fake/k1"`) {
		t.Fatalf("anthropic shape missing [route] description: %s", raw)
	}

	// By-id lookups carry the marker on both shapes.
	for _, shape := range []struct {
		anthropic bool
		want      string
	}{{false, `"description":"[route] fake/k1"`}, {true, `"description":"[route] fake/k1"`}} {
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/route-only", nil)
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+h.key)
		if shape.anthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, err := h.client.Do(req)
		must(t, err)
		rawID := readAll(t, resp)
		if resp.StatusCode != 200 || !strings.Contains(string(rawID), shape.want) {
			t.Fatalf("by-id (anthropic=%v) wrong: %d %s", shape.anthropic, resp.StatusCode, rawID)
		}
	}
}

func TestModelsGetByID(t *testing.T) {
	h := newHarness(t)
	h.seedRegistry()

	// OpenAI shape.
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/zzz-model", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := h.client.Do(req)
	must(t, err)
	raw := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"object":"model"`) ||
		!strings.Contains(string(raw), `"context_length":128000`) {
		t.Fatalf("openai get-by-id wrong: %d %s", resp.StatusCode, raw)
	}

	// Anthropic shape.
	req2, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/zzz-model", nil)
	must(t, err)
	req2.Header.Set("Authorization", "Bearer "+h.key)
	req2.Header.Set("anthropic-version", "2023-06-01")
	resp2, err := h.client.Do(req2)
	must(t, err)
	raw2 := readAll(t, resp2)
	if !strings.Contains(string(raw2), `"type":"model"`) ||
		!strings.Contains(string(raw2), `"max_output_tokens":8192`) {
		t.Fatalf("anthropic get-by-id wrong: %s", raw2)
	}

	// Discovery variants resolve by id on the Anthropic surface only.
	reqV, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/claude-zzz-model", nil)
	must(t, err)
	reqV.Header.Set("Authorization", "Bearer "+h.key)
	reqV.Header.Set("anthropic-version", "2023-06-01")
	respV, err := h.client.Do(reqV)
	must(t, err)
	rawV := readAll(t, respV)
	if respV.StatusCode != 200 || !strings.Contains(string(rawV), `"context_length":128000`) {
		t.Fatalf("variant get-by-id wrong: %d %s", respV.StatusCode, rawV)
	}
	reqV2, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/claude-zzz-model", nil)
	must(t, err)
	reqV2.Header.Set("Authorization", "Bearer "+h.key)
	respV2, err := h.client.Do(reqV2)
	must(t, err)
	readAll(t, respV2)
	if respV2.StatusCode != 404 {
		t.Fatalf("openai shape must not serve variants, got %d", respV2.StatusCode)
	}

	// Unknown id: protocol-shaped 404.
	req3, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/nope", nil)
	must(t, err)
	req3.Header.Set("Authorization", "Bearer "+h.key)
	resp3, err := h.client.Do(req3)
	must(t, err)
	raw3 := readAll(t, resp3)
	if resp3.StatusCode != 404 || !strings.Contains(string(raw3), "invalid_request_error") {
		t.Fatalf("openai 404 wrong: %d %s", resp3.StatusCode, raw3)
	}
}

// TestSurfacePrefixModelsShape: the prefixed mounts make the base_url the
// shape control point. /anthropic/v1/models renders the full Anthropic shape
// (decorations included) under bearer-only auth — the Claude Code
// ANTHROPIC_AUTH_TOKEN case the bare /v1 header sniff mis-serves — and
// /openai/v1/models stays plain even with sniffed headers present. Bare /v1
// keeps header sniffing (covered by the other tests in this file).
func TestSurfacePrefixModelsShape(t *testing.T) {
	h := newHarness(t)
	h.seedRegistry()

	// Bearer-only on the anthropic prefix: decorated Anthropic listing.
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/anthropic/v1/models", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := h.client.Do(req)
	must(t, err)
	raw := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"type":"model"`) {
		t.Fatalf("anthropic prefix listing wrong: %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"claude-zzz-model"`) {
		t.Fatalf("anthropic prefix must list discovery variants: %s", raw)
	}
	if strings.Contains(string(raw), `"object":"list"`) {
		t.Fatalf("anthropic prefix must not render the openai envelope: %s", raw)
	}

	// Sniffed headers cannot flip the openai prefix.
	req, err = http.NewRequest(http.MethodGet, h.srv.URL+"/openai/v1/models", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("x-api-key", h.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err = h.client.Do(req)
	must(t, err)
	raw = readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"object":"list"`) {
		t.Fatalf("openai prefix listing wrong: %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), `"claude-zzz-model"`) {
		t.Fatalf("openai prefix must stay plain: %s", raw)
	}

	// By-id: the anthropic prefix serves discovery variants, the openai
	// prefix 404s them.
	req, err = http.NewRequest(http.MethodGet, h.srv.URL+"/anthropic/v1/models/claude-zzz-model", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err = h.client.Do(req)
	must(t, err)
	raw = readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"context_length":128000`) {
		t.Fatalf("anthropic prefix by-id variant wrong: %d %s", resp.StatusCode, raw)
	}
	req, err = http.NewRequest(http.MethodGet, h.srv.URL+"/openai/v1/models/claude-zzz-model", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err = h.client.Do(req)
	must(t, err)
	readAll(t, resp)
	if resp.StatusCode != 404 {
		t.Fatalf("openai prefix must not serve variants, got %d", resp.StatusCode)
	}

	// Auth-failure bodies follow the prefix's shape.
	req, err = http.NewRequest(http.MethodGet, h.srv.URL+"/anthropic/v1/models", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, err = h.client.Do(req)
	must(t, err)
	raw = readAll(t, resp)
	if resp.StatusCode != 401 || !strings.Contains(string(raw), `"type":"error"`) {
		t.Fatalf("anthropic prefix auth error shape wrong: %d %s", resp.StatusCode, raw)
	}
	req, err = http.NewRequest(http.MethodGet, h.srv.URL+"/openai/v1/models", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, err = h.client.Do(req)
	must(t, err)
	raw = readAll(t, resp)
	if resp.StatusCode != 401 || strings.Contains(string(raw), `"type":"error"`) {
		t.Fatalf("openai prefix auth error shape wrong: %d %s", resp.StatusCode, raw)
	}
}

// TestSurfacePrefixChatEndpoints: the chat surfaces serve under their
// prefixes exactly as on bare /v1.
func TestSurfacePrefixChatEndpoints(t *testing.T) {
	h := newHarness(t)

	_, raw := h.post("/anthropic/v1/messages", `{"model":"claude-test","max_tokens":16,"stream":false}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "Hello from fake Anthropic") {
		t.Fatalf("anthropic prefix messages broken: %s", raw)
	}
	_, raw = h.post("/openai/v1/chat/completions", `{"model":"test-model","max_tokens":16,"stream":false}`, nil)
	if !strings.Contains(string(raw), "Hello from fake OpenAI") {
		t.Fatalf("openai prefix chat broken: %s", raw)
	}
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	raw := make([]byte, 1<<20)
	n, _ := resp.Body.Read(raw)
	return raw[:n]
}

func TestModelsDuplicateNamesMerge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The same name registered on two providers: the harness provider's row
	// carries context_window, the second provider's row carries
	// max_output_tokens.
	var pid int64
	chs, err := h.st.Channels.List(ctx)
	must(t, err)
	for _, c := range chs {
		if c.Protocol == "openai" {
			pid = c.ProviderID
			break
		}
	}
	pid2, err := h.st.Providers.Create(ctx, "fake2", nil)
	must(t, err)
	_, err = h.st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid2, Name: "fake2-openai", Protocol: "openai",
		BaseURL: h.openai.URL(), ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 5, Weight: 1,
		Passthrough: true,
	})
	must(t, err)
	must(h.t, h.st.Models.Create(ctx, &store.Model{
		ID: "dup-model", ProviderID: pid, UpstreamModel: "fake-chat",
		DisplayName: "dup-model", Enabled: true,
		ContextWindow: strPtrI64(32000),
	}))
	must(h.t, h.st.Models.Create(ctx, &store.Model{
		ID: "dup-model", ProviderID: pid2, UpstreamModel: "fake-chat",
		DisplayName: "dup-model", Enabled: true,
		MaxOutputTokens: strPtrI64(8192),
	}))
	h.rebuild()

	// Listed exactly once with metadata merged from both rows (Anthropic shape).
	_, raw := h.getModels(t, "", true)
	var list struct {
		Data []struct {
			ID              string `json:"id"`
			ContextLength   *int64 `json:"context_length"`
			MaxOutputTokens *int64 `json:"max_output_tokens"`
		} `json:"data"`
	}
	must(t, json.Unmarshal(raw, &list))
	dups := 0
	for _, m := range list.Data {
		if m.ID != "dup-model" {
			continue
		}
		dups++
		if m.ContextLength == nil || *m.ContextLength != 32000 ||
			m.MaxOutputTokens == nil || *m.MaxOutputTokens != 8192 {
			t.Fatalf("dup-model metadata not merged: %s", raw)
		}
	}
	if dups != 1 {
		t.Fatalf("want exactly one dup-model entry, got %d: %s", dups, raw)
	}

	// OpenAI shape merged too, and get-by-id serves the merged entry.
	_, raw = h.getModels(t, "", false)
	if !strings.Contains(string(raw), `"context_length":32000`) ||
		!strings.Contains(string(raw), `"max_output_tokens":8192`) {
		t.Fatalf("openai shape missing merged metadata: %s", raw)
	}
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/dup-model", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := h.client.Do(req)
	must(t, err)
	rawByID := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(string(rawByID), `"context_length":32000`) {
		t.Fatalf("get-by-id merged wrong: %d %s", resp.StatusCode, rawByID)
	}

	// Disabling the harness provider's row keeps the name listed via the
	// second provider's row, whose context_window is NULL (omitempty drops it).
	row, err := h.st.Models.Get(ctx, pid, "dup-model")
	must(t, err)
	row.Enabled = false
	must(h.t, h.st.Models.Update(ctx, &row))
	h.rebuild()
	_, raw = h.getModels(t, "", true)
	if !strings.Contains(string(raw), `"dup-model"`) ||
		strings.Contains(string(raw), `"context_length"`) ||
		!strings.Contains(string(raw), `"max_output_tokens":8192`) {
		t.Fatalf("second provider's row should still serve the name without cw: %s", raw)
	}

	// Disabling every row removes the name from the list and by-id lookups.
	row, err = h.st.Models.Get(ctx, pid2, "dup-model")
	must(t, err)
	row.Enabled = false
	must(h.t, h.st.Models.Update(ctx, &row))
	h.rebuild()
	_, raw = h.getModels(t, "", true)
	if strings.Contains(string(raw), `"dup-model"`) {
		t.Fatalf("disabled rows must not be listed: %s", raw)
	}
	req2, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/dup-model", nil)
	must(t, err)
	req2.Header.Set("Authorization", "Bearer "+h.key)
	resp2, err := h.client.Do(req2)
	must(t, err)
	if resp2.StatusCode != 404 {
		t.Fatalf("disabled model must 404 by id, got %d", resp2.StatusCode)
	}
	readAll(t, resp2)
}

// TestContext1mMarker: an anthropic-served model with a 1M context window
// lists a "<id>[1m]" entry (plus its claude-* mirror) on the Anthropic
// surface only, and requests — marker stripped by the client or sent
// verbatim — route to the bare identity.
func TestContext1mMarker(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var pid int64
	chs, err := h.st.Channels.List(ctx)
	must(h.t, err)
	for _, c := range chs {
		if c.Protocol == "anthropic" {
			pid = c.ProviderID
		}
	}
	must(h.t, h.st.Models.Create(ctx, &store.Model{
		ID: "kimi-k3", ProviderID: pid, UpstreamModel: "fake-chat",
		DisplayName: "kimi-k3", Enabled: true,
		ContextWindow: strPtrI64(1048576), MaxOutputTokens: strPtrI64(16384),
	}))
	h.rebuild()

	// The stripped (Claude Code) and the verbatim (marker kept) request forms
	// both resolve to the bare row identity.
	_, raw := h.post("/v1/messages", `{"model":"claude-kimi-k3","max_tokens":16,"stream":false}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "Hello from fake Anthropic") {
		t.Fatalf("claude-kimi-k3 did not route: %s", raw)
	}
	_, raw = h.post("/v1/messages", `{"model":"kimi-k3[1m]","max_tokens":16,"stream":false}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(raw), "Hello from fake Anthropic") {
		t.Fatalf("kimi-k3[1m] did not route: %s", raw)
	}

	// Listing: a 1M-capable model lists only the [1m] forms on the Anthropic
	// surface — the marked id and its claude-* mirror; the plain id and its
	// mirror are superseded (the marked id routes to the same identity via
	// suffix stripping). The marked entry's display name carries the suffix.
	_, raw = h.getModels(t, "", true)
	for _, want := range []string{`"id":"kimi-k3[1m]"`, `"display_name":"kimi-k3[1m]"`, `"id":"claude-kimi-k3[1m]"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("anthropic listing missing %s: %s", want, raw)
		}
	}
	for _, gone := range []string{`"id":"kimi-k3"`, `"id":"claude-kimi-k3"`} {
		if strings.Contains(string(raw), gone) {
			t.Fatalf("anthropic listing must drop the plain 1m forms, found %s: %s", gone, raw)
		}
	}
	_, raw = h.getModels(t, "", false)
	if !strings.Contains(string(raw), `"kimi-k3"`) {
		t.Fatalf("openai surface must keep the plain id: %s", raw)
	}
	if strings.Contains(string(raw), `"kimi-k3[1m]"`) || strings.Contains(string(raw), "claude-kimi-k3") {
		t.Fatalf("openai surface must stay plain: %s", raw)
	}

	// By-id resolves the marked id (limits copied) on the Anthropic surface.
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/kimi-k3[1m]", nil)
	must(h.t, err)
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := h.client.Do(req)
	must(h.t, err)
	raw = readAll(h.t, resp)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"context_length":1048576`) {
		t.Fatalf("marked get-by-id wrong: %d %s", resp.StatusCode, raw)
	}

	// The plain id is unlisted on the Anthropic surface once the [1m] entry
	// exists: by-id 404s there too (routing itself still accepts the name).
	reqPlain, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models/kimi-k3", nil)
	must(h.t, err)
	reqPlain.Header.Set("Authorization", "Bearer "+h.key)
	reqPlain.Header.Set("anthropic-version", "2023-06-01")
	respPlain, err := h.client.Do(reqPlain)
	must(h.t, err)
	readAll(h.t, respPlain)
	if respPlain.StatusCode != 404 {
		t.Fatalf("plain 1m id must be unlisted on the anthropic surface, got %d", respPlain.StatusCode)
	}

	// The log records the canonical bare identity, so the decorated request
	// forms (claude-* mirror, [1m] marker) coalesce into one model row
	// (log writes flush on a 200ms batch — poll briefly).
	var logs []store.RequestLog
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		logs, _, err = h.st.Logs.QueryLogs(ctx, store.LogFilter{Model: "kimi-k3"})
		must(h.t, err)
		if len(logs) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 canonical log rows, got %d", len(logs))
	}
	for _, l := range logs {
		if l.UpstreamModel != "fake-chat" || !l.Success {
			t.Fatalf("log row wrong: %+v", l)
		}
	}
}
