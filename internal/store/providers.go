package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Provider is a vendor account; its API keys are shared by all of its channels.
type Provider struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// ProviderKey is one upstream credential belonging to a provider.
type ProviderKey struct {
	ID         int64  `json:"id"`
	ProviderID int64  `json:"provider_id"`
	Label      string `json:"label"`
	APIKey     string `json:"-"`            // never serialized; admin API returns a masked copy
	APIKeyMask string `json:"api_key_mask"` // "abcd…1234" display form
	Weight     int    `json:"weight"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// MaskKey returns a display form that never exposes the full secret.
func MaskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

// ProviderRepo manages provider rows.
type ProviderRepo struct{ db *DB }

// CreateProvider inserts a new vendor account and returns its id.
func (r *ProviderRepo) Create(ctx context.Context, name string) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx,
		`INSERT INTO providers (name, enabled, created_at, updated_at) VALUES (?, 1, ?, ?)`,
		name, now(), now())
	if err != nil {
		return 0, fmt.Errorf("create provider: %w", err)
	}
	return res.LastInsertId()
}

// List returns all providers ordered by name.
func (r *ProviderRepo) List(ctx context.Context) ([]Provider, error) {
	rows, err := r.db.Read.QueryContext(ctx,
		`SELECT id, name, enabled, created_at, updated_at FROM providers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan provider: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns one provider or ErrNotFound.
func (r *ProviderRepo) Get(ctx context.Context, id int64) (Provider, error) {
	var p Provider
	err := r.db.Read.QueryRowContext(ctx,
		`SELECT id, name, enabled, created_at, updated_at FROM providers WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("get provider %d: %w", id, err)
	}
	return p, nil
}

// Rename updates a provider's display name.
func (r *ProviderRepo) Rename(ctx context.Context, id int64, name string) error {
	res, err := r.db.Write.ExecContext(ctx,
		`UPDATE providers SET name = ?, updated_at = ? WHERE id = ?`, name, now(), id)
	return checkAffected(res, err, "rename provider")
}

// SetEnabled toggles a provider without touching its channels.
func (r *ProviderRepo) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := r.db.Write.ExecContext(ctx,
		`UPDATE providers SET enabled = ?, updated_at = ? WHERE id = ?`, enabled, now(), id)
	return checkAffected(res, err, "toggle provider")
}

// Delete removes a provider; channels, keys, and bindings cascade.
func (r *ProviderRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	return checkAffected(res, err, "delete provider")
}

// ProviderKeyRepo manages upstream credentials.
type ProviderKeyRepo struct{ db *DB }

// CreateKey inserts a provider API key and returns its id.
func (r *ProviderKeyRepo) Create(ctx context.Context, providerID int64, label, apiKey string, weight int) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO provider_keys (provider_id, label, api_key, weight, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)`,
		providerID, label, apiKey, weight, now(), now())
	if err != nil {
		return 0, fmt.Errorf("create provider key: %w", err)
	}
	return res.LastInsertId()
}

// ListKeys returns the keys of one provider, with the secret masked for display.
func (r *ProviderKeyRepo) List(ctx context.Context, providerID int64) ([]ProviderKey, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, provider_id, label, api_key, weight, enabled, created_at, updated_at
		FROM provider_keys WHERE provider_id = ? ORDER BY id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list provider keys: %w", err)
	}
	defer rows.Close()
	out := []ProviderKey{}
	for rows.Next() {
		var k ProviderKey
		if err := rows.Scan(&k.ID, &k.ProviderID, &k.Label, &k.APIKey, &k.Weight, &k.Enabled, &k.CreatedAt, &k.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan provider key: %w", err)
		}
		k.APIKeyMask = MaskKey(k.APIKey)
		k.APIKey = "" // listings never carry the plaintext secret
		out = append(out, k)
	}
	return out, rows.Err()
}

// EnabledKeys returns the full secrets of a provider's enabled keys, ordered by
// id for deterministic rotation. Used by the routing snapshot, not by the API.
func (r *ProviderKeyRepo) EnabledKeys(ctx context.Context, providerID int64) ([]ProviderKey, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, provider_id, label, api_key, weight, enabled, created_at, updated_at
		FROM provider_keys WHERE provider_id = ? AND enabled = 1 ORDER BY id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list enabled keys: %w", err)
	}
	defer rows.Close()
	out := []ProviderKey{}
	for rows.Next() {
		var k ProviderKey
		if err := rows.Scan(&k.ID, &k.ProviderID, &k.Label, &k.APIKey, &k.Weight, &k.Enabled, &k.CreatedAt, &k.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan enabled key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// UpdateKey changes label/weight/enabled; empty label keeps the current value.
func (r *ProviderKeyRepo) Update(ctx context.Context, id int64, label *string, weight *int, enabled *bool) error {
	res, err := r.db.Write.ExecContext(ctx, `
		UPDATE provider_keys SET
			label   = COALESCE(?, label),
			weight  = COALESCE(?, weight),
			enabled = COALESCE(?, enabled),
			updated_at = ?
		WHERE id = ?`, label, weight, enabled, now(), id)
	return checkAffected(res, err, "update provider key")
}

// DeleteKey removes one credential.
func (r *ProviderKeyRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM provider_keys WHERE id = ?`, id)
	return checkAffected(res, err, "delete provider key")
}

// checkAffected converts a no-rows-affected UPDATE/DELETE into ErrNotFound.
func checkAffected(res sql.Result, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", what, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
