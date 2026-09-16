// Package anthropic implements the Anthropic-compatible wire codec: messages
// API request/response conversion to the canonical IR, and the message SSE
// reader/renderer (message_start / content_block_* / message_delta / stop).
package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PWZER/llm-switch/internal/ir"
)

// DefaultMaxTokens is injected when a request crosses into an Anthropic
// upstream without max_tokens (which Anthropic requires).
const DefaultMaxTokens = 8192

// — wire types ------------------------------------------------------------

type wireBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// image
	Source *struct {
		Type      string `json:"type"` // "base64" | "url"
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitempty"`
	// tool_use
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// thinking
	Thinking string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content json.RawMessage `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireThinking struct {
	Type         string `json:"type"` // "enabled" | "disabled"
	BudgetTokens int64  `json:"budget_tokens,omitempty"`
}

type wireRequest struct {
	Model         string        `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []wireMessage `json:"messages"`
	MaxTokens     int64         `json:"max_tokens"`
	Stream        bool          `json:"stream,omitempty"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Tools         []wireTool    `json:"tools,omitempty"`
	ToolChoice    *struct {
		Type                string `json:"type"` // auto|any|tool|none
		Name                string `json:"name,omitempty"`
		DisableParallelToolUse *bool `json:"disable_parallel_tool_use,omitempty"`
	} `json:"tool_choice,omitempty"`
	Thinking *wireThinking `json:"thinking,omitempty"`
}

// — request: wire -> IR ---------------------------------------------------

// DecodeRequest parses an Anthropic /v1/messages body into the IR.
func DecodeRequest(body []byte) (*ir.Request, error) {
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("invalid anthropic request: %w", err)
	}
	req := &ir.Request{
		Model:   w.Model,
		Stream:  w.Stream,
		MaxTokens: w.MaxTokens,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		StopSequences: w.StopSequences,
	}
	req.System = decodeSystem(w.System)
	for _, t := range w.Tools {
		req.Tools = append(req.Tools, ir.Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	if w.ToolChoice != nil {
		tc := &ir.ToolChoice{DisableParallel: w.ToolChoice.DisableParallelToolUse != nil && *w.ToolChoice.DisableParallelToolUse}
		switch w.ToolChoice.Type {
		case "auto":
			tc.Mode = ir.ToolChoiceAuto
		case "none":
			tc.Mode = ir.ToolChoiceNone
		case "any":
			tc.Mode = ir.ToolChoiceRequired
		case "tool":
			tc.Mode = ir.ToolChoiceTool
			tc.Name = w.ToolChoice.Name
		}
		req.ToolChoice = tc
	}
	if w.Thinking != nil && w.Thinking.Type != "" {
		req.Thinking = &ir.Thinking{Type: w.Thinking.Type, BudgetTokens: w.Thinking.BudgetTokens}
	}
	for _, m := range w.Messages {
		blocks := decodeBlocks(m.Content)
		role := ir.RoleUser
		if m.Role == "assistant" {
			role = ir.RoleAssistant
		}
		req.Messages = append(req.Messages, ir.Message{Role: role, Blocks: blocks})
	}
	return req, nil
}

func decodeSystem(raw json.RawMessage) []ir.Block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ir.Block{{Type: ir.BlockText, Text: s}}
	}
	return decodeBlocks(raw)
}

func decodeBlocks(raw json.RawMessage) []ir.Block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ir.Block{{Type: ir.BlockText, Text: s}}
	}
	var wire []wireBlock
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil
	}
	var out []ir.Block
	for _, wb := range wire {
		switch wb.Type {
		case "text":
			out = append(out, ir.Block{Type: ir.BlockText, Text: wb.Text})
		case "image":
			if wb.Source != nil {
				if wb.Source.Type == "url" {
					out = append(out, ir.Block{Type: ir.BlockImage, ImageIsURL: true, ImageData: wb.Source.URL, ImageMedia: wb.Source.MediaType})
				} else {
					out = append(out, ir.Block{Type: ir.BlockImage, ImageMedia: wb.Source.MediaType, ImageData: wb.Source.Data})
				}
			}
		case "thinking":
			out = append(out, ir.Block{Type: ir.BlockThinking, Text: wb.Thinking, Signature: wb.Signature})
		case "tool_use":
			out = append(out, ir.Block{
				Type: ir.BlockToolUse, ToolCallID: wb.ID, ToolName: wb.Name,
				InputJSON: string(wb.Input),
			})
		case "tool_result":
			var res []ir.Block
			if len(wb.Content) > 0 {
				var s string
				if err := json.Unmarshal(wb.Content, &s); err == nil {
					res = []ir.Block{{Type: ir.BlockText, Text: s}}
				} else {
					res = decodeBlocks(wb.Content)
				}
			}
			out = append(out, ir.Block{
				Type: ir.BlockToolResult, ToolCallID: wb.ToolUseID,
				ResultBlocks: res, IsError: wb.IsError,
			})
		}
	}
	return out
}

// — request: IR -> wire ---------------------------------------------------

// EncodeRequest renders the IR as an Anthropic messages body, applying the
// max_tokens default (Anthropic requires it) and temperature clamp (<=1).
func EncodeRequest(req *ir.Request) ([]byte, error) {
	w := wireRequest{Model: req.Model, Stream: req.Stream}
	w.MaxTokens = req.MaxTokens
	if w.MaxTokens <= 0 {
		w.MaxTokens = DefaultMaxTokens
	}
	if req.Temperature != nil {
		t := *req.Temperature
		if t > 1 {
			t = 1
		}
		w.Temperature = &t
	}
	w.TopP = req.TopP
	w.StopSequences = req.StopSequences

	if len(req.System) > 0 {
		w.System = json.RawMessage(mustJSON(blocksToWire(req.System)))
	}

	// Anthropic requires strict user/assistant alternation; consecutive
	// same-role messages are merged.
	for _, msg := range req.Messages {
		wireMsgs := messageToWire(msg)
		for _, wm := range wireMsgs {
			if n := len(w.Messages); n > 0 && w.Messages[n-1].Role == wm.Role {
				prev := decodeBlocks(w.Messages[n-1].Content)
				w.Messages[n-1].Content = json.RawMessage(mustJSON(append(prev, decodeBlocks(wm.Content)...)))
				continue
			}
			w.Messages = append(w.Messages, wm)
		}
	}

	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	if req.ToolChoice != nil {
		tc := struct {
			Type                   string `json:"type"`
			Name                   string `json:"name,omitempty"`
			DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
		}{}
		switch req.ToolChoice.Mode {
		case ir.ToolChoiceAuto:
			tc.Type = "auto"
		case ir.ToolChoiceNone:
			tc.Type = "none"
		case ir.ToolChoiceRequired:
			tc.Type = "any"
		case ir.ToolChoiceTool:
			tc.Type = "tool"
			tc.Name = req.ToolChoice.Name
		}
		if req.ToolChoice.DisableParallel {
			f := false
			tc.DisableParallelToolUse = &f
		}
		w.ToolChoice = &tc
	}
	if req.Thinking != nil && req.Thinking.Type != "" {
		wt := wireThinking{Type: req.Thinking.Type, BudgetTokens: req.Thinking.BudgetTokens}
		if req.Thinking.BudgetTokens > 0 {
			wt.Type = "enabled"
		}
		w.Thinking = &wt
	}
	return json.Marshal(w)
}

// messageToWire converts one canonical message into one or two wire messages:
// an assistant turn with tool_use blocks stays one message; a user turn whose
// tool_result blocks and text interleave keeps them in one content array.
func messageToWire(msg ir.Message) []wireMessage {
	role := "user"
	if msg.Role == ir.RoleAssistant {
		role = "assistant"
	}
	var blocks []wireBlock
	for _, b := range msg.Blocks {
		switch b.Type {
		case ir.BlockText:
			if b.Text != "" {
				blocks = append(blocks, wireBlock{Type: "text", Text: b.Text})
			}
		case ir.BlockImage:
			wb := wireBlock{Type: "image", Source: &struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type,omitempty"`
				Data      string `json:"data,omitempty"`
				URL       string `json:"url,omitempty"`
			}{}}
			if b.ImageIsURL {
				wb.Source.Type = "url"
				wb.Source.URL = b.ImageData
			} else {
				wb.Source.Type = "base64"
				wb.Source.MediaType = b.ImageMedia
				wb.Source.Data = b.ImageData
			}
			blocks = append(blocks, wb)
		case ir.BlockThinking:
			// Only replay signed thinking blocks (Anthropic validates the
			// signature); synthesized reasoning must never reach upstream.
			if b.Signature != "" {
				blocks = append(blocks, wireBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
			}
		case ir.BlockToolUse:
			blocks = append(blocks, wireBlock{
				Type: "tool_use", ID: b.ToolCallID, Name: b.ToolName,
				Input: json.RawMessage(orEmptyJSON(b.InputJSON)),
			})
		case ir.BlockToolResult:
			content := any("")
			if len(b.ResultBlocks) > 0 {
				if onlyText(b.ResultBlocks) {
					content = concatText(b.ResultBlocks)
				} else {
					content = blocksToWire(b.ResultBlocks)
				}
			} else {
				content = b.Text
			}
			blocks = append(blocks, wireBlock{
				Type: "tool_result", ToolUseID: b.ToolCallID,
				Content: json.RawMessage(mustJSON(content)), IsError: b.IsError,
			})
		}
	}
	if len(blocks) == 0 {
		// Anthropic rejects empty messages; a placeholder keeps alternation.
		blocks = []wireBlock{{Type: "text", Text: "(empty)"}}
	}
	return []wireMessage{{Role: role, Content: json.RawMessage(mustJSON(blocks))}}
}

func blocksToWire(blocks []ir.Block) []wireBlock {
	var out []wireBlock
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			out = append(out, wireBlock{Type: "text", Text: b.Text})
		case ir.BlockThinking:
			if b.Signature != "" {
				out = append(out, wireBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
			}
		case ir.BlockToolUse:
			out = append(out, wireBlock{
				Type: "tool_use", ID: b.ToolCallID, Name: b.ToolName,
				Input: json.RawMessage(orEmptyJSON(b.InputJSON)),
			})
		case ir.BlockToolResult:
			out = append(out, wireBlock{
				Type: "tool_result", ToolUseID: b.ToolCallID,
				Content: json.RawMessage(mustJSON(resultText(b))), IsError: b.IsError,
			})
		case ir.BlockImage:
			if b.ImageIsURL {
				out = append(out, wireBlock{Type: "image", Source: imageSource("url", b.ImageMedia, b.ImageData, b.ImageData)})
			} else {
				out = append(out, wireBlock{Type: "image", Source: imageSource("base64", b.ImageMedia, b.ImageData, "")})
			}
		}
	}
	return out
}

func imageSource(kind, media, data, url string) *struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
} {
	return &struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	}{Type: kind, MediaType: media, Data: data, URL: url}
}

func onlyText(blocks []ir.Block) bool {
	for _, b := range blocks {
		if b.Type != ir.BlockText {
			return false
		}
	}
	return true
}

func concatText(blocks []ir.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}

func resultText(b ir.Block) string {
	if len(b.ResultBlocks) > 0 {
		return concatText(b.ResultBlocks)
	}
	return b.Text
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

func orDefault(s, fb string) string {
	if s == "" {
		return fb
	}
	return s
}
