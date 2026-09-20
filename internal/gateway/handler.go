package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PWZER/llm-switch/internal/engine"
	"github.com/PWZER/llm-switch/internal/protocol/responses"
	"github.com/PWZER/llm-switch/internal/stats"
)

const (
	protocolOpenAI    = "openai"
	protocolAnthropic = "anthropic"
	// protocolOpenAIResponses is the client-facing Responses API surface
	// (POST /v1/responses). Channels never carry this value: Responses traffic
	// either passes through to an openai channel's responses_path or is
	// bridged to the channel's chat protocol.
	protocolOpenAIResponses = "openai-responses"

	defaultOpenAIChatPath    = "/chat/completions"
	defaultAnthropicChatPath = "/v1/messages"
	embeddingsPath           = "/embeddings"
)

// surfaceProtocol maps a client surface to the upstream wire protocol it
// prefers: the Responses surface is served by openai channels.
func surfaceProtocol(clientProtocol string) string {
	if clientProtocol == protocolOpenAIResponses {
		return protocolOpenAI
	}
	return clientProtocol
}

// Gateway is the data-plane service.
type Gateway struct {
	Holder *engine.Holder
	Pool   *AccountPool
	Client *http.Client
	Log    *stats.Logger
}

// New assembles a gateway.
func New(holder *engine.Holder, pool *AccountPool, client *http.Client, logger *stats.Logger) *Gateway {
	return &Gateway{Holder: holder, Pool: pool, Client: client, Log: logger}
}

// errAccountCooling means a route-pinned account is in cooldown; the caller
// should fail over to the next candidate.
var errAccountCooling = errors.New("pinned account is cooling down")

// pickAccount resolves the credential for one candidate: a route-pinned
// account is used verbatim (unless cooling), otherwise the provider's account
// pool rotates.
func (g *Gateway) pickAccount(cand engine.Candidate) (engine.Account, error) {
	if cand.Account != nil {
		if g.Pool.Cooling(cand.Account.ID) {
			return engine.Account{}, errAccountCooling
		}
		return *cand.Account, nil
	}
	return g.Pool.Pick(cand.Channel.Provider)
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
	r.Post("/responses", g.serve(protocolOpenAIResponses))
	r.Post("/messages", g.serve(protocolAnthropic))
	r.Post("/messages/count_tokens", g.countTokens)
	r.Post("/embeddings", g.embeddings)
	r.Get("/models", g.models)
	r.Get("/models/{modelID}", g.modelByID)
}

// serve is the shared pipeline for both protocol surfaces.
func (g *Gateway) serve(clientProtocol string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			slog.Warn("gateway rejected request", "reason", "read body", "err", err)
			writeProtocolError(w, r, clientProtocol, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
			return
		}
		model, stream, err := extractModelStream(body)
		if err != nil || model == "" {
			slog.Warn("gateway rejected request", "reason", "missing model field")
			writeProtocolError(w, r, clientProtocol, http.StatusBadRequest, "invalid_request_error", `missing or invalid "model" field`)
			return
		}
		if clientProtocol == protocolOpenAIResponses {
			// Stateless-only gate lives here (not in the codec) because the
			// passthrough path never decodes the body.
			if verr := responses.ValidateStateless(body); verr != nil {
				writeProtocolError(w, r, clientProtocol, http.StatusBadRequest, "invalid_request_error", verr.Error())
				return
			}
		}

		snap := g.Holder.Load()
		if snap == nil {
			writeProtocolError(w, r, clientProtocol, http.StatusServiceUnavailable, "api_error", "gateway is starting")
			return
		}
		ck, _ := ClientKeyFrom(r.Context())
		cands, canonical, found := snap.Resolve(model, surfaceProtocol(clientProtocol))
		// request_logs.model records the canonical resolved identity so
		// decorated discovery ids (claude-* mirrors, [1m] markers) coalesce;
		// client-facing strings keep the raw name.
		logModel := orDefault(canonical, model)
		if !found {
			slog.Warn("model not found",
				"model", model, "protocol", clientProtocol, "client_key", ck.Name)
			g.recordFailure(r, start, ck, logModel, "", clientProtocol, stream, http.StatusNotFound, "model_not_found", 1, nil)
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
			lastAccount  *engine.Account
		)
		for i := 0; i < maxAttempts; i++ {
			cand := cands[i]
			prov := cand.Channel.Provider
			account, err := g.pickAccount(cand)
			if errors.Is(err, errAccountCooling) {
				slog.Warn("route-pinned account cooling, failing over", "channel", cand.Channel.Name,
					"provider", prov.Name, "account_id", cand.Account.ID, "model", model, "attempt", i+1)
				lastStatus, lastErrType = http.StatusServiceUnavailable, "account_cooldown"
				lastUpstream = cand.UpstreamModel
				continue
			}
			if err != nil {
				g.recordFailure(r, start, ck, logModel, cand.UpstreamModel, clientProtocol, stream,
					http.StatusServiceUnavailable, "no_accounts", i+1, nil)
				writeProtocolError(w, r, clientProtocol, http.StatusServiceUnavailable,
					"api_error", "provider "+prov.Name+" has no enabled accounts")
				return
			}
			lastUpstream = cand.UpstreamModel
			acc := account
			lastAccount = &acc

			ptPath := responsesPassthroughPath(clientProtocol, cand.Channel)
			upPath := chatPath(cand.Channel)
			if ptPath != "" {
				upPath = ptPath
			}
			upBody, perr := prepareUpstreamBody(body, clientProtocol, cand.Channel.Protocol, cand.UpstreamModel, stream, ptPath != "")
			if perr != nil {
				slog.Warn("request preparation failed", "channel", cand.Channel.Name,
					"model", model, "err", perr)
				g.recordFailure(r, start, ck, logModel, cand.UpstreamModel, clientProtocol, stream,
					http.StatusBadRequest, "invalid_request", i+1, lastAccount)
				status := http.StatusBadRequest
				_, isBad := perr.(errBadRequest)
				if !isBad {
					status = http.StatusInternalServerError
				}
				writeProtocolError(w, r, clientProtocol, status, "invalid_request_error", perr.Error())
				return
			}

			resp, err := g.dispatch(r, cand, account.Secret, upBody, stream, clientProtocol, upPath)
			if err != nil {
				slog.Warn("upstream attempt failed", "channel", cand.Channel.Name,
					"provider", prov.Name, "account_id", account.ID, "model", model, "attempt", i+1, "err", err)
				g.Pool.Report(account.ID, OutcomeServerError, 0)
				lastStatus, lastErrType = http.StatusBadGateway, "network_error"
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
				slog.Warn("upstream retriable failure", "channel", cand.Channel.Name,
					"provider", prov.Name, "account_id", account.ID, "model", model, "attempt", i+1,
					"status", resp.StatusCode, "retry_after", ra.String())
				g.Pool.Report(account.ID, outcome, ra)
				lastStatus, lastErrType = resp.StatusCode, errType
				continue
			case statusAuth:
				slog.Warn("upstream rejected credentials", "channel", cand.Channel.Name,
					"provider", prov.Name, "account_id", account.ID, "status", resp.StatusCode)
				if eb := captureUpstreamError(resp); len(eb) > 0 {
					lastErrBody = eb
				}
				g.Pool.Report(account.ID, OutcomeAuthError, 0)
				lastStatus, lastErrType = resp.StatusCode, "auth_error"
				continue
			default:
				// Committed: 2xx relayed, non-retriable 4xx/5xx passed through.
				g.commit(w, r, start, resp, cand, account, ck, model, logModel,
					clientProtocol, stream, lastStatus, lastErrType, i+1)
				return
			}
		}

		// Every candidate failed before the first byte: synthesize an error.
		status := lastStatus
		if status == 0 || status < 400 {
			status = http.StatusBadGateway
		}
		slog.Error("all upstream candidates failed", "model", model,
			"client_key", ck.Name, "attempts", maxAttempts,
			"last_status", status, "error_type", orDefault(lastErrType, "upstream_error"))
		g.recordFailure(r, start, ck, logModel, lastUpstream, clientProtocol, stream, status, orDefault(lastErrType, "upstream_error"), maxAttempts, lastAccount)
		writeUpstreamError(w, r, clientProtocol, status, lastErrBody)
	}
}

// dispatch sends the prepared upstream request. Auth injected per channel
// style; hop-by-hop headers stripped. upPath overrides the channel chat path
// (used for Responses passthrough).
func (g *Gateway) dispatch(r *http.Request, cand engine.Candidate, secret string,
	body []byte, stream bool, clientProtocol, upPath string) (*http.Response, error) {

	ch := cand.Channel
	url := joinURL(ch.BaseURL, upPath)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytesReader(body))
	if err != nil {
		return nil, err
	}
	setUpstreamHeaders(req, r, ch, secret, clientProtocol, stream, len(body))
	return g.Client.Do(req)
}

// responsesPassthroughPath returns the upstream Responses endpoint when the
// request can be relayed to it byte-wise, else "". Only openai channels with
// an explicit responses_path qualify.
func responsesPassthroughPath(clientProtocol string, ch *engine.Channel) string {
	if clientProtocol != protocolOpenAIResponses || ch.Protocol != protocolOpenAI {
		return ""
	}
	if ch.ResponsesPath == nil || *ch.ResponsesPath == "" {
		return ""
	}
	p := *ch.ResponsesPath
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// commit relays a successful (or non-retriable) upstream response to the client
// and records the request log entry. model seeds the client-facing echo in
// converted relays; logModel (the canonical resolved name) feeds the log.
func (g *Gateway) commit(w http.ResponseWriter, r *http.Request, start time.Time,
	resp *http.Response, cand engine.Candidate, acc engine.Account, ck engine.ClientKey,
	model, logModel, clientProtocol string, stream bool,
	prevStatus int, prevErrType string, attempts int) {

	var (
		usage      usageInfo
		ttft       *int64
		err        error
		converting = cand.Channel.Protocol != clientProtocol
		// Responses passthrough must preempt the converted relays: the
		// upstream body is Responses-shaped even though the channel protocol
		// is "openai", so the usage tap keys on the responses protocol.
		ptPath   = responsesPassthroughPath(clientProtocol, cand.Channel)
		tapProto = cand.Channel.Protocol
	)
	if ptPath != "" {
		tapProto = clientProtocol
	}
	switch {
	case ptPath != "" && stream && resp.StatusCode < 400:
		usage, ttft, err = relayStream(w, r, resp, tapProto, g.idleTimeout())
	case stream && resp.StatusCode < 400 && converting:
		usage, ttft, err = relayConvertedStream(w, r, resp,
			cand.Channel.Protocol, clientProtocol, model, g.idleTimeout())
	case stream && resp.StatusCode < 400:
		usage, ttft, err = relayStream(w, r, resp, tapProto, g.idleTimeout())
	case ptPath != "":
		usage, err = relayNonStream(w, r, resp, tapProto)
	case converting:
		usage, err = relayConvertedNonStream(w, r, resp, cand.Channel.Protocol, clientProtocol, model)
	default:
		usage, err = relayNonStream(w, r, resp, tapProto)
	}

	success := err == nil && resp.StatusCode < 400
	errType := errorTypeFor(err, resp.StatusCode)
	if prevErrType != "" && !success {
		errType = prevErrType + "+" + errType
	}
	_ = prevStatus
	g.logRequest(r, start, ck, logModel, cand, &acc, clientProtocol, stream,
		resp.StatusCode, success, errType, attempts, usage, ttft)
	g.Pool.Report(acc.ID, OutcomeOK, 0)
}

// — /v1/models ------------------------------------------------------------

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	snap := g.Holder.Load()
	if snap == nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusServiceUnavailable, "api_error", "gateway is starting")
		return
	}
	anthropicShape := anthropicShapeRequest(r)
	nowS := time.Now().Unix()
	nowRFC := time.Now().UTC().Format(time.RFC3339)

	type limits struct {
		ContextLength   *int64 `json:"context_length,omitempty"`
		MaxOutputTokens *int64 `json:"max_output_tokens,omitempty"`
	}
	type anthropicModel struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Description string `json:"description,omitempty"`
		CreatedAt   string `json:"created_at"`
		limits
	}
	type openaiModel struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Created     int64  `json:"created"`
		OwnedBy     string `json:"owned_by"`
		Description string `json:"description,omitempty"`
		limits
	}
	// The Anthropic surface additionally lists the [1m] context variants and
	// the claude-* discovery variants (Claude Code's discovery only accepts
	// ids containing claude/anthropic; the [1m] suffix is its 1M-context
	// marker); the OpenAI surface stays with the plain list.
	models := snap.Models
	if anthropicShape {
		models = anthropicListing(snap)
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	}
	page, hasMore := listWindow(models, r.URL.Query())

	if anthropicShape {
		out := struct {
			Data    []anthropicModel `json:"data"`
			FirstID string           `json:"first_id"`
			LastID  string           `json:"last_id"`
			HasMore bool             `json:"has_more"`
		}{Data: make([]anthropicModel, 0, len(page))}
		for i, m := range page {
			out.Data = append(out.Data, anthropicModel{
				Type: "model", ID: m.ID, DisplayName: orDefault(m.DisplayName, m.ID), CreatedAt: nowRFC,
				Description: modelDescription(m),
				limits:      limits{ContextLength: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens},
			})
			if i == 0 {
				out.FirstID = m.ID
			}
			out.LastID = m.ID
		}
		out.HasMore = hasMore
		writeJSON(w, http.StatusOK, out)
		return
	}
	out := struct {
		Object  string        `json:"object"`
		Data    []openaiModel `json:"data"`
		FirstID string        `json:"first_id"`
		LastID  string        `json:"last_id"`
		HasMore bool          `json:"has_more"`
	}{Object: "list", Data: make([]openaiModel, 0, len(page))}
	for i, m := range page {
		out.Data = append(out.Data, openaiModel{
			ID: m.ID, Object: "model", Created: nowS, OwnedBy: "llm-switch",
			Description: modelDescription(m),
			limits:      limits{ContextLength: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens},
		})
		if i == 0 {
			out.FirstID = m.ID
		}
		out.LastID = m.ID
	}
	out.HasMore = hasMore
	writeJSON(w, http.StatusOK, out)
}

// modelByID serves GET /v1/models/{id} in the client's surface shape.
func (g *Gateway) modelByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "modelID")
	snap := g.Holder.Load()
	if snap == nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusServiceUnavailable, "api_error", "gateway is starting")
		return
	}
	models := snap.Models
	if anthropicShapeRequest(r) {
		// anthropicListing copies before extending: the snapshot slices are
		// shared across requests.
		models = anthropicListing(snap)
	}
	for _, m := range models {
		if m.ID != id {
			continue
		}
		if anthropicShapeRequest(r) {
			body := map[string]any{
				"type": "model", "id": m.ID,
				"display_name":      orDefault(m.DisplayName, m.ID),
				"created_at":        time.Now().UTC().Format(time.RFC3339),
				"context_length":    m.ContextWindow,
				"max_output_tokens": m.MaxOutputTokens,
			}
			if m.Provider != "" {
				body["description"] = modelDescription(m)
			}
			writeJSON(w, http.StatusOK, body)
			return
		}
		body := map[string]any{
			"id": m.ID, "object": "model", "created": time.Now().Unix(), "owned_by": "llm-switch",
			"context_length":    m.ContextWindow,
			"max_output_tokens": m.MaxOutputTokens,
		}
		if m.Provider != "" {
			body["description"] = modelDescription(m)
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	proto := protocolOpenAI
	if anthropicShapeRequest(r) {
		proto = protocolAnthropic
	}
	writeModelNotFound(w, r, proto, id)
}

// modelDescription renders the entry description with an explicit source
// marker: "[route]" for model-route entries (a route wins at resolve time
// even when a models row shares the name), "[model]" for provider registry
// rows. An empty base description stays empty (omitempty).
func modelDescription(m engine.ModelEntry) string {
	if m.Provider == "" {
		return ""
	}
	if m.IsRoute {
		return "[route] " + m.Provider
	}
	return "[model] " + m.Provider
}

// anthropicListing merges the plain list with the [1m] context variants and
// the claude-* discovery variants for the Anthropic-shaped surface. Returns a
// fresh slice: the snapshot fields are shared across requests.
func anthropicListing(snap *engine.Snapshot) []engine.ModelEntry {
	out := make([]engine.ModelEntry, 0,
		len(snap.Models)+len(snap.ContextVariants)+len(snap.DiscoveryVariants))
	out = append(out, snap.Models...)
	out = append(out, snap.ContextVariants...)
	out = append(out, snap.DiscoveryVariants...)
	return out
}

// anthropicShapeRequest reports whether the client expects the Anthropic
// surface shape (Anthropic SDKs and Claude Code always send one of these).
func anthropicShapeRequest(r *http.Request) bool {
	return r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != ""
}

// listWindow applies Models-API pagination over the id-sorted registry:
// limit (default 20, clamped 1..1000) and an after_id cursor (items strictly
// after the given id in sort order). Returns the page and whether more remain.
func listWindow(models []engine.ModelEntry, q url.Values) ([]engine.ModelEntry, bool) {
	limit := 20
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	start := 0
	if after := q.Get("after_id"); after != "" {
		for start < len(models) && models[start].ID <= after {
			start++
		}
	}
	end := start + limit
	if end > len(models) {
		end = len(models)
	}
	if start >= len(models) {
		return nil, false
	}
	return models[start:end], end < len(models)
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
	cands, _, found := snap.Resolve(model, protocolAnthropic)
	if !found {
		writeModelNotFound(w, r, protocolAnthropic, model)
		return
	}
	cand := cands[0]

	if cand.Channel.Protocol == protocolAnthropic {
		// Native upstream: forward to its count_tokens endpoint.
		account, err := g.pickAccount(cand)
		if err != nil {
			writeProtocolError(w, r, protocolAnthropic, http.StatusServiceUnavailable, "api_error", "no enabled accounts")
			return
		}
		url := joinURL(cand.Channel.BaseURL, orDefault(cand.Channel.ChatPath, "/v1/messages"))
		url = strings.TrimSuffix(url, "/messages") + "/count_tokens"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytesReader(rewriteModel(body, cand.UpstreamModel, false)))
		if err != nil {
			writeProtocolError(w, r, protocolAnthropic, http.StatusInternalServerError, "api_error", err.Error())
			return
		}
		setUpstreamHeaders(req, r, cand.Channel, account.Secret, protocolAnthropic, false, len(body))
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
	cands, canonical, found := snap.Resolve(model, protocolOpenAI)
	logModel := orDefault(canonical, model)
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
	account, err := g.pickAccount(*cand)
	if err != nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusServiceUnavailable, "api_error", "no enabled accounts")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		joinURL(cand.Channel.BaseURL, embeddingsPath), bytesReader(rewriteModel(body, cand.UpstreamModel, false)))
	if err != nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	setUpstreamHeaders(req, r, cand.Channel, account.Secret, protocolOpenAI, false, len(body))
	resp, err := g.Client.Do(req)
	if err != nil {
		writeProtocolError(w, r, protocolOpenAI, http.StatusBadGateway, "api_error", "upstream unreachable")
		return
	}
	usage, rerr := relayNonStream(w, r, resp, protocolOpenAI)
	ck, _ := ClientKeyFrom(r.Context())
	g.logRequest(r, start, ck, logModel, *cand, &account, protocolOpenAI, false,
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
// object survives untouched (unknown fields preserved). For OpenAI-shaped
// streaming passthrough it also injects stream_options.include_usage when the
// client did not set it, so usage lands in the final chunk for stats.
func rewriteModel(body []byte, upstreamModel string, stream bool) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body // not an object: forward as-is
	}
	nm, err := json.Marshal(upstreamModel)
	if err != nil {
		return body
	}
	m["model"] = nm
	if stream {
		if _, has := m["stream_options"]; !has {
			m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
	}
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
	model, upstreamModel, protocolIn string, stream bool, status int, errType string, attempts int, acc *engine.Account) {

	cand := engine.Candidate{UpstreamModel: upstreamModel}
	g.logRequest(r, start, ck, model, cand, acc, protocolIn, stream, status, false, errType, attempts, usageInfo{}, nil)
}

func (g *Gateway) logRequest(r *http.Request, start time.Time, ck engine.ClientKey,
	model string, cand engine.Candidate, acc *engine.Account, protocolIn string, stream bool,
	status int, success bool, errType string, attempts int, usage usageInfo, ttft *int64) {

	entry := storeLogEntry(r, start, ck, model, cand, acc, protocolIn, stream,
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
