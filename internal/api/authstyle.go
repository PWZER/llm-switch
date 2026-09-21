package api

import (
	"github.com/PWZER/llm-switch/internal/store"
)

// protocolRank orders protocols for the models-list auth-style pick: the
// models list is an OpenAI-style endpoint, so an openai channel donates its
// auth style first.
func protocolRank(protocol string) int {
	switch protocol {
	case "openai":
		return 0
	case "responses":
		return 1
	default: // anthropic
		return 2
	}
}

// defaultAuthStyle resolves an empty auth_style to the protocol default:
// bearer for openai/responses, x-api-key for anthropic.
func defaultAuthStyle(protocol string) string {
	if protocol == "anthropic" {
		return "x-api-key"
	}
	return "bearer"
}

// channelAuthStyle resolves a channel's effective auth style (explicit value
// wins, empty falls back to the protocol default).
func channelAuthStyle(ch *store.Channel) string {
	if ch.AuthStyle != "" {
		return ch.AuthStyle
	}
	return defaultAuthStyle(ch.Protocol)
}

// pickModelsAuthStyle chooses the auth header style for a provider's
// models-list endpoint: the enabled channel first in protocol preference
// order (openai → responses → anthropic) donates its auth style. With no
// channels at all, bearer is the safe default.
func pickModelsAuthStyle(channels []store.Channel, providerID int64) string {
	best := -1
	for i := range channels {
		if channels[i].ProviderID != providerID || !channels[i].Enabled {
			continue
		}
		if best < 0 || protocolRank(channels[i].Protocol) < protocolRank(channels[best].Protocol) {
			best = i
		}
	}
	if best < 0 {
		return "bearer"
	}
	return channelAuthStyle(&channels[best])
}
