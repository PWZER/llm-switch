package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PWZER/llm-switch/internal/ir"
)

func TestValidateStateless(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "clean", body: `{"model":"m","input":"hi","store":false}`},
		{name: "store", body: `{"model":"m","input":"hi","store":true}`, wantErr: `"store": false`},
		{name: "previous_response_id", body: `{"model":"m","input":"hi","previous_response_id":"resp_1"}`, wantErr: "previous_response_id"},
		{name: "background", body: `{"model":"m","input":"hi","background":true}`, wantErr: "background"},
		{name: "conversation", body: `{"model":"m","input":"hi","conversation":"conv_1"}`, wantErr: "conversation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStateless([]byte(tc.body))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want clean, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDecodeRequestStringInput(t *testing.T) {
	req, err := DecodeRequest([]byte(`{
		"model": "gpt-5", "input": "Hello there", "instructions": "Be brief",
		"max_output_tokens": 512, "temperature": 0.5, "stream": true
	}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Model != "gpt-5" || !req.Stream || req.MaxTokens != 512 {
		t.Fatalf("bad scalars: %+v", req)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Fatalf("bad temperature: %v", req.Temperature)
	}
	if len(req.System) != 1 || req.System[0].Text != "Be brief" {
		t.Fatalf("bad system: %+v", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser ||
		len(req.Messages[0].Blocks) != 1 || req.Messages[0].Blocks[0].Text != "Hello there" {
		t.Fatalf("bad messages: %+v", req.Messages)
	}
}

func TestDecodeRequestItems(t *testing.T) {
	body := `{
		"model": "gpt-5",
		"input": [
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev rules"}]},
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"look "},
				{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
			]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"prior reply"}]},
			{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call_output","call_id":"call_abc","output":"sunny"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}
		],
		"tools": [{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object"}}],
		"tool_choice": {"type":"function","name":"get_weather"},
		"parallel_tool_calls": false,
		"reasoning": {"effort":"high"}
	}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// developer -> System
	if len(req.System) != 1 || req.System[0].Text != "dev rules" {
		t.Fatalf("bad system: %+v", req.System)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("want 4 messages, got %d: %+v", len(req.Messages), req.Messages)
	}
	u, a := req.Messages[0], req.Messages[1]
	if u.Role != ir.RoleUser || len(u.Blocks) != 2 || u.Blocks[1].Type != ir.BlockImage {
		t.Fatalf("bad user message: %+v", u)
	}
	if a.Role != ir.RoleAssistant || a.Blocks[0].Text != "prior reply" {
		t.Fatalf("bad assistant message: %+v", a)
	}
	fc, fo := req.Messages[2], req.Messages[3]
	if fc.Blocks[0].Type != ir.BlockToolUse || fc.Blocks[0].ToolCallID != "call_abc" ||
		fc.Blocks[0].ToolName != "get_weather" {
		t.Fatalf("bad function_call: %+v", fc)
	}
	if fo.Blocks[0].Type != ir.BlockToolResult || fo.Blocks[0].ToolCallID != "call_abc" ||
		fo.Blocks[0].ResultBlocks[0].Text != "sunny" {
		t.Fatalf("bad function_call_output: %+v", fo)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("bad tools: %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ToolChoiceTool || req.ToolChoice.Name != "get_weather" {
		t.Fatalf("bad tool_choice: %+v", req.ToolChoice)
	}
	if !req.ToolChoice.DisableParallel {
		t.Fatalf("want DisableParallel")
	}
	if req.Thinking == nil || req.Thinking.Effort != "high" {
		t.Fatalf("bad thinking: %+v", req.Thinking)
	}
}

func TestDecodeRequestRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad tool type", `{"model":"m","input":"x","tools":[{"type":"web_search"}]}`},
		{"unknown item", `{"model":"m","input":[{"type":"item_reference","id":"x"}]}`},
		{"hosted tool_choice", `{"model":"m","input":"x","tool_choice":{"type":"allowed_tools"}}`},
		{"bad format", `{"model":"m","input":"x","text":{"format":{"type":"haiku"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeRequest([]byte(tc.body)); err == nil {
				t.Fatalf("want error")
			}
		})
	}
}

func TestDecodeRequestTextFormat(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","input":"x","text":{"format":{"type":"json_object"}}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(req.ResponseFormat) != `{"type":"json_object"}` {
		t.Fatalf("bad response_format: %s", req.ResponseFormat)
	}

	req, err = DecodeRequest([]byte(`{"model":"m","input":"x","text":{"format":{
		"type":"json_schema","name":"out","strict":true,"schema":{"type":"object"}}}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var rf struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string `json:"name"`
			Strict bool   `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(req.ResponseFormat, &rf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rf.Type != "json_schema" || rf.JSONSchema.Name != "out" || !rf.JSONSchema.Strict {
		t.Fatalf("bad nested json_schema: %s", req.ResponseFormat)
	}

	// type "text" -> no ResponseFormat
	req, err = DecodeRequest([]byte(`{"model":"m","input":"x","text":{"format":{"type":"text"}}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.ResponseFormat) != 0 {
		t.Fatalf("want empty response_format, got %s", req.ResponseFormat)
	}
}

func TestEncodeRequestRoundTrip(t *testing.T) {
	orig := &ir.Request{
		Model: "gpt-5", Stream: true, MaxTokens: 256,
		System: []ir.Block{{Type: ir.BlockText, Text: "be brief"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockText, Text: "weather?"}}},
			{Role: ir.RoleAssistant, Blocks: []ir.Block{{
				Type: ir.BlockToolUse, ToolCallID: "call_1", ToolName: "get_weather", InputJSON: `{"city":"SF"}`,
			}}},
			{Role: ir.RoleUser, Blocks: []ir.Block{{
				Type: ir.BlockToolResult, ToolCallID: "call_1",
				ResultBlocks: []ir.Block{{Type: ir.BlockText, Text: "sunny"}},
			}}},
			{Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockText, Text: "thanks"}}},
		},
		Tools:      []ir.Tool{{Name: "get_weather", Description: "w", Schema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceAuto, DisableParallel: true},
		Thinking:   &ir.Thinking{Effort: "low"},
	}
	body, err := EncodeRequest(orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("re-decode: %v (body=%s)", err, body)
	}
	if got.Model != orig.Model || !got.Stream || got.MaxTokens != 256 {
		t.Fatalf("scalar drift: %+v", got)
	}
	if len(got.System) != 1 || got.System[0].Text != "be brief" {
		t.Fatalf("system drift: %+v", got.System)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Fatalf("tools drift: %+v", got.Tools)
	}
	if got.Thinking == nil || got.Thinking.Effort != "low" {
		t.Fatalf("thinking drift: %+v", got.Thinking)
	}
	var hasToolUse, hasToolResult bool
	for _, m := range got.Messages {
		for _, b := range m.Blocks {
			if b.Type == ir.BlockToolUse && b.ToolCallID == "call_1" && b.ToolName == "get_weather" {
				hasToolUse = true
			}
			if b.Type == ir.BlockToolResult && b.ToolCallID == "call_1" && b.ResultBlocks[0].Text == "sunny" {
				hasToolResult = true
			}
		}
	}
	if !hasToolUse || !hasToolResult {
		t.Fatalf("tool round-trip lost (use=%v result=%v):\n%s", hasToolUse, hasToolResult, body)
	}
}

func TestDecodeResponse(t *testing.T) {
	body := `{
		"id": "resp_1", "object": "response", "status": "completed", "model": "gpt-5",
		"output": [
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}]},
			{"type":"message","id":"msg_1","role":"assistant",
			 "content":[{"type":"output_text","text":"Hi!","annotations":[]}]},
			{"type":"function_call","id":"fc_1","call_id":"call_z","name":"f","arguments":"{}"}
		],
		"usage": {"input_tokens":12,"output_tokens":6,"total_tokens":18,
			"input_tokens_details":{"cached_tokens":4},
			"output_tokens_details":{"reasoning_tokens":2}}
	}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "resp_1" || resp.Model != "gpt-5" {
		t.Fatalf("bad header: %+v", resp)
	}
	if len(resp.Blocks) != 3 ||
		resp.Blocks[0].Type != ir.BlockThinking || resp.Blocks[0].Text != "thinking" ||
		resp.Blocks[1].Type != ir.BlockText || resp.Blocks[1].Text != "Hi!" ||
		resp.Blocks[2].Type != ir.BlockToolUse || resp.Blocks[2].ToolCallID != "call_z" {
		t.Fatalf("bad blocks: %+v", resp.Blocks)
	}
	if resp.Usage.Input != 12 || resp.Usage.Output != 6 || resp.Usage.CacheRead != 4 || resp.Usage.Reasoning != 2 {
		t.Fatalf("bad usage: %+v", resp.Usage)
	}
}

func TestDecodeResponseIncomplete(t *testing.T) {
	resp, err := DecodeResponse([]byte(`{
		"id":"resp_2","object":"response","status":"incomplete","model":"m",
		"incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":null
	}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Stop != ir.StopMaxTokens {
		t.Fatalf("want StopMaxTokens, got %s", resp.Stop)
	}
}

func TestEncodeResponse(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "resp_x", Model: "m",
		Blocks: []ir.Block{
			{Type: ir.BlockThinking, Text: "hmm"},
			{Type: ir.BlockText, Text: "He"},
			{Type: ir.BlockText, Text: "llo"},
			{Type: ir.BlockToolUse, ToolCallID: "call_y", ToolName: "f", InputJSON: `{"a":1}`},
		},
		Stop:  ir.StopToolUse,
		Usage: ir.Usage{Input: 10, Output: 5, CacheRead: 3},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var wire struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Object string `json:"object"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Usage struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			TotalTokens        int64 `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if wire.ID != "resp_x" || wire.Status != "completed" || wire.Object != "response" {
		t.Fatalf("bad header: %s", out)
	}
	if len(wire.Output) != 3 ||
		wire.Output[0].Type != "reasoning" || wire.Output[0].Summary[0].Text != "hmm" ||
		wire.Output[1].Type != "message" || wire.Output[1].Content[0].Text != "Hello" ||
		wire.Output[2].Type != "function_call" || wire.Output[2].CallID != "call_y" ||
		wire.Output[2].Arguments != `{"a":1}` {
		t.Fatalf("bad output: %s", out)
	}
	if wire.Usage.InputTokens != 10 || wire.Usage.OutputTokens != 5 ||
		wire.Usage.TotalTokens != 15 || wire.Usage.InputTokensDetails.CachedTokens != 3 {
		t.Fatalf("bad usage: %s", out)
	}
}
