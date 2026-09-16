// Package responses implements the OpenAI Responses API wire codec
// (POST /v1/responses): request/response conversion to the canonical IR, and
// the `response.*` event stream reader/renderer. It is a client-surface
// protocol only — upstream Responses endpoints are reached via byte
// passthrough, never via IR re-encoding (EncodeRequest exists for the Codec
// interface and tests).
//
// Stateless only: `store`, `previous_response_id`, `background`, and
// `conversation` are rejected before routing. Reasoning history items are
// accepted on decode and dropped (never replayed upstream).
package responses

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/PWZER/llm-switch/internal/ir"
)

// — wire types ------------------------------------------------------------

type wireRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"` // string | item array
	Instructions       json.RawMessage `json:"instructions"`
	Stream             bool            `json:"stream"`
	MaxOutputTokens    *int64          `json:"max_output_tokens"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	Tools              []wireTool      `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	Reasoning          *wireReasoning  `json:"reasoning"`
	Text               *wireText       `json:"text"`
	Store              *bool           `json:"store"`
	PreviousResponseID string          `json:"previous_response_id"`
	Background         bool            `json:"background"`
	Conversation       json.RawMessage `json:"conversation"`
	Include            json.RawMessage `json:"include"`  // ignored
	Metadata           json.RawMessage `json:"metadata"` // ignored
}

type wireReasoning struct {
	Effort  string          `json:"effort"`
	Summary json.RawMessage `json:"summary"` // ignored
}

type wireText struct {
	Format json.RawMessage `json:"format"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"` // dropped: not part of the IR
}

type wireInputItem struct {
	Type      string          `json:"type"` // message | function_call | function_call_output | reasoning | item_reference | ...
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"` // string | part array
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"` // function_call_output: string | part array
}

// — request: wire -> IR ---------------------------------------------------

// ValidateStateless rejects stateful Responses features with an explicit
// message. Called from the gateway before routing (the passthrough path never
// decodes the body) and again in DecodeRequest.
func ValidateStateless(body []byte) error {
	var probe struct {
		Store              *bool           `json:"store"`
		PreviousResponseID string          `json:"previous_response_id"`
		Background         *bool           `json:"background"`
		Conversation       json.RawMessage `json:"conversation"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil // not decodable here; DecodeRequest produces the real error
	}
	if probe.Store != nil && *probe.Store {
		return errors.New(`stateful Responses sessions are not supported: set "store": false`)
	}
	if probe.PreviousResponseID != "" {
		return errors.New(`previous_response_id is not supported: this gateway is stateless; send the full conversation in "input"`)
	}
	if probe.Background != nil && *probe.Background {
		return errors.New("background responses are not supported")
	}
	if len(probe.Conversation) > 0 && string(probe.Conversation) != "null" {
		return errors.New("conversation is not supported: this gateway is stateless")
	}
	return nil
}

// DecodeRequest parses a Responses API request body into the IR.
func DecodeRequest(body []byte) (*ir.Request, error) {
	if err := ValidateStateless(body); err != nil {
		return nil, err
	}
	var wire wireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid responses request: %w", err)
	}

	req := &ir.Request{
		Model:  wire.Model,
		Stream: wire.Stream,
	}
	if wire.MaxOutputTokens != nil {
		req.MaxTokens = *wire.MaxOutputTokens
	}
	req.Temperature = wire.Temperature
	req.TopP = wire.TopP

	if txt := flattenText(wire.Instructions); txt != "" {
		req.System = append(req.System, ir.Block{Type: ir.BlockText, Text: txt})
	}

	if len(wire.Input) > 0 {
		var s string
		if err := json.Unmarshal(wire.Input, &s); err == nil {
			if s != "" {
				req.Messages = append(req.Messages, ir.Message{
					Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockText, Text: s}},
				})
			}
		} else {
			var items []wireInputItem
			if err := json.Unmarshal(wire.Input, &items); err != nil {
				return nil, fmt.Errorf("invalid responses input: %w", err)
			}
			for _, item := range items {
				if err := decodeInputItem(req, item); err != nil {
					return nil, err
				}
			}
		}
	}

	for _, t := range wire.Tools {
		if t.Type != "function" {
			return nil, fmt.Errorf("unsupported tool type %q: only %q tools are supported", t.Type, "function")
		}
		req.Tools = append(req.Tools, ir.Tool{Name: t.Name, Description: t.Description, Schema: t.Parameters})
	}
	if len(wire.ToolChoice) > 0 {
		tc, err := decodeToolChoice(wire.ToolChoice)
		if err != nil {
			return nil, err
		}
		req.ToolChoice = tc
	}
	if wire.ParallelToolCalls != nil && !*wire.ParallelToolCalls {
		if req.ToolChoice == nil {
			req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
		}
		req.ToolChoice.DisableParallel = true
	}
	if wire.Reasoning != nil && wire.Reasoning.Effort != "" {
		if req.Thinking == nil {
			req.Thinking = &ir.Thinking{}
		}
		req.Thinking.Effort = wire.Reasoning.Effort
	}
	if wire.Text != nil && len(wire.Text.Format) > 0 {
		rf, err := decodeResponseFormat(wire.Text.Format)
		if err != nil {
			return nil, err
		}
		if rf != nil {
			req.ResponseFormat = rf
		}
	}
	return req, nil
}

func decodeInputItem(req *ir.Request, item wireInputItem) error {
	typ := item.Type
	if typ == "" && item.Role != "" {
		typ = "message" // role-only items are chat-style messages
	}
	switch typ {
	case "message":
		blocks := decodeContentParts(item.Content)
		switch item.Role {
		case "system", "developer":
			for _, b := range blocks {
				if b.Type == ir.BlockText {
					req.System = append(req.System, b)
				}
			}
		case "assistant":
			if len(blocks) > 0 {
				req.Messages = append(req.Messages, ir.Message{Role: ir.RoleAssistant, Blocks: blocks})
			}
		default: // user, or unspecified role
			if len(blocks) > 0 {
				req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Blocks: blocks})
			}
		}
	case "function_call":
		req.Messages = append(req.Messages, ir.Message{
			Role: ir.RoleAssistant,
			Blocks: []ir.Block{{
				Type: ir.BlockToolUse, ToolCallID: item.CallID, ToolName: item.Name,
				InputJSON: item.Arguments,
			}},
		})
	case "function_call_output":
		req.Messages = append(req.Messages, ir.Message{
			Role: ir.RoleUser,
			Blocks: []ir.Block{{
				Type: ir.BlockToolResult, ToolCallID: item.CallID,
				ResultBlocks: []ir.Block{{Type: ir.BlockText, Text: flattenText(item.Output)}},
			}},
		})
	case "reasoning":
		// History reasoning is never replayed upstream (unsigned thinking is
		// rejected by every upstream protocol this gateway bridges to).
	default:
		return fmt.Errorf("unsupported input item type %q", item.Type)
	}
	return nil
}

// decodeResponseFormat converts the flat Responses text.format shape into the
// nested chat `response_format` shape (json_schema differs between the two).
func decodeResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("invalid text.format: %w", err)
	}
	switch probe.Type {
	case "", "text":
		return nil, nil
	case "json_object":
		return json.RawMessage(`{"type":"json_object"}`), nil
	case "json_schema":
		var f struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("invalid text.format json_schema: %w", err)
		}
		inner := map[string]any{"name": f.Name, "schema": f.Schema}
		if f.Description != "" {
			inner["description"] = f.Description
		}
		if f.Strict != nil {
			inner["strict"] = *f.Strict
		}
		return mustJSON(map[string]any{"type": "json_schema", "json_schema": inner}), nil
	default:
		return nil, fmt.Errorf("unsupported text.format type %q", probe.Type)
	}
}

func decodeToolChoice(raw json.RawMessage) (*ir.ToolChoice, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &ir.ToolChoice{Mode: ir.ToolChoiceAuto}, nil
		case "none":
			return &ir.ToolChoice{Mode: ir.ToolChoiceNone}, nil
		case "required":
			return &ir.ToolChoice{Mode: ir.ToolChoiceRequired}, nil
		default:
			return nil, fmt.Errorf("unsupported tool_choice %q", s)
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Type == "function" && obj.Name != "" {
			return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: obj.Name}, nil
		}
		if obj.Type == "allowed_tools" {
			return nil, errors.New("tool_choice allowed_tools is not supported")
		}
	}
	return nil, fmt.Errorf("unsupported tool_choice: only \"auto\"/\"none\"/\"required\" and {\"type\":\"function\",\"name\":...} are supported")
}

// decodeContentParts flattens message content (string or part array) into IR
// blocks. Part types: input_text/output_text/refusal -> text, input_image -> image.
func decodeContentParts(content json.RawMessage) []ir.Block {
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
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Refusal  string          `json:"refusal"`
		ImageURL json.RawMessage `json:"image_url"` // string in Responses; object tolerated
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil
	}
	var blocks []ir.Block
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: p.Text})
		case "refusal":
			blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: p.Refusal})
		case "input_image":
			var url string
			if err := json.Unmarshal(p.ImageURL, &url); err == nil && url != "" {
				blocks = append(blocks, imageBlock(url))
			} else {
				var obj struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(p.ImageURL, &obj); err == nil && obj.URL != "" {
					blocks = append(blocks, imageBlock(obj.URL))
				}
			}
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

// flattenText renders content (string or part array) as plain text.
func flattenText(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var b strings.Builder
	for _, blk := range decodeContentParts(content) {
		if blk.Type == ir.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// — request: IR -> wire ---------------------------------------------------

type wireOutRequest struct {
	Model             string          `json:"model"`
	Input             []wireReqItem   `json:"input"`
	Instructions      string          `json:"instructions,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Store             bool            `json:"store"`
	MaxOutputTokens   *int64          `json:"max_output_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Tools             []wireTool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning         *wireReasoning  `json:"reasoning,omitempty"`
	Text              *wireText       `json:"text,omitempty"`
}

type wireReqItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
}

// EncodeRequest renders the IR as a Responses API body. Not used for routing
// (upstream Responses traffic is passed through byte-wise), but keeps the
// Codec interface symmetric and enables round-trip tests.
func EncodeRequest(req *ir.Request) ([]byte, error) {
	w := wireOutRequest{Model: req.Model, Stream: req.Stream}
	if req.MaxTokens > 0 {
		t := req.MaxTokens
		w.MaxOutputTokens = &t
	}
	w.Temperature = req.Temperature
	w.TopP = req.TopP
	if txt := systemText(req.System); txt != "" {
		w.Instructions = txt
	}
	for _, msg := range req.Messages {
		switch msg.Role {
		case ir.RoleAssistant:
			item := wireReqItem{Type: "message", Role: "assistant"}
			var text strings.Builder
			var calls []ir.Block
			for _, b := range msg.Blocks {
				switch b.Type {
				case ir.BlockText:
					text.WriteString(b.Text)
				case ir.BlockToolUse:
					calls = append(calls, b)
				}
			}
			if text.Len() > 0 {
				item.Content = mustJSON([]map[string]any{{"type": "output_text", "text": text.String(), "annotations": []any{}}})
			}
			w.Input = append(w.Input, item)
			for _, b := range calls {
				w.Input = append(w.Input, wireReqItem{
					Type: "function_call", CallID: b.ToolCallID, Name: b.ToolName,
					Arguments: orEmptyJSON(b.InputJSON),
				})
			}
		case ir.RoleUser:
			for _, b := range msg.Blocks {
				switch b.Type {
				case ir.BlockToolResult:
					w.Input = append(w.Input, wireReqItem{
						Type: "function_call_output", CallID: b.ToolCallID, Output: resultText(b),
					})
				}
			}
			var rest []ir.Block
			for _, b := range msg.Blocks {
				if b.Type != ir.BlockToolResult {
					rest = append(rest, b)
				}
			}
			if len(rest) > 0 {
				item := wireReqItem{Type: "message", Role: "user"}
				if onlyText(rest) {
					item.Content = mustJSON(concatText(rest))
				} else {
					var parts []map[string]any
					for _, b := range rest {
						switch b.Type {
						case ir.BlockText:
							parts = append(parts, map[string]any{"type": "input_text", "text": b.Text})
						case ir.BlockImage:
							parts = append(parts, map[string]any{"type": "input_image", "image_url": imageURL(b)})
						}
					}
					item.Content = mustJSON(parts)
				}
				w.Input = append(w.Input, item)
			}
		}
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Schema})
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Mode {
		case ir.ToolChoiceAuto:
			w.ToolChoice = json.RawMessage(`"auto"`)
		case ir.ToolChoiceNone:
			w.ToolChoice = json.RawMessage(`"none"`)
		case ir.ToolChoiceRequired:
			w.ToolChoice = json.RawMessage(`"required"`)
		case ir.ToolChoiceTool:
			w.ToolChoice = mustJSON(map[string]any{"type": "function", "name": req.ToolChoice.Name})
		}
		if req.ToolChoice.DisableParallel {
			f := false
			w.ParallelToolCalls = &f
		}
	}
	if req.Thinking != nil && req.Thinking.Effort != "" {
		w.Reasoning = &wireReasoning{Effort: req.Thinking.Effort}
	}
	if len(req.ResponseFormat) > 0 {
		w.Text = &wireText{Format: encodeTextFormat(req.ResponseFormat)}
	}
	return json.Marshal(w)
}

// encodeTextFormat converts the nested chat response_format shape back to the
// flat Responses text.format shape.
func encodeTextFormat(raw json.RawMessage) json.RawMessage {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Type != "json_schema" {
		return raw
	}
	var nested struct {
		JSONSchema struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return raw
	}
	out := map[string]any{"type": "json_schema", "name": nested.JSONSchema.Name, "schema": nested.JSONSchema.Schema}
	if nested.JSONSchema.Description != "" {
		out["description"] = nested.JSONSchema.Description
	}
	if nested.JSONSchema.Strict != nil {
		out["strict"] = *nested.JSONSchema.Strict
	}
	return mustJSON(out)
}

func imageURL(b ir.Block) string {
	if b.ImageIsURL {
		return b.ImageData
	}
	return fmt.Sprintf("data:%s;base64,%s", b.ImageMedia, b.ImageData)
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

func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
