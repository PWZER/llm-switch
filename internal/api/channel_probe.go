package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/ir"
	"github.com/PWZER/llm-switch/internal/protocol"
	"github.com/PWZER/llm-switch/internal/store"
)

// testHTTPClient caps live endpoint tests at 30s. Non-stream only: a full
// tiny round trip is what "connection test" means here.
var testHTTPClient = &http.Client{Timeout: 30 * time.Second}

// handleTestChannel sends a minimal real request through one endpoint and
// reports success plus a timing breakdown (DNS / TCP / TLS / first byte /
// total). Used by the UI "Test" button on endpoint cards.
func (s *Server) handleTestChannel(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	ch, err := s.St.Channels.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	keys, err := s.St.Keys.EnabledKeys(req.Context(), ch.ProviderID)
	if err != nil || len(keys) == 0 {
		httpx.WriteEnvelopeError(w, req, http.StatusConflict, 40902, "provider has no enabled api key")
		return
	}
	key := keys[0] // rotate across tests: pick[0] each time is fine for a manual probe
	if len(keys) > 1 {
		key = keys[int(time.Now().UnixNano())%len(keys)]
	}

	// Target model resolution order: explicit body model -> first binding ->
	// first model fetched from this provider. With none of those, fall back to
	// a zero-cost models-list probe so a fresh endpoint is testable immediately.
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&body)
	model := body.Model
	if model == "" && len(ch.Models) > 0 {
		model = ch.Models[0].UpstreamModel
	}
	if model == "" {
		if cached, err := s.St.Models.List(req.Context()); err == nil {
			source := fmt.Sprintf("provider:%d", ch.ProviderID)
			for _, m := range cached {
				if m.Enabled && m.Source == source {
					model = m.ID
					break
				}
			}
		}
	}
	if model == "" {
		result := s.probeModelsList(req.Context(), &ch, []byte(key.APIKey))
		result.Model = ""
		httpx.WriteEnvelope(w, req, result)
		return
	}

	// Minimal live request through the channel's own codec (max_tokens=1 keeps cost tiny).
	codec, err := protocol.For(ir.Protocol(ch.Protocol))
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
		return
	}
	payload, err := codec.EncodeRequest(&ir.Request{
		Model:     model,
		MaxTokens: 1,
		Messages: []ir.Message{{
			Role:   ir.RoleUser,
			Blocks: []ir.Block{{Type: ir.BlockText, Text: "ping"}},
		}},
	})
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusInternalServerError, 50001, err.Error())
		return
	}

	result := s.probeEndpoint(req.Context(), &ch, []byte(key.APIKey), payload, model)
	httpx.WriteEnvelope(w, req, result)
}

// endpointTestResult is the payload returned to the UI.
type endpointTestResult struct {
	OK          bool   `json:"ok"`
	Mode        string `json:"mode"` // "chat" | "models_list"
	Status      int    `json:"status"` // HTTP status; 0 when the request failed earlier
	Error       string `json:"error,omitempty"`
	Model       string `json:"model"`
	Entries     int    `json:"entries,omitempty"` // models_list mode: entries returned
	KeyMask     string `json:"key_mask"`
	DNSMS       int64  `json:"dns_ms"`
	ConnectMS   int64  `json:"connect_ms"`
	TLSMS       int64  `json:"tls_ms"`
	FirstByteMS int64  `json:"first_byte_ms"`
	TotalMS     int64  `json:"total_ms"`
}

// probeEndpoint performs the timed live chat request.
func (s *Server) probeEndpoint(ctx context.Context, ch *store.Channel, secret []byte, payload []byte, model string) endpointTestResult {
	res := endpointTestResult{Mode: "chat", Model: model, KeyMask: store.MaskKey(string(secret))}

	url := strings.TrimSuffix(ch.BaseURL, "/") + "/" + strings.TrimPrefix(chatPathOf(ch), "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if ch.AuthStyle == "x-api-key" {
		httpReq.Header.Set("x-api-key", string(secret))
		if ch.Protocol == "anthropic" {
			httpReq.Header.Set("anthropic-version", "2023-06-01")
		}
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+string(secret))
	}
	httpReq.Header.Set("Content-Type", "application/json")

	var (
		dnsStart, dnsDone     time.Time
		connStart, connDone   time.Time
		tlsStart, tlsDone     time.Time
		firstByte             time.Time
	)
	start := time.Now()
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		ConnectStart:         func(string, string) { connStart = time.Now() },
		ConnectDone:          func(string, string, error) { connDone = time.Now() },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { tlsDone = time.Now() },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	httpReq = httpReq.WithContext(httptrace.WithClientTrace(ctx, trace))

	resp, err := testHTTPClient.Do(httpReq)
	if err != nil {
		res.Error = err.Error()
		res.TotalMS = time.Since(start).Milliseconds()
		return res
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	res.Status = resp.StatusCode
	res.TotalMS = time.Since(start).Milliseconds()

	if !dnsStart.IsZero() && !dnsDone.IsZero() {
		res.DNSMS = dnsDone.Sub(dnsStart).Milliseconds()
	}
	if !connStart.IsZero() && !connDone.IsZero() {
		res.ConnectMS = connDone.Sub(connStart).Milliseconds()
	}
	if !tlsStart.IsZero() && !tlsDone.IsZero() {
		res.TLSMS = tlsDone.Sub(tlsStart).Milliseconds()
	}
	if !firstByte.IsZero() {
		res.FirstByteMS = firstByte.Sub(start).Milliseconds()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Preserve the upstream error body (trimmed) for diagnosis.
		msg := strings.TrimSpace(string(respBody))
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		res.Error = fmt.Sprintf("upstream %d: %s", resp.StatusCode, msg)
		return res
	}
	res.OK = true
	return res
}

// probeModelsList is the zero-cost fallback connectivity test: GET the
// endpoint's model list instead of running a chat completion (no tokens spent).
func (s *Server) probeModelsList(ctx context.Context, ch *store.Channel, secret []byte) endpointTestResult {
	res := endpointTestResult{Mode: "models_list", KeyMask: store.MaskKey(string(secret))}

	base := strings.TrimSuffix(ch.BaseURL, "/")
	modelsPath := "/models"
	if ch.Protocol == "anthropic" {
		modelsPath = "/v1/models"
	}
	if ch.ModelsURL != nil && *ch.ModelsURL != "" {
		modelsPath = *ch.ModelsURL
		if !strings.HasPrefix(modelsPath, "/") {
			modelsPath = "/" + modelsPath
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+modelsPath, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if ch.AuthStyle == "x-api-key" {
		httpReq.Header.Set("x-api-key", string(secret))
		if ch.Protocol == "anthropic" {
			httpReq.Header.Set("anthropic-version", "2023-06-01")
		}
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+string(secret))
	}

	start := time.Now()
	var firstByte time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	resp, err := testHTTPClient.Do(httpReq.WithContext(httptrace.WithClientTrace(ctx, trace)))
	if err != nil {
		res.Error = err.Error()
		res.TotalMS = time.Since(start).Milliseconds()
		return res
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	res.Status = resp.StatusCode
	res.TotalMS = time.Since(start).Milliseconds()
	if !firstByte.IsZero() {
		res.FirstByteMS = firstByte.Sub(start).Milliseconds()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(respBody))
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		res.Error = fmt.Sprintf("upstream %d: %s", resp.StatusCode, msg)
		return res
	}
	// Both OpenAI and Anthropic shapes share the data[].id envelope.
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &list); err == nil {
		res.Entries = len(list.Data)
	}
	res.OK = true
	return res
}

// chatPathOf resolves the endpoint's upstream path (defaults per protocol).
func chatPathOf(ch *store.Channel) string {
	if ch.ChatPath != "" {
		if !strings.HasPrefix(ch.ChatPath, "/") {
			return "/" + ch.ChatPath
		}
		return ch.ChatPath
	}
	if ch.Protocol == "anthropic" {
		return "/v1/messages"
	}
	return "/chat/completions"
}
