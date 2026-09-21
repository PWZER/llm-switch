package api

import (
	"errors"
	"net/http"

	"github.com/PWZER/llm-switch/internal/httpx"
	"github.com/PWZER/llm-switch/internal/store"
)

type channelBody struct {
	ProviderID         int64  `json:"provider_id"`
	Protocol           string `json:"protocol"`
	BaseURL            string `json:"base_url"`
	ChatPath           string `json:"chat_path"`
	AuthStyle          string `json:"auth_style"`
	ExtraHeaders       string `json:"extra_headers"`
	Enabled            *bool  `json:"enabled"`
	SupportsEmbeddings *bool  `json:"supports_embeddings"`
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
	if !s.checkProtocolFree(w, req, body.ProviderID, body.Protocol, 0) {
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
	c.ProviderID = current.ProviderID
	c.CreatedAt = current.CreatedAt
	if c.Protocol == "" {
		c.Protocol = current.Protocol
	}
	if c.SupportsEmbeddings && c.Protocol != "openai" {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002,
			"supports_embeddings applies to openai channels only")
		return
	}
	if c.Protocol != current.Protocol &&
		!s.checkProtocolFree(w, req, current.ProviderID, c.Protocol, id) {
		return
	}
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

// checkProtocolFree enforces one endpoint per protocol per provider ahead of
// the DB UNIQUE constraint (excludeID ignores the row being updated). Writes
// the 409 response and returns false when the protocol is taken.
func (s *Server) checkProtocolFree(w http.ResponseWriter, req *http.Request, providerID int64, protocol string, excludeID int64) bool {
	existing, err := s.St.Channels.GetByProtocol(req.Context(), providerID, protocol)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		mapStoreErr(w, req, err)
		return false
	}
	if existing.ID == excludeID {
		return true
	}
	httpx.WriteEnvelopeError(w, req, http.StatusConflict, 40910,
		"provider already has a "+protocol+" endpoint — edit or delete it first")
	return false
}

func validateChannelBody(b *channelBody, creating bool) error {
	if creating {
		if b.ProviderID <= 0 {
			return errText("provider_id is required")
		}
		if b.BaseURL == "" {
			return errText("base_url is required")
		}
	}
	if creating && b.Protocol == "" {
		return errText(`protocol must be "openai", "anthropic" or "responses"`)
	}
	if b.Protocol != "" && b.Protocol != "openai" && b.Protocol != "anthropic" && b.Protocol != "responses" {
		return errText(`protocol must be "openai", "anthropic" or "responses"`)
	}
	if b.AuthStyle != "" && b.AuthStyle != "bearer" && b.AuthStyle != "x-api-key" {
		return errText(`auth_style must be "bearer" or "x-api-key"`)
	}
	if b.ExtraHeaders != "" && b.ExtraHeaders != "{}" && b.ExtraHeaders[0] != '{' {
		return errText("extra_headers must be a JSON object string")
	}
	protocol := b.Protocol
	if b.SupportsEmbeddings != nil && *b.SupportsEmbeddings && protocol != "" && protocol != "openai" {
		return errText("supports_embeddings applies to openai channels only")
	}
	return nil
}

// toChannel materializes the body into a store row, applying defaults on create.
func (b *channelBody) toChannel(creating bool) store.Channel {
	c := store.Channel{
		ProviderID:   b.ProviderID,
		Protocol:     b.Protocol,
		BaseURL:      b.BaseURL,
		ChatPath:     b.ChatPath,
		AuthStyle:    b.AuthStyle,
		ExtraHeaders: b.ExtraHeaders,
	}
	if creating {
		c.Enabled = true
	}
	if b.Enabled != nil {
		c.Enabled = *b.Enabled
	}
	if b.SupportsEmbeddings != nil {
		c.SupportsEmbeddings = *b.SupportsEmbeddings
	}
	if c.ExtraHeaders == "" {
		c.ExtraHeaders = "{}"
	}
	return c
}

type errText string

func (e errText) Error() string { return string(e) }
