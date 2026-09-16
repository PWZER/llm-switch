package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := New(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestMigrationsIdempotent(t *testing.T) {
	st := newTestStore(t)
	// New() already migrated; running again must be a no-op.
	if err := st.db.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var maxv int
	if err := st.db.Read.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&maxv); err != nil || maxv != 1 {
		t.Fatalf("want schema version 1, got %d (err=%v)", maxv, err)
	}
}

func TestProviderChannelAliasRoundtrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	pid, err := st.Providers.Create(ctx, "deepseek")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	kid, err := st.Keys.Create(ctx, pid, "primary", "sk-secret-123456", 2)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	cid, err := st.Channels.Create(ctx, &Channel{
		ProviderID: pid, Name: "ds-openai", Protocol: "openai",
		BaseURL: "https://api.deepseek.com", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}",
		Enabled: true, Priority: 10, Weight: 1, AutoBind: true,
		Passthrough: true,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := st.Channels.ReplaceBindings(ctx, cid, []ChannelModel{
		{ChannelID: cid, Model: "deepseek-flash", UpstreamModel: "deepseek-flash"},
	}); err != nil {
		t.Fatalf("replace bindings: %v", err)
	}

	// Channel roundtrip with bindings and provider name.
	c, err := st.Channels.Get(ctx, cid)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if c.ProviderName != "deepseek" || len(c.Models) != 1 || c.Models[0].UpstreamModel != "deepseek-flash" {
		t.Fatalf("unexpected channel: %+v", c)
	}

	// Keys are masked in listings but full in EnabledKeys.
	listed, err := st.Keys.List(ctx, pid)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list keys: %v", err)
	}
	if listed[0].APIKey != "" || listed[0].APIKeyMask == "" {
		t.Fatalf("listing leaked or lost the key: %+v", listed[0])
	}
	enabled, err := st.Keys.EnabledKeys(ctx, pid)
	if err != nil || len(enabled) != 1 || enabled[0].APIKey != "sk-secret-123456" {
		t.Fatalf("enabled keys: %v", err)
	}

	// Alias upsert = hot-switch primitive.
	a := Alias{Name: "main", ChannelID: cid, UpstreamModel: "deepseek-flash"}
	if err := st.Aliases.Upsert(ctx, &a); err != nil {
		t.Fatalf("upsert alias: %v", err)
	}
	a.UpstreamModel = "deepseek-v4-pro"
	if err := st.Aliases.Upsert(ctx, &a); err != nil {
		t.Fatalf("re-upsert alias: %v", err)
	}
	got, err := st.Aliases.Get(ctx, "main")
	if err != nil || got.UpstreamModel != "deepseek-v4-pro" {
		t.Fatalf("alias hot-switch failed: %+v err=%v", got, err)
	}

	// Deleting the provider cascades to keys/channels/bindings/aliases.
	if err := st.Providers.Delete(ctx, pid); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if _, err := st.Channels.Get(ctx, cid); err != ErrNotFound {
		t.Fatalf("channel should cascade-delete, got %v", err)
	}
	if _, err := st.Keys.List(ctx, kid); err == nil {
		// List of a missing provider returns empty rather than erroring; verify
		// via direct count instead.
		var n int
		if err := st.db.Read.QueryRow(`SELECT COUNT(*) FROM provider_keys`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("keys should cascade-delete, got %d (err=%v)", n, err)
		}
	}
	var aliases int
	if err := st.db.Read.QueryRow(`SELECT COUNT(*) FROM aliases`).Scan(&aliases); err != nil || aliases != 0 {
		t.Fatalf("aliases should cascade-delete, got %d (err=%v)", aliases, err)
	}
}

func TestSettingsAndAPIKeys(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if v, err := st.Settings.Get(ctx, "retention_days"); err != nil || v != "30" {
		t.Fatalf("seeded setting: %q err=%v", v, err)
	}
	if err := st.Settings.SetMany(ctx, map[string]string{"retention_days": "14", "log_bodies": "1"}); err != nil {
		t.Fatalf("set many: %v", err)
	}
	if v, _ := st.Settings.Get(ctx, "retention_days"); v != "14" {
		t.Fatalf("setting update lost: %q", v)
	}

	plain, hash, prefix := "sk-lsw-abcd", "cafe", "sk-lsw-abcd…"
	kid, err := st.APIKeys.Create(ctx, "agent", hash, prefix, nil, nil)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	got, err := st.APIKeys.LookupByHash(ctx, hash)
	if err != nil || got.ID != kid || got.Prefix != prefix {
		t.Fatalf("lookup: %+v err=%v", got, err)
	}
	if _, err := st.APIKeys.LookupByHash(ctx, "wrong"); err != ErrNotFound {
		t.Fatalf("wrong hash should be ErrNotFound, got %v", err)
	}
	if err := st.APIKeys.Update(ctx, kid, nil, boolPtr(false)); err != nil {
		t.Fatalf("disable key: %v", err)
	}
	if _, err := st.APIKeys.LookupByHash(ctx, hash); err != ErrNotFound {
		t.Fatalf("disabled key must not authenticate, got %v", err)
	}
	_ = plain
}

func boolPtr(b bool) *bool { return &b }
