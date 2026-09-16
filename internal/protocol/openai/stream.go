package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PWZER/llm-switch/internal/ir"
)

// — response: wire <-> IR -------------------------------------------------

type wireResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int        `json:"index"`
		Delta   *wireDelta `json:"delta,omitempty"`
		Message *struct {
			Role             string         `json:"role"`
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type wireDelta struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	PromptCacheHitTokens *int64 `json:"prompt_cache_hit_tokens"`
}

func (u *wireUsage) toIR() ir.Usage {
	out := ir.Usage{Input: u.PromptTokens, Output: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CacheRead = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.Reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	if u.PromptCacheHitTokens != nil {
		out.CacheRead = *u.PromptCacheHitTokens
	}
	return out
}

// mapFinishReason maps an OpenAI finish_reason to the canonical stop reason.
func mapFinishReason(s string) ir.StopReason {
	switch s {
	case "length":
		return ir.StopMaxTokens
	case "tool_calls", "function_call":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	default: // "stop" and anything unrecognized
		return ir.StopEndTurn
	}
}

// mapStopReason maps the canonical stop reason back to OpenAI finish_reason.
func mapStopReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens:
		return "length"
	case ir.StopToolUse:
		return "tool_calls"
	case ir.StopContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

// DecodeResponse converts a complete OpenAI response body to the IR.
func DecodeResponse(body []byte) (*ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("invalid openai response: %w", err)
	}
	if w.Error != nil {
		return &ir.Response{ErrType: w.Error.Type, ErrText: w.Error.Message}, nil
	}
	resp := &ir.Response{ID: w.ID, Model: w.Model}
	if len(w.Choices) > 0 {
		c := w.Choices[0]
		if c.Message != nil {
			if c.Message.ReasoningContent != "" {
				resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockThinking, Text: c.Message.ReasoningContent})
			}
			if c.Message.Content != "" {
				resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockText, Text: c.Message.Content})
			}
			for _, tc := range c.Message.ToolCalls {
				resp.Blocks = append(resp.Blocks, ir.Block{
					Type: ir.BlockToolUse, ToolCallID: tc.ID, ToolName: tc.Function.Name,
					InputJSON: tc.Function.Arguments,
				})
			}
		}
		if c.FinishReason != nil {
			resp.Stop = mapFinishReason(*c.FinishReason)
		}
	}
	if w.Usage != nil {
		resp.Usage = w.Usage.toIR()
	}
	return resp, nil
}

// EncodeResponse renders the IR as a complete OpenAI response body.
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	if resp.ErrText != "" {
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"message": resp.ErrText,
				"type":    orDefault(resp.ErrType, "api_error"),
			},
		})
	}

	var text, reasoning strings.Builder
	var toolCalls []map[string]any
	for _, b := range resp.Blocks {
		switch b.Type {
		case ir.BlockText:
			text.WriteString(b.Text)
		case ir.BlockThinking:
			reasoning.WriteString(b.Text)
		case ir.BlockToolUse:
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ToolCallID,
				"type": "function",
				"function": map[string]any{
					"name":      b.ToolName,
					"arguments": orEmptyJSON(b.InputJSON),
				},
			})
		}
	}
	content := any(nil)
	if text.Len() > 0 {
		content = text.String()
	}
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	body := map[string]any{
		"id":     orDefault(resp.ID, "chatcmpl-lsw"),
		"object": "chat.completion",
		"model":  orDefault(resp.Model, "unknown"),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": mapStopReason(resp.Stop),
		}},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.Input,
			"completion_tokens": resp.Usage.Output,
			"total_tokens":      resp.Usage.Input + resp.Usage.Output,
			"prompt_tokens_details":     map[string]any{"cached_tokens": resp.Usage.CacheRead},
			"completion_tokens_details": map[string]any{"reasoning_tokens": resp.Usage.Reasoning},
		},
	}
	return json.Marshal(body)
}

func orDefault(s, fb string) string {
	if s == "" {
		return fb
	}
	return s
}
