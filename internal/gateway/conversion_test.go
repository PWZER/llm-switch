package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// The harness provider has both an openai and an anthropic channel, and rows
// are provider-scoped with same-protocol routing preferred — so each test
// first disables the surface's own protocol, forcing the request onto the
// opposite-protocol channel and exercising the full conversion matrix.

func TestAnthropicClientToOpenAIUpstreamNonStream(t *testing.T) {
	h := newHarness(t)
	h.disableHarnessProtocol("anthropic") // force the cross-protocol bridge
	resp, raw := h.post("/v1/messages", `{
		"model":"test-model","max_tokens":100,
		"system":"You are helpful.",
		"messages":[{"role":"user","content":"Hi"}]
	}`, map[string]string{"anthropic-version": "2023-06-01"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			CacheRead    int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	must(t, json.Unmarshal(raw, &out))
	if out.Type != "message" || len(out.Content) != 1 || out.Content[0].Text != "Hello from fake OpenAI" {
		t.Fatalf("unexpected body: %s", raw)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("stop_reason %q", out.StopReason)
	}
	// Usage mapping: openai prompt/completion/cache -> anthropic input/output/cache_read.
	// OpenAI prompt_tokens (12) includes cached tokens (4); the Anthropic wire
	// convention excludes them, so input_tokens = 12 - 4 = 8.
	if out.Usage.InputTokens != 8 || out.Usage.OutputTokens != 6 || out.Usage.CacheRead != 4 {
		t.Fatalf("usage mapping wrong: %+v", out.Usage)
	}

	// Upstream request checks: model rewritten, system hoisted, max_tokens passed.
	var up struct {
		Model    string `json:"model"`
		MaxToken any    `json:"max_tokens"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	must(t, json.Unmarshal(h.openai.LastBody(), &up))
	if up.Model != "fake-chat" {
		t.Fatalf("model not rewritten: %q", up.Model)
	}
	if up.MaxToken == nil || up.MaxToken.(float64) != 100 {
		t.Fatalf("max_tokens lost: %v", up.MaxToken)
	}
	if len(up.Messages) < 2 || up.Messages[0].Role != "system" {
		t.Fatalf("system message missing: %s", h.openai.LastBody())
	}
}

func TestAnthropicClientToOpenAIUpstreamStream(t *testing.T) {
	h := newHarness(t)
	h.disableHarnessProtocol("anthropic") // force the cross-protocol bridge
	resp, raw := h.post("/v1/messages", `{"model":"test-model","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`,
		map[string]string{"anthropic-version": "2023-06-01"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	s := string(raw)
	for _, want := range []string{
		"event: message_start",
		`"type":"content_block_start"`,
		`"type":"text_delta"`,
		`"text":"Hello"`,
		`event: content_block_stop`,
		`"stop_reason":"end_turn"`,
		`"output_tokens":6`,
		"event: message_stop",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("converted stream missing %q:\n%s", want, s)
		}
	}
	// Strict framing: no delta before its content_block_start.
	if strings.Index(s, "text_delta") < strings.Index(s, "content_block_start") {
		t.Fatalf("delta emitted before block start:\n%s", s)
	}
}

func TestAnthropicClientToOpenAIUpstreamTools(t *testing.T) {
	h := newHarness(t)
	h.disableHarnessProtocol("anthropic") // force the cross-protocol bridge
	// Turn 1: tool definition -> tool_use block back.
	resp, raw := h.post("/v1/messages", `{
		"model":"test-model","max_tokens":100,
		"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"weather in Beijing?"}]
	}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	must(t, json.Unmarshal(raw, &out))
	if out.StopReason != "tool_use" || len(out.Content) != 1 || out.Content[0].Type != "tool_use" {
		t.Fatalf("tool_use conversion failed: %s", raw)
	}
	if out.Content[0].ID != "call_fake_1" || out.Content[0].Name != "get_weather" {
		t.Fatalf("tool identity wrong: %s", raw)
	}
	var input map[string]any
	must(t, json.Unmarshal(out.Content[0].Input, &input))
	if input["city"] != "Beijing" {
		t.Fatalf("accumulated args wrong: %s", out.Content[0].Input)
	}

	// Turn 2: tool_result history -> upstream sees role:"tool" message.
	_, raw = h.post("/v1/messages", `{
		"model":"test-model","max_tokens":100,
		"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_fake_1","name":"get_weather","input":{"city":"Beijing"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_fake_1","content":"sunny 25C"}]}
		]
	}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("turn2 status %d: %s", resp.StatusCode, raw)
	}
	var up struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    any    `json:"content"`
		} `json:"messages"`
	}
	must(t, json.Unmarshal(h.openai.LastBody(), &up))
	var found bool
	for _, m := range up.Messages {
		if m.Role == "tool" && m.ToolCallID == "call_fake_1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool result not mapped to role:tool message: %s", h.openai.LastBody())
	}
}

func TestOpenAIClientToAnthropicUpstreamNonStream(t *testing.T) {
	h := newHarness(t)
	h.disableHarnessProtocol("openai") // force the cross-protocol bridge
	resp, raw := h.post("/v1/chat/completions", `{"model":"claude-test","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	must(t, json.Unmarshal(raw, &out))
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello from fake Anthropic" {
		t.Fatalf("unexpected body: %s", raw)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason %q", out.Choices[0].FinishReason)
	}
	// Anthropic input_tokens (10) excludes cache_read (3) + cache_creation (2);
	// the OpenAI wire convention includes cache tokens, so prompt_tokens = 15.
	if out.Usage.PromptTokens != 15 || out.Usage.CompletionTokens != 5 {
		t.Fatalf("usage mapping wrong: %+v", out.Usage)
	}
	if out.Usage.PromptTokensDetails.CachedTokens != 3 {
		t.Fatalf("cache mapping wrong: %+v", out.Usage)
	}

	// Upstream request: max_tokens injected (Anthropic requires it).
	var up struct {
		MaxTokens int64 `json:"max_tokens"`
	}
	must(t, json.Unmarshal(h.anthro.LastBody(), &up))
	if up.MaxTokens != 8192 {
		t.Fatalf("default max_tokens not injected: %d", up.MaxTokens)
	}
}

func TestOpenAIClientToAnthropicUpstreamStream(t *testing.T) {
	h := newHarness(t)
	h.disableHarnessProtocol("openai") // force the cross-protocol bridge
	resp, raw := h.post("/v1/chat/completions", `{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	s := string(raw)
	for _, want := range []string{
		`"role":"assistant"`,
		`"content":"Hello"`,
		`"content":" from fake"`,
		`"finish_reason":"stop"`,
		`"prompt_tokens":15`, // anthropic input 10 + cache_read 3 + cache_write 2 (openai convention)
		"[DONE]",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("converted stream missing %q:\n%s", want, s)
		}
	}
}

func TestOpenAIClientToAnthropicUpstreamTools(t *testing.T) {
	h := newHarness(t)
	h.disableHarnessProtocol("openai") // force the cross-protocol bridge
	resp, raw := h.post("/v1/chat/completions", `{
		"model":"claude-test",
		"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"weather?"}]
	}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	must(t, json.Unmarshal(raw, &out))
	if len(out.Choices) != 1 || out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("tool_calls conversion failed: %s", raw)
	}
	tc := out.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "toolu_fake_1" || tc[0].Function.Name != "get_weather" {
		t.Fatalf("tool identity wrong: %s", raw)
	}
	if tc[0].Function.Arguments != `{"city":"Beijing"}` {
		t.Fatalf("arguments not accumulated: %q", tc[0].Function.Arguments)
	}
}
