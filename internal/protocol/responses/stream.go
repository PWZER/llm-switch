package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/PWZER/llm-switch/internal/ir"
)

// — response: wire <-> IR -------------------------------------------------

type wireResponse struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"` // "response"
	CreatedAt         int64         `json:"created_at"`
	Status            string        `json:"status"` // completed | incomplete | failed | in_progress | ...
	Model             string        `json:"model"`
	Output            []wireOutItem `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// wireOutItem covers every output item type the IR can express: message,
// function_call, and reasoning (summary). Hosted-tool items never appear here.
type wireOutItem struct {
	Type      string        `json:"type"` // message | function_call | reasoning
	ID        string        `json:"id,omitempty"`
	Role      string        `json:"role,omitempty"`
	Status    string        `json:"status,omitempty"`
	Content   []wireContent `json:"content,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	Name      string        `json:"name,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
	Summary   []wireContent `json:"summary,omitempty"`
}

type wireContent struct {
	Type        string `json:"type"` // output_text | summary_text
	Text        string `json:"text"`
	Annotations []any  `json:"annotations,omitempty"`
}

type wireUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *wireUsage) toIR() ir.Usage {
	out := ir.Usage{Input: u.InputTokens, Output: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CacheRead = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.Reasoning = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

func usageToWire(u ir.Usage) map[string]any {
	return map[string]any{
		"input_tokens":          u.Input,
		"output_tokens":         u.Output,
		"total_tokens":          u.Input + u.Output,
		"input_tokens_details":  map[string]any{"cached_tokens": u.CacheRead},
		"output_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
	}
}

// DecodeResponse converts a complete Responses API body to the IR.
func DecodeResponse(body []byte) (*ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("invalid responses response: %w", err)
	}
	if w.Error != nil && w.Error.Message != "" {
		return &ir.Response{ErrType: "api_error", ErrText: w.Error.Message}, nil
	}
	resp := &ir.Response{ID: w.ID, Model: w.Model}
	for _, item := range w.Output {
		switch item.Type {
		case "message":
			var text strings.Builder
			for _, c := range item.Content {
				if c.Type == "output_text" {
					text.WriteString(c.Text)
				}
			}
			if text.Len() > 0 {
				resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockText, Text: text.String()})
			}
		case "reasoning":
			var text strings.Builder
			for _, c := range item.Summary {
				if c.Type == "summary_text" {
					text.WriteString(c.Text)
				}
			}
			if text.Len() > 0 {
				resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockThinking, Text: text.String()})
			}
		case "function_call":
			resp.Blocks = append(resp.Blocks, ir.Block{
				Type: ir.BlockToolUse, ToolCallID: item.CallID, ToolName: item.Name,
				InputJSON: item.Arguments,
			})
		}
	}
	if w.Status == "incomplete" {
		reason := ""
		if w.IncompleteDetails != nil {
			reason = w.IncompleteDetails.Reason
		}
		resp.Stop = mapIncompleteReason(reason)
	} else {
		resp.Stop = ir.StopEndTurn
	}
	if w.Usage != nil {
		resp.Usage = w.Usage.toIR()
	}
	return resp, nil
}

func mapIncompleteReason(reason string) ir.StopReason {
	if reason == "max_output_tokens" {
		return ir.StopMaxTokens
	}
	return ir.StopEndTurn
}

// EncodeResponse renders the IR as a complete Responses API body. Output items
// preserve block order; contiguous text/thinking blocks merge into one item.
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	if resp.ErrText != "" {
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"code":    orString(resp.ErrType, "api_error"),
				"message": resp.ErrText,
			},
		})
	}

	id := orString(resp.ID, "resp_lsw")
	out := map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"model":               orString(resp.Model, "unknown"),
		"status":              "completed",
		"output":              outputItems(resp.Blocks),
		"usage":               usageToWire(resp.Usage),
		"parallel_tool_calls": true,
		"tool_choice":         "none",
		"tools":               []any{},
	}
	if resp.Stop == ir.StopMaxTokens {
		out["status"] = "incomplete"
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return json.Marshal(out)
}

// outputItems folds IR blocks into Responses output items: a run of thinking
// blocks becomes one reasoning item, a run of text blocks one message item,
// and every tool_use its own function_call item.
func outputItems(blocks []ir.Block) []wireOutItem {
	items := []wireOutItem{}
	seq := 0
	nextID := func(prefix string) string {
		seq++
		return fmt.Sprintf("%s_lsw_%d", prefix, seq)
	}
	for i := 0; i < len(blocks); {
		switch blocks[i].Type {
		case ir.BlockThinking:
			var text strings.Builder
			for ; i < len(blocks) && blocks[i].Type == ir.BlockThinking; i++ {
				text.WriteString(blocks[i].Text)
			}
			items = append(items, wireOutItem{
				Type: "reasoning", ID: nextID("rs"),
				Summary: []wireContent{{Type: "summary_text", Text: text.String()}},
			})
		case ir.BlockText:
			var text strings.Builder
			for ; i < len(blocks) && blocks[i].Type == ir.BlockText; i++ {
				text.WriteString(blocks[i].Text)
			}
			items = append(items, wireOutItem{
				Type: "message", ID: nextID("msg"), Role: "assistant", Status: "completed",
				Content: []wireContent{{Type: "output_text", Text: text.String(), Annotations: []any{}}},
			})
		case ir.BlockToolUse:
			items = append(items, wireOutItem{
				Type: "function_call", ID: nextID("fc"), Status: "completed",
				CallID: blocks[i].ToolCallID, Name: blocks[i].ToolName,
				Arguments: orEmptyJSON(blocks[i].InputJSON),
			})
			i++
		default:
			i++
		}
	}
	return items
}

func orString(s, fb string) string {
	if s == "" {
		return fb
	}
	return s
}
