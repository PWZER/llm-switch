// Package auth provides admin password bootstrap/login (bcrypt + sessions)
// and client gateway-key verification helpers.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/PWZER/llm-switch/internal/store"
)

const sessionTTL = 7 * 24 * time.Hour

// Admin handles password bootstrap, login throttling, and sessions.
type Admin struct {
	st *store.Store

	mu       sync.Mutex
	failures int
	lockUntil time.Time
}

// NewAdmin creates the admin authenticator.
func NewAdmin(st *store.Store) *Admin { return &Admin{st: st} }

// Bootstrap ensures a password hash exists. It returns the plaintext password
// only when it generated one (so the caller can log it once); otherwise the
// returned string is empty.
func (a *Admin) Bootstrap(ctx context.Context, seed string) (generated string, err error) {
	_, err = a.st.Admin.PasswordHash(ctx)
	switch {
	case err == nil:
		return "", nil // already provisioned; seed ignored
	case errors.Is(err, store.ErrNotFound):
	default:
		return "", fmt.Errorf("check admin password: %w", err)
	}

	if seed == "" {
		raw := make([]byte, 12)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("generate admin password: %w", err)
		}
		seed = hex.EncodeToString(raw)
		generated = seed
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(seed), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash admin password: %w", err)
	}
	if err := a.st.Admin.SetPasswordHash(ctx, string(hash)); err != nil {
		return "", err
	}
	return generated, nil
}

// ErrLocked is returned while the login endpoint is throttled.
var ErrLocked = errors.New("too many failed logins; try again later")

// ErrInvalidPassword is returned for a wrong password.
var ErrInvalidPassword = errors.New("invalid password")

// Login verifies the password and issues a session token.
func (a *Admin) Login(ctx context.Context, password string) (token string, expires time.Time, err error) {
	a.mu.Lock()
	if time.Now().Before(a.lockUntil) {
		remaining := a.lockUntil
		a.mu.Unlock()
		return "", time.Time{}, fmt.Errorf("%w (until %s)", ErrLocked, remaining.Format(time.Kitchen))
	}
	a.mu.Unlock()

	hash, err := a.st.Admin.PasswordHash(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return "", time.Time{}, ErrInvalidPassword
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		a.mu.Lock()
		a.failures++
		if a.failures >= 5 {
			a.lockUntil = time.Now().Add(time.Minute)
			a.failures = 0
		}
		a.mu.Unlock()
		return "", time.Time{}, ErrInvalidPassword
	}

	a.mu.Lock()
	a.failures = 0
	a.mu.Unlock()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}
	token = hex.EncodeToString(raw)
	expires = time.Now().Add(sessionTTL)
	if err := a.st.Admin.CreateSession(ctx, token, expires.Unix()); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// Valid reports whether a session token is currently active.
func (a *Admin) Valid(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	_, err := a.st.Admin.SessionExpiry(ctx, token)
	return err == nil
}

// Logout removes a session token.
func (a *Admin) Logout(ctx context.Context, token string) error {
	return a.st.Admin.DeleteSession(ctx, token)
}

// ChangePassword verifies the old password and stores a new hash.
func (a *Admin) ChangePassword(ctx context.Context, oldPassword, newPassword string) error {
	hash, err := a.st.Admin.PasswordHash(ctx)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(oldPassword)) != nil {
		return ErrInvalidPassword
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash new password: %w", err)
	}
	return a.st.Admin.SetPasswordHash(ctx, string(newHash))
}

// HashKey returns the sha256 hex of a client key (storage/lookup form).
func HashKey(key string) string {
	sum := sha256Hex(key)
	return sum
}
