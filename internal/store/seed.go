package store

import (
	"context"
	"errors"
	"fmt"
)

// seedChannel is one protocol endpoint of a seeded vendor. AuthStyle empty
// means the protocol default (bearer for openai/responses, x-api-key for
// anthropic).
type seedChannel struct {
	Protocol  string // "openai" | "anthropic" | "responses"
	BaseURL   string
	ChatPath  string
	AuthStyle string
}

// seedProvider is one vendor's default configuration.
type seedProvider struct {
	Name      string
	ModelsURL string // provider-level models-list fetch URL (absolute)
	Channels  []seedChannel
}

// defaultProviders holds the docs-verified built-in vendor endpoints.
var defaultProviders = []seedProvider{
	{
		Name:      "OpenAI",
		ModelsURL: "https://api.openai.com/v1/models",
		Channels: []seedChannel{
			{Protocol: "openai", BaseURL: "https://api.openai.com/v1", ChatPath: "/chat/completions"},
			{Protocol: "responses", BaseURL: "https://api.openai.com/v1", ChatPath: "/responses"},
		},
	},
	{
		Name:      "Anthropic",
		ModelsURL: "https://api.anthropic.com/v1/models",
		Channels: []seedChannel{
			{Protocol: "anthropic", BaseURL: "https://api.anthropic.com", ChatPath: "/v1/messages"},
		},
	},
	{
		Name:      "Zhipu GLM",
		ModelsURL: "https://open.bigmodel.cn/api/paas/v4/models",
		Channels: []seedChannel{
			{Protocol: "openai", BaseURL: "https://open.bigmodel.cn/api/paas/v4", ChatPath: "/chat/completions"},
			{Protocol: "anthropic", BaseURL: "https://open.bigmodel.cn/api/anthropic", ChatPath: "/v1/messages"},
		},
	},
	{
		Name:      "DeepSeek",
		ModelsURL: "https://api.deepseek.com/models",
		Channels: []seedChannel{
			{Protocol: "openai", BaseURL: "https://api.deepseek.com", ChatPath: "/chat/completions"},
			{Protocol: "anthropic", BaseURL: "https://api.deepseek.com/anthropic", ChatPath: "/v1/messages"},
		},
	},
	{
		Name:      "KimiAPI",
		ModelsURL: "https://api.moonshot.cn/v1/models",
		Channels: []seedChannel{
			{Protocol: "openai", BaseURL: "https://api.moonshot.cn/v1", ChatPath: "/chat/completions"},
			{Protocol: "anthropic", BaseURL: "https://api.moonshot.cn/anthropic", ChatPath: "/v1/messages"},
		},
	},
	{
		Name:      "KimiCoding",
		ModelsURL: "https://api.kimi.com/coding/v1/models",
		Channels: []seedChannel{
			{Protocol: "openai", BaseURL: "https://api.kimi.com/coding/v1", ChatPath: "/chat/completions"},
			{Protocol: "anthropic", BaseURL: "https://api.kimi.com/coding/", ChatPath: "/v1/messages"},
		},
	},
}

// seededProvidersKey marks the seed as done so deleting every provider later
// does not resurrect the defaults on the next boot.
const seededProvidersKey = "seeded_providers"

// SeedDefaultProviders creates the built-in vendor providers (with their
// models-list fetch URLs) and channels on first boot. No-op (seeded=false)
// once the seeded_providers setting exists. Credentials and model rows are
// never seeded — accounts stay user-supplied and the registry starts empty
// by design.
func (s *Store) SeedDefaultProviders(ctx context.Context) (seeded bool, err error) {
	if _, err := s.Settings.Get(ctx, seededProvidersKey); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}

	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin seed tx: %w", err)
	}
	defer tx.Rollback()

	ts := now()
	for _, p := range defaultProviders {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO providers (name, models_url, enabled, created_at, updated_at) VALUES (?, ?, 1, ?, ?)`,
			p.Name, p.ModelsURL, ts, ts)
		if err != nil {
			return false, fmt.Errorf("seed provider %q: %w", p.Name, err)
		}
		pid, err := res.LastInsertId()
		if err != nil {
			return false, fmt.Errorf("seed provider %q id: %w", p.Name, err)
		}
		for _, ch := range p.Channels {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO channels (provider_id, protocol, base_url, chat_path, auth_style,
					extra_headers, enabled, supports_embeddings, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, '{}', 1, 0, ?, ?)`,
				pid, ch.Protocol, ch.BaseURL, ch.ChatPath, ch.AuthStyle, ts, ts); err != nil {
				return false, fmt.Errorf("seed channel %s/%s: %w", p.Name, ch.Protocol, err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, '1', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		seededProvidersKey, ts); err != nil {
		return false, fmt.Errorf("mark providers seeded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit seed: %w", err)
	}
	return true, nil
}
