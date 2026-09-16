package gateway

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/PWZER/llm-switch/internal/engine"
	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

// writeProtocolError renders an error in the client's protocol shape.
func writeProtocolError(w http.ResponseWriter, r *http.Request, protocol string, status int, errType, msg string) {
	writeJSON(w, status, protocolErrorBody(protocol, errType, msg))
}

func protocolErrorBody(protocol, errType, msg string) map[string]any {
	if protocol == protocolAnthropic {
		return map[string]any{
			"type":  "error",
			"error": map[string]any{"type": orDefault(errType, "api_error"), "message": msg},
		}
	}
	return map[string]any{
		"error": map[string]any{"message": msg, "type": orDefault(errType, "api_error")},
	}
}

// writeModelNotFound returns the 404 shape each SDK expects, with an
// actionable hint: model_not_found always means routing has no match.
func writeModelNotFound(w http.ResponseWriter, r *http.Request, protocol, model string) {
	hint := " (no model route or registered model serves this name — register the model on a provider or create a model route)"
	if protocol == protocolAnthropic {
		writeProtocolError(w, r, protocol, http.StatusNotFound, "not_found_error", "model: "+model+hint)
		return
	}
	writeProtocolError(w, r, protocol, http.StatusNotFound, "invalid_request_error", "model "+model+" not found"+hint)
}

// writeUpstreamError forwards the last upstream error body when available;
// otherwise it synthesizes a protocol-shaped error.
func writeUpstreamError(w http.ResponseWriter, r *http.Request, protocol string, status int, body []byte) {
	if len(body) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	writeProtocolError(w, r, protocol, status, "api_error", "all upstream channels failed")
}

// captureUpstreamError reads (bounded) and closes an upstream body so the
// failover-exhausted path can pass the last error through verbatim.
func captureUpstreamError(resp *http.Response) []byte {
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	return body
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// storeLogEntry assembles the request_logs row. cand.Channel may be nil when
// routing never reached an upstream (model not found, no accounts, all failed);
// acc is nil on those same early-failure paths.
func storeLogEntry(r *http.Request, start time.Time, ck engine.ClientKey,
	model string, cand engine.Candidate, acc *engine.Account, protocolIn string, stream bool,
	status int, success bool, errType string, attempts int, usage usageInfo, ttft *int64) store.RequestLog {

	entry := store.RequestLog{
		TS:               time.Now().UnixMilli(),
		RequestID:        httpx.RequestID(r),
		APIKeyName:       ck.Name,
		Model:            model,
		UpstreamModel:    cand.UpstreamModel,
		ProtocolIn:       protocolIn,
		ProtocolOut:      protocolIn,
		Stream:           stream,
		Status:           status,
		Success:          success,
		Attempts:         attempts,
		PromptTokens:     usage.Prompt,
		CompletionTokens: usage.Completion,
		CacheReadTokens:  usage.CacheRead,
		CacheWriteTokens: usage.CacheWrite,
		ReasoningTokens:  usage.Reasoning,
		LatencyMS:        time.Since(start).Milliseconds(),
		FirstTokenMS:     ttft,
	}
	if ck.ID != 0 {
		id := ck.ID
		entry.APIKeyID = &id
	}
	if acc != nil {
		aid := acc.ID
		entry.AccountID = &aid
		entry.AccountName = acc.DisplayName()
	}
	if c := cand.Channel; c != nil {
		cid := c.ID
		entry.ChannelID = &cid
		entry.ChannelName = c.Name
		if c.Provider != nil {
			pid := c.Provider.ID
			entry.ProviderID = &pid
			entry.ProviderName = c.Provider.Name
		}
		entry.ProtocolOut = c.Protocol
	}
	if errType != "" {
		e := errType
		entry.ErrorType = &e
	}
	return entry
}
