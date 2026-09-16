package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Account is one upstream credential set belonging to exactly one provider.
// The API key is one attribute of the account; quota probes are configured
// per account.
type Account struct {
	ID          int64           `json:"id"`
	ProviderID  int64           `json:"provider_id"`
	Label       string          `json:"label"`
	APIKey      string          `json:"-"`            // never serialized; admin API returns a masked copy
	APIKeyMask  string          `json:"api_key_mask"` // "abcd…1234" display form
	Weight      int             `json:"weight"`
	Enabled     bool            `json:"enabled"`
	UsageProbes json.RawMessage `json:"usage_probes"` // JSON array of {type, path, name?, auth_style?} quota probes
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
	// ProviderName is denormalized for cross-provider listings (ListAll only).
	ProviderName string `json:"provider_name,omitempty"`
}

// MaskKey returns a display form that never exposes the full secret.
func MaskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

// AccountRepo manages upstream accounts.
type AccountRepo struct{ db *DB }

// Create inserts a provider account and returns its id.
func (r *AccountRepo) Create(ctx context.Context, providerID int64, label, apiKey string, weight int, usageProbes string) (int64, error) {
	if usageProbes == "" {
		usageProbes = "[]"
	}
	res, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO accounts (provider_id, label, api_key, weight, enabled, usage_probes, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?)`,
		providerID, label, apiKey, weight, usageProbes, now(), now())
	if err != nil {
		return 0, fmt.Errorf("create account: %w", err)
	}
	return res.LastInsertId()
}

// List returns the accounts of one provider, with the secret masked for display.
func (r *AccountRepo) List(ctx context.Context, providerID int64) ([]Account, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, provider_id, label, api_key, weight, enabled, usage_probes, created_at, updated_at
		FROM accounts WHERE provider_id = ? ORDER BY id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		a.APIKeyMask = MaskKey(a.APIKey)
		a.APIKey = "" // listings never carry the plaintext secret
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAll returns every account across providers, joined with the provider
// name, secrets masked for display.
func (r *AccountRepo) ListAll(ctx context.Context) ([]Account, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT a.id, a.provider_id, a.label, a.api_key, a.weight, a.enabled, a.usage_probes,
			a.created_at, a.updated_at, p.name
		FROM accounts a JOIN providers p ON p.id = a.provider_id
		ORDER BY p.name, a.id`)
	if err != nil {
		return nil, fmt.Errorf("list all accounts: %w", err)
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		var a Account
		var probes string
		if err := rows.Scan(&a.ID, &a.ProviderID, &a.Label, &a.APIKey, &a.Weight, &a.Enabled,
			&probes, &a.CreatedAt, &a.UpdatedAt, &a.ProviderName); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		a.UsageProbes = json.RawMessage(probes)
		a.APIKeyMask = MaskKey(a.APIKey)
		a.APIKey = "" // listings never carry the plaintext secret
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get returns one account with its plaintext secret, or ErrNotFound. Used by
// admin actions that authenticate upstream with the account's key.
func (r *AccountRepo) Get(ctx context.Context, id int64) (Account, error) {
	row := r.db.Read.QueryRowContext(ctx, `
		SELECT id, provider_id, label, api_key, weight, enabled, usage_probes, created_at, updated_at
		FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account %d: %w", id, err)
	}
	return a, nil
}

// EnabledAccounts returns the full secrets of a provider's enabled accounts,
// ordered by id for deterministic rotation. Used by the routing snapshot, not
// by the API.
func (r *AccountRepo) EnabledAccounts(ctx context.Context, providerID int64) ([]Account, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, provider_id, label, api_key, weight, enabled, usage_probes, created_at, updated_at
		FROM accounts WHERE provider_id = ? AND enabled = 1 ORDER BY id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list enabled accounts: %w", err)
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan enabled account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Update changes label/weight/enabled; nil fields keep their current value.
// provider_id is immutable: re-attaching an account means delete + recreate.
func (r *AccountRepo) Update(ctx context.Context, id int64, label *string, weight *int, enabled *bool) error {
	res, err := r.db.Write.ExecContext(ctx, `
		UPDATE accounts SET
			label   = COALESCE(?, label),
			weight  = COALESCE(?, weight),
			enabled = COALESCE(?, enabled),
			updated_at = ?
		WHERE id = ?`, label, weight, enabled, now(), id)
	return checkAffected(res, err, "update account")
}

// SetUsageProbes stores the account's quota probe configuration (JSON array).
func (r *AccountRepo) SetUsageProbes(ctx context.Context, id int64, probesJSON string) error {
	res, err := r.db.Write.ExecContext(ctx,
		`UPDATE accounts SET usage_probes = ?, updated_at = ? WHERE id = ?`, probesJSON, now(), id)
	return checkAffected(res, err, "set usage probes")
}

// Delete removes one account.
func (r *AccountRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	return checkAffected(res, err, "delete account")
}

type accountScanner interface{ Scan(dest ...any) error }

func scanAccount(rows accountScanner) (Account, error) {
	var a Account
	var probes string
	if err := rows.Scan(&a.ID, &a.ProviderID, &a.Label, &a.APIKey, &a.Weight, &a.Enabled,
		&probes, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return a, err
	}
	a.UsageProbes = json.RawMessage(probes)
	return a, nil
}
