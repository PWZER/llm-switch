package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PWZER/llm-switch/internal/payload"
)

// usageInfo is the normalized usage snapshot harvested from upstream traffic.
// Fields without an upstream counterpart stay zero.
type usageInfo struct {
	Prompt     int64
	Completion int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
}

const maxBodyBytes = 32 << 20 // 32 MiB request body cap

// tapStreamLine inspects one SSE line from a streaming upstream and harvests
// usage fields. Best-effort: malformed frames are ignored, never fatal.
func tapStreamLine(protocol string, line []byte, u *usageInfo) {
	payload, ok := sseDataPayload(line)
	if !ok {
		return
	}
	if protocol == protocolOpenAIResponses || protocol == protocolResponses {
		// Responses-shaped body: usage nests under response.completed /
		// response.incomplete, or top-level for the non-stream object.
		tapResponsesUsage(payload, u)
		return
	}
	var frame struct {
		Type  string `json:"type"`
		Usage *struct {
			InputTokens  *int64 `json:"input_tokens"`
			OutputTokens *int64 `json:"output_tokens"`
			PromptTokens *int64 `json:"prompt_tokens"`
			// OpenAI names completion tokens differently from Anthropic.
			CompletionTokens *int64 `json:"completion_tokens"`
			// OpenAI modern + detail forms
			PromptTokensDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens *int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			// Anthropic cache fields
			CacheReadTokens    *int64 `json:"cache_read_input_tokens"`
			CacheCreationToken *int64 `json:"cache_creation_input_tokens"`
			// DeepSeek flat cache fields
			PromptCacheHitTokens *int64 `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
		Message *struct {
			Usage *struct {
				InputTokens        *int64 `json:"input_tokens"`
				CacheReadTokens    *int64 `json:"cache_read_input_tokens"`
				CacheCreationToken *int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return
	}

	switch {
	case frame.Type == "message_start" && frame.Message != nil && frame.Message.Usage != nil:
		// Anthropic message_start: input + cache read/write. Normalized to the
		// total-input convention: Prompt includes cache tokens.
		mu := frame.Message.Usage
		if mu.InputTokens != nil {
			u.Prompt = *mu.InputTokens
		}
		if mu.CacheReadTokens != nil {
			u.CacheRead = *mu.CacheReadTokens
		}
		if mu.CacheCreationToken != nil {
			u.CacheWrite = *mu.CacheCreationToken
		}
		u.Prompt += u.CacheRead + u.CacheWrite
	case frame.Type == "message_delta" && frame.Usage != nil:
		// Anthropic message_delta: official spec carries output_tokens only,
		// but several Anthropic-compatible vendors (Zhipu/DeepSeek) put the
		// FULL usage here and omit it from message_start — merge both sides.
		if frame.Usage.OutputTokens != nil {
			u.Completion = *frame.Usage.OutputTokens
		}
		if frame.Usage.CacheReadTokens != nil && *frame.Usage.CacheReadTokens > 0 {
			u.CacheRead = *frame.Usage.CacheReadTokens
		}
		if frame.Usage.CacheCreationToken != nil && *frame.Usage.CacheCreationToken > 0 {
			u.CacheWrite = *frame.Usage.CacheCreationToken
		}
		if frame.Usage.InputTokens != nil && *frame.Usage.InputTokens > 0 {
			u.Prompt = *frame.Usage.InputTokens + u.CacheRead + u.CacheWrite
		}
	case frame.Usage != nil && frame.Usage.PromptTokens == nil && frame.Usage.InputTokens != nil:
		// Non-stream Anthropic message body: top-level usage with Anthropic
		// field names (input_tokens excludes cache).
		fu := frame.Usage
		u.Prompt = *fu.InputTokens
		if fu.CacheReadTokens != nil {
			u.CacheRead = *fu.CacheReadTokens
		}
		if fu.CacheCreationToken != nil {
			u.CacheWrite = *fu.CacheCreationToken
		}
		u.Prompt += u.CacheRead + u.CacheWrite
		if fu.OutputTokens != nil {
			u.Completion = *fu.OutputTokens
		}
	case frame.Usage != nil:
		// OpenAI chunk with usage (include_usage) or non-stream body shape.
		fu := frame.Usage
		if fu.PromptTokens != nil {
			u.Prompt = *fu.PromptTokens
		}
		if fu.CompletionTokens != nil {
			u.Completion = *fu.CompletionTokens
		} else if fu.OutputTokens != nil {
			u.Completion = *fu.OutputTokens
		}
		if fu.PromptTokensDetails != nil && fu.PromptTokensDetails.CachedTokens != nil {
			u.CacheRead = *fu.PromptTokensDetails.CachedTokens
		}
		if fu.CompletionTokensDetails != nil && fu.CompletionTokensDetails.ReasoningTokens != nil {
			u.Reasoning = *fu.CompletionTokensDetails.ReasoningTokens
		}
		if fu.PromptCacheHitTokens != nil {
			u.CacheRead = *fu.PromptCacheHitTokens
		}
	}
}

// responsesWireUsage mirrors the usage object of the Responses API.
// input_tokens already includes cached tokens (OpenAI convention).
type responsesWireUsage struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// tapResponsesUsage harvests usage from a Responses-shaped payload: streaming
// events nest it under response.completed / response.incomplete; the
// non-stream body is the bare response object (type "response" after the
// tap's `data:` wrapper). Gated on the responses protocol so the generic
// chat/anthropic parsing below never sees these field names.
func tapResponsesUsage(payload []byte, u *usageInfo) {
	var frame struct {
		Type     string              `json:"type"`
		Usage    *responsesWireUsage `json:"usage"` // non-stream bare response object
		Response *struct {
			Usage *responsesWireUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return
	}
	ru := frame.Usage
	if frame.Response != nil && frame.Response.Usage != nil {
		ru = frame.Response.Usage
	}
	if ru == nil {
		return
	}
	if ru.InputTokens != nil {
		u.Prompt = *ru.InputTokens
	}
	if ru.OutputTokens != nil {
		u.Completion = *ru.OutputTokens
	}
	if ru.InputTokensDetails != nil && ru.InputTokensDetails.CachedTokens != nil {
		u.CacheRead = *ru.InputTokensDetails.CachedTokens
	}
	if ru.OutputTokensDetails != nil && ru.OutputTokensDetails.ReasoningTokens != nil {
		u.Reasoning = *ru.OutputTokensDetails.ReasoningTokens
	}
}

// hasContentDelta reports whether a `data:` payload carries generated content
// (the TTFT signal), as opposed to handshake/keep-alive frames such as the
// OpenAI role-only chunk, Anthropic message_start/ping, or Responses
// response.created. Unparseable payloads and unknown protocols conservatively
// count as content so ttft is never lost to a parsing gap.
func hasContentDelta(protocol string, payload []byte) bool {
	switch protocol {
	case protocolOpenAI:
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          json.RawMessage `json:"content"`
					ReasoningContent json.RawMessage `json:"reasoning_content"`
					ToolCalls        json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return true
		}
		for _, c := range chunk.Choices {
			if nonEmptyJSONString(c.Delta.Content) || nonEmptyJSONString(c.Delta.ReasoningContent) ||
				nonEmptyJSONArray(c.Delta.ToolCalls) {
				return true
			}
		}
		return false
	case protocolAnthropic:
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			return true
		}
		// content_block_delta covers text_delta / thinking_delta /
		// input_json_delta — every form of generated output.
		return ev.Type == "content_block_delta"
	case protocolResponses, protocolOpenAIResponses:
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			return true
		}
		// response.output_text.delta, response.reasoning_summary_text.delta,
		// response.function_call_arguments.delta, ...
		return strings.HasSuffix(ev.Type, ".delta")
	default:
		return true
	}
}

// nonEmptyJSONString reports whether raw is a JSON string with content
// (excludes null, "", and absent fields).
func nonEmptyJSONString(raw json.RawMessage) bool {
	return len(raw) > 2 && raw[0] == '"'
}

// nonEmptyJSONArray reports whether raw is a JSON array with at least one
// element (excludes null, [], and absent fields).
func nonEmptyJSONArray(raw json.RawMessage) bool {
	return len(raw) > 2 && raw[0] == '['
}

// sseDataPayload returns the JSON payload of a `data:` line, if any.
func sseDataPayload(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}

// tapNonStreamBody harvests usage from a complete JSON response body.
func tapNonStreamBody(protocol string, body []byte) usageInfo {
	var u usageInfo
	tapStreamLine(protocol, append([]byte("data: "), body...), &u)
	return u
}

// relayNonStream forwards a buffered upstream response to the client. sink,
// when non-nil, collects the body bytes for payload recording.
func relayNonStream(w http.ResponseWriter, r *http.Request, resp *http.Response, protocol string, sink *payload.Buffer) (usageInfo, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return usageInfo{}, err
	}
	if sink != nil {
		sink.Write(body)
	}
	copyResponseHeaders(w.Header(), resp)
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		return tapNonStreamBody(protocol, body), err
	}
	return tapNonStreamBody(protocol, body), nil
}

// relayStream pipes an SSE response to the client line by line, flushing per
// line, while tapping usage. An idle watchdog aborts the upstream if no bytes
// arrive within idleTimeout. sink, when non-nil, collects the relayed bytes
// for payload recording — a pure memory append, never blocking the loop.
// start is when the upstream request was dispatched: the reported ttft is the
// upstream-side time to the first content-bearing frame.
func relayStream(w http.ResponseWriter, r *http.Request, resp *http.Response, protocol string, idleTimeout time.Duration, sink *payload.Buffer, start time.Time) (usageInfo, *int64, error) {
	defer resp.Body.Close()

	h := w.Header()
	copyResponseHeaders(h, resp)
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	var u usageInfo
	var ttft *int64

	wdCtx, wdReset, wdStop := idleWatchdog(r.Context(), idleTimeout)
	defer wdStop()
	reader := bufio.NewReaderSize(resp.Body, 64*1024)

	for {
		wdReset()
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if ttft == nil {
				if data, ok := sseDataPayload(line); ok && hasContentDelta(protocol, data) {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
			}
			tapStreamLine(protocol, line, &u)
			if sink != nil {
				sink.Write(line)
			}
			if _, werr := w.Write(line); werr != nil {
				return u, ttft, werr // client went away; stop quietly
			}
			if bytes.Contains(line, []byte("\n")) && flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if isClientCanceled(r) {
				return u, ttft, errClientCanceled
			}
			if wdCtx.Err() != nil {
				slog.Warn("upstream stream idle timeout")
				return u, ttft, errUpstreamIdleTimeout
			}
			if err == io.EOF {
				return u, ttft, nil
			}
			slog.Warn("upstream stream read failed", "err", err)
			return u, ttft, err
		}
	}
}

// idleWatchdog cancels the derived context when reset() has not been called
// within d. Used to bound upstream silence on streams.
func idleWatchdog(parent context.Context, d time.Duration) (ctx context.Context, reset func(), stop func()) {
	ctx, cancel := context.WithCancel(parent)
	timer := time.AfterFunc(d, cancel)
	return ctx,
		func() { timer.Reset(d) },
		func() { timer.Stop(); cancel() }
}

func isClientCanceled(r *http.Request) bool {
	return r.Context().Err() != nil
}

var (
	errClientCanceled      = errText("client canceled")
	errUpstreamIdleTimeout = errText("upstream idle timeout")
)

type errText string

func (e errText) Error() string { return string(e) }

func copyResponseHeaders(dst http.Header, resp *http.Response) {
	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "content-length", "connection", "transfer-encoding":
			continue // recomputed by net/http
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// retryAfter parses the Retry-After header (seconds or HTTP-date).
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
