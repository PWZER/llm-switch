// Package openai implements the OpenAI-compatible wire codec: chat
// completions request/response conversion to the canonical IR, and the
// chat.completion.chunk SSE reader/renderer.
package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PWZER/llm-switch/internal/ir"
)

// — wire types ------------------------------------------------------------

type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	// ReasoningContent carries DeepSeek/GLM-style reasoning in responses.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type wireToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type wireRequest struct {
	Model          string        `json:"model"`
	Messages       []wireMessage `json:"messages"`
	Stream         bool          `json:"stream,omitempty"`
	StreamOptions  *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	MaxTokens     *int64            `json:"max_tokens,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	Stop          []string          `json:"stop,omitempty"`
	Tools         []wireTool        `json:"tools,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
	Thinking      *wireThinking     `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
}

type wireThinking struct {
	Type string `json:"type"`
}

// — request: wire -> IR ---------------------------------------------------

// DecodeRequest parses an OpenAI chat.completions request body into the IR.
func DecodeRequest(body []byte) (*ir.Request, error) {
	var wire wireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid openai request: %w", err)
	}

	req := &ir.Request{
		Model:  wire.Model,
		Stream: wire.Stream,
	}
	if wire.MaxTokens != nil {
		req.MaxTokens = *wire.MaxTokens
	}
	req.Temperature = wire.Temperature
	req.TopP = wire.TopP
	req.StopSequences = wire.Stop

	for _, wt := range wire.Tools {
		req.Tools = append(req.Tools, ir.Tool{
			Name: wt.Function.Name, Description: wt.Function.Description, Schema: wt.Function.Parameters,
		})
	}
	req.ToolChoice = decodeToolChoice(wire.ToolChoice)
	if wire.Thinking != nil && wire.Thinking.Type != "" {
		req.Thinking = &ir.Thinking{Type: wire.Thinking.Type}
	}
	if wire.ReasoningEffort != "" {
		if req.Thinking == nil {
			req.Thinking = &ir.Thinking{}
		}
		req.Thinking.Effort = wire.ReasoningEffort
	}

	for _, m := range wire.Messages {
		switch m.Role {
		case "system", "developer":
			text := contentText(m.Content)
			if text != "" {
				req.System = append(req.System, ir.Block{Type: ir.BlockText, Text: text})
			}
		case "assistant":
			blocks := decodeContentBlocks(m.Content)
			if m.ReasoningContent != "" {
				// History reasoning from a previous cross-protocol turn is
				// dropped on decode: OpenAI upstreams reject it on replay.
				_ = m.ReasoningContent
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, ir.Block{
					Type: ir.BlockToolUse, ToolCallID: tc.ID, ToolName: tc.Function.Name,
					InputJSON: tc.Function.Arguments,
				})
			}
			if len(blocks) > 0 {
				req.Messages = append(req.Messages, ir.Message{Role: ir.RoleAssistant, Blocks: blocks})
			}
		case "tool":
			req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Blocks: []ir.Block{{
				Type: ir.BlockToolResult, ToolCallID: m.ToolCallID,
				ResultBlocks: []ir.Block{{Type: ir.BlockText, Text: contentText(m.Content)}},
			}}})
		case "user":
			req.Messages = append(req.Messages, ir.Message{
				Role: ir.RoleUser, Blocks: decodeContentBlocks(m.Content),
			})
		}
	}
	return req, nil
}

func decodeToolChoice(raw json.RawMessage) *ir.ToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
		case "none":
			return &ir.ToolChoice{Mode: ir.ToolChoiceNone}
		case "required":
			return &ir.ToolChoice{Mode: ir.ToolChoiceRequired}
		}
		return nil
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: obj.Function.Name}
	}
	return nil
}

// contentText flattens a content field (string or part array) to text.
func contentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func decodeContentBlocks(content json.RawMessage) []ir.Block {
	if len(content) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ir.Block{{Type: ir.BlockText, Text: s}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil
	}
	var blocks []ir.Block
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: p.Text})
		case "image_url":
			blocks = append(blocks, imageBlock(p.ImageURL.URL))
		}
	}
	return blocks
}

func imageBlock(url string) ir.Block {
	const dataPrefix = "data:"
	if strings.HasPrefix(url, dataPrefix) {
		rest := url[len(dataPrefix):]
		media := "image/png"
		if idx := strings.Index(rest, ";base64,"); idx >= 0 {
			media = rest[:idx]
			rest = rest[idx+len(";base64,"):]
		}
		return ir.Block{Type: ir.BlockImage, ImageMedia: media, ImageData: rest}
	}
	return ir.Block{Type: ir.BlockImage, ImageIsURL: true, ImageData: url, ImageMedia: "image/*"}
}

// — request: IR -> wire ---------------------------------------------------

// EncodeRequest renders the IR as an OpenAI chat.completions body. When
// stream is true the gateway asks for usage in the final chunk.
func EncodeRequest(req *ir.Request) ([]byte, error) {
	w := wireRequest{
		Model: req.Model,
		Stream: req.Stream,
	}
	if req.Stream {
		w.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	if req.MaxTokens > 0 {
		t := req.MaxTokens
		w.MaxTokens = &t
	}
	w.Temperature = req.Temperature
	w.TopP = req.TopP
	w.Stop = req.StopSequences

	if len(req.System) > 0 {
		w.Messages = append(w.Messages, wireMessage{
			Role: "system", Content: json.RawMessage(mustJSON(systemText(req.System))),
		})
	}

	var pendingToolText []string // tool results waiting to be emitted as role:"tool" messages
	flushToolResults := func() {
		for _, text := range pendingToolText {
			// Split per tool result: each carries its own tool_call_id.
			id, content := parseToolResultText(text)
			w.Messages = append(w.Messages, wireMessage{
				Role: "tool", ToolCallID: id, Content: json.RawMessage(mustJSON(content)),
			})
		}
		pendingToolText = nil
	}

	for _, msg := range req.Messages {
		// OpenAI requires role:"tool" messages to immediately follow the
		// assistant tool_calls turn; keep them consecutive.
		if msg.Role == ir.RoleUser && hasToolResults(msg) {
			for _, b := range msg.Blocks {
				if b.Type == ir.BlockToolResult {
					pendingToolText = append(pendingToolText, b.ToolCallID+"\x00"+resultText(b))
				}
			}
			continue
		}
		flushToolResults()

		switch msg.Role {
		case ir.RoleAssistant:
			wm := wireMessage{Role: "assistant"}
			var text strings.Builder
			for _, b := range msg.Blocks {
				switch b.Type {
				case ir.BlockText:
					text.WriteString(b.Text)
				case ir.BlockThinking:
					// Never replay reasoning to OpenAI upstreams (DeepSeek rejects it).
				}
			}
			if text.Len() > 0 {
				wm.Content = json.RawMessage(mustJSON(text.String()))
			}
			for _, b := range msg.Blocks {
				if b.Type == ir.BlockToolUse {
					wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
						ID: b.ToolCallID, Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: b.ToolName, Arguments: orEmptyJSON(b.InputJSON)},
					})
				}
			}
			w.Messages = append(w.Messages, wm)
		case ir.RoleUser:
			wm := wireMessage{Role: "user"}
			if onlyText(msg.Blocks) {
				wm.Content = json.RawMessage(mustJSON(concatText(msg.Blocks)))
			} else {
				var parts []map[string]any
				for _, b := range msg.Blocks {
					switch b.Type {
					case ir.BlockText:
						parts = append(parts, map[string]any{"type": "text", "text": b.Text})
					case ir.BlockImage:
						if b.ImageIsURL {
							parts = append(parts, map[string]any{
								"type": "image_url",
								"image_url": map[string]string{"url": b.ImageData},
							})
						} else {
							parts = append(parts, map[string]any{
								"type": "image_url",
								"image_url": map[string]string{
									"url": fmt.Sprintf("data:%s;base64,%s", b.ImageMedia, b.ImageData),
								},
							})
						}
					case ir.BlockToolResult:
						parts = append(parts, map[string]any{"type": "text", "text": resultText(b)})
					}
				}
				wm.Content = json.RawMessage(mustJSON(parts))
			}
			w.Messages = append(w.Messages, wm)
		}
	}
	flushToolResults()

	for _, t := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.Schema
		w.Tools = append(w.Tools, wt)
	}
	if req.ToolChoice != nil {
		w.ToolChoice = encodeToolChoice(req.ToolChoice)
	}
	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "enabled", "disabled":
			w.Thinking = &wireThinking{Type: req.Thinking.Type}
		}
		if req.Thinking.Effort != "" {
			w.ReasoningEffort = req.Thinking.Effort
		}
	}
	return json.Marshal(w)
}

func encodeToolChoice(tc *ir.ToolChoice) json.RawMessage {
	switch tc.Mode {
	case ir.ToolChoiceAuto:
		return json.RawMessage(`"auto"`)
	case ir.ToolChoiceNone:
		return json.RawMessage(`"none"`)
	case ir.ToolChoiceRequired:
		return json.RawMessage(`"required"`)
	case ir.ToolChoiceTool:
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		})
		return b
	}
	return nil
}

func systemText(blocks []ir.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == ir.BlockText {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func onlyText(blocks []ir.Block) bool {
	for _, b := range blocks {
		if b.Type != ir.BlockText {
			return false
		}
	}
	return true
}

func hasToolResults(m ir.Message) bool {
	for _, b := range m.Blocks {
		if b.Type == ir.BlockToolResult {
			return true
		}
	}
	return false
}

func concatText(blocks []ir.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}

func resultText(b ir.Block) string {
	var out strings.Builder
	if len(b.ResultBlocks) > 0 {
		for _, rb := range b.ResultBlocks {
			if rb.Type == ir.BlockText {
				out.WriteString(rb.Text)
			}
		}
		return out.String()
	}
	return b.Text
}

// parseToolResultText splits the "\x00"-joined id/content marker.
func parseToolResultText(s string) (id, content string) {
	if idx := strings.Index(s, "\x00"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return "", s
}

func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}
