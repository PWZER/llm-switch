package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Channel is a protocol-specific routable endpoint belonging to a provider.
// A provider carries at most one channel per protocol
// (UNIQUE(provider_id, protocol)) — the protocol is the endpoint's identity.
// AuthStyle empty means the protocol default (bearer for openai/responses,
// x-api-key for anthropic); ChatPath empty resolves to the protocol default
// path at dispatch time.
type Channel struct {
	ID                 int64  `json:"id"`
	ProviderID         int64  `json:"provider_id"`
	ProviderName       string `json:"provider_name"`
	Protocol           string `json:"protocol"` // "openai" | "anthropic" | "responses"
	BaseURL            string `json:"base_url"`
	ChatPath           string `json:"chat_path"`
	AuthStyle          string `json:"auth_style"` // "" | "bearer" | "x-api-key"
	ExtraHeaders       string `json:"extra_headers"`
	Enabled            bool   `json:"enabled"`
	SupportsEmbeddings bool   `json:"supports_embeddings"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

const channelColumns = `c.id, c.provider_id, p.name, c.protocol, c.base_url, c.chat_path,
	c.auth_style, c.extra_headers, c.enabled, c.supports_embeddings, c.created_at, c.updated_at`

// ChannelRepo manages channels.
type ChannelRepo struct{ db *DB }

func scanChannel(row interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	err := row.Scan(&c.ID, &c.ProviderID, &c.ProviderName, &c.Protocol, &c.BaseURL, &c.ChatPath,
		&c.AuthStyle, &c.ExtraHeaders, &c.Enabled, &c.SupportsEmbeddings, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// Create inserts a channel and returns its id. The (provider_id, protocol)
// UNIQUE constraint rejects a second endpoint of the same protocol.
func (r *ChannelRepo) Create(ctx context.Context, c *Channel) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO channels (provider_id, protocol, base_url, chat_path, auth_style,
			extra_headers, enabled, supports_embeddings, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ProviderID, c.Protocol, c.BaseURL, c.ChatPath, c.AuthStyle,
		c.ExtraHeaders, c.Enabled, c.SupportsEmbeddings, now(), now())
	if err != nil {
		return 0, fmt.Errorf("create channel: %w", err)
	}
	return res.LastInsertId()
}

// List returns all channels with provider names, ordered by provider then id.
func (r *ChannelRepo) List(ctx context.Context) ([]Channel, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT `+channelColumns+` FROM channels c JOIN providers p ON p.id = c.provider_id
		ORDER BY c.provider_id, c.id`)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	out := []Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get returns one channel, or ErrNotFound.
func (r *ChannelRepo) Get(ctx context.Context, id int64) (Channel, error) {
	row := r.db.Read.QueryRowContext(ctx, `
		SELECT `+channelColumns+` FROM channels c JOIN providers p ON p.id = c.provider_id
		WHERE c.id = ?`, id)
	c, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("get channel %d: %w", id, err)
	}
	return c, nil
}

// GetByProtocol returns the provider's channel of one protocol (unique by
// schema), or ErrNotFound.
func (r *ChannelRepo) GetByProtocol(ctx context.Context, providerID int64, protocol string) (Channel, error) {
	row := r.db.Read.QueryRowContext(ctx, `
		SELECT `+channelColumns+` FROM channels c JOIN providers p ON p.id = c.provider_id
		WHERE c.provider_id = ? AND c.protocol = ?`, providerID, protocol)
	c, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("get channel (%d, %s): %w", providerID, protocol, err)
	}
	return c, nil
}

// Update rewrites the mutable fields of a channel.
func (r *ChannelRepo) Update(ctx context.Context, c *Channel) error {
	res, err := r.db.Write.ExecContext(ctx, `
		UPDATE channels SET protocol = ?, base_url = ?, chat_path = ?, auth_style = ?,
			extra_headers = ?, enabled = ?, supports_embeddings = ?, updated_at = ?
		WHERE id = ?`,
		c.Protocol, c.BaseURL, c.ChatPath, c.AuthStyle,
		c.ExtraHeaders, c.Enabled, c.SupportsEmbeddings, now(), c.ID)
	return checkAffected(res, err, "update channel")
}

// Delete removes a channel; model route targets pinned to it are degraded by
// the caller (Store.DeleteChannel).
func (r *ChannelRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	return checkAffected(res, err, "delete channel")
}
