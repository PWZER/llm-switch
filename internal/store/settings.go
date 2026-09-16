package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// SettingsRepo manages the flat key/value settings table.
type SettingsRepo struct{ db *DB }

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Get returns the value for key, or ErrNotFound.
func (r *SettingsRepo) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := r.db.Read.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting %q: %w", key, err)
	}
	return v, nil
}

// GetAll returns every setting keyed by name, sorted for stable output.
func (r *SettingsRepo) GetAll(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.Read.QueryContext(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	var keys []string
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[k] = v
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate settings: %w", err)
	}
	sort.Strings(keys)
	return out, nil
}

// Set upserts one setting.
func (r *SettingsRepo) Set(ctx context.Context, key, value string) error {
	_, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, now())
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}

// SetMany upserts several settings in one transaction.
func (r *SettingsRepo) SetMany(ctx context.Context, kv map[string]string) error {
	tx, err := r.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin settings tx: %w", err)
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now()); err != nil {
			return fmt.Errorf("set setting %q: %w", k, err)
		}
	}
	return tx.Commit()
}
