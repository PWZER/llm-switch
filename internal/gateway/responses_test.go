package gateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PWZER/llm-switch/internal/store"
)

func strPtr(s string) *string { return &s }

// addResponsesChannel adds a second openai channel carrying a responses_path
// (passthrough-capable) at the given priority to the harness provider, and
// ensures model is registered on that provider (provider-scoped rows expand
// to the new channel automatically).
func (h *harness) addResponsesChannel(model string, priority int) {
	h.t.Helper()
	ctx := context.Background()
	provs, err := h.st.Providers.List(ctx)
	must(h.t, err)
	_, err = h.st.Channels.Create(ctx, &store.Channel{
		ProviderID: provs[0].ID, Name: "fake-responses", Protocol: "openai",
		BaseURL: h.openai.URL(), ChatPath: "/chat/completions",
		ResponsesPath: strPtr("/responses"),
		AuthStyle:     "bearer", ExtraHeaders: "{}", Enabled: true, Priority: priority, Weight: 1,
		Passthrough: true,
	})
	must(h.t, err)
	_, err = h.st.Models.EnsureModel(ctx, &store.Model{
		ID: model, ProviderID: provs[0].ID, UpstreamModel: "fake-chat",
	})
	must(h.t, err)
	h.rebuild()
}

func TestResponsesPassthroughNonStream(t *testing.T) {
	h := newHarness(t)
	h.addResponsesChannel("test-model", 20)

	resp, raw := h.post("/v1/responses", `{"model":"test-model","input":"Hi","store":false,"text":{"format":{"type":"text"}}}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	s := string(raw)
	if !strings.Contains(s, "Hello from fake Responses") || !strings.Contains(s, `"status":"completed"`) {
		t.Fatalf("unexpected body: %s", s)
	}
	// Upstream saw the body with only the model rewritten: no stream_options
	// injection (chat vocabulary), unknown fields preserved.
	last := string(h.openai.LastBody())
	var up map[string]any
	must(t, json.Unmarshal([]byte(last), &up))
	if up["model"] != "fake-chat" {
		t.Fatalf("model not rewritten: %s", last)
	}
	if _, has := up["stream_options"]; has {
		t.Fatalf("stream_options must not be injected into responses bodies: %s", last)
	}
	if _, has := up["text"]; !has {
		t.Fatalf("unknown fields not preserved: %s", last)
	}

	// Usage must be harvested from the top-level usage of the bare
	// (non-stream) response object.
	time.Sleep(400 * time.Millisecond)
	logs, _, err := h.st.Logs.QueryLogs(context.Background(), store.LogFilter{Model: "test-model"})
	must(t, err)
	found := false
	for _, l := range logs {
		if l.Success && l.PromptTokens == 12 && l.CompletionTokens == 6 && l.CacheReadTokens == 4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("responses non-stream usage not logged: %+v", logs)
	}
}

func TestResponsesPassthroughStream(t *testing.T) {
	h := newHarness(t)
	h.addResponsesChannel("test-model", 20)

	resp, raw := h.post("/v1/responses", `{"model":"test-model","input":"Hi","stream":true}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	s := string(raw)
	// Byte fidelity: event: lines relayed untouched.
	for _, want := range []string{
		"event: response.created", "event: response.output_text.delta",
		"event: response.completed", `"cached_tokens":4`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("stream missing %q:\n%s", want, s)
		}
	}

	// Usage must be harvested from the nested response.completed payload.
	time.Sleep(400 * time.Millisecond)
	logs, _, err := h.st.Logs.QueryLogs(context.Background(), store.LogFilter{Model: "test-model"})
	must(t, err)
	found := false
	for _, l := range logs {
		if l.Success && l.PromptTokens == 12 && l.CompletionTokens == 6 && l.CacheReadTokens == 4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("responses usage not logged: %+v", logs)
	}
}

func TestResponsesBridgeToChatNonStream(t *testing.T) {
	h := newHarness(t) // no responses_path anywhere: bridge via IR
	resp, raw := h.post("/v1/responses", `{"model":"test-model","input":"Hi"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Object string `json:"object"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	must(t, json.Unmarshal(raw, &out))
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("bad header: %s", raw)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "message" ||
		out.Output[0].Content[0].Text != "Hello from fake OpenAI" {
		t.Fatalf("bad output: %s", raw)
	}
	if out.Usage.InputTokens != 12 || out.Usage.OutputTokens != 6 || out.Usage.InputTokensDetails.CachedTokens != 4 {
		t.Fatalf("usage not mapped: %s", raw)
	}
	// Upstream received a chat-completions body.
	var up struct {
		Model    string `json:"model"`
		Messages []any  `json:"messages"`
	}
	must(t, json.Unmarshal(h.openai.LastBody(), &up))
	if up.Model != "fake-chat" || len(up.Messages) == 0 {
		t.Fatalf("upstream body is not chat-shaped: %s", h.openai.LastBody())
	}
}

func TestResponsesBridgeToChatStream(t *testing.T) {
	h := newHarness(t)
	resp, raw := h.post("/v1/responses", `{"model":"test-model","input":"Hi","stream":true}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	s := string(raw)
	for _, want := range []string{
		"event: response.created", "event: response.output_item.added",
		`"delta":"Hello"`, `"delta":" stream"`,
		"event: response.output_item.done", "event: response.completed",
		`"input_tokens":12`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("stream missing %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "event: response.completed") != 1 {
		t.Fatalf("want exactly one terminal event:\n%s", s)
	}
}

func TestResponsesBridgeToAnthropic(t *testing.T) {
	h := newHarness(t)
	h.disableHarnessProtocol("openai") // claude-test must bridge to the anthropic channel
	// No max_output_tokens: the anthropic encoder must inject the default.
	resp, raw := h.post("/v1/responses", `{"model":"claude-test","input":"Hi","instructions":"Be nice"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "Hello from fake Anthropic") {
		t.Fatalf("unexpected body: %s", raw)
	}
	var up struct {
		MaxTokens int64           `json:"max_tokens"`
		System    json.RawMessage `json:"system"` // block array
		Model     string          `json:"model"`
	}
	must(t, json.Unmarshal(h.anthro.LastBody(), &up))
	if up.MaxTokens != 8192 {
		t.Fatalf("default max_tokens not injected: %s", h.anthro.LastBody())
	}
	if !strings.Contains(string(up.System), "Be nice") {
		t.Fatalf("instructions not mapped to system: %s", h.anthro.LastBody())
	}
	if up.Model != "fake-chat" {
		t.Fatalf("model not rewritten: %s", h.anthro.LastBody())
	}
}

func TestResponsesResponseFormatRouting(t *testing.T) {
	h := newHarness(t)
	// anthropic upstream: text.format is unsupported -> 400, fail fast. The
	// model lives on an anthropic-only provider so the openai-preferring
	// Responses surface must bridge.
	h.addSingleProtocolModel("claude-fmt", "anthropic")
	resp, raw := h.post("/v1/responses",
		`{"model":"claude-fmt","input":"Hi","text":{"format":{"type":"json_object"}}}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "not supported") {
		t.Fatalf("unexpected error body: %s", raw)
	}
	if h.anthro.Requests() != 0 {
		t.Fatalf("request must not reach the upstream")
	}

	// openai upstream: converted request carries response_format.
	resp2, raw2 := h.post("/v1/responses",
		`{"model":"test-model","input":"Hi","text":{"format":{"type":"json_object"}}}`, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("want 200, got %d: %s", resp2.StatusCode, raw2)
	}
	if !strings.Contains(string(h.openai.LastBody()), `"response_format":{"type":"json_object"}`) {
		t.Fatalf("response_format not forwarded upstream: %s", h.openai.LastBody())
	}
}

func TestResponsesToolsRoundTrip(t *testing.T) {
	h := newHarness(t)
	tools := `[{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object"}}]`
	// Turn 1: client gets a function_call item with the upstream call id.
	resp, raw := h.post("/v1/responses",
		`{"model":"test-model","input":"weather?","tools":`+tools+`}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	must(t, json.Unmarshal(raw, &out))
	if len(out.Output) != 1 || out.Output[0].Type != "function_call" ||
		out.Output[0].CallID != "call_fake_1" || out.Output[0].Arguments != `{"city":"Beijing"}` {
		t.Fatalf("bad tool output: %s", raw)
	}

	// Turn 2: the function_call_output must land as a chat role:"tool" message
	// with the same verbatim id.
	turn2 := `{"model":"test-model","tools":` + tools + `,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
		{"type":"function_call","call_id":"call_fake_1","name":"get_weather","arguments":"{\"city\":\"Beijing\"}"},
		{"type":"function_call_output","call_id":"call_fake_1","output":"sunny"}
	]}`
	resp2, raw2 := h.post("/v1/responses", turn2, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("turn2 status %d: %s", resp2.StatusCode, raw2)
	}
	last := string(h.openai.LastBody())
	if !strings.Contains(last, `"tool_call_id":"call_fake_1"`) || !strings.Contains(last, `"role":"tool"`) {
		t.Fatalf("function_call_output not mapped to role:tool: %s", last)
	}
}

func TestResponsesFailoverToChat(t *testing.T) {
	h := newHarness(t)
	h.addResponsesChannel("test-model", 20) // passthrough channel tried first
	h.openai.FailFirstN(1)

	resp, raw := h.post("/v1/responses", `{"model":"test-model","input":"Hi","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("failover failed, status %d: %s", resp.StatusCode, raw)
	}
	if h.openai.Requests() != 2 {
		t.Fatalf("unexpected request count: %d", h.openai.Requests())
	}
	time.Sleep(400 * time.Millisecond)
	logs, _, err := h.st.Logs.QueryLogs(context.Background(), store.LogFilter{Model: "test-model"})
	must(t, err)
	found := false
	for _, l := range logs {
		if l.Attempts == 2 && l.Success &&
			l.ProtocolIn == "openai-responses" && l.ProtocolOut == "openai" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failover log row: %+v", logs)
	}
}

func TestResponsesRejections(t *testing.T) {
	h := newHarness(t)
	cases := []string{
		`{"model":"test-model","input":"x","store":true}`,
		`{"model":"test-model","input":"x","previous_response_id":"resp_1"}`,
		`{"model":"test-model","input":"x","background":true}`,
		`{"model":"test-model","input":"x","conversation":"conv_1"}`,
		`{"model":"test-model","input":"x","tools":[{"type":"web_search"}]}`,
		`{"model":"test-model","input":[{"type":"item_reference","id":"i1"}]}`,
		`{"model":"test-model","input":"x","tool_choice":{"type":"allowed_tools"}}`,
	}
	for _, body := range cases {
		resp, raw := h.post("/v1/responses", body, nil)
		if resp.StatusCode != 400 {
			t.Fatalf("want 400 for %s, got %d: %s", body, resp.StatusCode, raw)
		}
	}
	if h.openai.Requests() != 0 {
		t.Fatalf("rejected requests must not reach upstreams: %d", h.openai.Requests())
	}
}

func TestResponsesModelNotFound(t *testing.T) {
	h := newHarness(t)
	resp, raw := h.post("/v1/responses", `{"model":"nope","input":"x"}`, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "invalid_request_error") ||
		!strings.Contains(string(raw), "model nope not found") {
		t.Fatalf("unexpected 404 shape: %s", raw)
	}
}
