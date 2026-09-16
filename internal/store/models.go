package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Model is one provider-served client-facing model name: the row is both the
// registry entry (listing, metadata, enabled flag) and the routing record
// (the provider whose live channels serve the name, under which upstream
// alias), keyed (provider_id, id). UpstreamModel "" means the identity
// mapping.
type Model struct {
	ID              string `json:"id"`
	ProviderID      int64  `json:"provider_id"`
	UpstreamModel   string `json:"upstream_model"`
	DisplayName     string `json:"display_name"`
	ContextWindow   *int64 `json:"context_window"`
	MaxOutputTokens *int64 `json:"max_output_tokens"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// Upstream returns the name sent to the upstream: the alias, or the
// client-facing id when no alias is set.
func (m *Model) Upstream() string {
	if m.UpstreamModel != "" {
		return m.UpstreamModel
	}
	return m.ID
}

// ModelRouteTarget is one entry of a model route's ordered failover chain.
// AccountID optionally pins the target to one account of the target channel's
// provider; 0/nil means the provider's account pool rotates as usual.
type ModelRouteTarget struct {
	ChannelID     int64  `json:"channel_id"`
	UpstreamModel string `json:"upstream_model"`
	AccountID     *int64 `json:"account_id,omitempty"`
}

// ModelRoute is a stable client-facing model name resolving to an ordered
// list of (channel, upstream_model) targets: the first healthy target
// serves, the rest are failover.
type ModelRoute struct {
	Name      string             `json:"name"`
	Targets   []ModelRouteTarget `json:"targets"`
	UpdatedAt int64              `json:"updated_at"`
}

// ModelRepo manages the per-provider model rows.
type ModelRepo struct{ db *DB }

const modelColumns = `id, provider_id, upstream_model, display_name, context_window,
	max_output_tokens, enabled, created_at, updated_at`

// Create inserts one model row on a provider.
func (r *ModelRepo) Create(ctx context.Context, m *Model) error {
	_, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO models (id, provider_id, upstream_model, display_name, context_window,
			max_output_tokens, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ProviderID, m.UpstreamModel, m.DisplayName, m.ContextWindow,
		m.MaxOutputTokens, m.Enabled, now(), now())
	if err != nil {
		return fmt.Errorf("create model %q on provider %d: %w", m.ID, m.ProviderID, err)
	}
	return nil
}

// List returns all model rows ordered by id, then provider_id — deterministic
// for the snapshot merge.
func (r *ModelRepo) List(ctx context.Context) ([]Model, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT `+modelColumns+` FROM models ORDER BY id, provider_id`)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	out := []Model{}
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.UpstreamModel, &m.DisplayName,
			&m.ContextWindow, &m.MaxOutputTokens, &m.Enabled, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns one model row by (providerID, id) or ErrNotFound.
func (r *ModelRepo) Get(ctx context.Context, providerID int64, id string) (Model, error) {
	var m Model
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT `+modelColumns+` FROM models WHERE provider_id = ? AND id = ?`, providerID, id).
		Scan(&m.ID, &m.ProviderID, &m.UpstreamModel, &m.DisplayName, &m.ContextWindow,
			&m.MaxOutputTokens, &m.Enabled, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("get model %q: %w", id, err)
	}
	return m, nil
}

// EnsureModel inserts a model row when absent, and backfills context_window /
// max_output_tokens on existing rows that are still NULL (upstreams rarely
// return limits; when one does, refresh fills the blanks). Display_name,
// upstream_model, enabled and any manually-set limits are never touched.
// Reports true when the row was inserted or enriched.
func (r *ModelRepo) EnsureModel(ctx context.Context, m *Model) (bool, error) {
	res, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO models (id, provider_id, upstream_model, display_name, context_window,
			max_output_tokens, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(provider_id, id) DO UPDATE SET
			context_window    = COALESCE(models.context_window, excluded.context_window),
			max_output_tokens = COALESCE(models.max_output_tokens, excluded.max_output_tokens),
			updated_at        = excluded.updated_at
		WHERE (models.context_window IS NULL AND excluded.context_window IS NOT NULL)
		   OR (models.max_output_tokens IS NULL AND excluded.max_output_tokens IS NOT NULL)`,
		m.ID, m.ProviderID, m.Upstream(), m.ID, m.ContextWindow, m.MaxOutputTokens, now(), now())
	if err != nil {
		return false, fmt.Errorf("ensure model %q: %w", m.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Update edits a model row identified by m.ProviderID + m.ID.
func (r *ModelRepo) Update(ctx context.Context, m *Model) error {
	res, err := r.db.Write.ExecContext(ctx, `
		UPDATE models SET upstream_model = ?, display_name = ?, enabled = ?,
			context_window = ?, max_output_tokens = ?, updated_at = ?
		WHERE provider_id = ? AND id = ?`,
		m.UpstreamModel, m.DisplayName, m.Enabled, m.ContextWindow, m.MaxOutputTokens,
		now(), m.ProviderID, m.ID)
	return checkAffected(res, err, "update model")
}

// Delete removes a model row by (providerID, id).
func (r *ModelRepo) Delete(ctx context.Context, providerID int64, id string) error {
	res, err := r.db.Write.ExecContext(ctx,
		`DELETE FROM models WHERE provider_id = ? AND id = ?`, providerID, id)
	return checkAffected(res, err, "delete model")
}

// FirstEnabled returns the first enabled model row of a provider (test probes
// pick it for real mini completions) or ErrNotFound.
func (r *ModelRepo) FirstEnabled(ctx context.Context, providerID int64) (Model, error) {
	var m Model
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT `+modelColumns+` FROM models WHERE provider_id = ? AND enabled = 1
		ORDER BY id LIMIT 1`, providerID).
		Scan(&m.ID, &m.ProviderID, &m.UpstreamModel, &m.DisplayName, &m.ContextWindow,
			&m.MaxOutputTokens, &m.Enabled, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("first enabled model of provider %d: %w", providerID, err)
	}
	return m, nil
}

// ModelRouteRepo manages hot-switchable model routes.
type ModelRouteRepo struct{ db *DB }

// Upsert inserts or replaces a model route's whole target chain
// (the hot-switch primitive — atomic per row).
func (r *ModelRouteRepo) Upsert(ctx context.Context, a *ModelRoute) error {
	if len(a.Targets) == 0 {
		return fmt.Errorf("model route %q needs at least one target", a.Name)
	}
	targetsJSON, err := json.Marshal(a.Targets)
	if err != nil {
		return fmt.Errorf("marshal model route targets: %w", err)
	}
	_, err = r.db.Write.ExecContext(ctx, `
		INSERT INTO model_routes (name, targets_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			targets_json = excluded.targets_json,
			updated_at = excluded.updated_at`,
		a.Name, string(targetsJSON), now())
	if err != nil {
		return fmt.Errorf("upsert model route %q: %w", a.Name, err)
	}
	return nil
}

func parseRouteTargets(raw string) []ModelRouteTarget {
	var targets []ModelRouteTarget
	_ = json.Unmarshal([]byte(raw), &targets)
	return targets
}

// List returns all model routes ordered by name.
func (r *ModelRouteRepo) List(ctx context.Context) ([]ModelRoute, error) {
	rows, err := r.db.Read.QueryContext(ctx,
		`SELECT name, targets_json, updated_at FROM model_routes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list model routes: %w", err)
	}
	defer rows.Close()
	out := []ModelRoute{}
	for rows.Next() {
		var a ModelRoute
		var raw string
		if err := rows.Scan(&a.Name, &raw, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model route: %w", err)
		}
		a.Targets = parseRouteTargets(raw)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get returns one model route or ErrNotFound.
func (r *ModelRouteRepo) Get(ctx context.Context, name string) (ModelRoute, error) {
	var a ModelRoute
	var raw string
	err := r.db.Read.QueryRowContext(ctx,
		`SELECT name, targets_json, updated_at FROM model_routes WHERE name = ?`, name).
		Scan(&a.Name, &raw, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, fmt.Errorf("get model route %q: %w", name, err)
	}
	a.Targets = parseRouteTargets(raw)
	return a, nil
}

// Delete removes a model route.
func (r *ModelRouteRepo) Delete(ctx context.Context, name string) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM model_routes WHERE name = ?`, name)
	return checkAffected(res, err, "delete model route")
}

// RemoveChannel strips a channel from every model route's target chain and
// deletes routes left with no targets. Called when a channel (or its
// provider) is removed, so no route silently holds dangling targets.
func (r *ModelRouteRepo) RemoveChannel(ctx context.Context, channelID int64) error {
	routes, err := r.List(ctx)
	if err != nil {
		return err
	}
	for _, a := range routes {
		kept := a.Targets[:0:0]
		for _, tgt := range a.Targets {
			if tgt.ChannelID != channelID {
				kept = append(kept, tgt)
			}
		}
		switch {
		case len(kept) == len(a.Targets):
			// untouched
		case len(kept) == 0:
			if err := r.Delete(ctx, a.Name); err != nil {
				return err
			}
		default:
			if err := r.Upsert(ctx, &ModelRoute{Name: a.Name, Targets: kept, UpdatedAt: now()}); err != nil {
				return err
			}
		}
	}
	return nil
}
