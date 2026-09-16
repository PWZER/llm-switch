package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Channel is a protocol-specific routable endpoint belonging to a provider.
type Channel struct {
	ID                  int64          `json:"id"`
	ProviderID          int64          `json:"provider_id"`
	ProviderName        string         `json:"provider_name"`
	Name                string         `json:"name"`
	Protocol            string         `json:"protocol"` // "openai" | "anthropic"
	BaseURL             string         `json:"base_url"`
	ChatPath            string         `json:"chat_path"`
	AuthStyle           string         `json:"auth_style"` // "bearer" | "x-api-key"
	ModelsURL           *string        `json:"models_url"`
	ExtraHeaders        string         `json:"extra_headers"`
	Enabled             bool           `json:"enabled"`
	Priority            int            `json:"priority"`
	Weight              int            `json:"weight"`
	AutoBind            bool           `json:"auto_bind"`
	SupportsEmbeddings  bool           `json:"supports_embeddings"`
	Passthrough         bool           `json:"passthrough"`
	ForceUpstreamStream bool           `json:"force_upstream_stream"`
	CreatedAt           int64          `json:"created_at"`
	UpdatedAt           int64          `json:"updated_at"`
	Models              []ChannelModel `json:"models,omitempty"` // populated by List/Get with bindings
}

// ChannelModel maps one client-facing model name to the upstream name on a channel.
type ChannelModel struct {
	ChannelID     int64  `json:"channel_id"`
	Model         string `json:"model"`
	UpstreamModel string `json:"upstream_model"`
}

const channelColumns = `c.id, c.provider_id, p.name, c.name, c.protocol, c.base_url, c.chat_path,
	c.auth_style, c.models_url, c.extra_headers, c.enabled, c.priority, c.weight,
	c.auto_bind, c.supports_embeddings, c.passthrough, c.force_upstream_stream,
	c.created_at, c.updated_at`

// ChannelRepo manages channels and their model bindings.
type ChannelRepo struct{ db *DB }

func scanChannel(row interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	var modelsURL sql.NullString
	err := row.Scan(&c.ID, &c.ProviderID, &c.ProviderName, &c.Name, &c.Protocol, &c.BaseURL, &c.ChatPath,
		&c.AuthStyle, &modelsURL, &c.ExtraHeaders, &c.Enabled, &c.Priority, &c.Weight,
		&c.AutoBind, &c.SupportsEmbeddings, &c.Passthrough, &c.ForceUpstreamStream,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, err
	}
	if modelsURL.Valid {
		u := modelsURL.String
		c.ModelsURL = &u
	}
	return c, nil
}

// Create inserts a channel and returns its id.
func (r *ChannelRepo) Create(ctx context.Context, c *Channel) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO channels (provider_id, name, protocol, base_url, chat_path, auth_style,
			models_url, extra_headers, enabled, priority, weight, auto_bind,
			supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ProviderID, c.Name, c.Protocol, c.BaseURL, c.ChatPath, c.AuthStyle,
		c.ModelsURL, c.ExtraHeaders, c.Enabled, c.Priority, c.Weight, c.AutoBind,
		c.SupportsEmbeddings, c.Passthrough, c.ForceUpstreamStream, now(), now())
	if err != nil {
		return 0, fmt.Errorf("create channel: %w", err)
	}
	return res.LastInsertId()
}

// List returns all channels with provider names and model bindings.
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return r.attachBindings(ctx, out)
}

// Get returns one channel with bindings, or ErrNotFound.
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
	out, err := r.attachBindings(ctx, []Channel{c})
	if err != nil {
		return c, err
	}
	return out[0], nil
}

// Update rewrites the mutable fields of a channel.
func (r *ChannelRepo) Update(ctx context.Context, c *Channel) error {
	res, err := r.db.Write.ExecContext(ctx, `
		UPDATE channels SET name = ?, protocol = ?, base_url = ?, chat_path = ?, auth_style = ?,
			models_url = ?, extra_headers = ?, enabled = ?, priority = ?, weight = ?,
			auto_bind = ?, supports_embeddings = ?, passthrough = ?, force_upstream_stream = ?,
			updated_at = ?
		WHERE id = ?`,
		c.Name, c.Protocol, c.BaseURL, c.ChatPath, c.AuthStyle,
		c.ModelsURL, c.ExtraHeaders, c.Enabled, c.Priority, c.Weight,
		c.AutoBind, c.SupportsEmbeddings, c.Passthrough, c.ForceUpstreamStream,
		now(), c.ID)
	return checkAffected(res, err, "update channel")
}

// Delete removes a channel; its bindings and aliases cascade.
func (r *ChannelRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	return checkAffected(res, err, "delete channel")
}

// ReplaceBindings atomically rewrites a channel's model bindings.
func (r *ChannelRepo) ReplaceBindings(ctx context.Context, channelID int64, models []ChannelModel) error {
	tx, err := r.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bindings tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_models WHERE channel_id = ?`, channelID); err != nil {
		return fmt.Errorf("clear bindings: %w", err)
	}
	for _, m := range models {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channel_models (channel_id, model, upstream_model) VALUES (?, ?, ?)`,
			channelID, m.Model, m.UpstreamModel); err != nil {
			return fmt.Errorf("insert binding %q: %w", m.Model, err)
		}
	}
	return tx.Commit()
}

// attachBindings fills the Models slice of each channel in one query and
// returns the slice (bindings append to elements, so callers must use the result).
func (r *ChannelRepo) attachBindings(ctx context.Context, channels []Channel) ([]Channel, error) {
	if len(channels) == 0 {
		return channels, nil
	}
	byID := make(map[int64]int, len(channels))
	for i, c := range channels {
		byID[c.ID] = i
	}
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT channel_id, model, upstream_model FROM channel_models
		ORDER BY channel_id, model`)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m ChannelModel
		if err := rows.Scan(&m.ChannelID, &m.Model, &m.UpstreamModel); err != nil {
			return nil, fmt.Errorf("scan binding: %w", err)
		}
		if idx, ok := byID[m.ChannelID]; ok {
			channels[idx].Models = append(channels[idx].Models, m)
		}
	}
	return channels, rows.Err()
}
