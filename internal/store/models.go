package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Model is a client-facing entry in the merged GET /v1/models registry.
type Model struct {
	ID              string  `json:"id"`
	DisplayName     string  `json:"display_name"`
	Source          string  `json:"source"` // "manual" | "provider:<id>"
	ContextWindow   *int64  `json:"context_window"`
	MaxOutputTokens *int64  `json:"max_output_tokens"`
	Enabled         bool    `json:"enabled"`
	CreatedAt       int64   `json:"created_at"`
	UpdatedAt       int64   `json:"updated_at"`
}

// Alias is a stable client-facing name pointing at (channel, upstream_model).
type Alias struct {
	Name          string `json:"name"`
	ChannelID     int64  `json:"channel_id"`
	UpstreamModel string `json:"upstream_model"`
	UpdatedAt     int64  `json:"updated_at"`
}

// ModelRepo manages the model registry.
type ModelRepo struct{ db *DB }

// UpsertModel inserts or updates a model entry. Source and enabled flags are
// only overwritten on explicit update, so provider refreshes never clobber
// manual entries.
func (r *ModelRepo) Upsert(ctx context.Context, m *Model) error {
	_, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO models (id, display_name, source, context_window, max_output_tokens,
			enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display_name      = excluded.display_name,
			source            = excluded.source,
			context_window    = COALESCE(excluded.context_window, models.context_window),
			max_output_tokens = COALESCE(excluded.max_output_tokens, models.max_output_tokens),
			enabled           = excluded.enabled,
			updated_at        = excluded.updated_at`,
		m.ID, m.DisplayName, m.Source, m.ContextWindow, m.MaxOutputTokens,
		m.Enabled, now(), now())
	if err != nil {
		return fmt.Errorf("upsert model %q: %w", m.ID, err)
	}
	return nil
}

// List returns all models ordered by id.
func (r *ModelRepo) List(ctx context.Context) ([]Model, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, display_name, source, context_window, max_output_tokens, enabled,
			created_at, updated_at
		FROM models ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	out := []Model{}
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.DisplayName, &m.Source, &m.ContextWindow, &m.MaxOutputTokens,
			&m.Enabled, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// EnabledModels returns only registry entries served in GET /v1/models.
func (r *ModelRepo) EnabledModels(ctx context.Context) ([]Model, error) {
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Model
	for _, m := range all {
		if m.Enabled {
			out = append(out, m)
		}
	}
	return out, nil
}

// Get returns one model or ErrNotFound.
func (r *ModelRepo) Get(ctx context.Context, id string) (Model, error) {
	var m Model
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT id, display_name, source, context_window, max_output_tokens, enabled,
			created_at, updated_at
		FROM models WHERE id = ?`, id).
		Scan(&m.ID, &m.DisplayName, &m.Source, &m.ContextWindow, &m.MaxOutputTokens,
			&m.Enabled, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("get model %q: %w", id, err)
	}
	return m, nil
}

// EnsureModel inserts a model entry only when absent, so provider refreshes
// never clobber manual entries or flip enabled flags (upsert-not-replace rule).
func (r *ModelRepo) EnsureModel(ctx context.Context, id, source string) (bool, error) {
	res, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO models (id, display_name, source, enabled, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		id, id, source, now(), now())
	if err != nil {
		return false, fmt.Errorf("ensure model %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Update toggles/edits a model entry.
func (r *ModelRepo) Update(ctx context.Context, m *Model) error {
	res, err := r.db.Write.ExecContext(ctx, `
		UPDATE models SET display_name = ?, enabled = ?, context_window = ?,
			max_output_tokens = ?, updated_at = ?
		WHERE id = ?`,
		m.DisplayName, m.Enabled, m.ContextWindow, m.MaxOutputTokens, now(), m.ID)
	return checkAffected(res, err, "update model")
}

// Delete removes a model entry.
func (r *ModelRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
	return checkAffected(res, err, "delete model")
}

// AliasRepo manages hot-switchable model aliases.
type AliasRepo struct{ db *DB }

// UpsertAlias inserts or re-targets an alias (the hot-switch primitive).
func (r *AliasRepo) Upsert(ctx context.Context, a *Alias) error {
	_, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO aliases (name, channel_id, upstream_model, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			channel_id = excluded.channel_id,
			upstream_model = excluded.upstream_model,
			updated_at = excluded.updated_at`,
		a.Name, a.ChannelID, a.UpstreamModel, now())
	if err != nil {
		return fmt.Errorf("upsert alias %q: %w", a.Name, err)
	}
	return nil
}

// List returns all aliases ordered by name.
func (r *AliasRepo) List(ctx context.Context) ([]Alias, error) {
	rows, err := r.db.Read.QueryContext(ctx,
		`SELECT name, channel_id, upstream_model, updated_at FROM aliases ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()
	out := []Alias{}
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.Name, &a.ChannelID, &a.UpstreamModel, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan alias: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get returns one alias or ErrNotFound.
func (r *AliasRepo) Get(ctx context.Context, name string) (Alias, error) {
	var a Alias
	err := r.db.Read.QueryRowContext(ctx,
		`SELECT name, channel_id, upstream_model, updated_at FROM aliases WHERE name = ?`, name).
		Scan(&a.Name, &a.ChannelID, &a.UpstreamModel, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, fmt.Errorf("get alias %q: %w", name, err)
	}
	return a, nil
}

// Delete removes an alias.
func (r *AliasRepo) Delete(ctx context.Context, name string) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM aliases WHERE name = ?`, name)
	return checkAffected(res, err, "delete alias")
}
