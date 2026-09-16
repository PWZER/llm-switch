// Command mockupstream is a canned OpenAI + Anthropic compatible upstream for
// local end-to-end verification of llm-switch without real provider keys.
//
// Endpoints:
//
//	POST /chat/completions              OpenAI chat, stream + non-stream
//	POST /responses                     OpenAI Responses API, stream + non-stream
//	POST /v1/messages                   Anthropic messages, stream + non-stream
//	GET  /models                        OpenAI model list
//	GET  /user/balance                  DeepSeek-style balance (usage probes)
//	GET  /api/monitor/usage/quota/limit GLM coding-plan quota windows (usage probes)
//
// Behavior knobs: -fail-first N makes the first N requests return 429 so
// failover can be exercised. -model renames the served upstream model.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

var (
	addr      = flag.String("addr", ":9091", "listen address")
	failFirst = flag.Int("fail-first", 0, "fail the first N chat/message requests with 429")
	modelName = flag.String("model", "fake-chat", "upstream model name to serve")
)

var calls atomic.Int64

func main() {
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", openaiChat)
	mux.HandleFunc("/responses", openaiResponses)
	mux.HandleFunc("/v1/messages", anthropicMessages)
	mux.HandleFunc("/models", listModels)
	mux.HandleFunc("/v1/models", listModels)
	mux.HandleFunc("/user/balance", userBalance)
	mux.HandleFunc("/api/monitor/usage/quota/limit", planQuota)
	log.Printf("mockupstream listening on %s (fail-first=%d model=%s)", *addr, *failFirst, *modelName)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func gated(w http.ResponseWriter) bool {
	if calls.Add(1) > int64(*failFirst) {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprintln(w, `{"error":{"message":"mock rate limit","type":"rate_limit_error"}}`)
	return false
}

func readBody(r *http.Request) (model string, stream bool) {
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Model == "" {
		body.Model = *modelName
	}
	return body.Model, body.Stream
}

func listModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"object": "list", "data": []any{
		map[string]any{"id": *modelName, "object": "model"},
	}})
}

func openaiChat(w http.ResponseWriter, r *http.Request) {
	if !gated(w) {
		return
	}
	_, stream := readBody(r)
	if !stream {
		writeJSON(w, map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion", "model": *modelName,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "Hello from mockupstream (openai)"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18,
				"prompt_tokens_details": map[string]any{"cached_tokens": 5},
			},
		})
		return
	}
	sse(w,
		`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" from mockupstream"}}]}`,
		`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":5}}}`,
		"data: [DONE]",
	)
}

func anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if !gated(w) {
		return
	}
	_, stream := readBody(r)
	if !stream {
		writeJSON(w, map[string]any{
			"id": "msg_mock", "type": "message", "role": "assistant", "model": *modelName,
			"content":     []any{map[string]any{"type": "text", "text": "Hello from mockupstream (anthropic)"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 9, "output_tokens": 6, "cache_read_input_tokens": 2},
		})
		return
	}
	sse(w,
		`event: message_start
data: {"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","content":[],"usage":{"input_tokens":9,"cache_read_input_tokens":2}}}`,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" from mockupstream"}}`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`,
		`event: message_stop
data: {"type":"message_stop"}`,
	)
}

func openaiResponses(w http.ResponseWriter, r *http.Request) {
	if !gated(w) {
		return
	}
	_, stream := readBody(r)
	if !stream {
		writeJSON(w, map[string]any{
			"id": "resp_mock", "object": "response", "status": "completed", "model": *modelName,
			"output": []any{map[string]any{
				"type": "message", "id": "msg_mock", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "Hello from mockupstream (responses)", "annotations": []any{}}},
			}},
			"usage": map[string]any{
				"input_tokens": 13, "output_tokens": 4, "total_tokens": 17,
				"input_tokens_details": map[string]any{"cached_tokens": 3},
			},
		})
		return
	}
	sse(w,
		fmt.Sprintf(`event: response.created
data: {"type":"response.created","response":{"id":"resp_mock","model":%q}}`, *modelName),
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_mock","output_index":0,"content_index":0,"delta":"Hello"}`,
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_mock","output_index":0,"content_index":0,"delta":" from mockupstream"}`,
		fmt.Sprintf(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_mock","status":"completed","model":%q,"usage":{"input_tokens":13,"output_tokens":4,"total_tokens":17,"input_tokens_details":{"cached_tokens":3}}}}`, *modelName),
	)
}

// userBalance serves a DeepSeek-shaped balance body (usage probe target).
func userBalance(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"is_available": true,
		"balance_infos": []any{map[string]any{
			"currency": "CNY", "total_balance": "110.00",
			"granted_balance": "10.00", "touched_balance": "100.00",
		}},
	})
}

// planQuota serves a GLM coding-plan shaped quota body (usage probe target).
func planQuota(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"data": map[string]any{
			"planName": "GLM Coding Mock",
			"limits": []any{
				map[string]any{"type": "TOKENS_LIMIT", "window": map[string]any{"unit": "hour", "number": 5},
					"remaining": 71, "unlimited": false, "nextResetTime": time.Now().Add(3 * time.Hour).UnixMilli()},
				map[string]any{"type": "TOKENS_LIMIT", "window": map[string]any{"unit": "day", "number": 7},
					"remaining": 88, "unlimited": false, "nextResetTime": time.Now().Add(48 * time.Hour).UnixMilli()},
			},
		},
	})
}

func sse(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f := w.(http.Flusher)
	for _, frame := range frames {
		fmt.Fprintln(w, frame)
		fmt.Fprintln(w)
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	if err := enc.Encode(body); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
