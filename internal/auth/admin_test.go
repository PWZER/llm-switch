package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/PWZER/llm-switch/internal/store"
)

func newTestAdmin(t *testing.T) (*Admin, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir, filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.New(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return NewAdmin(st), st
}

func TestBootstrapGenerated(t *testing.T) {
	a, st := newTestAdmin(t)
	ctx := context.Background()

	pw, err := a.Bootstrap(ctx, "")
	if err != nil || pw == "" {
		t.Fatalf("first bootstrap should generate a password: %q err=%v", pw, err)
	}
	again, err := a.Bootstrap(ctx, "ignored")
	if err != nil || again != "" {
		t.Fatalf("second bootstrap must keep the existing password: %q err=%v", again, err)
	}
	if _, ok := st.Settings.Get(ctx, "retention_days"); ok != nil {
		t.Fatalf("settings must survive: %v", ok)
	}
	if !a.Valid(ctx, mustLogin(t, a, pw)) {
		t.Fatal("login with generated password should work")
	}
}

func TestLoginFlow(t *testing.T) {
	a, _ := newTestAdmin(t)
	ctx := context.Background()
	if _, err := a.Bootstrap(ctx, "hunter22"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	if _, _, err := a.Login(ctx, "wrong"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("want ErrInvalidPassword, got %v", err)
	}
	token, expires, err := a.Login(ctx, "hunter22")
	if err != nil || token == "" || expires.IsZero() {
		t.Fatalf("login: %v", err)
	}
	if !a.Valid(ctx, token) {
		t.Fatal("token should be valid")
	}
	if err := a.ChangePassword(ctx, "wrong", "whatever8"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("change with wrong old password: %v", err)
	}
	if err := a.ChangePassword(ctx, "hunter22", "newpass99"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, _, err := a.Login(ctx, "hunter22"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatal("old password must stop working")
	}
	if _, _, err := a.Login(ctx, "newpass99"); err != nil {
		t.Fatalf("new password login: %v", err)
	}
	if err := a.Logout(ctx, token); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if a.Valid(ctx, token) {
		t.Fatal("token must be invalid after logout")
	}
}

func mustLogin(t *testing.T, a *Admin, pw string) string {
	t.Helper()
	token, _, err := a.Login(context.Background(), pw)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return token
}
