package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PWZER/llm-switch/internal/ir"
)

// — response: wire <-> IR -------------------------------------------------

type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u *wireUsage) toIR() ir.Usage {
	if u == nil {
		return ir.Usage{}
	}
	// IR invariant: Input is the total input count INCLUDING cache tokens
	// (OpenAI convention); Anthropic wire input_tokens excludes them.
	return ir.Usage{Input: u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		Output: u.OutputTokens,
		CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}
}

// anthropicInputTokens converts the canonical total-input count back to the
// Anthropic wire convention (cache tokens reported separately).
func anthropicInputTokens(u ir.Usage) int64 {
	if n := u.Input - u.CacheRead - u.CacheWrite; n > 0 {
		return n
	}
	return 0
}

type wireResponse struct {
	ID         string      `json:"id"`
	Model      string      `json:"model"`
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      *wireUsage  `json:"usage"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// DecodeResponse converts a complete Anthropic message body to the IR.
func DecodeResponse(body []byte) (*ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("invalid anthropic response: %w", err)
	}
	if w.Error != nil {
		return &ir.Response{ErrType: w.Error.Type, ErrText: w.Error.Message}, nil
	}
	resp := &ir.Response{ID: w.ID, Model: w.Model, Stop: mapStopReason(w.StopReason)}
	for _, b := range w.Content {
		switch b.Type {
		case "text":
			resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockText, Text: b.Text})
		case "thinking":
			resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockThinking, Text: b.Thinking, Signature: b.Signature})
		case "tool_use":
			resp.Blocks = append(resp.Blocks, ir.Block{
				Type: ir.BlockToolUse, ToolCallID: b.ID, ToolName: b.Name,
				InputJSON: string(b.Input),
			})
		}
	}
	resp.Usage = w.Usage.toIR()
	return resp, nil
}

func mapStopReason(s string) ir.StopReason {
	switch s {
	case "max_tokens":
		return ir.StopMaxTokens
	case "tool_use":
		return ir.StopToolUse
	case "stop_sequence":
		return ir.StopStopSequence
	case "refusal":
		return ir.StopContentFilter
	default: // "end_turn" and the rest
		return ir.StopEndTurn
	}
}

func mapStopToAnthropic(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens:
		return "max_tokens"
	case ir.StopToolUse:
		return "tool_use"
	case ir.StopStopSequence:
		return "stop_sequence"
	case ir.StopContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// EncodeResponse renders the IR as a complete Anthropic message body.
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	if resp.ErrText != "" {
		return json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": orDefault(resp.ErrType, "api_error"), "message": resp.ErrText},
		})
	}
	content := []any{}
	for _, b := range resp.Blocks {
		switch b.Type {
		case ir.BlockText:
			content = append(content, map[string]any{"type": "text", "text": b.Text})
		case ir.BlockThinking:
			blk := map[string]any{"type": "thinking", "thinking": b.Text}
			if b.Signature != "" {
				blk["signature"] = b.Signature
			}
			content = append(content, blk)
		case ir.BlockToolUse:
			var input any
			if err := json.Unmarshal([]byte(orEmptyJSON(b.InputJSON)), &input); err != nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": b.ToolCallID, "name": b.ToolName, "input": input,
			})
		}
	}
	if len(content) == 0 {
		// Anthropic responses always carry at least one content block.
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return json.Marshal(map[string]any{
		"id":          orDefault(resp.ID, "msg_lsw"),
		"type":        "message",
		"role":        "assistant",
		"model":       orDefault(resp.Model, "unknown"),
		"content":     content,
		"stop_reason": mapStopToAnthropic(resp.Stop),
		"usage": map[string]any{
			"input_tokens":                anthropicInputTokens(resp.Usage),
			"output_tokens":               resp.Usage.Output,
			"cache_read_input_tokens":     resp.Usage.CacheRead,
			"cache_creation_input_tokens": resp.Usage.CacheWrite,
		},
	})
}

// — stream reader: upstream Anthropic SSE -> IR events ---------------------

type sseFrame struct {
	Type    string `json:"type"`
	Message *struct {
		Model string     `json:"model"`
		Usage *wireUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *wireBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		StopReason  string `json:"stop_reason"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// StreamReader folds upstream Anthropic message SSE into canonical events.
// Feed one `data:` payload per call.
type StreamReader struct {
	started    bool
	seqTool    int
	finished   bool
	inputUsage wireUsage // raw wire fields; toIR applied at EvFinish
	emitted    []ir.Event
}

// NewStreamReader creates a reader for upstream Anthropic SSE.
func NewStreamReader() *StreamReader { return &StreamReader{} }

// Feed consumes one SSE data payload and returns any canonical events.
func (s *StreamReader) Feed(payload []byte) []ir.Event {
	s.emitted = s.emitted[:0]
	line := strings.TrimSpace(string(payload))
	if line == "" {
		return nil
	}
	var frame sseFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return nil // tolerate malformed frames
	}
	switch frame.Type {
	case "error":
		msg := "upstream error"
		if frame.Error != nil {
			msg = frame.Error.Message
		}
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvError, ErrText: msg})
	case "message_start":
		s.started = true
		ev := ir.Event{Kind: ir.EvStart}
		if frame.Message != nil {
			ev.Model = frame.Message.Model
			s.inputUsage = wireUsage{}
			if frame.Message.Usage != nil {
				s.inputUsage = *frame.Message.Usage
				s.inputUsage.OutputTokens = 0
			}
		}
		s.emitted = append(s.emitted, ev)
	case "content_block_start":
		if frame.ContentBlock != nil && frame.ContentBlock.Type == "tool_use" {
			s.emitted = append(s.emitted, ir.Event{
				Kind: ir.EvToolStart, ToolIndex: s.seqTool,
				ToolID: frame.ContentBlock.ID, ToolName: frame.ContentBlock.Name,
			})
			s.seqTool++
		}
	case "content_block_delta":
		if frame.Delta == nil {
			break
		}
		switch frame.Delta.Type {
		case "text_delta":
			if frame.Delta.Text != "" {
				s.emitted = append(s.emitted, ir.Event{Kind: ir.EvTextDelta, Text: frame.Delta.Text})
			}
		case "thinking_delta":
			if frame.Delta.Thinking != "" {
				s.emitted = append(s.emitted, ir.Event{Kind: ir.EvThinkDelta, Text: frame.Delta.Thinking})
			}
		case "input_json_delta":
			if frame.Delta.PartialJSON != "" {
				s.emitted = append(s.emitted, ir.Event{
					Kind: ir.EvToolDelta, ToolIndex: s.seqTool - 1, ArgsFragment: frame.Delta.PartialJSON,
				})
			}
		case "signature_delta":
			// No canonical representation; dropped.
		}
	case "message_delta":
		if s.finished {
			break
		}
		s.finished = true
		stop := ir.StopEndTurn
		usage := s.inputUsage
		if frame.Delta != nil && frame.Delta.StopReason != "" {
			stop = mapStopReason(frame.Delta.StopReason)
		}
		if frame.Usage != nil {
			usage.OutputTokens = frame.Usage.OutputTokens
			// Anthropic-compatible vendors sometimes repeat the full usage in
			// message_delta (and omit it from message_start) — merge non-zeros.
			if frame.Usage.InputTokens > 0 {
				usage.InputTokens = frame.Usage.InputTokens
			}
			if frame.Usage.CacheReadInputTokens > 0 {
				usage.CacheReadInputTokens = frame.Usage.CacheReadInputTokens
			}
			if frame.Usage.CacheCreationInputTokens > 0 {
				usage.CacheCreationInputTokens = frame.Usage.CacheCreationInputTokens
			}
		}
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvFinish, Stop: stop, Usage: usage.toIR()})
	case "message_stop":
		// Terminal framing; EvFinish already emitted on message_delta.
	case "ping":
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvPing})
	}
	return s.take()
}

// Finish is a no-op for Anthropic: message_stop terminates the stream and the
// finish event was already emitted by message_delta.
func (s *StreamReader) Finish() []ir.Event { return nil }

func (s *StreamReader) take() []ir.Event {
	if len(s.emitted) == 0 {
		return nil
	}
	out := make([]ir.Event, len(s.emitted))
	copy(out, s.emitted)
	return out
}

// — stream renderer: IR events -> client Anthropic SSE ---------------------

// Renderer renders canonical events as an Anthropic message SSE stream with
// lazy content-block starts and strict start/stop pairing.
type Renderer struct {
	started   bool
	nextIndex int    // next content_block index
	openKind  string // "" | "text" | "thinking" | "tool_use"
	finished  bool
	model     string
}

// NewRenderer creates a client-side Anthropic SSE renderer.
func NewRenderer(model string) *Renderer { return &Renderer{model: model} }

// NewRendererFor creates the renderer with a client-facing model name.
func NewRendererFor(model string) *Renderer { return &Renderer{model: model} }

// Start emits the message_start frame.
func (r *Renderer) Start() ([]byte, error) {
	return eventFrame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_lsw", "type": "message", "role": "assistant",
			"model":   orDefault(r.model, "unknown"),
			"content": []any{},
			"usage": map[string]any{
				"input_tokens": 0, "output_tokens": 0,
				"cache_read_input_tokens": 0, "cache_creation_input_tokens": 0,
			},
		},
	}), nil
}

// Frame renders one canonical event into SSE bytes (may be empty).
func (r *Renderer) Frame(ev ir.Event) ([]byte, error) {
	switch ev.Kind {
	case ir.EvStart:
		r.started = true // message_start already sent by Start()
		return nil, nil
	case ir.EvTextDelta:
		start, _ := r.openBlock("text")
		out := start
		return append(out, eventFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": r.nextIndex - 1,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})...), nil
	case ir.EvThinkDelta:
		start, _ := r.openBlock("thinking")
		out := start
		return append(out, eventFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": r.nextIndex - 1,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
		})...), nil
	case ir.EvToolStart:
		stop := r.closeOpenBlock()
		idx := r.nextIndex
		r.nextIndex++
		r.openKind = "tool_use"
		out := stop
		out = append(out, eventFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{
				"type": "tool_use", "id": ev.ToolID, "name": ev.ToolName, "input": map[string]any{},
			},
		})...)
		return out, nil
	case ir.EvToolDelta:
		return eventFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": r.nextIndex - 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ArgsFragment},
		}), nil
	case ir.EvPing:
		return eventFrame("ping", map[string]any{"type": "ping"}), nil
	case ir.EvFinish:
		out := r.closeOpenBlock()
		r.finished = true
		out = append(out, eventFrame("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": mapStopToAnthropic(ev.Stop), "stop_sequence": nil},
			"usage": map[string]any{
				"input_tokens":                anthropicInputTokens(ev.Usage),
				"output_tokens":               ev.Usage.Output,
				"cache_read_input_tokens":     ev.Usage.CacheRead,
				"cache_creation_input_tokens": ev.Usage.CacheWrite,
			},
		})...)
		// message_delta and message_stop are one logical terminal sequence.
		return append(out, eventFrame("message_stop", map[string]any{"type": "message_stop"})...), nil
	case ir.EvError:
		out := r.closeOpenBlock()
		r.finished = true
		out = append(out, eventFrame("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": ev.ErrText},
		})...)
		// Anthropic clients require message_stop even after an error.
		out = append(out, eventFrame("message_stop", map[string]any{"type": "message_stop"})...)
		return out, nil
	}
	return nil, nil
}

// Done emits the terminal message_stop if the stream ended without one.
func (r *Renderer) Done() []byte {
	if r.finished {
		return nil
	}
	r.finished = true
	out := r.closeOpenBlock()
	return append(out, eventFrame("message_stop", map[string]any{"type": "message_stop"})...)
}

// openBlock closes any open block and lazily starts one of the given kind.
// It returns the bytes for the (optional) stop + start frames.
func (r *Renderer) openBlock(kind string) ([]byte, bool) {
	if r.openKind == kind {
		return nil, true
	}
	stop := r.closeOpenBlock()
	idx := r.nextIndex
	r.nextIndex++
	r.openKind = kind
	block := map[string]any{"type": kind}
	if kind == "text" {
		block["text"] = ""
	} else {
		block["thinking"] = ""
	}
	return append(stop, eventFrame("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": block,
	})...), true
}

func (r *Renderer) closeOpenBlock() []byte {
	if r.openKind == "" {
		return nil
	}
	idx := r.nextIndex - 1
	r.openKind = ""
	return eventFrame("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": idx,
	})
}

func eventFrame(name string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out := make([]byte, 0, len(name)+len(b)+16)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, '\n', 'd', 'a', 't', 'a', ':', ' ')
	out = append(out, b...)
	out = append(out, '\n', '\n')
	return out
}
