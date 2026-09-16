// Package ir defines the canonical intermediate representation shared by both
// protocol codecs. With this hub, N client protocols x M upstream protocols
// need N+M converters instead of N*M.
package ir

import "encoding/json"

// Protocol names a wire format.
type Protocol string

const (
	OpenAI    Protocol = "openai"
	Anthropic Protocol = "anthropic"
)

// BlockType is the canonical content block kind.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockImage      BlockType = "image"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockThinking   BlockType = "thinking"
)

// Block is one canonical content block. Field meaning depends on Type.
type Block struct {
	Type BlockType

	Text string // BlockText / thinking payload for BlockThinking
	Signature string // thinking signature; empty for synthesized thinking

	// BlockImage
	ImageMedia string // e.g. "image/png"
	ImageData  string // base64 payload, or a URL when ImageIsURL
	ImageIsURL bool

	// BlockToolUse / BlockToolResult
	ToolCallID string
	ToolName   string
	InputJSON  string // tool_use arguments as a raw JSON string (streaming friendly)

	ResultBlocks []Block // tool_result content
	IsError      bool    // tool_result
}

// Message is one canonical conversation turn.
type Message struct {
	Role   Role
	Blocks []Block
}

// Role is a canonical speaker role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Tool is a function definition.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema of the parameters
}

// ToolChoiceMode enumerates canonical tool-choice modes.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required" // anthropic "any"
	ToolChoiceTool     ToolChoiceMode = "tool"
)

// ToolChoice narrows which tool the model may call.
type ToolChoice struct {
	Mode            ToolChoiceMode
	Name            string // required when Mode == ToolChoiceTool
	DisableParallel bool
}

// Thinking is the canonical thinking/reasoning control.
// Type: "" (unset) | "enabled" | "disabled" | "adaptive"; Effort carries the
// OpenAI-style reasoning_effort ("low"|"medium"|"high").
type Thinking struct {
	Type         string
	BudgetTokens int64
	Effort       string
}

// Request is the canonical chat request.
type Request struct {
	Model         string
	Stream        bool
	System        []Block
	Messages      []Message
	Tools         []Tool
	ToolChoice    *ToolChoice
	MaxTokens     int64
	Temperature   *float64
	TopP          *float64
	StopSequences []string
	Thinking      *Thinking
}

// Usage is the canonical token accounting.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
}

// StopReason is the canonical stop cause (Anthropic-style vocabulary).
type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"
	StopMaxTokens    StopReason = "max_tokens"
	StopToolUse      StopReason = "tool_use"
	StopStopSequence StopReason = "stop_sequence"
	StopContentFilter StopReason = "content_filter"
)

// Response is the canonical complete response.
type Response struct {
	ID      string
	Model   string
	Blocks  []Block
	Stop    StopReason
	Usage   Usage
	ErrType string // upstream error type when the response is an error
	ErrText string
}

// EventKind enumerates canonical stream events.
type EventKind string

const (
	EvStart     EventKind = "start"      // carries Model
	EvTextDelta EventKind = "text"       // carries Text
	EvThinkDelta EventKind = "thinking"  // carries Text
	EvToolStart EventKind = "tool_start" // carries ToolID, ToolName, ToolIndex
	EvToolDelta EventKind = "tool_delta" // carries ToolIndex, ArgsFragment
	EvToolEnd   EventKind = "tool_end"   // carries ToolIndex
	EvFinish    EventKind = "finish"     // carries Stop, Usage
	EvPing      EventKind = "ping"
	EvError     EventKind = "error" // carries ErrText
)

// Event is one canonical stream event.
type Event struct {
	Kind         EventKind
	Model        string
	Text         string
	ToolID       string
	ToolName     string
	ToolIndex    int
	ArgsFragment string
	Stop         StopReason
	Usage        Usage
	ErrText      string
}

// Accumulate folds a stream of events into a canonical Response (for
// stream-to-non-stream bridging and retry-safe logging).
func Accumulate(events []Event) Response {
	var resp Response
	var toolArgs []string
	currentTool := -1
	for _, ev := range events {
		switch ev.Kind {
		case EvStart:
			resp.Model = ev.Model
		case EvTextDelta:
			resp.Blocks = appendText(resp.Blocks, ev.Text)
		case EvThinkDelta:
			resp.Blocks = appendThinking(resp.Blocks, ev.Text)
		case EvToolStart:
			currentTool = ev.ToolIndex
			for len(toolArgs) <= ev.ToolIndex {
				toolArgs = append(toolArgs, "")
			}
			resp.Blocks = append(resp.Blocks, Block{
				Type: BlockToolUse, ToolCallID: ev.ToolID, ToolName: ev.ToolName,
			})
		case EvToolDelta:
			for len(toolArgs) <= ev.ToolIndex {
				toolArgs = append(toolArgs, "")
			}
			toolArgs[ev.ToolIndex] += ev.ArgsFragment
		case EvToolEnd:
			currentTool = -1
		case EvFinish:
			resp.Stop = ev.Stop
			resp.Usage = ev.Usage
		case EvError:
			resp.ErrText = ev.ErrText
		}
	}
	_ = currentTool
	// Attach accumulated argument JSON to tool_use blocks in order.
	ti := 0
	for i := range resp.Blocks {
		if resp.Blocks[i].Type == BlockToolUse {
			if ti < len(toolArgs) {
				resp.Blocks[i].InputJSON = toolArgs[ti]
			}
			ti++
		}
	}
	return resp
}

func appendText(blocks []Block, text string) []Block {
	if len(blocks) > 0 && blocks[len(blocks)-1].Type == BlockText {
		blocks[len(blocks)-1].Text += text
		return blocks
	}
	return append(blocks, Block{Type: BlockText, Text: text})
}

func appendThinking(blocks []Block, text string) []Block {
	if len(blocks) > 0 && blocks[len(blocks)-1].Type == BlockThinking {
		blocks[len(blocks)-1].Text += text
		return blocks
	}
	return append(blocks, Block{Type: BlockThinking, Text: text})
}
