package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PWZER/llm-switch/internal/ir"
)

func feedAll(t *testing.T, s *StreamReader, frames ...string) []ir.Event {
	t.Helper()
	var out []ir.Event
	for _, f := range frames {
		out = append(out, s.Feed([]byte(f))...)
	}
	return out
}

func kindList(evs []ir.Event) []ir.EventKind {
	out := make([]ir.EventKind, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
	}
	return out
}

func kindsEqual(a []ir.EventKind, b ...ir.EventKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStreamReaderMessageStream(t *testing.T) {
	s := NewStreamReader()
	evs := feedAll(t, s,
		`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"He"}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"llo"}`,
		`{"type":"response.output_text.done","item_id":"msg_1","text":"Hello"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed",
			"usage":{"input_tokens":12,"output_tokens":6,
				"input_tokens_details":{"cached_tokens":4}}}}`,
	)
	want := []ir.EventKind{ir.EvStart, ir.EvTextDelta, ir.EvTextDelta, ir.EvFinish}
	if !kindsEqual(kindList(evs), want...) {
		t.Fatalf("events: %v", evs)
	}
	fin := evs[len(evs)-1]
	if fin.Stop != ir.StopEndTurn || fin.Usage.Input != 12 || fin.Usage.Output != 6 || fin.Usage.CacheRead != 4 {
		t.Fatalf("bad finish: %+v", fin)
	}
}

func TestStreamReaderToolStream(t *testing.T) {
	s := NewStreamReader()
	evs := feedAll(t, s,
		`{"type":"response.created","response":{"model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,
		  "item":{"type":"function_call","id":"fc_1","call_id":"call_x","name":"f","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"a\""}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":":1}"}`,
		`{"type":"response.output_item.done","output_index":0,
		  "item":{"type":"function_call","id":"fc_1","call_id":"call_x","name":"f","arguments":"{\"a\":1}"}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`,
	)
	want := []ir.EventKind{ir.EvStart, ir.EvToolStart, ir.EvToolDelta, ir.EvToolDelta, ir.EvToolEnd, ir.EvFinish}
	if !kindsEqual(kindList(evs), want...) {
		t.Fatalf("events: %v", evs)
	}
	if evs[1].ToolID != "call_x" || evs[1].ToolName != "f" {
		t.Fatalf("bad tool start: %+v", evs[1])
	}
	var args string
	for _, ev := range evs {
		if ev.Kind == ir.EvToolDelta {
			args += ev.ArgsFragment
		}
	}
	if args != `{"a":1}` {
		t.Fatalf("bad args: %q", args)
	}
	if evs[len(evs)-1].Stop != ir.StopToolUse {
		t.Fatalf("want StopToolUse, got %s", evs[len(evs)-1].Stop)
	}
}

func TestStreamReaderArgsOnlyInDone(t *testing.T) {
	s := NewStreamReader()
	evs := feedAll(t, s,
		`{"type":"response.output_item.added","output_index":0,
		  "item":{"type":"function_call","id":"fc_1","call_id":"call_x","name":"f","arguments":""}}`,
		`{"type":"response.output_item.done","output_index":0,
		  "item":{"type":"function_call","id":"fc_1","call_id":"call_x","name":"f","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	)
	want := []ir.EventKind{ir.EvToolStart, ir.EvToolDelta, ir.EvToolEnd, ir.EvFinish}
	if !kindsEqual(kindList(evs), want...) {
		t.Fatalf("events: %v", evs)
	}
	if evs[1].ArgsFragment != "{}" {
		t.Fatalf("want full args from done, got %q", evs[1].ArgsFragment)
	}
}

func TestStreamReaderIncompleteAndFailed(t *testing.T) {
	s := NewStreamReader()
	evs := feedAll(t, s,
		`{"type":"response.incomplete","response":{"status":"incomplete",
			"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":9,"output_tokens":1}}}`,
	)
	if !kindsEqual(kindList(evs), ir.EvFinish) || evs[0].Stop != ir.StopMaxTokens || evs[0].Usage.Input != 9 {
		t.Fatalf("events: %v", evs)
	}

	s2 := NewStreamReader()
	evs2 := feedAll(t, s2,
		`{"type":"response.failed","response":{"status":"failed","error":{"code":"x","message":"boom"}}}`,
	)
	if !kindsEqual(kindList(evs2), ir.EvError) || evs2[0].ErrText != "boom" {
		t.Fatalf("events: %v", evs2)
	}
	if extra := s2.Finish(); len(extra) != 0 {
		t.Fatalf("Finish after failure must be empty, got %v", extra)
	}
}

func TestStreamReaderFinishSynthesizes(t *testing.T) {
	s := NewStreamReader()
	feedAll(t, s, `{"type":"response.output_text.delta","delta":"partial"}`)
	evs := s.Finish()
	if !kindsEqual(kindList(evs), ir.EvFinish) || evs[0].Stop != ir.StopEndTurn {
		t.Fatalf("events: %v", evs)
	}
}

func TestRendererMessageStream(t *testing.T) {
	r := NewRendererFor("test-model")
	var out []byte
	write := func(b []byte, err error) {
		if err != nil {
			t.Fatalf("frame: %v", err)
		}
		out = append(out, b...)
	}
	write(r.Frame(ir.Event{Kind: ir.EvStart, Model: "upstream-model"}))
	write(r.Frame(ir.Event{Kind: ir.EvTextDelta, Text: "He"}))
	write(r.Frame(ir.Event{Kind: ir.EvTextDelta, Text: "llo"}))
	write(r.Frame(ir.Event{Kind: ir.EvFinish,
		Stop:  ir.StopEndTurn,
		Usage: ir.Usage{Input: 12, Output: 6, CacheRead: 4}}))
	if done := r.Done(); len(done) != 0 {
		t.Fatalf("Done after finish must be empty")
	}
	s := string(out)
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		`"type":"message"`,
		"event: response.output_text.delta",
		`"delta":"He"`,
		`"delta":"llo"`,
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"input_tokens":12`,
		`"output_tokens":6`,
		`"cached_tokens":4`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Count(s, "event: response.completed") != 1 {
		t.Fatalf("want exactly one terminal event:\n%s", s)
	}
	// output_index contiguous from 0
	if !strings.Contains(s, `"output_index":0`) {
		t.Fatalf("want output_index 0:\n%s", s)
	}
	// every frame framed as event: + data:
	for _, line := range strings.Split(s, "\n\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "event: ") || !strings.Contains(line, "\ndata: ") {
			t.Fatalf("bad framing: %q", line)
		}
	}
}

func TestRendererToolRoundTrip(t *testing.T) {
	r := NewRendererFor("m")
	var out []byte
	write := func(b []byte, err error) {
		if err != nil {
			t.Fatalf("frame: %v", err)
		}
		out = append(out, b...)
	}
	write(r.Frame(ir.Event{Kind: ir.EvStart}))
	write(r.Frame(ir.Event{Kind: ir.EvTextDelta, Text: "calling"}))
	write(r.Frame(ir.Event{Kind: ir.EvToolStart, ToolIndex: 0, ToolID: "call_9", ToolName: "f"}))
	write(r.Frame(ir.Event{Kind: ir.EvToolDelta, ToolIndex: 0, ArgsFragment: `{"x":1}`}))
	write(r.Frame(ir.Event{Kind: ir.EvFinish, Stop: ir.StopToolUse, Usage: ir.Usage{Input: 1, Output: 1}}))
	s := string(out)
	for _, want := range []string{
		`"call_id":"call_9"`, // verbatim tool id
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"x\":1}"`,
		`"status":"completed"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// message item (index 0) closes before function_call item (index 1) opens
	if strings.Index(s, `"output_index":0`) > strings.Index(s, `"output_index":1`) {
		t.Fatalf("item indexes out of order:\n%s", s)
	}
	// terminal output array carries both items
	var done struct {
		Response struct {
			Output []struct {
				Type string `json:"type"`
			} `json:"output"`
		} `json:"response"`
	}
	idx := strings.LastIndex(s, "event: response.completed")
	dataPart := s[idx:]
	dataPart = dataPart[strings.Index(dataPart, "data: ")+len("data: "):]
	dataPart = strings.TrimSuffix(strings.TrimSpace(dataPart), "\n")
	if err := json.Unmarshal([]byte(dataPart), &done); err != nil {
		t.Fatalf("terminal unmarshal: %v", err)
	}
	if len(done.Response.Output) != 2 ||
		done.Response.Output[0].Type != "message" || done.Response.Output[1].Type != "function_call" {
		t.Fatalf("terminal output items: %s", dataPart)
	}
}

func TestRendererDoneWithoutFinish(t *testing.T) {
	r := NewRendererFor("m")
	r.Frame(ir.Event{Kind: ir.EvStart})
	r.Frame(ir.Event{Kind: ir.EvTextDelta, Text: "partial"})
	done := r.Done()
	if done == nil || !strings.Contains(string(done), "event: response.completed") {
		t.Fatalf("want synthesized terminal, got %q", done)
	}
	if again := r.Done(); again != nil {
		t.Fatalf("Done must be idempotent")
	}
}

func TestRendererErrorTerminal(t *testing.T) {
	r := NewRendererFor("m")
	r.Frame(ir.Event{Kind: ir.EvStart})
	frame, _ := r.Frame(ir.Event{Kind: ir.EvError, ErrText: "upstream exploded"})
	if !strings.Contains(string(frame), "event: response.failed") ||
		!strings.Contains(string(frame), "upstream exploded") {
		t.Fatalf("bad failure frame: %q", frame)
	}
	if done := r.Done(); done != nil {
		t.Fatalf("no completed after failed, got %q", done)
	}
}

// TestRelaySimulation feeds upstream Responses SSE through the reader and the
// renderer exactly like relayConvertedStream does for the bridge direction.
func TestRelaySimulation(t *testing.T) {
	reader := NewStreamReader()
	renderer := NewRendererFor("glm-5.3")
	var out []byte
	for _, payload := range []string{
		`{"type":"response.created","response":{"model":"upstream-m"}}`,
		`{"type":"response.output_text.delta","delta":"A"}`,
		`{"type":"response.output_text.delta","delta":"B"}`,
		`{"type":"response.completed","response":{"status":"completed",
			"usage":{"input_tokens":7,"output_tokens":3}}}`,
	} {
		for _, ev := range reader.Feed([]byte(payload)) {
			frame, err := renderer.Frame(ev)
			if err != nil {
				t.Fatalf("frame: %v", err)
			}
			out = append(out, frame...)
		}
	}
	for _, ev := range reader.Finish() {
		frame, _ := renderer.Frame(ev)
		out = append(out, frame...)
	}
	if done := renderer.Done(); len(done) > 0 {
		out = append(out, done...)
	}
	s := string(out)
	if strings.Count(s, "event: response.completed") != 1 {
		t.Fatalf("want exactly one terminal:\n%s", s)
	}
	if !strings.Contains(s, `"model":"glm-5.3"`) {
		t.Fatalf("terminal should carry client-facing model:\n%s", s)
	}
}
