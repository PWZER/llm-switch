package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AdminRepo manages the single-row admin password record and UI sessions.
type AdminRepo struct{ db *DB }

// PasswordHash returns the stored bcrypt hash, or ErrNotFound before first boot.
func (r *AdminRepo) PasswordHash(ctx context.Context) (string, error) {
	var h string
	err := r.db.Read.QueryRowContext(ctx, `SELECT password_hash FROM admin_auth WHERE id = 1`).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get admin password: %w", err)
	}
	return h, nil
}

// SetPasswordHash inserts or updates the admin password record.
func (r *AdminRepo) SetPasswordHash(ctx context.Context, hash string) error {
	_, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO admin_auth (id, password_hash, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		hash, now())
	if err != nil {
		return fmt.Errorf("set admin password: %w", err)
	}
	return nil
}

// CreateSession stores a session token with its expiry (unix seconds).
func (r *AdminRepo) CreateSession(ctx context.Context, token string, expiresAt int64) error {
	_, err := r.db.Write.ExecContext(ctx,
		`INSERT INTO sessions (token, created_at, expires_at) VALUES (?, ?, ?)`,
		token, now(), expiresAt)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// SessionExpiry returns the expiry of a valid, unexpired token, or ErrNotFound.
func (r *AdminRepo) SessionExpiry(ctx context.Context, token string) (time.Time, error) {
	var exp int64
	err := r.db.Read.QueryRowContext(ctx,
		`SELECT expires_at FROM sessions WHERE token = ? AND expires_at > ?`,
		token, now()).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("lookup session: %w", err)
	}
	return time.Unix(exp, 0), nil
}

// DeleteSession removes a token (logout).
func (r *AdminRepo) DeleteSession(ctx context.Context, token string) error {
	_, err := r.db.Write.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// PurgeSessions deletes expired sessions and reports how many were removed.
func (r *AdminRepo) PurgeSessions(ctx context.Context) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now())
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
