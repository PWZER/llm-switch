package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// APIKey is a gateway-issued client key. `Key` stores the sha256 hex of the
// plaintext; `Prefix` is the display form ("sk-lsw-abcd…"). The plaintext is
// returned exactly once by the create endpoint and never stored.
type APIKey struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Key        string `json:"-"`
	Prefix     string `json:"prefix"`
	Enabled    bool   `json:"enabled"`
	TokenLimit *int64 `json:"token_limit"`
	ExpiresAt  *int64 `json:"expires_at"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	LastUsedAt *int64 `json:"last_used_at"`
}

// APIKeyRepo manages client-facing gateway keys.
type APIKeyRepo struct{ db *DB }

// Create inserts a key from its hash and display prefix, returning the row id.
func (r *APIKeyRepo) Create(ctx context.Context, name, keyHash, prefix string, tokenLimit, expiresAt *int64) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO api_keys (name, key, prefix, enabled, token_limit, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?, ?, ?)`,
		name, keyHash, prefix, tokenLimit, expiresAt, now(), now())
	if err != nil {
		return 0, fmt.Errorf("create api key: %w", err)
	}
	return res.LastInsertId()
}

// List returns all keys (masked) ordered by id.
func (r *APIKeyRepo) List(ctx context.Context) ([]APIKey, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, name, prefix, enabled, token_limit, expires_at, created_at, updated_at, last_used_at
		FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Enabled, &k.TokenLimit, &k.ExpiresAt,
			&k.CreatedAt, &k.UpdatedAt, &k.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// LookupByHash finds an enabled, unexpired key by its sha256 hash. Used on the
// gateway hot path from the routing snapshot cache.
func (r *APIKeyRepo) LookupByHash(ctx context.Context, keyHash string) (APIKey, error) {
	var k APIKey
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT id, name, prefix, enabled, token_limit, expires_at, created_at, updated_at, last_used_at
		FROM api_keys
		WHERE key = ? AND enabled = 1 AND (expires_at IS NULL OR expires_at > ?)`,
		keyHash, now()).
		Scan(&k.ID, &k.Name, &k.Prefix, &k.Enabled, &k.TokenLimit, &k.ExpiresAt,
			&k.CreatedAt, &k.UpdatedAt, &k.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	if err != nil {
		return k, fmt.Errorf("lookup api key: %w", err)
	}
	return k, nil
}

// EnabledKeyHashes returns (id, name, key-hash) for enabled, unexpired keys.
// Used by the routing snapshot; the hash is the lookup key, not a secret.
func (r *APIKeyRepo) EnabledKeyHashes(ctx context.Context) ([]struct {
	ID   int64
	Name string
	Hash string
}, error) {
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, name, key FROM api_keys
		WHERE enabled = 1 AND (expires_at IS NULL OR expires_at > ?)`, now())
	if err != nil {
		return nil, fmt.Errorf("list api key hashes: %w", err)
	}
	defer rows.Close()
	var out []struct {
		ID   int64
		Name string
		Hash string
	}
	for rows.Next() {
		var row struct {
			ID   int64
			Name string
			Hash string
		}
		if err := rows.Scan(&row.ID, &row.Name, &row.Hash); err != nil {
			return nil, fmt.Errorf("scan api key hash: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Update toggles/renames a key.
func (r *APIKeyRepo) Update(ctx context.Context, id int64, name *string, enabled *bool) error {
	res, err := r.db.Write.ExecContext(ctx, `
		UPDATE api_keys SET name = COALESCE(?, name), enabled = COALESCE(?, enabled), updated_at = ?
		WHERE id = ?`, name, enabled, now(), id)
	return checkAffected(res, err, "update api key")
}

// Delete removes a key; agents using it start receiving 401 immediately
// (after the routing snapshot reloads).
func (r *APIKeyRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return checkAffected(res, err, "delete api key")
}

// TouchLastUsed records key usage asynchronously (throttled by the caller).
func (r *APIKeyRepo) TouchLastUsed(ctx context.Context, id int64) error {
	_, err := r.db.Write.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now(), id)
	return err
}
