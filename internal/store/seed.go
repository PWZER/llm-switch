package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// seedChannel is one protocol endpoint of a seeded vendor.
type seedChannel struct {
	Name          string
	Protocol      string // "openai" | "anthropic"
	BaseURL       string
	ChatPath      string
	AuthStyle     string
	ResponsesPath string // upstream Responses API path; "" = bridge via IR
}

// seedProvider is one vendor's default configuration.
type seedProvider struct {
	Name      string
	ModelsURL string // provider-level models-list fetch URL (absolute)
	Channels  []seedChannel
}

// defaultProviders mirrors the docs-verified endpoint presets in
// web/src/data/presets.ts (frontend form prefill) — keep the two in sync.
var defaultProviders = []seedProvider{
	{
		Name:      "OpenAI",
		ModelsURL: "https://api.openai.com/v1/models",
		Channels: []seedChannel{
			{
				Name: "openai", Protocol: "openai",
				BaseURL: "https://api.openai.com/v1", ChatPath: "/chat/completions",
				AuthStyle:     "bearer",
				ResponsesPath: "/responses",
			},
		},
	},
	{
		Name:      "Anthropic",
		ModelsURL: "https://api.anthropic.com/v1/models",
		Channels: []seedChannel{
			{
				Name: "anthropic", Protocol: "anthropic",
				BaseURL: "https://api.anthropic.com", ChatPath: "/v1/messages",
				AuthStyle: "x-api-key",
			},
		},
	},
	{
		Name:      "Zhipu GLM",
		ModelsURL: "https://open.bigmodel.cn/api/paas/v4/models",
		Channels: []seedChannel{
			{
				Name: "zhipu-openai", Protocol: "openai",
				BaseURL: "https://open.bigmodel.cn/api/paas/v4", ChatPath: "/chat/completions",
				AuthStyle: "bearer",
			},
			{
				Name: "zhipu-anthropic", Protocol: "anthropic",
				BaseURL: "https://open.bigmodel.cn/api/anthropic", ChatPath: "/v1/messages",
				AuthStyle: "x-api-key",
			},
		},
	},
	{
		Name:      "DeepSeek",
		ModelsURL: "https://api.deepseek.com/models",
		Channels: []seedChannel{
			{
				Name: "deepseek-openai", Protocol: "openai",
				BaseURL: "https://api.deepseek.com", ChatPath: "/chat/completions",
				AuthStyle: "bearer",
			},
			{
				Name: "deepseek-anthropic", Protocol: "anthropic",
				BaseURL: "https://api.deepseek.com/anthropic", ChatPath: "/v1/messages",
				AuthStyle: "x-api-key",
			},
		},
	},
	{
		Name:      "KimiAPI",
		ModelsURL: "https://api.moonshot.cn/v1/models",
		Channels: []seedChannel{
			{
				Name: "kimi-openai", Protocol: "openai",
				BaseURL: "https://api.moonshot.cn/v1", ChatPath: "/chat/completions",
				AuthStyle: "bearer",
			},
			{
				Name: "kimi-anthropic", Protocol: "anthropic",
				BaseURL: "https://api.moonshot.cn/anthropic", ChatPath: "/v1/messages",
				AuthStyle: "x-api-key",
			},
		},
	},
	{
		Name:      "KimiCoding",
		ModelsURL: "https://api.kimi.com/coding/v1/models",
		Channels: []seedChannel{
			{
				Name: "kimi-coding-openai", Protocol: "openai",
				BaseURL: "https://api.kimi.com/coding/v1", ChatPath: "/chat/completions",
				AuthStyle: "bearer",
			},
			{
				Name: "kimi-coding-anthropic", Protocol: "anthropic",
				BaseURL: "https://api.kimi.com/coding/", ChatPath: "/v1/messages",
				AuthStyle: "x-api-key",
			},
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
			var responsesPath sql.NullString
			if ch.ResponsesPath != "" {
				responsesPath = sql.NullString{String: ch.ResponsesPath, Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO channels (provider_id, name, protocol, base_url, chat_path, auth_style,
					responses_path, extra_headers, enabled, priority, weight,
					supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, '{}', 1, 10, 1, 0, 1, 0, ?, ?)`,
				pid, ch.Name, ch.Protocol, ch.BaseURL, ch.ChatPath, ch.AuthStyle,
				responsesPath, ts, ts); err != nil {
				return false, fmt.Errorf("seed channel %q: %w", ch.Name, err)
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
