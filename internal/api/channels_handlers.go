package api

import (
	"net/http"
	"strings"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

type channelBody struct {
	ProviderID          int64   `json:"provider_id"`
	Name                string  `json:"name"`
	Protocol            string  `json:"protocol"`
	BaseURL             string  `json:"base_url"`
	ChatPath            string  `json:"chat_path"`
	AuthStyle           string  `json:"auth_style"`
	ResponsesPath       *string `json:"responses_path"`
	ExtraHeaders        string  `json:"extra_headers"`
	Enabled             *bool   `json:"enabled"`
	Priority            *int    `json:"priority"`
	Weight              *int    `json:"weight"`
	SupportsEmbeddings  *bool   `json:"supports_embeddings"`
	Passthrough         *bool   `json:"passthrough"`
	ForceUpstreamStream *bool   `json:"force_upstream_stream"`
}

func (s *Server) handleListChannels(w http.ResponseWriter, req *http.Request) {
	channels, err := s.St.Channels.List(req.Context())
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, channels)
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, req *http.Request) {
	var body channelBody
	if !readJSON(w, req, &body) {
		return
	}
	if err := validateChannelBody(&body, true); err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
		return
	}
	c := body.toChannel(true)
	id, err := s.St.Channels.Create(req.Context(), &c)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	out, err := s.St.Channels.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelopeStatus(w, req, http.StatusCreated, out)
}

func (s *Server) handleGetChannel(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	c, err := s.St.Channels.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, c)
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	var body channelBody
	if !readJSON(w, req, &body) {
		return
	}
	if err := validateChannelBody(&body, false); err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, err.Error())
		return
	}
	// Merge onto the current row so partial updates are possible.
	current, err := s.St.Channels.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	c := body.toChannel(false)
	c.ID = current.ID
	c.CreatedAt = current.CreatedAt
	if err := s.St.Channels.Update(req.Context(), &c); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	out, err := s.St.Channels.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, out)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	if err := s.St.DeleteChannel(req.Context(), id); err != nil {
		mapStoreErr(w, req, err)
		return
	}
	httpx.WriteEnvelope(w, req, map[string]bool{"ok": true})
}

func validateChannelBody(b *channelBody, creating bool) error {
	if creating {
		if b.ProviderID <= 0 {
			return errText("provider_id is required")
		}
		if b.Name == "" {
			return errText("name is required")
		}
		if b.Protocol != "openai" && b.Protocol != "anthropic" {
			return errText(`protocol must be "openai" or "anthropic"`)
		}
		if b.BaseURL == "" {
			return errText("base_url is required")
		}
	}
	if b.Protocol != "" && b.Protocol != "openai" && b.Protocol != "anthropic" {
		return errText(`protocol must be "openai" or "anthropic"`)
	}
	if b.AuthStyle != "" && b.AuthStyle != "bearer" && b.AuthStyle != "x-api-key" {
		return errText(`auth_style must be "bearer" or "x-api-key"`)
	}
	if b.ExtraHeaders != "" && b.ExtraHeaders != "{}" && b.ExtraHeaders[0] != '{' {
		return errText("extra_headers must be a JSON object string")
	}
	if b.ResponsesPath != nil {
		p := strings.TrimSpace(*b.ResponsesPath)
		if p != "" {
			if strings.Contains(p, "://") || strings.ContainsAny(p, " \t\r\n") {
				return errText("responses_path must be a URL path like /responses")
			}
			if b.Protocol == "anthropic" {
				return errText("responses_path applies to openai channels only")
			}
		}
	}
	return nil
}

// toChannel materializes the body into a store row, applying defaults on create.
func (b *channelBody) toChannel(creating bool) store.Channel {
	c := store.Channel{
		ProviderID:    b.ProviderID,
		Name:          b.Name,
		Protocol:      b.Protocol,
		BaseURL:       b.BaseURL,
		ChatPath:      b.ChatPath,
		AuthStyle:     b.AuthStyle,
		ResponsesPath: b.ResponsesPath,
		ExtraHeaders:  b.ExtraHeaders,
	}
	if creating {
		c.Enabled = true
		c.Passthrough = true
		c.Weight = 1
	}
	if b.Enabled != nil {
		c.Enabled = *b.Enabled
	}
	if b.Priority != nil {
		c.Priority = *b.Priority
	}
	if b.Weight != nil {
		c.Weight = *b.Weight
	}
	if b.SupportsEmbeddings != nil {
		c.SupportsEmbeddings = *b.SupportsEmbeddings
	}
	if b.Passthrough != nil {
		c.Passthrough = *b.Passthrough
	}
	if b.ForceUpstreamStream != nil {
		c.ForceUpstreamStream = *b.ForceUpstreamStream
	}
	if c.AuthStyle == "" {
		c.AuthStyle = "bearer"
	}
	if c.ExtraHeaders == "" {
		c.ExtraHeaders = "{}"
	}
	return c
}

type errText string

func (e errText) Error() string { return string(e) }
