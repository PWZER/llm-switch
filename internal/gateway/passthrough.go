package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	var frame struct {
		Type  string `json:"type"`
		Usage *struct {
			InputTokens  *int64 `json:"input_tokens"`
			OutputTokens *int64 `json:"output_tokens"`
			PromptTokens *int64 `json:"prompt_tokens"`
			// OpenAI names completion tokens differently from Anthropic.
			CompletionTokens *int64 `json:"completion_tokens"`
			// OpenAI modern + detail forms
			PromptTokensDetails     *struct {
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
		// Anthropic message_start: input + cache read/write.
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
	case frame.Type == "message_delta" && frame.Usage != nil:
		// Anthropic message_delta: final output tokens.
		if frame.Usage.OutputTokens != nil {
			u.Completion = *frame.Usage.OutputTokens
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

// relayNonStream forwards a buffered upstream response to the client.
func relayNonStream(w http.ResponseWriter, r *http.Request, resp *http.Response, protocol string) (usageInfo, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return usageInfo{}, err
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
// arrive within idleTimeout.
func relayStream(w http.ResponseWriter, r *http.Request, resp *http.Response, protocol string, idleTimeout time.Duration) (usageInfo, *int64, error) {
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
	start := time.Now()

	wdCtx, wdReset, wdStop := idleWatchdog(r.Context(), idleTimeout)
	defer wdStop()
	reader := bufio.NewReaderSize(resp.Body, 64*1024)

	for {
		wdReset()
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if ttft == nil {
				ms := time.Since(start).Milliseconds()
				ttft = &ms
			}
			tapStreamLine(protocol, line, &u)
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
				return u, ttft, errUpstreamIdleTimeout
			}
			if err == io.EOF {
				return u, ttft, nil
			}
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
