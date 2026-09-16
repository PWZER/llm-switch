package responses

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/PWZER/llm-switch/internal/ir"
)

// StreamReader folds upstream Responses SSE payloads into canonical IR events.
// The relay strips `event:` lines before Feed, so frames are keyed off the
// JSON `type` field (every Responses event payload carries one).
type StreamReader struct {
	started         bool
	finished        bool
	failed          bool
	sawFunctionCall bool
	finish          *ir.StopReason
	usage           ir.Usage

	seqTool  int            // sequential tool index allocation
	itemIdx  map[string]int // item_id -> ToolIndex
	outIdx   map[int]int    // output_index -> ToolIndex
	argsSeen map[int]bool   // ToolIndex -> arguments streamed via deltas

	emitted []ir.Event
}

// NewStreamReader creates a reader for upstream Responses SSE.
func NewStreamReader() *StreamReader {
	return &StreamReader{
		itemIdx:  map[string]int{},
		outIdx:   map[int]int{},
		argsSeen: map[int]bool{},
	}
}

type wireFrame struct {
	Type        string        `json:"type"`
	Response    *wireResponse `json:"response"`
	Item        *wireOutItem  `json:"item"`
	ItemID      string        `json:"item_id"`
	OutputIndex *int          `json:"output_index"`
	Delta       string        `json:"delta"`
	Message     string        `json:"message"`
}

// Feed consumes one `data:` payload and returns the canonical events it
// produced. Unknown event types are ignored.
func (s *StreamReader) Feed(payload []byte) []ir.Event {
	s.emitted = s.emitted[:0]
	line := strings.TrimSpace(string(payload))
	if line == "" || line == "[DONE]" {
		return nil
	}
	var f wireFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		return nil // tolerate malformed frames
	}

	switch f.Type {
	case "response.created", "response.in_progress", "response.queued":
		if !s.started {
			s.started = true
			model := ""
			if f.Response != nil {
				model = f.Response.Model
			}
			s.emitted = append(s.emitted, ir.Event{Kind: ir.EvStart, Model: model})
		}
	case "response.output_item.added":
		if f.Item != nil && f.Item.Type == "function_call" {
			idx := s.seqTool
			s.seqTool++
			if f.Item.ID != "" {
				s.itemIdx[f.Item.ID] = idx
			}
			if f.OutputIndex != nil {
				s.outIdx[*f.OutputIndex] = idx
			}
			s.sawFunctionCall = true
			s.emitted = append(s.emitted, ir.Event{
				Kind: ir.EvToolStart, ToolIndex: idx,
				ToolID: f.Item.CallID, ToolName: f.Item.Name,
			})
		}
	case "response.output_text.delta":
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvTextDelta, Text: f.Delta})
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvThinkDelta, Text: f.Delta})
	case "response.function_call_arguments.delta":
		if idx, ok := s.resolveTool(&f); ok {
			s.argsSeen[idx] = true
			s.emitted = append(s.emitted, ir.Event{Kind: ir.EvToolDelta, ToolIndex: idx, ArgsFragment: f.Delta})
		}
	case "response.output_item.done":
		// Upstreams that skip argument deltas carry the full arguments in the
		// done item; surface them so tool calls are never lost.
		if f.Item != nil && f.Item.Type == "function_call" {
			if idx, ok := s.resolveTool(&f); ok {
				if f.Item.Arguments != "" && !s.argsSeen[idx] {
					s.emitted = append(s.emitted, ir.Event{
						Kind: ir.EvToolDelta, ToolIndex: idx, ArgsFragment: f.Item.Arguments,
					})
				}
				s.emitted = append(s.emitted, ir.Event{Kind: ir.EvToolEnd, ToolIndex: idx})
			}
		}
	case "response.completed":
		stop := ir.StopEndTurn
		if s.sawFunctionCall {
			stop = ir.StopToolUse
		}
		s.latchFinish(&stop, f.Response)
	case "response.incomplete":
		reason := ""
		if f.Response != nil && f.Response.IncompleteDetails != nil {
			reason = f.Response.IncompleteDetails.Reason
		}
		stop := mapIncompleteReason(reason)
		s.latchFinish(&stop, f.Response)
	case "response.failed":
		msg := ""
		if f.Response != nil && f.Response.Error != nil {
			msg = f.Response.Error.Message
		}
		s.failed = true
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvError, ErrText: msg})
	case "error":
		s.failed = true
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvError, ErrText: f.Message})
	}
	return s.take()
}

// Finish flushes the terminal event if the stream ended without one.
func (s *StreamReader) Finish() []ir.Event {
	s.emitted = s.emitted[:0]
	if s.finished || s.failed {
		return s.take()
	}
	stop := ir.StopEndTurn
	if s.sawFunctionCall {
		stop = ir.StopToolUse
	}
	s.latchFinish(&stop, nil)
	return s.take()
}

func (s *StreamReader) latchFinish(stop *ir.StopReason, resp *wireResponse) {
	if s.finished {
		return
	}
	s.finished = true
	if resp != nil && resp.Usage != nil {
		s.usage = resp.Usage.toIR()
	}
	s.finish = stop
	s.emitted = append(s.emitted, ir.Event{Kind: ir.EvFinish, Stop: *stop, Usage: s.usage})
}

func (s *StreamReader) resolveTool(f *wireFrame) (int, bool) {
	if f.ItemID != "" {
		if idx, ok := s.itemIdx[f.ItemID]; ok {
			return idx, true
		}
	}
	if f.OutputIndex != nil {
		if idx, ok := s.outIdx[*f.OutputIndex]; ok {
			return idx, true
		}
	}
	return 0, false
}

func (s *StreamReader) take() []ir.Event {
	if len(s.emitted) == 0 {
		return nil
	}
	out := make([]ir.Event, len(s.emitted))
	copy(out, s.emitted)
	return out
}

// Renderer renders canonical events as Responses SSE with `event:`/`data:`
// framing. Items open lazily on their first delta and close on switch or at
// the terminal event, keeping output_index contiguous. Start is side-effect
// free: the relay probes it on a throwaway instance, so response.created is
// emitted from the first EvStart instead.
type Renderer struct {
	model     string
	id        string
	createdAt int64
	nextIndex int              // output_index allocator (contiguous)
	seq       int              // item id allocator
	open      *openItem        // currently open item
	items     []map[string]any // closed item snapshots, replayed at the terminal event
	finished  bool
}

type openItem struct {
	kind   string // "message" | "reasoning" | "function_call"
	index  int
	id     string
	callID string
	name   string
	text   strings.Builder
}

// NewRendererFor creates a client-side Responses renderer.
func NewRendererFor(model string) *Renderer {
	return &Renderer{
		model:     model,
		id:        "resp_lsw",
		createdAt: time.Now().Unix(),
	}
}

// Start emits nothing.
func (r *Renderer) Start() ([]byte, error) { return nil, nil }

// Frame renders one canonical event into SSE bytes (may be empty).
func (r *Renderer) Frame(ev ir.Event) ([]byte, error) {
	if r.finished {
		return nil, nil
	}
	switch ev.Kind {
	case ir.EvStart:
		return eventFrame("response.created", map[string]any{
			"response": map[string]any{
				"id": r.id, "object": "response", "created_at": r.createdAt,
				"status": "in_progress", "model": orString(r.model, "unknown"),
				"output": []any{},
			},
			"sequence_number": 0,
		}), nil

	case ir.EvTextDelta:
		pre, ok := r.ensureOpen("message")
		if !ok {
			return nil, nil
		}
		r.open.text.WriteString(ev.Text)
		out := eventFrame("response.output_text.delta", map[string]any{
			"item_id": r.open.id, "output_index": r.open.index, "content_index": 0,
			"delta": ev.Text,
		})
		return append(pre, out...), nil

	case ir.EvThinkDelta:
		pre, ok := r.ensureOpen("reasoning")
		if !ok {
			return nil, nil
		}
		r.open.text.WriteString(ev.Text)
		out := eventFrame("response.reasoning_summary_text.delta", map[string]any{
			"item_id": r.open.id, "output_index": r.open.index, "summary_index": 0,
			"delta": ev.Text,
		})
		return append(pre, out...), nil

	case ir.EvToolStart:
		closeOut := r.closeOpen()
		itemID := r.nextItemID("fc")
		item := map[string]any{
			"id": itemID, "type": "function_call",
			"call_id": ev.ToolID, // verbatim: round-trips via function_call_output
			"name":    ev.ToolName, "arguments": "", "status": "in_progress",
		}
		r.open = &openItem{kind: "function_call", index: r.alloc(), id: itemID, callID: ev.ToolID, name: ev.ToolName}
		return append(closeOut, eventFrame("response.output_item.added", map[string]any{
			"output_index": r.open.index, "item": item,
		})...), nil

	case ir.EvToolDelta:
		pre, ok := r.ensureOpen("function_call")
		if !ok {
			return nil, nil
		}
		r.open.text.WriteString(ev.ArgsFragment)
		out := eventFrame("response.function_call_arguments.delta", map[string]any{
			"item_id": r.open.id, "output_index": r.open.index, "delta": ev.ArgsFragment,
		})
		return append(pre, out...), nil

	case ir.EvToolEnd:
		if r.open != nil && r.open.kind == "function_call" {
			return r.closeOpen(), nil
		}
		return nil, nil

	case ir.EvFinish:
		out := r.closeOpen()
		r.finished = true
		resp := map[string]any{
			"id": r.id, "object": "response", "created_at": r.createdAt,
			"model": orString(r.model, "unknown"), "output": r.items,
			"usage": usageToWire(ev.Usage),
		}
		if ev.Stop == ir.StopMaxTokens {
			resp["status"] = "incomplete"
			resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			out = append(out, eventFrame("response.incomplete", map[string]any{"response": resp})...)
		} else {
			resp["status"] = "completed"
			out = append(out, eventFrame("response.completed", map[string]any{"response": resp})...)
		}
		return out, nil

	case ir.EvError:
		out := r.closeOpen()
		r.finished = true
		out = append(out, eventFrame("response.failed", map[string]any{
			"response": map[string]any{
				"id": r.id, "object": "response", "created_at": r.createdAt,
				"status": "failed", "model": orString(r.model, "unknown"), "output": r.items,
				"error": map[string]any{"code": "api_error", "message": ev.ErrText},
			},
		})...)
		return out, nil
	}
	return nil, nil // EvPing and unknown kinds
}

// Done emits a terminal completed event if the stream ended without a finish.
func (r *Renderer) Done() []byte {
	if r.finished {
		return nil
	}
	r.finished = true
	out := r.closeOpen()
	return append(out, eventFrame("response.completed", map[string]any{
		"response": map[string]any{
			"id": r.id, "object": "response", "created_at": r.createdAt,
			"status": "completed", "model": orString(r.model, "unknown"), "output": r.items,
			"usage": usageToWire(ir.Usage{}),
		},
	})...)
}

// ensureOpen opens an item of the given kind if none (or another kind) is
// open. The returned bytes close the previous item and announce the new one;
// ok is false only for an unsupported kind.
func (r *Renderer) ensureOpen(kind string) ([]byte, bool) {
	if r.open != nil && r.open.kind == kind {
		return nil, true
	}
	closeOut := r.closeOpen()
	prefix, ok := map[string]string{"message": "msg", "reasoning": "rs", "function_call": "fc"}[kind]
	if !ok {
		return nil, false
	}
	itemID := r.nextItemID(prefix)
	item := map[string]any{"id": itemID, "type": kind, "status": "in_progress"}
	switch kind {
	case "message":
		item["role"] = "assistant"
		item["content"] = []any{}
	case "reasoning":
		item["summary"] = []any{}
	case "function_call":
		item["call_id"] = ""
		item["name"] = ""
		item["arguments"] = ""
	}
	r.open = &openItem{kind: kind, index: r.alloc(), id: itemID}
	return append(closeOut, eventFrame("response.output_item.added", map[string]any{
		"output_index": r.open.index, "item": item,
	})...), true
}

// closeOpen closes the currently open item, emitting its done events and
// recording the final item snapshot for the terminal output array.
func (r *Renderer) closeOpen() []byte {
	if r.open == nil {
		return nil
	}
	o := r.open
	r.open = nil
	var out []byte
	var snapshot map[string]any
	switch o.kind {
	case "message":
		text := o.text.String()
		out = append(out, eventFrame("response.output_text.done", map[string]any{
			"item_id": o.id, "output_index": o.index, "content_index": 0, "text": text,
		})...)
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		out = append(out, eventFrame("response.content_part.done", map[string]any{
			"item_id": o.id, "output_index": o.index, "content_index": 0, "part": part,
		})...)
		snapshot = map[string]any{
			"id": o.id, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{part},
		}
	case "reasoning":
		text := o.text.String()
		out = append(out, eventFrame("response.reasoning_summary_text.done", map[string]any{
			"item_id": o.id, "output_index": o.index, "summary_index": 0, "text": text,
		})...)
		summary := map[string]any{"type": "summary_text", "text": text}
		snapshot = map[string]any{
			"id": o.id, "type": "reasoning", "summary": []any{summary},
		}
	case "function_call":
		args := orEmptyJSON(o.text.String())
		out = append(out, eventFrame("response.function_call_arguments.done", map[string]any{
			"item_id": o.id, "output_index": o.index, "arguments": args,
		})...)
		snapshot = map[string]any{
			"id": o.id, "type": "function_call", "call_id": o.callID, "name": o.name,
			"arguments": args, "status": "completed",
		}
	}
	out = append(out, eventFrame("response.output_item.done", map[string]any{
		"output_index": o.index, "item": snapshot,
	})...)
	r.items = append(r.items, snapshot)
	return out
}

func (r *Renderer) alloc() int {
	idx := r.nextIndex
	r.nextIndex++
	return idx
}

func (r *Renderer) nextItemID(prefix string) string {
	r.seq++
	return prefix + "_lsw_" + strconv.Itoa(r.seq)
}

func eventFrame(event string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out := make([]byte, 0, len(event)+len(b)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, b...)
	out = append(out, '\n', '\n')
	return out
}
