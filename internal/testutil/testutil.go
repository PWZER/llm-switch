// Package testutil provides scripted fake upstreams (OpenAI- and
// Anthropic-shaped) for integration tests and the manual e2e recipe.
package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Fake is a scripted upstream. failFirst makes the first N requests fail with
// 429 (for failover tests); Requests counts received calls. LastBody keeps the
// most recent chat/messages request body for assertions.
type Fake struct {
	Server   *httptest.Server
	Protocol string // "openai" | "anthropic"

	mu       sync.Mutex
	failN    int
	requests int
	lastBody []byte
}

// NewOpenAI starts a fake OpenAI-compatible upstream.
func NewOpenAI() *Fake { return newFake("openai") }

// NewAnthropic starts a fake Anthropic-compatible upstream.
func NewAnthropic() *Fake { return newFake("anthropic") }

func newFake(protocol string) *Fake {
	f := &Fake{Protocol: protocol}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.Server = httptest.NewServer(mux)
	return f
}

// URL returns the fake's base URL.
func (f *Fake) URL() string { return f.Server.URL }

// FailFirstN makes the next n requests return 429 before succeeding.
func (f *Fake) FailFirstN(n int) {
	f.mu.Lock()
	f.failN = n
	f.mu.Unlock()
}

// Requests returns how many chat/message calls the fake received.
func (f *Fake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// LastBody returns the most recent chat/messages request body.
func (f *Fake) LastBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(f.lastBody))
	copy(out, f.lastBody)
	return out
}

func (f *Fake) remember(body []byte) {
	f.mu.Lock()
	f.lastBody = body
	f.mu.Unlock()
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/models" && f.Protocol == "openai":
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{map[string]any{"id": "fake-chat", "object": "model"}},
		})
		return
	case r.URL.Path == "/v1/models" && f.Protocol == "anthropic":
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []any{map[string]any{"type": "model", "id": "fake-chat", "display_name": "Fake Chat"}},
		})
		return
	case r.URL.Path == "/chat/completions" && f.Protocol == "openai":
		f.chat(w, r)
		return
	case r.URL.Path == "/v1/messages" && f.Protocol == "anthropic":
		f.messages(w, r)
		return
	default:
		http.NotFound(w, r)
	}
}

// gate records a request and reports whether it should fail with 429.
func (f *Fake) gate() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	return f.requests > f.failN
}

func (f *Fake) chat(w http.ResponseWriter, r *http.Request) {
	if !f.gate() {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"message": "slow down", "type": "rate_limit_error"}})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	f.remember(raw)
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []any  `json:"tools"`
	}
	_ = json.Unmarshal(raw, &body)

	// Tool scenario: when tools are advertised, reply with a tool call.
	if len(body.Tools) > 0 {
		toolCall := map[string]any{
			"id":   "call_fake_1",
			"type": "function",
			"function": map[string]any{
				"name":      "get_weather",
				"arguments": `{"city":"Beijing"}`,
			},
		}
		if !body.Stream {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "chatcmpl-fake-tool", "model": body.Model,
				"choices": []any{map[string]any{
					"index": 0,
					"message": map[string]any{
						"role": "assistant", "content": nil, "tool_calls": []any{toolCall},
					},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 5, "total_tokens": 25},
			})
			return
		}
		sse(w, [][]byte{
			[]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}`),
			[]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_fake_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`),
			[]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`),
			[]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]}}]}`),
			[]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
			[]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`),
			[]byte("data: [DONE]"),
		})
		return
	}

	if !body.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":    "chatcmpl-fake-1",
			"model": body.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "Hello from fake OpenAI"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": 12, "completion_tokens": 6, "total_tokens": 18,
				"prompt_tokens_details": map[string]any{"cached_tokens": 4},
			},
		})
		return
	}
	sse(w, [][]byte{
		[]byte(`data: {"id":"chatcmpl-fake-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`),
		[]byte(`data: {"id":"chatcmpl-fake-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"}}]}`),
		[]byte(`data: {"id":"chatcmpl-fake-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" stream"}}]}`),
		[]byte(`data: {"id":"chatcmpl-fake-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		[]byte(`data: {"id":"chatcmpl-fake-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":4}}}`),
		[]byte("data: [DONE]"),
	})
}

func (f *Fake) messages(w http.ResponseWriter, r *http.Request) {
	if !f.gate() {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "rate_limit_error", "message": "slow down"}})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	f.remember(raw)
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []any  `json:"tools"`
	}
	_ = json.Unmarshal(raw, &body)

	if len(body.Tools) > 0 {
		toolUse := map[string]any{
			"type": "tool_use", "id": "toolu_fake_1", "name": "get_weather",
			"input": map[string]any{"city": "Beijing"},
		}
		if !body.Stream {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "msg_fake_tool", "type": "message", "role": "assistant", "model": body.Model,
				"content":     []any{toolUse},
				"stop_reason": "tool_use",
				"usage":       map[string]any{"input_tokens": 18, "output_tokens": 6},
			})
			return
		}
		sse(w, [][]byte{
			[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ft\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":18}}}"),
			[]byte(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_fake_1","name":"get_weather","input":{}}}`),
			[]byte(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}}`),
			[]byte(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Beijing\"}"}}`),
			[]byte(`event: content_block_stop
data: {"type":"content_block_stop","index":0}`),
			[]byte(`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}`),
			[]byte(`event: message_stop
data: {"type":"message_stop"}`),
		})
		return
	}

	if !body.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "msg_fake_1", "type": "message", "role": "assistant", "model": body.Model,
			"content":     []any{map[string]any{"type": "text", "text": "Hello from fake Anthropic"}},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 5,
				"cache_read_input_tokens": 3, "cache_creation_input_tokens": 2,
			},
		})
		return
	}
	sse(w, [][]byte{
		[]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_fake_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}}`),
		[]byte(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		[]byte(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" from fake"}}`),
		[]byte(`event: content_block_stop
data: {"type":"content_block_stop","index":0}`),
		[]byte(`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`),
		[]byte(`event: message_stop
data: {"type":"message_stop"}`),
	})
}

func sse(w http.ResponseWriter, frames [][]byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	for _, frame := range frames {
		fmt.Fprintln(w, string(frame))
		fmt.Fprintln(w) // blank separator line
		flusher.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}

var _ = strings.TrimSpace
