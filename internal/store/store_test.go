package store

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
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

func int64p(v int64) *int64 { return &v }

func TestMigrationsIdempotent(t *testing.T) {
	st := newTestStore(t)
	// New() already migrated; running again must be a no-op.
	if err := st.db.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var maxv int
	if err := st.db.Read.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&maxv); err != nil || maxv != 2 {
		t.Fatalf("want schema version 2, got %d (err=%v)", maxv, err)
	}
}

// TestMigrationFoldLegacyModels applies 0001 by hand, plants legacy
// channel_models + registry rows, runs the remaining migrations and checks
// the fold: registry rows keep their metadata, bindings backfill the
// upstream alias (first alias wins on conflicting bindings) and create rows
// for binding-only names; manual rows (provider_id 0) are dropped. The
// per-channel models_url moves up to the provider (first configured wins).
func TestMigrationFoldLegacyModels(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	body, err := migrationsFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read 0001: %v", err)
	}
	if _, err := db.Write.Exec(string(body)); err != nil {
		t.Fatalf("apply 0001: %v", err)
	}
	if _, err := db.Write.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := db.Write.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (1, 1)`); err != nil {
		t.Fatalf("record version 1: %v", err)
	}

	stmts := []string{
		`INSERT INTO providers (id, name, enabled, created_at, updated_at) VALUES (1, 'legacy', 1, 1, 1)`,
		`INSERT INTO channels (id, provider_id, name, protocol, base_url, chat_path, auth_style,
			models_url, extra_headers, enabled, priority, weight, auto_bind, created_at, updated_at)
			VALUES (1, 1, 'legacy-openai', 'openai', 'http://127.0.0.1:1', '/chat/completions', 'bearer',
			'http://127.0.0.1:1/models', '{}', 1, 10, 1, 1, 1, 1)`,
		`INSERT INTO channels (id, provider_id, name, protocol, base_url, chat_path, auth_style,
			extra_headers, enabled, priority, weight, auto_bind, created_at, updated_at)
			VALUES (2, 1, 'legacy-anthropic', 'anthropic', 'http://127.0.0.1:2', '/v1/messages', 'x-api-key',
			'{}', 1, 5, 1, 1, 1, 1)`,
		// Registry metadata row for the bound name (provider-scoped).
		`INSERT INTO models (id, display_name, provider_id, context_window, max_output_tokens, enabled, created_at, updated_at)
			VALUES ('m', 'Legacy M', 1, 131072, 8192, 1, 1, 1)`,
		// Manual row with no provider: must be dropped by the fold.
		`INSERT INTO models (id, display_name, provider_id, enabled, created_at, updated_at)
			VALUES ('orphan', '', 0, 1, 1, 1)`,
		`INSERT INTO channel_models (channel_id, model, upstream_model) VALUES (1, 'm', 'm-001')`,
		// Conflicting alias on the second channel: the first channel's wins.
		`INSERT INTO channel_models (channel_id, model, upstream_model) VALUES (2, 'm', 'm-002')`,
		// Binding-only name (no registry row): a row is created for it.
		`INSERT INTO channel_models (channel_id, model, upstream_model) VALUES (2, 'bound-only', 'bound-upstream')`,
	}
	for _, q := range stmts {
		if _, err := db.Write.Exec(q); err != nil {
			t.Fatalf("seed legacy row: %v\n%s", err, q)
		}
	}

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate to 2: %v", err)
	}

	st, err := New(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()

	m, err := st.Models.Get(ctx, 1, "m")
	if err != nil {
		t.Fatalf("folded row missing: %v", err)
	}
	if m.UpstreamModel != "m-001" || m.DisplayName != "Legacy M" ||
		m.ContextWindow == nil || *m.ContextWindow != 131072 ||
		m.MaxOutputTokens == nil || *m.MaxOutputTokens != 8192 || !m.Enabled {
		t.Fatalf("folded row wrong: %+v", m)
	}

	// The binding-only name became a provider-scoped row with its alias.
	bo, err := st.Models.Get(ctx, 1, "bound-only")
	if err != nil || bo.UpstreamModel != "bound-upstream" || !bo.Enabled {
		t.Fatalf("binding-only row wrong: %+v err=%v", bo, err)
	}

	all, err := st.Models.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("manual orphan rows must be dropped, got %+v", all)
	}

	// The fetch URL moved from the first configured channel to the provider.
	p, err := st.Providers.Get(ctx, 1)
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if p.ModelsURL == nil || *p.ModelsURL != "http://127.0.0.1:1/models" {
		t.Fatalf("models_url not backfilled: %+v", p)
	}

	// auto_bind and models_url are gone from the rebuilt channels table.
	if _, err := db.Read.Query(`SELECT auto_bind FROM channels`); err == nil {
		t.Fatal("auto_bind column survived the rebuild")
	}
	if _, err := db.Read.Query(`SELECT models_url FROM channels`); err == nil {
		t.Fatal("models_url column survived the rebuild")
	}
}

func TestProviderChannelRouteRoundtrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	pid, err := st.Providers.Create(ctx, "deepseek", nil)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	aid, err := st.Accounts.Create(ctx, pid, "primary", "sk-secret-123456", 2, "")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if got, err := st.Accounts.Get(ctx, aid); err != nil || got.APIKey != "sk-secret-123456" || got.ProviderID != pid {
		t.Fatalf("get account: %+v err=%v", got, err)
	}

	cid, err := st.Channels.Create(ctx, &Channel{
		ProviderID: pid, Name: "ds-openai", Protocol: "openai",
		BaseURL: "https://api.deepseek.com", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}",
		Enabled: true, Priority: 10, Weight: 1,
		Passthrough: true,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := st.Models.EnsureModel(ctx, &Model{
		ID: "deepseek-flash", ProviderID: pid, UpstreamModel: "deepseek-flash",
	}); err != nil {
		t.Fatalf("register model: %v", err)
	}

	// Channel roundtrip with provider name; the model row is keyed per provider.
	c, err := st.Channels.Get(ctx, cid)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if c.ProviderName != "deepseek" {
		t.Fatalf("unexpected channel: %+v", c)
	}
	m, err := st.Models.Get(ctx, pid, "deepseek-flash")
	if err != nil || m.Upstream() != "deepseek-flash" {
		t.Fatalf("model row: %+v err=%v", m, err)
	}

	// Accounts are masked in listings but full in EnabledAccounts.
	listed, err := st.Accounts.List(ctx, pid)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list accounts: %v", err)
	}
	if listed[0].APIKey != "" || listed[0].APIKeyMask == "" {
		t.Fatalf("listing leaked or lost the key: %+v", listed[0])
	}
	enabled, err := st.Accounts.EnabledAccounts(ctx, pid)
	if err != nil || len(enabled) != 1 || enabled[0].APIKey != "sk-secret-123456" {
		t.Fatalf("enabled accounts: %v", err)
	}

	// Model route upsert = hot-switch primitive with an ordered failover chain.
	a := ModelRoute{Name: "main", Targets: []ModelRouteTarget{
		{ProviderID: pid, ChannelID: int64p(cid), UpstreamModel: "deepseek-flash"},
	}}
	if err := st.Routes.Upsert(ctx, &a); err != nil {
		t.Fatalf("upsert model route: %v", err)
	}
	a.Targets = []ModelRouteTarget{
		{ProviderID: pid, ChannelID: int64p(cid), UpstreamModel: "deepseek-flash"},
		{ProviderID: pid, ChannelID: int64p(cid), UpstreamModel: "deepseek-v4-pro"},
	}
	if err := st.Routes.Upsert(ctx, &a); err != nil {
		t.Fatalf("re-upsert model route: %v", err)
	}
	got, err := st.Routes.Get(ctx, "main")
	if err != nil || len(got.Targets) != 2 || got.Targets[1].UpstreamModel != "deepseek-v4-pro" {
		t.Fatalf("model route multi-target failed: %+v err=%v", got, err)
	}

	// Deleting the provider cascades to accounts/channels/models rows and strips
	// the provider's channels from model route target chains (routes left empty
	// go away).
	if err := st.DeleteProvider(ctx, pid); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if _, err := st.Channels.Get(ctx, cid); err != ErrNotFound {
		t.Fatalf("channel should cascade-delete, got %v", err)
	}
	var accounts int
	if err := st.db.Read.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil || accounts != 0 {
		t.Fatalf("accounts should cascade-delete, got %d (err=%v)", accounts, err)
	}
	var models int
	if err := st.db.Read.QueryRow(`SELECT COUNT(*) FROM models`).Scan(&models); err != nil || models != 0 {
		t.Fatalf("models should cascade-delete, got %d (err=%v)", models, err)
	}
	var routes int
	if err := st.db.Read.QueryRow(`SELECT COUNT(*) FROM model_routes`).Scan(&routes); err != nil || routes != 0 {
		t.Fatalf("model routes should cascade-delete, got %d (err=%v)", routes, err)
	}
}

// Provider-scoped route targets: provider_id owns the target and channel_id
// (JSON null when absent) optionally pins one endpoint. Deleting a pinned
// endpoint degrades its target to provider auto-select (legacy rows get the
// provider backfilled); deleting the provider removes its targets outright.
func TestModelRouteProviderTargets(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	pid, err := st.Providers.Create(ctx, "p", nil)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	newChannel := func(name string) int64 {
		t.Helper()
		cid, err := st.Channels.Create(ctx, &Channel{
			ProviderID: pid, Name: name, Protocol: "openai",
			BaseURL: "http://127.0.0.1:1", ChatPath: "/chat/completions",
			AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 1, Weight: 1,
		})
		if err != nil {
			t.Fatalf("channel %s: %v", name, err)
		}
		return cid
	}
	cid := newChannel("c1")

	// A provider-scoped (auto-select) target round-trips and stores an
	// explicit JSON null channel_id.
	a := &ModelRoute{Name: "auto", Targets: []ModelRouteTarget{
		{ProviderID: pid, UpstreamModel: "m1"},
	}}
	if err := st.Routes.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert provider target: %v", err)
	}
	if got, err := st.Routes.Get(ctx, "auto"); err != nil || len(got.Targets) != 1 ||
		got.Targets[0].ProviderID != pid || got.Targets[0].ChannelID != nil {
		t.Fatalf("provider target round-trip: %+v err=%v", got, err)
	}
	var rawJSON string
	if err := st.db.Read.QueryRow(`SELECT targets_json FROM model_routes WHERE name = 'auto'`).Scan(&rawJSON); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.Contains(rawJSON, `"channel_id":null`) {
		t.Fatalf("auto target must store an explicit null channel_id: %s", rawJSON)
	}

	// Channel-pinned targets round-trip; a provider-less (legacy) target is
	// tolerated at the store layer.
	a.Targets = []ModelRouteTarget{
		{ProviderID: pid, ChannelID: int64p(cid), UpstreamModel: "m2"},
		{ChannelID: int64p(cid), UpstreamModel: "legacy"},
	}
	if err := st.Routes.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert pinned targets: %v", err)
	}
	if got, err := st.Routes.Get(ctx, "auto"); err != nil || len(got.Targets) != 2 ||
		got.Targets[0].ChannelID == nil || *got.Targets[0].ChannelID != cid ||
		got.Targets[0].ProviderID != pid || got.Targets[1].ProviderID != 0 {
		t.Fatalf("pinned targets round-trip: %+v err=%v", got, err)
	}

	// Guard: a target with neither id, or without an upstream model, is rejected.
	for i, bad := range []ModelRouteTarget{
		{UpstreamModel: "m"},
		{ProviderID: pid},
		{ChannelID: int64p(cid)},
	} {
		if err := st.Routes.Upsert(ctx, &ModelRoute{Name: "bad", Targets: []ModelRouteTarget{bad}}); err == nil {
			t.Fatalf("invalid target %d must be rejected", i)
		}
	}

	// A legacy stored row (no provider_id) still parses.
	if _, err := st.db.Write.ExecContext(ctx,
		`INSERT INTO model_routes (name, targets_json, updated_at) VALUES ('legacy-row', ?, 0)`,
		`[{"channel_id":`+int64ToString(cid)+`,"upstream_model":"m"}]`); err != nil {
		t.Fatalf("plant legacy row: %v", err)
	}

	// Deleting the pinned endpoint degrades both targets of "auto" to provider
	// auto-select and backfills the legacy row.
	if err := st.DeleteChannel(ctx, cid); err != nil {
		t.Fatalf("delete channel: %v", err)
	}
	if got, err := st.Routes.Get(ctx, "auto"); err != nil || len(got.Targets) != 2 {
		t.Fatalf("degraded route lost: %+v err=%v", got, err)
	} else {
		for i, tgt := range got.Targets {
			if tgt.ChannelID != nil || tgt.ProviderID != pid {
				t.Fatalf("target %d must degrade to provider auto: %+v", i, tgt)
			}
		}
	}
	if got, err := st.Routes.Get(ctx, "legacy-row"); err != nil || len(got.Targets) != 1 ||
		got.Targets[0].ChannelID != nil || got.Targets[0].ProviderID != pid {
		t.Fatalf("legacy row must degrade with provider backfill: %+v err=%v", got, err)
	}

	// Degrading onto an identical auto target dedupes instead of stacking a
	// redundant failover copy.
	cid2 := newChannel("c2")
	a.Targets = []ModelRouteTarget{
		{ProviderID: pid, ChannelID: int64p(cid2), UpstreamModel: "m"},
		{ProviderID: pid, UpstreamModel: "m"},
	}
	if err := st.Routes.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert dedupe route: %v", err)
	}
	if err := st.DeleteChannel(ctx, cid2); err != nil {
		t.Fatalf("delete channel 2: %v", err)
	}
	if got, err := st.Routes.Get(ctx, "auto"); err != nil || len(got.Targets) != 1 ||
		got.Targets[0].ChannelID != nil || got.Targets[0].UpstreamModel != "m" {
		t.Fatalf("degraded duplicate must dedupe: %+v err=%v", got, err)
	}

	// Deleting the provider removes its targets outright; other providers'
	// targets survive (a channel pin on a deleted provider has nothing to
	// degrade to).
	pid2, err := st.Providers.Create(ctx, "p2", nil)
	if err != nil {
		t.Fatalf("provider2: %v", err)
	}
	cid3, err := st.Channels.Create(ctx, &Channel{
		ProviderID: pid2, Name: "c3", Protocol: "openai",
		BaseURL: "http://127.0.0.1:2", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 1, Weight: 1,
	})
	if err != nil {
		t.Fatalf("channel3: %v", err)
	}
	mix := &ModelRoute{Name: "mix", Targets: []ModelRouteTarget{
		{ProviderID: pid, UpstreamModel: "m"},
		{ProviderID: pid2, ChannelID: int64p(cid3), UpstreamModel: "m"},
	}}
	if err := st.Routes.Upsert(ctx, mix); err != nil {
		t.Fatalf("upsert mix route: %v", err)
	}
	if err := st.DeleteProvider(ctx, pid); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if got, err := st.Routes.Get(ctx, "mix"); err != nil || len(got.Targets) != 1 ||
		got.Targets[0].ProviderID != pid2 {
		t.Fatalf("mix route must keep only the surviving provider target: %+v err=%v", got, err)
	}
	for _, gone := range []string{"auto", "legacy-row"} {
		if _, err := st.Routes.Get(ctx, gone); err != ErrNotFound {
			t.Fatalf("route %q should be gone, got %v", gone, err)
		}
	}
}

func int64ToString(v int64) string {
	return strconv.FormatInt(v, 10)
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
