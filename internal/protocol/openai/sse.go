package openai

import (
	"encoding/json"
	"strings"

	"github.com/PWZER/llm-switch/internal/ir"
)

// StreamReader folds upstream OpenAI chat.completion.chunk SSE into canonical
// IR events. Feed one `data:` payload per call (nil/[DONE] terminates).
type StreamReader struct {
	started  bool
	finish   *ir.StopReason
	usage    ir.Usage
	seqTool  int         // OpenAI chunk index -> sequential tool index
	chunkIdx map[int]int // upstream tool_calls index -> ToolIndex
	emitted  []ir.Event
}

// NewStreamReader creates a reader for upstream OpenAI SSE.
func NewStreamReader() *StreamReader {
	return &StreamReader{chunkIdx: map[int]int{}}
}

// Feed consumes one SSE data payload and returns any canonical events it
// produced. An empty payload or `[DONE]` flushes the terminal finish event.
func (s *StreamReader) Feed(payload []byte) []ir.Event {
	s.emitted = s.emitted[:0]
	line := strings.TrimSpace(string(payload))
	if line == "" {
		return nil
	}
	if line == "[DONE]" {
		s.emitFinish()
		return s.take()
	}
	var chunk wireResponse
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil // tolerate malformed frames; usage may be lost
	}
	if chunk.Error != nil {
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvError, ErrText: chunk.Error.Message})
		return s.take()
	}
	if !s.started {
		s.started = true
		s.emitted = append(s.emitted, ir.Event{Kind: ir.EvStart, Model: chunk.Model})
	}
	if len(chunk.Choices) > 0 {
		c := chunk.Choices[0]
		if c.Delta != nil {
			if c.Delta.ReasoningContent != "" {
				s.emitted = append(s.emitted, ir.Event{Kind: ir.EvThinkDelta, Text: c.Delta.ReasoningContent})
			}
			if c.Delta.Content != "" {
				s.emitted = append(s.emitted, ir.Event{Kind: ir.EvTextDelta, Text: c.Delta.Content})
			}
			for _, tc := range c.Delta.ToolCalls {
				idx, ok := s.chunkIdx[tc.Index]
				if !ok {
					idx = s.seqTool
					s.seqTool++
					s.chunkIdx[tc.Index] = idx
					s.emitted = append(s.emitted, ir.Event{
						Kind: ir.EvToolStart, ToolIndex: idx,
						ToolID: tc.ID, ToolName: tc.Function.Name,
					})
				}
				if tc.Function.Arguments != "" {
					s.emitted = append(s.emitted, ir.Event{
						Kind: ir.EvToolDelta, ToolIndex: idx, ArgsFragment: tc.Function.Arguments,
					})
				}
			}
		}
		if c.FinishReason != nil {
			stop := mapFinishReason(*c.FinishReason)
			s.finish = &stop
		}
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage.toIR()
	}
	return s.take()
}

// Finish flushes the terminal event if the stream ended without [DONE].
func (s *StreamReader) Finish() []ir.Event {
	s.emitted = s.emitted[:0]
	s.emitFinish()
	return s.take()
}

func (s *StreamReader) emitFinish() {
	if s.finish == nil {
		stop := ir.StopEndTurn
		s.finish = &stop
	}
	s.emitted = append(s.emitted, ir.Event{Kind: ir.EvFinish, Stop: *s.finish, Usage: s.usage})
}

func (s *StreamReader) take() []ir.Event {
	if len(s.emitted) == 0 {
		return nil
	}
	out := make([]ir.Event, len(s.emitted))
	copy(out, s.emitted)
	return out
}

// Renderer renders canonical events as OpenAI chat.completion.chunk SSE
// frames. Each frame is delivered via Frame; the caller writes and flushes it.
type Renderer struct {
	started  bool
	model    string
	toolSeq  int
	finished bool
}

// NewRenderer creates a client-side OpenAI SSE renderer.
func NewRenderer() *Renderer { return &Renderer{} }

// NewRendererFor creates the renderer with a client-facing model name.
func NewRendererFor(model string) *Renderer { return &Renderer{model: model} }

// Start emits nothing; the first delta triggers the role chunk.
func (r *Renderer) Start() ([]byte, error) { return nil, nil }

// Frame renders one canonical event into SSE bytes (may be empty).
func (r *Renderer) Frame(ev ir.Event) ([]byte, error) {
	switch ev.Kind {
	case ir.EvStart:
		r.started = true
		return chunkFrame(map[string]any{
			"id": r.id(), "object": "chat.completion.chunk",
			"model": orDefault(r.model, "unknown"),
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
			}},
		}), nil
	case ir.EvTextDelta:
		r.started = true
		return chunkFrame(map[string]any{
			"id": r.id(), "object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": ev.Text},
			}},
		}), nil
	case ir.EvThinkDelta:
		return chunkFrame(map[string]any{
			"id": r.id(), "object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"reasoning_content": ev.Text},
			}},
		}), nil
	case ir.EvToolStart:
		r.toolSeq = ev.ToolIndex
		return chunkFrame(map[string]any{
			"id": r.id(), "object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": ev.ToolIndex, "id": ev.ToolID, "type": "function",
						"function": map[string]any{"name": ev.ToolName, "arguments": ""},
					}},
				},
			}},
		}), nil
	case ir.EvToolDelta:
		return chunkFrame(map[string]any{
			"id": r.id(), "object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    ev.ToolIndex,
						"function": map[string]any{"arguments": ev.ArgsFragment},
					}},
				},
			}},
		}), nil
	case ir.EvFinish:
		if r.finished {
			return nil, nil
		}
		r.finished = true
		finish := mapStopReason(ev.Stop)
		out := chunkFrame(map[string]any{
			"id": r.id(), "object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": finish,
			}},
		})
		out = append(out, chunkFrame(map[string]any{
			"id": r.id(), "object": "chat.completion.chunk",
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": ev.Usage.Input, "completion_tokens": ev.Usage.Output,
				"total_tokens": ev.Usage.Input + ev.Usage.Output,
			},
		})...)
		out = append(out, []byte("data: [DONE]\n\n")...)
		return out, nil
	case ir.EvError:
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": ev.ErrText, "type": "api_error"},
		})
		return append(append([]byte("data: "), b...), '\n', '\n'), nil
	}
	return nil, nil
}

func (r *Renderer) id() string {
	if r.model != "" {
		return "chatcmpl-lsw-" + r.model
	}
	return "chatcmpl-lsw"
}

// Done emits the terminal [DONE] marker if the stream ended without a finish.
func (r *Renderer) Done() []byte {
	if r.finished {
		return nil
	}
	r.finished = true
	return []byte("data: [DONE]\n\n")
}

func chunkFrame(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out := make([]byte, 0, len(b)+8)
	out = append(out, "data: "...)
	out = append(out, b...)
	out = append(out, '\n', '\n')
	return out
}
