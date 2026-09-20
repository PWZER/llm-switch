package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// seedTransferFixture builds one fully-populated provider (account, two
// channels, two model rows), a route pinning both channels and the account,
// one client key and two settings — the whole dependency closure in one place.
func seedTransferFixture(t *testing.T, st *Store) (pid, aid, cidOpenAI, cidAnthro int64) {
	t.Helper()
	ctx := context.Background()

	pid, err := st.Providers.Create(ctx, "ds", strp("https://api.deepseek.com/models"))
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	aid, err = st.Accounts.Create(ctx, pid, "main", "sk-ds-secret-0001", 2,
		`[{"type":"balance","path":"/user/balance","auth_style":"bearer"}]`)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	cidOpenAI, err = st.Channels.Create(ctx, &Channel{
		ProviderID: pid, Name: "ds-openai", Protocol: "openai",
		BaseURL: "https://api.deepseek.com", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ResponsesPath: strp("/responses"), ExtraHeaders: `{"X-Debug":"1"}`,
		Enabled: true, Priority: 10, Weight: 1, Passthrough: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	cidAnthro, err = st.Channels.Create(ctx, &Channel{
		ProviderID: pid, Name: "ds-anthropic", Protocol: "anthropic",
		BaseURL: "https://api.deepseek.com/anthropic",
		AuthStyle: "x-api-key", ExtraHeaders: "{}",
		Enabled: true, Priority: 5, Weight: 1,
	})
	if err != nil {
		t.Fatalf("channel 2: %v", err)
	}
	if err := st.Models.Create(ctx, &Model{
		ID: "deepseek-chat", ProviderID: pid, DisplayName: "DeepSeek Chat",
		ContextWindow: int64p(65536), MaxOutputTokens: int64p(8192), Enabled: true,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if err := st.Models.Create(ctx, &Model{
		ID: "deepseek-pro", ProviderID: pid, UpstreamModel: "deepseek-v4-pro", Enabled: false,
	}); err != nil {
		t.Fatalf("model 2: %v", err)
	}
	if err := st.Routes.Upsert(ctx, &ModelRoute{Name: "main", Targets: []ModelRouteTarget{
		{ProviderID: pid, ChannelID: int64p(cidOpenAI), UpstreamModel: "deepseek-chat"},
		{ProviderID: pid, ChannelID: int64p(cidAnthro), AccountID: int64p(aid), UpstreamModel: "deepseek-pro"},
	}}); err != nil {
		t.Fatalf("route: %v", err)
	}
	if _, err := st.APIKeys.Create(ctx, "agent", testKeyHash, "sk-lsw-ab…89", int64p(1000), nil); err != nil {
		t.Fatalf("api key: %v", err)
	}
	if err := st.Settings.SetMany(ctx, map[string]string{"retention_days": "14", "log_bodies": "true"}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	return pid, aid, cidOpenAI, cidAnthro
}

const testKeyHash = "abababababababababababababababababababababababababababababababab"

func strp(s string) *string { return &s }

// Round-trip: export everything from one store, import into a fresh one, and
// verify every row landed with its natural identity — including plaintext
// account keys, full model metadata and route pins remapped to the new IDs.
func TestConfigExportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	srcPID, _, _, _ := seedTransferFixture(t, src)

	doc, err := src.ExportConfig(ctx, ExportSections, true)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if doc.Version != ExportVersion {
		t.Fatalf("version: %d", doc.Version)
	}

	dst := newTestStore(t)
	// Pre-seed one provider so the destination's autoincrement ids diverge
	// from the source's — pins must remap, never copy.
	if _, err := dst.Providers.Create(ctx, "pre-existing", nil); err != nil {
		t.Fatalf("pre-seed provider: %v", err)
	}
	res, err := dst.ImportConfig(ctx, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Providers.Created != 1 || res.Accounts.Created != 1 || res.Channels.Created != 2 ||
		res.Models.Created != 2 || res.ModelRoutes.Created != 1 || res.APIKeys.Created != 1 {
		t.Fatalf("unexpected counts: %+v", res)
	}

	ps, err := dst.Providers.List(ctx)
	if err != nil || len(ps) != 2 {
		t.Fatalf("providers: %+v err=%v", ps, err)
	}
	var dstPID int64
	for _, p := range ps {
		if p.Name == "ds" {
			dstPID = p.ID
			if p.ModelsURL == nil || *p.ModelsURL != "https://api.deepseek.com/models" || !p.Enabled {
				t.Fatalf("provider: %+v", p)
			}
		}
	}
	if dstPID == 0 || dstPID == srcPID {
		t.Fatalf("ids must be remapped, not copied: src=%d dst=%d", srcPID, dstPID)
	}

	accounts, err := dst.Accounts.List(ctx, dstPID)
	if err != nil || len(accounts) != 1 || accounts[0].Label != "main" || accounts[0].Weight != 2 || !accounts[0].Enabled {
		t.Fatalf("accounts: %+v err=%v", accounts, err)
	}
	a, err := dst.Accounts.Get(ctx, accounts[0].ID)
	if err != nil || a.APIKey != "sk-ds-secret-0001" ||
		string(a.UsageProbes) != `[{"type":"balance","path":"/user/balance","auth_style":"bearer"}]` {
		t.Fatalf("account plaintext/probes: %+v err=%v", a, err)
	}

	chs, err := dst.Channels.List(ctx)
	if err != nil || len(chs) != 2 {
		t.Fatalf("channels: %+v err=%v", chs, err)
	}
	byName := map[string]Channel{}
	for _, c := range chs {
		byName[c.Name] = c
	}
	openai, ok := byName["ds-openai"]
	if !ok || openai.Protocol != "openai" || openai.BaseURL != "https://api.deepseek.com" ||
		openai.ChatPath != "/chat/completions" || openai.ResponsesPath == nil || *openai.ResponsesPath != "/responses" ||
		openai.ExtraHeaders != `{"X-Debug":"1"}` || openai.AuthStyle != "bearer" ||
		openai.Priority != 10 || !openai.Passthrough || !openai.Enabled {
		t.Fatalf("openai channel: %+v", openai)
	}
	anthro, ok := byName["ds-anthropic"]
	if !ok || anthro.Protocol != "anthropic" || anthro.BaseURL != "https://api.deepseek.com/anthropic" ||
		anthro.AuthStyle != "x-api-key" || anthro.ResponsesPath != nil || anthro.Priority != 5 {
		t.Fatalf("anthropic channel: %+v", anthro)
	}

	m1, err := dst.Models.Get(ctx, dstPID, "deepseek-chat")
	if err != nil || m1.UpstreamModel != "" || m1.DisplayName != "DeepSeek Chat" ||
		m1.ContextWindow == nil || *m1.ContextWindow != 65536 ||
		m1.MaxOutputTokens == nil || *m1.MaxOutputTokens != 8192 || !m1.Enabled {
		t.Fatalf("model 1: %+v err=%v", m1, err)
	}
	m2, err := dst.Models.Get(ctx, dstPID, "deepseek-pro")
	if err != nil || m2.UpstreamModel != "deepseek-v4-pro" || m2.Enabled {
		t.Fatalf("model 2: %+v err=%v", m2, err)
	}

	route, err := dst.Routes.Get(ctx, "main")
	if err != nil || len(route.Targets) != 2 {
		t.Fatalf("route: %+v err=%v", route, err)
	}
	t0, t1 := route.Targets[0], route.Targets[1]
	if t0.ProviderID != dstPID || t0.ChannelID == nil || *t0.ChannelID != openai.ID ||
		t0.AccountID != nil || t0.UpstreamModel != "deepseek-chat" {
		t.Fatalf("target 0 not remapped: %+v (openai id %d)", t0, openai.ID)
	}
	if t1.ChannelID == nil || *t1.ChannelID != anthro.ID || t1.AccountID == nil || *t1.AccountID != accounts[0].ID {
		t.Fatalf("target 1 not remapped: %+v (anthro id %d, account id %d)", t1, anthro.ID, accounts[0].ID)
	}

	if got, err := dst.APIKeys.LookupByHash(ctx, testKeyHash); err != nil || got.Name != "agent" ||
		got.TokenLimit == nil || *got.TokenLimit != 1000 {
		t.Fatalf("client key hash must stay valid: %+v err=%v", got, err)
	}
	if v, err := dst.Settings.Get(ctx, "retention_days"); err != nil || v != "14" {
		t.Fatalf("settings: %q err=%v", v, err)
	}
	if v, _ := dst.Settings.Get(ctx, "log_bodies"); v != "true" {
		t.Fatalf("log_bodies: %q", v)
	}
}

// Upsert-merge: a pre-existing provider matched by name is updated, children
// are matched/created individually, and rows the document does not mention
// (the extra account, the unrelated route) are left untouched.
func TestImportUpsertMerge(t *testing.T) {
	ctx := context.Background()
	dst := newTestStore(t)
	pid, err := dst.Providers.Create(ctx, "ds", strp("https://old.example.com/models"))
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	extraID, err := dst.Accounts.Create(ctx, pid, "extra", "sk-extra-key-0001", 1, "")
	if err != nil {
		t.Fatalf("extra account: %v", err)
	}
	cid, err := dst.Channels.Create(ctx, &Channel{
		ProviderID: pid, Name: "ds-openai", Protocol: "openai",
		BaseURL: "https://old.example.com", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if _, err := dst.Models.EnsureModel(ctx, &Model{ID: "deepseek-chat", ProviderID: pid, DisplayName: "Old Name"}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if err := dst.Routes.Upsert(ctx, &ModelRoute{Name: "keepme", Targets: []ModelRouteTarget{
		{ProviderID: pid, ChannelID: int64p(cid), UpstreamModel: "deepseek-chat"},
	}}); err != nil {
		t.Fatalf("route: %v", err)
	}

	src := newTestStore(t)
	_, _, _, _ = seedTransferFixture(t, src)
	doc, err := src.ExportConfig(ctx, []string{SectionProviders, SectionRoutes}, true)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	res, err := dst.ImportConfig(ctx, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Providers.Updated != 1 || res.Providers.Created != 0 {
		t.Fatalf("provider counts: %+v", res.Providers)
	}
	if res.Channels.Updated != 1 || res.Channels.Created != 1 {
		t.Fatalf("channel counts: %+v", res.Channels)
	}
	if res.Accounts.Created != 1 || res.Accounts.Updated != 0 {
		t.Fatalf("account counts: %+v", res.Accounts)
	}
	if res.Models.Updated != 1 || res.Models.Created != 1 {
		t.Fatalf("model counts: %+v", res.Models)
	}

	ps, _ := dst.Providers.List(ctx)
	if len(ps) != 1 || ps[0].ModelsURL == nil || *ps[0].ModelsURL != "https://api.deepseek.com/models" {
		t.Fatalf("provider must be updated in place: %+v", ps)
	}
	chs, _ := dst.Channels.List(ctx)
	for _, c := range chs {
		if c.Name == "ds-openai" && (c.ID != cid || c.BaseURL != "https://api.deepseek.com") {
			t.Fatalf("matched channel must be updated in place (id %d): %+v", cid, c)
		}
	}
	if a, err := dst.Accounts.Get(ctx, extraID); err != nil || a.Label != "extra" || a.Weight != 1 {
		t.Fatalf("unmentioned account must be untouched: %+v err=%v", a, err)
	}
	if r, err := dst.Routes.Get(ctx, "keepme"); err != nil || len(r.Targets) != 1 || r.Targets[0].ChannelID == nil || *r.Targets[0].ChannelID != cid {
		t.Fatalf("unmentioned route must be untouched: %+v err=%v", r, err)
	}
	m, _ := dst.Models.Get(ctx, pid, "deepseek-chat")
	if m.DisplayName != "DeepSeek Chat" {
		t.Fatalf("matched model row must be fully updated: %+v", m)
	}
	if _, err := dst.Models.Get(ctx, pid, "deepseek-pro"); err != nil {
		t.Fatalf("new model row missing: %v", err)
	}
}

// Account matching: identical key matches across a renamed label; a keyless
// document entry matches by label and must preserve the stored key; a keyless
// entry that cannot match is skipped with a warning instead of creating a
// permanently broken account.
func TestImportAccountMatching(t *testing.T) {
	ctx := context.Background()

	// (a) key match wins across a label change: no duplicate row.
	dst := newTestStore(t)
	pid, _ := dst.Providers.Create(ctx, "pa", nil)
	aid, _ := dst.Accounts.Create(ctx, pid, "old", "sk-match-key-123456", 1, "")
	doc := &ConfigExport{Version: ExportVersion, Providers: []ExportProvider{{
		Name: "pa",
		Accounts: []ExportAccount{{
			Label: "new", APIKey: "sk-match-key-123456", Weight: 5, Enabled: true,
			UsageProbes: json.RawMessage("[]"),
		}},
	}}}
	res, err := dst.ImportConfig(ctx, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Accounts.Updated != 1 || res.Accounts.Created != 0 {
		t.Fatalf("key match must update, counts: %+v", res.Accounts)
	}
	a, _ := dst.Accounts.Get(ctx, aid)
	if a.Label != "new" || a.Weight != 5 || a.APIKey != "sk-match-key-123456" {
		t.Fatalf("matched account wrong: %+v", a)
	}

	// (b) keyless entry matched by label: fields update, the stored key stays.
	dst = newTestStore(t)
	pid, _ = dst.Providers.Create(ctx, "pb", nil)
	aid, _ = dst.Accounts.Create(ctx, pid, "old", "sk-keep-key-0123456", 1, "")
	doc = &ConfigExport{Version: ExportVersion, Providers: []ExportProvider{{
		Name: "pb",
		Accounts: []ExportAccount{{
			Label: "old", Weight: 3, Enabled: false, UsageProbes: json.RawMessage("[]"),
		}},
	}}}
	res, err = dst.ImportConfig(ctx, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Accounts.Updated != 1 || res.Accounts.Skipped != 0 {
		t.Fatalf("label match counts: %+v", res.Accounts)
	}
	a, _ = dst.Accounts.Get(ctx, aid)
	if a.APIKey != "sk-keep-key-0123456" || a.Weight != 3 || a.Enabled {
		t.Fatalf("label-matched account wrong: %+v", a)
	}

	// (c) keyless entry with no label: cannot match, must not be created.
	dst = newTestStore(t)
	pid, _ = dst.Providers.Create(ctx, "pc", nil)
	dst.Accounts.Create(ctx, pid, "other", "sk-unrelated-01234", 1, "")
	doc = &ConfigExport{Version: ExportVersion, Providers: []ExportProvider{{
		Name: "pc",
		Accounts: []ExportAccount{{Label: "", Weight: 1, Enabled: true, UsageProbes: json.RawMessage("[]")}},
	}}}
	res, err = dst.ImportConfig(ctx, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Accounts.Skipped != 1 || res.Accounts.Created != 0 || len(res.Warnings) == 0 {
		t.Fatalf("keyless unmatchable account must skip with warning: %+v", res)
	}
	if listed, _ := dst.Accounts.List(ctx, pid); len(listed) != 1 {
		t.Fatalf("no account row may be created: %+v", listed)
	}
}

// Route-target pins resolve by name against the destination DB: known pins are
// remapped, unknown pins degrade to provider auto-select with a warning,
// unknown providers drop the target, and a provider referenced only by the
// route is still found when the document carries no providers section.
func TestImportRoutePinRemapAndDegrade(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	srcPID, _, _, _ := seedTransferFixture(t, src)

	// A second route whose first target pins an unknown channel and whose
	// second target references an unknown provider.
	if err := src.Routes.Upsert(ctx, &ModelRoute{Name: "broken", Targets: []ModelRouteTarget{
		{ProviderID: srcPID, UpstreamModel: "deepseek-chat"},
	}}); err != nil {
		t.Fatalf("route: %v", err)
	}
	doc, err := src.ExportConfig(ctx, ExportSections, true)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	doc.ModelRoutes = append(doc.ModelRoutes,
		ExportRoute{Name: "degraded", Targets: []ExportRouteTarget{
			{Provider: "ds", Channel: strp("nope"), UpstreamModel: "deepseek-chat"},
		}},
		ExportRoute{Name: "dead", Targets: []ExportRouteTarget{
			{Provider: "ghost", UpstreamModel: "m"},
		}},
	)

	dst := newTestStore(t)
	res, err := dst.ImportConfig(ctx, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "degraded") || !strings.Contains(joined, "nope") {
		t.Fatalf("unknown channel pin must warn: %v", res.Warnings)
	}
	if !strings.Contains(joined, "dead") || !strings.Contains(joined, "ghost") {
		t.Fatalf("unknown provider target must warn: %v", res.Warnings)
	}

	main, err := dst.Routes.Get(ctx, "main")
	if err != nil || len(main.Targets) != 2 {
		t.Fatalf("main route: %+v err=%v", main, err)
	}
	ps, _ := dst.Providers.List(ctx)
	dstPID := ps[0].ID
	degraded, err := dst.Routes.Get(ctx, "degraded")
	if err != nil || len(degraded.Targets) != 1 || degraded.Targets[0].ChannelID != nil ||
		degraded.Targets[0].ProviderID != dstPID {
		t.Fatalf("unknown pin must degrade to provider auto-select: %+v err=%v", degraded, err)
	}
	if _, err := dst.Routes.Get(ctx, "dead"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("route with no resolvable targets must be skipped, got %v", err)
	}

	// Routes-only document: the provider already exists in the destination and
	// is not in the document — target resolution must still succeed.
	routesOnly := &ConfigExport{Version: ExportVersion, ModelRoutes: []ExportRoute{{
		Name: "solo", Targets: []ExportRouteTarget{
			{Provider: "ds", Channel: strp("ds-openai"), UpstreamModel: "deepseek-chat"},
		},
	}}}
	res, err = dst.ImportConfig(ctx, routesOnly)
	if err != nil {
		t.Fatalf("routes-only import: %v", err)
	}
	if res.ModelRoutes.Created != 1 {
		t.Fatalf("routes-only counts: %+v", res.ModelRoutes)
	}
	solo, err := dst.Routes.Get(ctx, "solo")
	if err != nil || len(solo.Targets) != 1 || solo.Targets[0].ProviderID != dstPID ||
		solo.Targets[0].ChannelID == nil {
		t.Fatalf("routes-only pin resolution: %+v err=%v", solo, err)
	}
}

// Settings import: values are validated against the allowlist kinds, unknown
// or internal keys are dropped with warnings, and the seeded_providers guard
// is never touched.
func TestImportSettingsAllowlist(t *testing.T) {
	ctx := context.Background()
	dst := newTestStore(t)

	bad := &ConfigExport{Version: ExportVersion, Settings: map[string]string{"retention_days": "abc"}}
	if _, err := dst.ImportConfig(ctx, bad); !errors.Is(err, ErrBadDocument) {
		t.Fatalf("bad int value must be ErrBadDocument, got %v", err)
	}

	if err := dst.Settings.Set(ctx, "seeded_providers", "1"); err != nil {
		t.Fatalf("seed guard: %v", err)
	}
	doc := &ConfigExport{Version: ExportVersion, Settings: map[string]string{
		"retention_days":   "7",
		"seeded_providers": "0",
		"future_key":       "x",
	}}
	res, err := dst.ImportConfig(ctx, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Settings.Updated != 1 || len(res.Warnings) != 2 {
		t.Fatalf("settings counts/warnings: %+v", res)
	}
	if v, _ := dst.Settings.Get(ctx, "retention_days"); v != "7" {
		t.Fatalf("retention_days: %q", v)
	}
	if v, _ := dst.Settings.Get(ctx, "seeded_providers"); v != "1" {
		t.Fatalf("seed guard must be untouched, got %q", v)
	}
	seeded, err := dst.SeedDefaultProviders(ctx)
	if err != nil || seeded {
		t.Fatalf("guard must prevent reseeding: %v err=%v", seeded, err)
	}
}

// Dependency closure: exporting only model_routes pulls in the full subtree
// of every provider a target references, and nothing else.
func TestExportClosure(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if _, err := st.Providers.Create(ctx, "unused", nil); err != nil {
		t.Fatalf("provider a: %v", err)
	}
	_, _, _, _ = seedTransferFixture(t, st)

	doc, err := st.ExportConfig(ctx, []string{SectionRoutes}, false)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(doc.ModelRoutes) != 1 || doc.ModelRoutes[0].Name != "main" {
		t.Fatalf("routes: %+v", doc.ModelRoutes)
	}
	if len(doc.Providers) != 1 || doc.Providers[0].Name != "ds" {
		t.Fatalf("closure must include exactly the referenced provider: %+v", doc.Providers)
	}
	p := doc.Providers[0]
	if len(p.Accounts) != 1 || len(p.Channels) != 2 || len(p.Models) != 2 {
		t.Fatalf("closure must carry the full subtree: %+v", p)
	}
	if doc.APIKeys != nil || doc.Settings != nil {
		t.Fatalf("unselected sections must be absent: %+v", doc)
	}
}

// Secrets: excluding account keys leaves no plaintext anywhere in the
// marshalled document (probes still export); including them round-trips.
func TestExportNoSecretLeak(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedTransferFixture(t, st)

	doc, err := st.ExportConfig(ctx, ExportSections, false)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "sk-ds-secret-0001") {
		t.Fatal("exported document leaks the account key with includeAccountKeys=false")
	}
	if !strings.Contains(string(raw), `"type":"balance"`) {
		t.Fatal("usage probes must still be exported")
	}
	docOn, err := st.ExportConfig(ctx, ExportSections, true)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	rawOn, _ := json.Marshal(docOn)
	if !strings.Contains(string(rawOn), "sk-ds-secret-0001") {
		t.Fatal("includeAccountKeys=true must carry the plaintext key")
	}
}

// Failures: a bad document is rejected before any write, and an error mid-
// transaction leaves the store unchanged.
func TestImportValidationAndRollback(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := st.ImportConfig(ctx, &ConfigExport{Version: 2}); !errors.Is(err, ErrBadDocument) {
		t.Fatalf("unsupported version must be ErrBadDocument, got %v", err)
	}
	if _, err := st.ImportConfig(ctx, &ConfigExport{Version: 1}); !errors.Is(err, ErrBadDocument) {
		t.Fatalf("empty document must be ErrBadDocument, got %v", err)
	}
	dup := &ConfigExport{Version: ExportVersion, Providers: []ExportProvider{
		{Name: "x"}, {Name: "x"},
	}}
	if _, err := st.ImportConfig(ctx, dup); !errors.Is(err, ErrBadDocument) {
		t.Fatalf("duplicate provider names must be ErrBadDocument, got %v", err)
	}
	badHash := &ConfigExport{Version: ExportVersion, APIKeys: []ExportAPIKey{
		{Name: "k", KeyHash: "nothex"},
	}}
	if _, err := st.ImportConfig(ctx, badHash); !errors.Is(err, ErrBadDocument) {
		t.Fatalf("malformed key hash must be ErrBadDocument, got %v", err)
	}
	if ps, _ := st.Providers.List(ctx); len(ps) != 0 {
		t.Fatalf("failed imports must not write: %+v", ps)
	}

	// A cancelled context aborts the transaction; the store stays empty.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	src := newTestStore(t)
	_, _, _, _ = seedTransferFixture(t, src)
	doc, err := src.ExportConfig(ctx, ExportSections, true)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := st.ImportConfig(cancelled, doc); err == nil {
		t.Fatal("cancelled import must fail")
	}
	if ps, _ := st.Providers.List(ctx); len(ps) != 0 {
		t.Fatalf("rolled-back import must leave no rows: %+v", ps)
	}
}

// Dry-run: PlanImport reports exactly what ImportConfig will do (same counts,
// same warnings) while writing nothing.
func TestImportPlanDryRun(t *testing.T) {
	ctx := context.Background()
	dst := newTestStore(t)
	pid, _ := dst.Providers.Create(ctx, "p", nil)
	// Two accounts sharing one label: label matching is ambiguous and must
	// warn, matching the lowest id.
	dst.Accounts.Create(ctx, pid, "dup", "sk-key-lowest-001", 1, "")
	dst.Accounts.Create(ctx, pid, "dup", "sk-key-highest-02", 1, "")

	src := newTestStore(t)
	_, _, _, _ = seedTransferFixture(t, src)
	doc, err := src.ExportConfig(ctx, ExportSections, true)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	plan, err := dst.PlanImport(ctx, doc)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if ps, _ := dst.Providers.List(ctx); len(ps) != 1 || ps[0].Name != "p" {
		t.Fatalf("plan must not write: %+v", ps)
	}

	res, err := dst.ImportConfig(ctx, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if plan.Providers != res.Providers || plan.Accounts != res.Accounts ||
		plan.Channels != res.Channels || plan.Models != res.Models ||
		plan.ModelRoutes != res.ModelRoutes || plan.APIKeys != res.APIKeys ||
		plan.Settings != res.Settings {
		t.Fatalf("plan counts must match import: plan=%+v import=%+v", plan, res)
	}
	if strings.Join(plan.Warnings, "|") != strings.Join(res.Warnings, "|") {
		t.Fatalf("plan warnings must match import: %v vs %v", plan.Warnings, res.Warnings)
	}

	// Ambiguous label matching: the fixture's "main" account has no label
	// collision, so drive the ambiguity warning directly.
	dupDoc := &ConfigExport{Version: ExportVersion, Providers: []ExportProvider{{
		Name: "p",
		Accounts: []ExportAccount{{Label: "dup", Weight: 9, Enabled: true, UsageProbes: json.RawMessage("[]")}},
	}}}
	dupPlan, err := dst.PlanImport(ctx, dupDoc)
	if err != nil {
		t.Fatalf("dup plan: %v", err)
	}
	found := false
	for _, w := range dupPlan.Warnings {
		if strings.Contains(w, "dup") && strings.Contains(w, "multiple") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ambiguous label match must warn: %v", dupPlan.Warnings)
	}
}
