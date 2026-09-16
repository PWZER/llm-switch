// Package protocol pairs each wire protocol's codec behind one interface and
// provides the registry used by the gateway's conversion path.
package protocol

import (
	"fmt"

	"github.com/PWZER/llm-switch/internal/ir"
	"github.com/PWZER/llm-switch/internal/protocol/anthropic"
	"github.com/PWZER/llm-switch/internal/protocol/openai"
	"github.com/PWZER/llm-switch/internal/protocol/responses"
)

// StreamReader folds upstream SSE payloads into canonical events.
type StreamReader interface {
	// Feed consumes one `data:` payload and returns the events it produced.
	Feed(payload []byte) []ir.Event
	// Finish flushes terminal events when the stream ended without a marker.
	Finish() []ir.Event
}

// Renderer renders canonical events into the client protocol's SSE bytes.
type Renderer interface {
	// Start emits the stream preamble (may be empty).
	Start() ([]byte, error)
	// Frame renders one canonical event (may be empty).
	Frame(ev ir.Event) ([]byte, error)
	// Done emits the terminal marker if the stream ended without one.
	Done() []byte
}

// Codec converts between one wire protocol and the IR.
type Codec interface {
	DecodeRequest(body []byte) (*ir.Request, error)
	EncodeRequest(req *ir.Request) ([]byte, error)
	DecodeResponse(body []byte) (*ir.Response, error)
	EncodeResponse(resp *ir.Response) ([]byte, error)
	NewStreamReader() StreamReader
	NewRenderer(model string) Renderer
}

type openaiCodec struct{}

func (openaiCodec) DecodeRequest(body []byte) (*ir.Request, error) { return openai.DecodeRequest(body) }
func (openaiCodec) EncodeRequest(req *ir.Request) ([]byte, error)  { return openai.EncodeRequest(req) }
func (openaiCodec) DecodeResponse(body []byte) (*ir.Response, error) {
	return openai.DecodeResponse(body)
}
func (openaiCodec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	return openai.EncodeResponse(resp)
}
func (openaiCodec) NewStreamReader() StreamReader { return openai.NewStreamReader() }
func (openaiCodec) NewRenderer(model string) Renderer {
	return openai.NewRendererFor(model)
}

type anthropicCodec struct{}

func (anthropicCodec) DecodeRequest(body []byte) (*ir.Request, error) {
	return anthropic.DecodeRequest(body)
}
func (anthropicCodec) EncodeRequest(req *ir.Request) ([]byte, error) {
	return anthropic.EncodeRequest(req)
}
func (anthropicCodec) DecodeResponse(body []byte) (*ir.Response, error) {
	return anthropic.DecodeResponse(body)
}
func (anthropicCodec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	return anthropic.EncodeResponse(resp)
}
func (anthropicCodec) NewStreamReader() StreamReader { return anthropic.NewStreamReader() }
func (anthropicCodec) NewRenderer(model string) Renderer {
	return anthropic.NewRendererFor(model)
}

type responsesCodec struct{}

func (responsesCodec) DecodeRequest(body []byte) (*ir.Request, error) {
	return responses.DecodeRequest(body)
}
func (responsesCodec) EncodeRequest(req *ir.Request) ([]byte, error) {
	return responses.EncodeRequest(req)
}
func (responsesCodec) DecodeResponse(body []byte) (*ir.Response, error) {
	return responses.DecodeResponse(body)
}
func (responsesCodec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	return responses.EncodeResponse(resp)
}
func (responsesCodec) NewStreamReader() StreamReader { return responses.NewStreamReader() }
func (responsesCodec) NewRenderer(model string) Renderer {
	return responses.NewRendererFor(model)
}

// For returns the codec of a protocol.
func For(p ir.Protocol) (Codec, error) {
	switch p {
	case ir.OpenAI:
		return openaiCodec{}, nil
	case ir.Anthropic:
		return anthropicCodec{}, nil
	case ir.OpenAIResponses:
		return responsesCodec{}, nil
	default:
		return nil, fmt.Errorf("unknown protocol %q", p)
	}
}
