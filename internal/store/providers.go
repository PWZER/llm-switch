package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Provider is a vendor account: a name, the models-list fetch URL
// (ModelsURL, absolute — the registry is provider-scoped, so the fetch URL
// is too), plus its protocol endpoints (channels). Credentials live on the
// provider's accounts.
type Provider struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	ModelsURL *string `json:"models_url"`
	Enabled   bool    `json:"enabled"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

// ProviderRepo manages provider rows.
type ProviderRepo struct{ db *DB }

const providerColumns = `id, name, models_url, enabled, created_at, updated_at`

// Create inserts a new vendor account and returns its id.
func (r *ProviderRepo) Create(ctx context.Context, name string, modelsURL *string) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx,
		`INSERT INTO providers (name, models_url, enabled, created_at, updated_at) VALUES (?, ?, 1, ?, ?)`,
		name, modelsURL, now(), now())
	if err != nil {
		return 0, fmt.Errorf("create provider: %w", err)
	}
	return res.LastInsertId()
}

// List returns all providers ordered by name.
func (r *ProviderRepo) List(ctx context.Context) ([]Provider, error) {
	rows, err := r.db.Read.QueryContext(ctx,
		`SELECT `+providerColumns+` FROM providers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.ModelsURL, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
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
		`SELECT `+providerColumns+` FROM providers WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.ModelsURL, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("get provider %d: %w", id, err)
	}
	return p, nil
}

// Update edits a provider's mutable fields (name, models_url).
func (r *ProviderRepo) Update(ctx context.Context, p *Provider) error {
	res, err := r.db.Write.ExecContext(ctx,
		`UPDATE providers SET name = ?, models_url = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.ModelsURL, now(), p.ID)
	return checkAffected(res, err, "update provider")
}

// SetEnabled toggles a provider without touching its channels.
func (r *ProviderRepo) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := r.db.Write.ExecContext(ctx,
		`UPDATE providers SET enabled = ?, updated_at = ? WHERE id = ?`, enabled, now(), id)
	return checkAffected(res, err, "toggle provider")
}

// Delete removes a provider; channels, accounts, and bindings cascade.
func (r *ProviderRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	return checkAffected(res, err, "delete provider")
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
