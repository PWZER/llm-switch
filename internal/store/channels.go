package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Channel is a protocol-specific routable endpoint belonging to a provider.
type Channel struct {
	ID                  int64   `json:"id"`
	ProviderID          int64   `json:"provider_id"`
	ProviderName        string  `json:"provider_name"`
	Name                string  `json:"name"`
	Protocol            string  `json:"protocol"` // "openai" | "anthropic"
	BaseURL             string  `json:"base_url"`
	ChatPath            string  `json:"chat_path"`
	AuthStyle           string  `json:"auth_style"`     // "bearer" | "x-api-key"
	ResponsesPath       *string `json:"responses_path"` // upstream Responses API endpoint; nil/"" = bridge via IR
	ExtraHeaders        string  `json:"extra_headers"`
	Enabled             bool    `json:"enabled"`
	Priority            int     `json:"priority"`
	Weight              int     `json:"weight"`
	SupportsEmbeddings  bool    `json:"supports_embeddings"`
	Passthrough         bool    `json:"passthrough"`
	ForceUpstreamStream bool    `json:"force_upstream_stream"`
	CreatedAt           int64   `json:"created_at"`
	UpdatedAt           int64   `json:"updated_at"`
}

const channelColumns = `c.id, c.provider_id, p.name, c.name, c.protocol, c.base_url, c.chat_path,
	c.auth_style, c.responses_path, c.extra_headers, c.enabled, c.priority, c.weight,
	c.supports_embeddings, c.passthrough, c.force_upstream_stream, c.created_at, c.updated_at`

// ChannelRepo manages channels.
type ChannelRepo struct{ db *DB }

func scanChannel(row interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	var responsesPath sql.NullString
	err := row.Scan(&c.ID, &c.ProviderID, &c.ProviderName, &c.Name, &c.Protocol, &c.BaseURL, &c.ChatPath,
		&c.AuthStyle, &responsesPath, &c.ExtraHeaders, &c.Enabled, &c.Priority, &c.Weight,
		&c.SupportsEmbeddings, &c.Passthrough, &c.ForceUpstreamStream, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, err
	}
	if responsesPath.Valid {
		p := responsesPath.String
		c.ResponsesPath = &p
	}
	return c, nil
}

// Create inserts a channel and returns its id.
func (r *ChannelRepo) Create(ctx context.Context, c *Channel) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO channels (provider_id, name, protocol, base_url, chat_path, auth_style,
			responses_path, extra_headers, enabled, priority, weight,
			supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ProviderID, c.Name, c.Protocol, c.BaseURL, c.ChatPath, c.AuthStyle,
		c.ResponsesPath, c.ExtraHeaders, c.Enabled, c.Priority, c.Weight,
		c.SupportsEmbeddings, c.Passthrough, c.ForceUpstreamStream, now(), now())
	if err != nil {
		return 0, fmt.Errorf("create channel: %w", err)
	}
	return res.LastInsertId()
}

// List returns all channels with provider names.
func (r *ChannelRepo) List(ctx context.Context) ([]Channel, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT `+channelColumns+` FROM channels c JOIN providers p ON p.id = c.provider_id
		ORDER BY c.provider_id, c.priority DESC, c.id`)
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

// Update rewrites the mutable fields of a channel.
func (r *ChannelRepo) Update(ctx context.Context, c *Channel) error {
	res, err := r.db.Write.ExecContext(ctx, `
		UPDATE channels SET name = ?, protocol = ?, base_url = ?, chat_path = ?, auth_style = ?,
			responses_path = ?, extra_headers = ?, enabled = ?, priority = ?, weight = ?,
			supports_embeddings = ?, passthrough = ?, force_upstream_stream = ?, updated_at = ?
		WHERE id = ?`,
		c.Name, c.Protocol, c.BaseURL, c.ChatPath, c.AuthStyle,
		c.ResponsesPath, c.ExtraHeaders, c.Enabled, c.Priority, c.Weight,
		c.SupportsEmbeddings, c.Passthrough, c.ForceUpstreamStream,
		now(), c.ID)
	return checkAffected(res, err, "update channel")
}

// Delete removes a channel; its model rows cascade (FK) and model routes are
// stripped by the caller (Store.DeleteChannel).
func (r *ChannelRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	return checkAffected(res, err, "delete channel")
}
