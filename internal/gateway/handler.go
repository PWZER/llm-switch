package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/engine"
	"github.com/PWZER/llm-switch/internal/stats"
)

const (
	protocolOpenAI    = "openai"
	protocolAnthropic = "anthropic"

	defaultOpenAIChatPath    = "/chat/completions"
	defaultAnthropicChatPath = "/v1/messages"
	embeddingsPath           = "/embeddings"
)

// Gateway is the data-plane service.
type Gateway struct {
	Holder *engine.Holder
	Pool   *KeyPool
	Client *http.Client
	Log    *stats.Logger
}

// New assembles a gateway.
func New(holder *engine.Holder, pool *KeyPool, client *http.Client, logger *stats.Logger) *Gateway {
	return &Gateway{Holder: holder, Pool: pool, Client: client, Log: logger}
}

type ctxKey int

const clientKeyCtxKey ctxKey = 0

// WithClientKey attaches the authenticated client key to the request context.
func WithClientKey(ctx context.Context, key engine.ClientKey) context.Context {
	return context.WithValue(ctx, clientKeyCtxKey, key)
}

// ClientKeyFrom returns the authenticated client key, if any.
func ClientKeyFrom(ctx context.Context) (engine.ClientKey, bool) {
	if v, ok := ctx.Value(clientKeyCtxKey).(engine.ClientKey); ok {
		return v, true
	}
	return engine.ClientKey{}, false
}

// ValidateClientKey returns a snapshot-based validator for the auth middleware.
// The lookup is a map hit on the pinned snapshot: no DB access on the hot path.
func ValidateClientKey(holder *engine.Holder) func(key string) (engine.ClientKey, bool) {
	return func(key string) (engine.ClientKey, bool) {
		if key == "" {
			return engine.ClientKey{}, false
		}
		snap := holder.Load()
		if snap == nil {
			return engine.ClientKey{}, false
		}
		ck, ok := snap.ClientKeys[engine.HashKey(key)]
		return ck, ok
	}
}

// Mount registers the data-plane routes (relative to the mount point, /v1).
// The caller is responsible for wrapping them with client-key auth.
func (g *Gateway) Mount(r chi.Router) {
	r.Post("/chat/completions", g.serve(protocolOpenAI))
	r.Post("/messages", g.serve(protocolAnthropic))
	r.Post("/messages/count_tokens", g.countTokens)
	r.Post("/embeddings", g.embeddings)
	r.Get("/models", g.models)
}

// serve is the shared pipeline for both protocol surfaces.
func (g *Gateway) serve(clientProtocol string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeProtocolError(w, r, clientProtocol, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
			return
		}
		model, stream, err := extractModelStream(body)
		if err != nil || model == "" {
			writeProtocolError(w, r, clientProtocol, http.StatusBadRequest, "invalid_request_error", `missing or invalid "model" field`)
			return
		}

		snap := g.Holder.Load()
		if snap == nil {
			writeProtocolError(w, r, clientProtocol, http.StatusServiceUnavailable, "api_error", "gateway is starting")
			return
		}
		ck, _ := ClientKeyFrom(r.Context())
		cands, _, found := snap.Resolve(model)
		if !found {
			g.recordFailure(r, start, ck, model, "", clientProtocol, stream, http.StatusNotFound, "model_not_found", 1)
			writeModelNotFound(w, r, clientProtocol, model)
			return
		}

		maxAttempts := snap.SettingInt("max_failover_attempts", 3)
		if maxAttempts < 1 {
			maxAttempts = 1
		}
		if maxAttempts > len(cands) {
			maxAttempts = len(cands)
		}

		var (
			lastStatus   int
			lastErrBody  []byte
			lastErrType  string
			lastUpstream string
		)
		for i := 0; i < maxAttempts; i++ {
			cand := cands[i]
			prov := cand.Channel.Provider
			key, err := g.Pool.Pick(prov)
			if err != nil {
				g.recordFailure(r, start, ck, model, cand.UpstreamModel, clientProtocol, stream,
					http.StatusServiceUnavailable, "no_keys", i+1)
				writeProtocolError(w, r, clientProtocol, http.StatusServiceUnavailable,
					"api_error", "provider "+prov.Name+" has no enabled api keys")
				return
			}
			lastUpstream = cand.UpstreamModel

			upBody, perr := prepareUpstreamBody(body, clientProtocol, cand.Channel.Protocol, cand.UpstreamModel)
			if perr != nil {
				g.recordFailure(r, start, ck, model, cand.UpstreamModel, clientProtocol, stream,
					http.StatusBadRequest, "invalid_request", i+1)
				status := http.StatusBadRequest
				_, isBad := perr.(errBadRequest)
				if !isBad {
					status = http.StatusInternalServerError
				}
				writeProtocolError(w, r, clientProtocol, status, "invalid_request_error", perr.Error())
				return
			}

			resp, err := g.dispatch(r, cand, key.Secret, upBody, stream, clientProtocol)
			if err != nil {
				g.Pool.Report(key.ID, OutcomeServerError, 0)
				lastStatus, lastErrType = http.StatusBadGateway, "network_error"
				slog.Info("upstream attempt failed", "channel", cand.Channel.Name, "err", err)
				continue
			}

			switch classifyStatus(resp.StatusCode) {
			case statusRetriable:
				ra := retryAfter(resp.Header)
				if eb := captureUpstreamError(resp); len(eb) > 0 {
					lastErrBody = eb
				}
				outcome := OutcomeServerError
				errType := "upstream_5xx"
				if resp.StatusCode == http.StatusTooManyRequests {
					outcome = OutcomeRateLimited
					errType = "rate_limit"
				} else if resp.StatusCode == http.StatusRequestTimeout {
					errType = "timeout"
				}
				g.Pool.Report(key.ID, outcome, ra)
				lastStatus, lastErrType = resp.StatusCode, errType
				slog.Info("upstream retriable failure", "channel", cand.Channel.Name, "status", resp.StatusCode)
				continue
			case statusAuth:
				if eb := captureUpstreamError(resp); len(eb) > 0 {
					lastErrBody = eb
				}
				g.Pool.Report(key.ID, OutcomeAuthError, 0)
				lastStatus, lastErrType = resp.StatusCode, "auth_error"
				continue
			default:
				// Committed: 2xx relayed, non-retriable 4xx/5xx passed through.
				g.commit(w, r, start, resp, cand, key, ck, model,
					clientProtocol, stream, lastStatus, lastErrType, i+1)
				return
			}
		}

		// Every candidate failed before the first byte: synthesize an error.
		status := lastStatus
		if status == 0 || status < 400 {
			status = http.StatusBadGateway
		}
		g.recordFailure(r, start, ck, model, lastUpstream, clientProtocol, stream, status, orDefault(lastErrType, "upstream_error"), maxAttempts)
		writeUpstreamError(w, r, clientProtocol, status, lastErrBody)
	}
}

// dispatch sends the prepared upstream request. Auth injected per channel
// style; hop-by-hop headers stripped.
func (g *Gateway) dispatch(r *http.Request, cand engine.Candidate, secret string,
	body []byte, stream bool, clientProtocol string) (*http.Response, error) {

	ch := cand.Channel
	url := joinURL(ch.BaseURL, chatPath(ch))
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytesReader(body))
	if err != nil {
		return nil, err
	}
	setUpstreamHeaders(req, r, ch, secret, clientProtocol, stream, len(body))
	return g.Client.Do(req)
}

// commit relays a successful (or non-retriable) upstream response to the client
// and records the request log entry.
func (g *Gateway) commit(w http.ResponseWriter, r *http.Request, start time.Time,
	resp *http.Response, cand engine.Candidate, key engine.Key, ck engine.ClientKey,
	model, clientProtocol string, stream bool,
	prevStatus int, prevErrType string, attempts int) {

	var (
		usage      usageInfo
		ttft       *int64
		err        error
		converting = cand.Channel.Protocol != clientProtocol
	)
	switch {
	case stream && resp.StatusCode < 400 && converting:
		usage, ttft, err = relayConvertedStream(w, r, resp,
			cand.Channel.Protocol, clientProtocol, model, g.idleTimeout())
	case stream && resp.StatusCode < 400:
		usage, ttft, err = relayStream(w, r, resp, cand.Channel.Protocol, g.idleTimeout())
	case converting:
		usage, err = relayConvertedNonStream(w, r, resp, cand.Channel.Protocol, clientProtocol, model)
	default:
		usage, err = relayNonStream(w, r, resp, cand.Channel.Protocol)
	}

	success := err == nil && resp.StatusCode < 400
	errType := errorTypeFor(err, resp.StatusCode)
	if prevErrType != "" && !success {
		errType = prevErrType + "+" + errType
	}
	_ = prevStatus
	g.logRequest(r, start, ck, model, cand, clientProtocol, stream,
		resp.StatusCode, success, errType, attempts, usage, ttft)
	g.Pool.Report(key.ID, OutcomeOK, 0)
}

// — /v1/models ------------------------------------------------------------

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	snap := g.Holder.Load()
	if snap == nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusServiceUnavailable, "api_error", "gateway is starting")
		return
	}
	anthropicShape := r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != ""
	type openaiModel struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	type anthropicModel struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	}
	nowS := time.Now().Unix()
	nowRFC := time.Now().UTC().Format(time.RFC3339)
	data := snap.Models
	if anthropicShape {
		out := struct {
			Data    []anthropicModel `json:"data"`
			FirstID string           `json:"first_id"`
			HasMore bool             `json:"has_more"`
		}{Data: make([]anthropicModel, 0, len(data))}
		for i, m := range data {
			out.Data = append(out.Data, anthropicModel{Type: "model", ID: m.ID, DisplayName: orDefault(m.DisplayName, m.ID), CreatedAt: nowRFC})
			if i == 0 {
				out.FirstID = m.ID
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	out := struct {
		Object string        `json:"object"`
		Data   []openaiModel `json:"data"`
	}{Object: "list", Data: make([]openaiModel, 0, len(data))}
	for _, m := range data {
		out.Data = append(out.Data, openaiModel{ID: m.ID, Object: "model", Created: nowS, OwnedBy: "llm-switch"})
	}
	writeJSON(w, http.StatusOK, out)
}

// — count_tokens ----------------------------------------------------------

func (g *Gateway) countTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeProtocolError(w, r, protocolAnthropic, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model, _, err := extractModelStream(body)
	if err != nil || model == "" {
		writeProtocolError(w, r, protocolAnthropic, http.StatusBadRequest, "invalid_request_error", `missing "model"`)
		return
	}
	snap := g.Holder.Load()
	if snap == nil {
		writeProtocolError(w, r, protocolAnthropic, http.StatusServiceUnavailable, "api_error", "gateway is starting")
		return
	}
	cands, _, found := snap.Resolve(model)
	if !found {
		writeModelNotFound(w, r, protocolAnthropic, model)
		return
	}
	cand := cands[0]

	if cand.Channel.Protocol == protocolAnthropic {
		// Native upstream: forward to its count_tokens endpoint.
		key, err := g.Pool.Pick(cand.Channel.Provider)
		if err != nil {
			writeProtocolError(w, r, protocolAnthropic, http.StatusServiceUnavailable, "api_error", "no enabled api keys")
			return
		}
		url := joinURL(cand.Channel.BaseURL, orDefault(cand.Channel.ChatPath, "/v1/messages"))
		url = strings.TrimSuffix(url, "/messages") + "/count_tokens"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytesReader(rewriteModel(body, cand.UpstreamModel)))
		if err != nil {
			writeProtocolError(w, r, protocolAnthropic, http.StatusInternalServerError, "api_error", err.Error())
			return
		}
		setUpstreamHeaders(req, r, cand.Channel, key.Secret, protocolAnthropic, false, len(body))
		resp, err := g.Client.Do(req)
		if err != nil {
			writeProtocolError(w, r, protocolAnthropic, http.StatusBadGateway, "api_error", "upstream unreachable")
			return
		}
		defer resp.Body.Close()
		// Never hard-fail count_tokens: fall back to the estimate on any
		// non-2xx upstream answer.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}
		drainAndClose(resp)
	}
	// OpenAI upstream (or forward failed): documented approximation.
	n := estimateTokens(body)
	writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": n})
}

// — embeddings ------------------------------------------------------------

func (g *Gateway) embeddings(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model, _, err := extractModelStream(body)
	if err != nil || model == "" {
		writeProtocolError(w, r, protocolOpenAI, http.StatusBadRequest, "invalid_request_error", `missing "model"`)
		return
	}
	snap := g.Holder.Load()
	if snap == nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusServiceUnavailable, "api_error", "gateway is starting")
		return
	}
	cands, _, found := snap.Resolve(model)
	if !found {
		writeModelNotFound(w, r, protocolOpenAI, model)
		return
	}
	var cand *engine.Candidate
	for i := range cands {
		if cands[i].Channel.Protocol == protocolOpenAI && cands[i].Channel.SupportsEmbeddings {
			cand = &cands[i]
			break
		}
	}
	if cand == nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusNotFound,
			"invalid_request_error", "no embeddings-capable channel serves model "+model)
		return
	}
	key, err := g.Pool.Pick(cand.Channel.Provider)
	if err != nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusServiceUnavailable, "api_error", "no enabled api keys")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		joinURL(cand.Channel.BaseURL, embeddingsPath), bytesReader(rewriteModel(body, cand.UpstreamModel)))
	if err != nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	setUpstreamHeaders(req, r, cand.Channel, key.Secret, protocolOpenAI, false, len(body))
	resp, err := g.Client.Do(req)
	if err != nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusBadGateway, "api_error", "upstream unreachable")
		return
	}
	usage, rerr := relayNonStream(w, r, resp, protocolOpenAI)
	ck, _ := ClientKeyFrom(r.Context())
	g.logRequest(r, start, ck, model, *cand, protocolOpenAI, false,
		resp.StatusCode, rerr == nil && resp.StatusCode < 400, errorTypeFor(rerr, resp.StatusCode), 1, usage, nil)
}

// — helpers ---------------------------------------------------------------

func (g *Gateway) idleTimeout() time.Duration {
	snap := g.Holder.Load()
	if snap == nil {
		return 300 * time.Second
	}
	return time.Duration(snap.SettingInt("stream_idle_timeout_s", 300)) * time.Second
}

// chatPath resolves the explicit upstream path, defaulting per protocol.
func chatPath(ch *engine.Channel) string {
	if p := orDefault(ch.ChatPath, ""); p != "" {
		if !strings.HasPrefix(p, "/") {
			return "/" + p
		}
		return p
	}
	if ch.Protocol == protocolAnthropic {
		return defaultAnthropicChatPath
	}
	return defaultOpenAIChatPath
}

func joinURL(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

// rewriteModel rewrites only the "model" field; every other byte of the JSON
// object survives untouched (unknown fields preserved).
func rewriteModel(body []byte, upstreamModel string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body // not an object: forward as-is
	}
	nm, err := json.Marshal(upstreamModel)
	if err != nil {
		return body
	}
	m["model"] = nm
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func extractModelStream(body []byte) (model string, stream bool, err error) {
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false, err
	}
	return probe.Model, probe.Stream, nil
}

// setUpstreamHeaders applies header hygiene: strip client auth, set upstream
// auth per channel style, forward protocol headers selectively, merge
// extra_headers last.
func setUpstreamHeaders(req *http.Request, clientReq *http.Request, ch *engine.Channel,
	secret, clientProtocol string, stream bool, contentLen int) {

	h := req.Header
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	}
	if ch.AuthStyle == "x-api-key" {
		h.Set("x-api-key", secret)
		if ch.Protocol == protocolAnthropic {
			h.Set("anthropic-version", "2023-06-01")
		}
	} else {
		h.Set("Authorization", "Bearer "+secret)
	}
	// Protocol-scoped passthrough: anthropic-beta only to Anthropic upstreams.
	if ch.Protocol == protocolAnthropic {
		if beta := clientReq.Header.Get("anthropic-beta"); beta != "" {
			h.Set("anthropic-beta", beta)
		}
	}
	for k, v := range ch.ExtraHeaders {
		h.Set(k, v)
	}
	req.ContentLength = int64(contentLen)
	req.Host = "" // derived from URL
}

type statusKind int

const (
	statusOK statusKind = iota
	statusAuth
	statusRetriable
)

func classifyStatus(code int) statusKind {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return statusAuth
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests ||
		code >= 500:
		return statusRetriable
	default:
		return statusOK
	}
}

func drainAndClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

func errorTypeFor(err error, status int) string {
	switch {
	case errors.Is(err, errClientCanceled):
		return "client_canceled"
	case errors.Is(err, errUpstreamIdleTimeout):
		return "idle_timeout"
	case err != nil:
		return "network_error"
	case status >= 500:
		return "upstream_5xx"
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "auth_error"
	case status >= 400:
		return "client_error"
	default:
		return ""
	}
}

func (g *Gateway) recordFailure(r *http.Request, start time.Time, ck engine.ClientKey,
	model, upstreamModel, protocolIn string, stream bool, status int, errType string, attempts int) {

	cand := engine.Candidate{UpstreamModel: upstreamModel}
	g.logRequest(r, start, ck, model, cand, protocolIn, stream, status, false, errType, attempts, usageInfo{}, nil)
}

func (g *Gateway) logRequest(r *http.Request, start time.Time, ck engine.ClientKey,
	model string, cand engine.Candidate, protocolIn string, stream bool,
	status int, success bool, errType string, attempts int, usage usageInfo, ttft *int64) {

	entry := storeLogEntry(r, start, ck, model, cand, protocolIn, stream,
		status, success, errType, attempts, usage, ttft)
	if g.Log != nil {
		g.Log.Log(entry)
	}
}

// estimateTokens approximates the Anthropic count_tokens response for OpenAI
// upstreams: ~chars/4 Latin-heavy, ~chars/1.6 CJK-heavy, plus per-message overhead.
func estimateTokens(body []byte) int64 {
	runs, cjk := 0, 0
	for _, rn := range string(body) {
		runs++
		if rn >= 0x2E80 { // CJK and friends
			cjk++
		}
	}
	latin := runs - cjk
	tokens := int64(latin)/4 + int64(cjk)/2 + int64(strings.Count(string(body), "\"role\""))*4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func orDefault(s, fb string) string {
	if s == "" {
		return fb
	}
	return s
}
