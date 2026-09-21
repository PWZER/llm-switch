package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/ir"
	"github.com/PWZER/llm-switch/internal/protocol"
	"github.com/PWZER/llm-switch/internal/store"
)

// testHTTPClient caps live endpoint tests at 30s.
var testHTTPClient = &http.Client{Timeout: 30 * time.Second}

// Test classes — the connectivity verdict is independent of model rows:
//
//	ok          2xx: full completion round trip works with the given model
//	validation  4xx validation error on a model-less probe: the API route is
//	            alive and reachable (no tokens used); register a model for a
//	            full test
//	auth        401/403: reachable — key validation is the account test's job,
//	            so a connectivity probe (no key) reports OK; a keyed probe
//	            (account test) reports the auth failure
//	model       4xx naming a model problem: reachable, upstream rejected the model
//	path        404 on the chat path: reachable, but base_url/chat_path is wrong
//	unreachable network/TLS failure: no HTTP response at all
const (
	testClassOK          = "ok"
	testClassValidation  = "validation"
	testClassAuth        = "auth"
	testClassModel       = "model"
	testClassPath        = "path"
	testClassUnreachable = "unreachable"
)

// handleTestChannel checks endpoint CONNECTIVITY only: a model-less probe
// without any credential. Any HTTP response proves the URL is reachable —
// including 401/403 (auth is validated at the account level, not here).
// Works without accounts or model rows; no tokens are ever consumed.
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

	codec, err := protocol.For(ir.Protocol(ch.Protocol))
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
		return
	}
	payload, err := codec.EncodeRequest(&ir.Request{
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

	result := s.probeEndpoint(req.Context(), &ch, nil, payload, "")
	switch {
	case result.OK:
		slog.Info("endpoint connectivity test passed", "channel_id", ch.ID,
			"class", result.Class, "status", result.Status, "total_ms", result.TotalMS)
	default:
		slog.Warn("endpoint connectivity test failed", "channel_id", ch.ID, "class", result.Class,
			"status", result.Status, "err", result.Error, "total_ms", result.TotalMS)
	}
	httpx.WriteEnvelope(w, req, result)
}

// endpointTestResult is the payload returned to the UI.
type endpointTestResult struct {
	OK          bool   `json:"ok"`
	Class       string `json:"class"`  // ok|validation|auth|model|path|unreachable
	Status      int    `json:"status"` // HTTP status; 0 when the request failed earlier
	Error       string `json:"error,omitempty"`
	Model       string `json:"model"` // empty = model-less probe
	KeyMask     string `json:"key_mask"`
	DNSMS       int64  `json:"dns_ms"`
	ConnectMS   int64  `json:"connect_ms"`
	TLSMS       int64  `json:"tls_ms"`
	FirstByteMS int64  `json:"first_byte_ms"`
	TotalMS     int64  `json:"total_ms"`
}

// probeEndpoint performs the timed live request against the real chat path
// and classifies the outcome. A nil/empty secret sends NO auth header
// (connectivity probe): 401/403 then still counts as reachable.
func (s *Server) probeEndpoint(ctx context.Context, ch *store.Channel, secret []byte, payload []byte, model string) endpointTestResult {
	res := endpointTestResult{Class: testClassUnreachable, Model: model}
	if len(secret) > 0 {
		res.KeyMask = store.MaskKey(string(secret))
	}

	url := strings.TrimSuffix(ch.BaseURL, "/") + "/" + strings.TrimPrefix(chatPathOf(ch), "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if len(secret) > 0 {
		if channelAuthStyle(ch) == "x-api-key" {
			httpReq.Header.Set("x-api-key", string(secret))
			if ch.Protocol == "anthropic" {
				httpReq.Header.Set("anthropic-version", "2023-06-01")
			}
		} else {
			httpReq.Header.Set("Authorization", "Bearer "+string(secret))
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	var (
		dnsStart, dnsDone   time.Time
		connStart, connDone time.Time
		tlsStart, tlsDone   time.Time
		firstByte           time.Time
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

	msg := strings.TrimSpace(string(respBody))
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Class = testClassOK
		res.OK = true
		return res
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Class = testClassAuth
		if len(secret) == 0 {
			// Connectivity probe: the endpoint answered, auth is checked at
			// the account level — this is a PASS for URL reachability.
			res.OK = true
			res.Error = fmt.Sprintf("reachable (upstream %d; auth not checked — test an account for key validation). upstream body: %s", resp.StatusCode, msg)
			return res
		}
		res.Error = fmt.Sprintf("authentication failed (upstream %d): check the account's API key. upstream body: %s", resp.StatusCode, msg)
		return res
	case resp.StatusCode == 404:
		// The route did not match: base_url or chat_path is wrong for this
		// upstream (or the vendor simply does not implement it).
		res.Class = testClassPath
		res.Error = fmt.Sprintf("chat path not found on upstream (HTTP 404): check base_url/chat_path. upstream body: %s", msg)
		return res
	case resp.StatusCode == 400 || resp.StatusCode == 422:
		if model == "" {
			// Model-less probe: a validation error is the EXPECTED answer and
			// proves the API route is alive. No tokens were consumed.
			res.Class = testClassValidation
			res.Error = fmt.Sprintf("API alive (upstream %d validation response, no tokens used): register a model on the channel for a full completion test. upstream body: %s", resp.StatusCode, msg)
			return res
		}
		res.Class = testClassModel
		res.Error = fmt.Sprintf("upstream rejected the request/model (HTTP %d): %s", resp.StatusCode, msg)
		return res
	default:
		res.Class = testClassModel
		res.Error = fmt.Sprintf("upstream %d: %s", resp.StatusCode, msg)
		return res
	}
}

// probeParams describes an endpoint by form values instead of a saved channel,
// so the UI can test connectivity BEFORE saving. For models-preview the only
// endpoint field that matters is auth_style: the fetch URL is provider-level
// (models_url, absolute — never derived from the endpoint's base_url).
type probeParams struct {
	Protocol  string `json:"protocol"`
	BaseURL   string `json:"base_url"`
	ChatPath  string `json:"chat_path"`
	AuthStyle string `json:"auth_style"`
	ModelsURL string `json:"models_url"` // models-preview only: absolute fetch URL
	AccountID int64  `json:"account_id"` // models-preview only: resolve the secret from a saved account
	APIKey    string `json:"api_key"`    // models-preview only: pasted key for an unsaved account
}

// handleProbeEndpoint tests connectivity of an UNSAVED endpoint described by
// form values. POST /api/v1/providers/{id}/probe
func (s *Server) handleProbeEndpoint(w http.ResponseWriter, req *http.Request) {
	s.handleEndpointFormProbe(w, req, false)
}

// handlePreviewModels fetches the model list at an explicit URL and returns
// the ids without saving anything. POST /api/v1/providers/{id}/models-preview
func (s *Server) handlePreviewModels(w http.ResponseWriter, req *http.Request) {
	s.handleEndpointFormProbe(w, req, true)
}

func (s *Server) handleEndpointFormProbe(w http.ResponseWriter, req *http.Request, modelsOnly bool) {
	providerID, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var params probeParams
	if !readJSON(w, req, &params) {
		return
	}
	if params.Protocol != "openai" && params.Protocol != "anthropic" && params.Protocol != "responses" {
		params.Protocol = "openai"
	}
	if params.AuthStyle == "" {
		params.AuthStyle = defaultAuthStyle(params.Protocol)
	}

	if modelsOnly {
		if err := validateModelsURL(&params.ModelsURL); err != nil || params.ModelsURL == "" {
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "models_url is required (absolute http(s) URL)")
			return
		}
		// The upstream models list requires authentication: the credential is
		// chosen explicitly (saved account or pasted key), never defaulted.
		secret, err := s.resolveAccountSecret(req.Context(), providerID, params.AccountID, params.APIKey)
		if err != nil {
			if errors.Is(err, errAccountUnavailable) {
				httpx.WriteEnvelopeError(w, req, http.StatusNotFound, 40401, err.Error())
				return
			}
			httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
			return
		}
		models, err := getModelsURL(req.Context(), params.ModelsURL, params.AuthStyle, secret)
		if err != nil {
			slog.Warn("model preview failed", "models_url", params.ModelsURL, "err", err)
			httpx.WriteEnvelopeError(w, req, http.StatusBadGateway, 60001, err.Error())
			return
		}
		ids := make([]string, 0, len(models))
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		httpx.WriteEnvelope(w, req, map[string]any{"models": ids})
		return
	}

	if params.BaseURL == "" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "base_url is required")
		return
	}
	tmp := &store.Channel{
		Protocol:  params.Protocol,
		BaseURL:   params.BaseURL,
		ChatPath:  params.ChatPath,
		AuthStyle: params.AuthStyle,
	}

	// Connectivity-only probe: no credential, model-less payload — mirrors the
	// channel test for unsaved form values.
	codec, err := protocol.For(ir.Protocol(params.Protocol))
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
		return
	}
	payload, err := codec.EncodeRequest(&ir.Request{
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
	result := s.probeEndpoint(req.Context(), tmp, nil, payload, "")
	switch {
	case result.OK:
		slog.Info("endpoint probe passed", "base_url", params.BaseURL, "total_ms", result.TotalMS)
	case result.Class == testClassValidation:
		slog.Info("endpoint probe: API alive (no model)", "base_url", params.BaseURL, "total_ms", result.TotalMS)
	default:
		slog.Warn("endpoint probe failed", "base_url", params.BaseURL, "class", result.Class, "err", result.Error)
	}
	httpx.WriteEnvelope(w, req, result)
}

// errAccountUnavailable means the named account does not exist, is disabled,
// or belongs to another provider (404-class).
var errAccountUnavailable = errors.New("account not found or disabled")

// resolveAccountSecret resolves the upstream credential for a form probe.
// Explicit selection only — there is NO silent fallback to the first enabled
// account: a saved account id wins (validated against the provider), else a
// pasted key is used verbatim, else the request is malformed.
func (s *Server) resolveAccountSecret(ctx context.Context, providerID, accountID int64, explicitKey string) (string, error) {
	if accountID > 0 {
		account, err := s.St.Accounts.Get(ctx, accountID)
		if err != nil || account.ProviderID != providerID || !account.Enabled {
			return "", errAccountUnavailable
		}
		return account.APIKey, nil
	}
	if explicitKey != "" {
		return explicitKey, nil
	}
	return "", errors.New("account_id or api_key is required")
}

// chatPathOf resolves the endpoint's upstream path (defaults per protocol).
func chatPathOf(ch *store.Channel) string {
	if ch.ChatPath != "" {
		if !strings.HasPrefix(ch.ChatPath, "/") {
			return "/" + ch.ChatPath
		}
		return ch.ChatPath
	}
	switch ch.Protocol {
	case "anthropic":
		return "/v1/messages"
	case "responses":
		return "/responses"
	default:
		return "/chat/completions"
	}
}
