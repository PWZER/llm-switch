package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/ir"
	"github.com/PWZER/llm-switch/internal/protocol"
	"github.com/PWZER/llm-switch/internal/store"
)

// Account availability test depths:
//
//	quick  GET the upstream model list with the account's key — 200 proves the
//	       key is valid; zero tokens consumed
//	deep   a real mini completion (max_tokens=1) through one channel — the
//	       closest thing to production traffic; consumes a few tokens
const (
	testDepthQuick = "quick"
	testDepthDeep  = "deep"
)

// accountTestResult is the payload returned to the UI. The embedded endpoint
// result carries the deep-test verdict and timing breakdown; the quick test
// fills ModelCount/Models instead.
type accountTestResult struct {
	endpointTestResult
	Depth      string   `json:"depth"`
	ModelCount int      `json:"model_count,omitempty"`
	Models     []string `json:"models,omitempty"` // quick test: first ids, capped
}

// handleTestAccount runs the two-tier availability test of one account.
// POST /accounts/{id}/test {depth: "quick"|"deep", channel_id?, model?}
func (s *Server) handleTestAccount(w http.ResponseWriter, req *http.Request) {
	accountID, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body struct {
		Depth     string `json:"depth"`
		ChannelID int64  `json:"channel_id"`
		Model     string `json:"model"`
	}
	_ = json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&body)
	if body.Depth == "" {
		body.Depth = testDepthQuick
	}
	if body.Depth != testDepthQuick && body.Depth != testDepthDeep {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, `depth must be "quick" or "deep"`)
		return
	}
	account, err := s.St.Accounts.Get(req.Context(), accountID)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	if !account.Enabled {
		httpx.WriteEnvelopeError(w, req, http.StatusConflict, 40903, "account is disabled")
		return
	}

	if body.Depth == testDepthQuick {
		s.runAccountQuickTest(w, req, &account)
		return
	}
	s.runAccountDeepTest(w, req, &account, body.ChannelID, body.Model)
}

// runAccountQuickTest authenticates with the account's key against the
// provider's configured models_url — 200 means the key works.
func (s *Server) runAccountQuickTest(w http.ResponseWriter, req *http.Request, account *store.Account) {
	res := accountTestResult{Depth: testDepthQuick}
	res.KeyMask = store.MaskKey(account.APIKey)
	start := time.Now()

	provider, err := s.St.Providers.Get(req.Context(), account.ProviderID)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	if provider.ModelsURL == nil || *provider.ModelsURL == "" {
		res.Class = testClassUnreachable
		res.Error = "provider has no models_url configured — set it on the provider first"
		httpx.WriteEnvelope(w, req, res)
		return
	}

	// The auth header style comes from the provider's endpoints (OpenAI
	// protocol first — the models list is an OpenAI-style endpoint).
	authStyle := "bearer"
	if channels := s.enabledProviderChannels(req, account.ProviderID); len(channels) > 0 {
		authStyle = channels[0].AuthStyle
	}

	models, err := getModelsURL(req.Context(), *provider.ModelsURL, authStyle, account.APIKey)
	if err != nil {
		res.Class = testClassUnreachable
		res.Error = err.Error()
		var httpErr *upstreamHTTPError
		if errors.As(err, &httpErr) {
			res.Status = httpErr.status
			switch {
			case httpErr.status == 401 || httpErr.status == 403:
				res.Class = testClassAuth
				res.Error = "authentication failed: check the account's API key. upstream: " + httpErr.msg
			case httpErr.status == 404 || httpErr.status == 405:
				res.Class = testClassPath
			}
		}
		res.TotalMS = time.Since(start).Milliseconds()
		slog.Warn("account quick test failed", "account_id", account.ID, "err", err)
		httpx.WriteEnvelope(w, req, res)
		return
	}
	res.OK = true
	res.Class = testClassOK
	res.Status = http.StatusOK
	res.ModelCount = len(models)
	res.TotalMS = time.Since(start).Milliseconds()
	const capIDs = 50
	for i, m := range models {
		if i >= capIDs {
			break
		}
		res.Models = append(res.Models, m.ID)
	}
	slog.Info("account quick test passed", "account_id", account.ID,
		"models", res.ModelCount, "total_ms", res.TotalMS)
	httpx.WriteEnvelope(w, req, res)
}

// runAccountDeepTest sends a real max_tokens=1 mini completion through one
// channel with the account's key. Model resolution: explicit model -> the
// provider's first enabled model row (model-less probe otherwise).
func (s *Server) runAccountDeepTest(w http.ResponseWriter, req *http.Request, account *store.Account, channelID int64, model string) {
	channels := s.enabledProviderChannels(req, account.ProviderID)
	if len(channels) == 0 {
		httpx.WriteEnvelope(w, req, accountTestResult{
			endpointTestResult: endpointTestResult{
				Class: testClassUnreachable, KeyMask: store.MaskKey(account.APIKey),
				Error: "provider has no enabled channel to test through",
			},
			Depth: testDepthDeep,
		})
		return
	}
	var ch *store.Channel
	if channelID > 0 {
		for _, c := range channels {
			if c.ID == channelID {
				ch = c
				break
			}
		}
		if ch == nil {
			httpx.WriteEnvelopeError(w, req, http.StatusNotFound, 40402, "channel not found or not enabled on this provider")
			return
		}
	} else {
		ch = channels[0]
	}

	if model == "" {
		if first, err := s.St.Models.FirstEnabled(req.Context(), account.ProviderID); err == nil {
			model = first.Upstream()
		}
	}

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

	res := accountTestResult{Depth: testDepthDeep}
	res.endpointTestResult = s.probeEndpoint(req.Context(), ch, []byte(account.APIKey), payload, model)
	switch {
	case res.OK:
		slog.Info("account deep test passed", "account_id", account.ID, "channel", ch.Name,
			"model", res.Model, "status", res.Status, "total_ms", res.TotalMS)
	default:
		slog.Warn("account deep test failed", "account_id", account.ID, "channel", ch.Name,
			"class", res.Class, "status", res.Status, "err", res.Error)
	}
	httpx.WriteEnvelope(w, req, res)
}

// enabledProviderChannels returns the provider's enabled channels, OpenAI
// protocol first (the models list is an OpenAI-style endpoint).
func (s *Server) enabledProviderChannels(req *http.Request, providerID int64) []*store.Channel {
	all, err := s.St.Channels.List(req.Context())
	if err != nil {
		return nil
	}
	var channels []*store.Channel
	for i := range all {
		if all[i].ProviderID == providerID && all[i].Enabled {
			channels = append(channels, &all[i])
		}
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].Protocol == "openai" && channels[j].Protocol != "openai" })
	return channels
}
