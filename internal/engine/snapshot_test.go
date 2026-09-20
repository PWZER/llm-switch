package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PWZER/llm-switch/internal/store"
)

// newTestSnapshot seeds a store with provider-scoped model rows and a model
// route, then builds a snapshot over it. Provider "fake" has both an
// anthropic and an openai channel (rows expand to candidates on every live
// channel of their provider); provider "fake2" is openai-only.
func newTestSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir, filepath.Join(dir, "engine.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.New(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	ctx := context.Background()
	pid, err := st.Providers.Create(ctx, "fake", nil)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	// The primary channel is anthropic-protocol: the claude-*/[1m] listing
	// decorations only apply to anthropic-served names.
	cid, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Name: "fake-anthropic", Protocol: "anthropic",
		BaseURL: "http://127.0.0.1:1", ChatPath: "/v1/messages",
		AuthStyle: "x-api-key", ExtraHeaders: "{}", Enabled: true, Priority: 10, Weight: 1,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if _, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Name: "fake-oai", Protocol: "openai",
		BaseURL: "http://127.0.0.1:2", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 10, Weight: 1,
	}); err != nil {
		t.Fatalf("oai channel: %v", err)
	}
	// An OpenAI-protocol-only provider: names served only here never get the
	// claude-*/[1m] decorations.
	pid2, err := st.Providers.Create(ctx, "fake2", nil)
	if err != nil {
		t.Fatalf("provider2: %v", err)
	}
	if _, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid2, Name: "fake2-oai", Protocol: "openai",
		BaseURL: "http://127.0.0.1:3", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 10, Weight: 1,
	}); err != nil {
		t.Fatalf("fake2 channel: %v", err)
	}

	seed := func(providerID int64, id string, enabled bool, cw int64) {
		m := &store.Model{
			ID: id, ProviderID: providerID, UpstreamModel: "fake-chat",
			DisplayName: id, Enabled: enabled,
		}
		if cw > 0 {
			m.ContextWindow = &cw
		}
		if err := st.Models.Create(ctx, m); err != nil {
			t.Fatalf("model row %s: %v", id, err)
		}
	}
	seed(pid, "test-model", true, 0)
	seed(pid, "claude-test", true, 0)
	// Disabled row: neither listed nor routable.
	seed(pid, "off-model", false, 0)
	seed(pid, "reg-model", true, 32000)
	seed(pid, "big-model", true, 1048576)  // crosses the 1M marker threshold
	seed(pid2, "oai-model", true, 2097152) // ≥ 1M but openai-served: never marked

	if err := st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "my-route",
		Targets: []store.ModelRouteTarget{{ChannelID: cid, UpstreamModel: "fake-chat"}},
	}); err != nil {
		t.Fatalf("model route: %v", err)
	}

	snap, err := buildSnapshot(ctx, st)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return snap
}

func findEntry(snap *Snapshot, id string) *ModelEntry {
	for i := range snap.Models {
		if snap.Models[i].ID == id {
			return &snap.Models[i]
		}
	}
	return nil
}

func TestSnapshotListFilteringAndProviders(t *testing.T) {
	snap := newTestSnapshot(t)

	// Listed: every enabled row's name plus the model route.
	for _, id := range []string{"reg-model", "test-model", "claude-test", "big-model", "oai-model", "my-route"} {
		if findEntry(snap, id) == nil {
			t.Fatalf("missing %q in snapshot list", id)
		}
	}

	// Disabled row: hidden from clients.
	if e := findEntry(snap, "off-model"); e != nil {
		t.Fatalf("disabled row must not be listed: %+v", e)
	}

	// Description renders as "{provider}({endpoint})" — the serving channel
	// that would handle the name first.
	for _, id := range []string{"reg-model", "test-model", "my-route"} {
		if e := findEntry(snap, id); e != nil && e.Provider != "fake(fake-anthropic)" {
			t.Fatalf("%s provider = %q, want %q", id, e.Provider, "fake(fake-anthropic)")
		}
	}
	if e := findEntry(snap, "oai-model"); e != nil && e.Provider != "fake2(fake2-oai)" {
		t.Fatalf("oai-model provider = %q, want %q", e.Provider, "fake2(fake2-oai)")
	}

	// Row limits survive the merge.
	if e := findEntry(snap, "reg-model"); e != nil {
		if e.ContextWindow == nil || *e.ContextWindow != 32000 {
			t.Fatalf("reg-model limits lost: %+v", e)
		}
	}
}

func TestSnapshotDiscoveryVariants(t *testing.T) {
	snap := newTestSnapshot(t)

	variantIDs := map[string]bool{}
	for _, v := range snap.DiscoveryVariants {
		if !strings.HasPrefix(v.ID, "claude-") {
			t.Fatalf("variant id lacks prefix: %+v", v)
		}
		variantIDs[v.ID] = true
	}

	// One variant per anthropic-served listed name lacking claude/anthropic;
	// none for ids that already match the filter or are disabled.
	for _, want := range []string{"claude-reg-model", "claude-test-model", "claude-my-route"} {
		if !variantIDs[want] {
			t.Fatalf("missing discovery variant %q in %v", want, variantIDs)
		}
	}
	for _, absent := range []string{"claude-claude-test", "claude-anthropic-real", "claude-off-model"} {
		if variantIDs[absent] {
			t.Fatalf("unexpected variant %q", absent)
		}
	}

	// Provider and limits are copied from the source entry.
	for _, v := range snap.DiscoveryVariants {
		if v.ID != "claude-reg-model" {
			continue
		}
		if v.Provider != "fake(fake-anthropic)" || v.ContextWindow == nil || *v.ContextWindow != 32000 {
			t.Fatalf("variant metadata not copied: %+v", v)
		}
		return
	}
	t.Fatal("claude-reg-model variant not found")
}

// TestSnapshotContext1mMarkers: entries with a merged context window >=
// 1,000,000 that are anthropic-served also list a decorated "<id>[1m]" id in
// ContextVariants (limits copied, plain id kept).
func TestSnapshotContext1mMarkers(t *testing.T) {
	snap := newTestSnapshot(t)

	findVariant := func(id string) *ModelEntry {
		for i := range snap.ContextVariants {
			if snap.ContextVariants[i].ID == id {
				return &snap.ContextVariants[i]
			}
		}
		return nil
	}

	// Marked entry: decorated id listed next to the plain one, limits copied,
	// display name decorated to stay distinguishable from the plain entry.
	marked := findVariant("big-model[1m]")
	if marked == nil {
		t.Fatal("missing big-model[1m] in snapshot ContextVariants")
	}
	if marked.ContextWindow == nil || *marked.ContextWindow != 1048576 || marked.Provider != "fake(fake-anthropic)" {
		t.Fatalf("marked entry metadata not copied: %+v", marked)
	}
	if marked.DisplayName != "big-model[1m]" {
		t.Fatalf("marked display name not decorated: %q", marked.DisplayName)
	}
	if findEntry(snap, "big-model") == nil {
		t.Fatal("plain big-model must stay listed")
	}

	// Below threshold, no limits, or openai-served: never marked.
	for _, absent := range []string{"reg-model[1m]", "test-model[1m]", "my-route[1m]", "oai-model[1m]", "big-model[1m][1m]"} {
		if findVariant(absent) != nil || findEntry(snap, absent) != nil {
			t.Fatalf("unexpected marked entry %q", absent)
		}
	}

	// The discovery pass mirrors marked ids too.
	want := "claude-big-model[1m]"
	found := false
	for _, v := range snap.DiscoveryVariants {
		if v.ID == want {
			found = true
		}
		if v.ID == "claude-oai-model" {
			t.Fatalf("openai-served name must not be mirrored: %+v", v)
		}
	}
	if !found {
		t.Fatalf("missing discovery variant %q", want)
	}
}

func TestResolveBaseline(t *testing.T) {
	snap := newTestSnapshot(t)

	// Every found branch returns the canonical client-facing name: the
	// route's own name for route hits, the bare (marker/prefix-stripped)
	// row name for row hits.
	if cands, name, ok := snap.Resolve("my-route", "anthropic"); !ok || name != "my-route" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("route resolve broken: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("test-model", "anthropic"); !ok || name != "test-model" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("row resolve broken: %v %q %v", cands, name, ok)
	}

	// A disabled row is unroutable: listed ⇔ routable.
	if _, _, ok := snap.Resolve("off-model", "anthropic"); ok {
		t.Fatal("disabled row must not resolve")
	}

	// claude-* requests route to the stripped row name (client caches that
	// learned claude-* naming keep working), while claude-* must never invent
	// a route for an unknown base name.
	if cands, name, ok := snap.Resolve("claude-test", "anthropic"); !ok || name != "claude-test" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("literal claude row must win: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("claude-test-model", "anthropic"); !ok || name != "test-model" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("stripped row must route: %v %q %v", cands, name, ok)
	}
	if _, _, ok := snap.Resolve("claude-nope", "anthropic"); ok {
		t.Fatal("stripped unknown name must not resolve")
	}
	if _, _, ok := snap.Resolve("nope", "anthropic"); ok {
		t.Fatal("unknown model must not resolve")
	}

	// The [1m] listing marker is stripped before matching: suffixed requests
	// resolve to the bare route/row identity (Claude Code strips the marker
	// client-side, others may send it verbatim).
	if cands, name, ok := snap.Resolve("my-route[1m]", "anthropic"); !ok || name != "my-route" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("[1m]-suffixed route resolve broken: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("test-model[1m]", "anthropic"); !ok || name != "test-model" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("[1m]-suffixed row resolve broken: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("claude-test[1m]", "anthropic"); !ok || name != "claude-test" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("literal claude row must win after marker strip: %v %q %v", cands, name, ok)
	}

	// claude- stripping now checks routes before rows, so the listed
	// claude-<route> mirror resolves to the route chain.
	if cands, name, ok := snap.Resolve("claude-my-route", "anthropic"); !ok || name != "my-route" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("claude-<route> resolve broken: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("claude-my-route[1m]", "anthropic"); !ok || name != "my-route" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("claude-<route>[1m] resolve broken: %v %q %v", cands, name, ok)
	}

	// Marker noise stays unroutable.
	for _, absent := range []string{"[1m]", "nope[1m]", "claude-nope[1m]", "my-route[1m][1m]"} {
		if _, _, ok := snap.Resolve(absent, "anthropic"); ok {
			t.Fatalf("degenerate name %q must not resolve", absent)
		}
	}
}

// TestResolveProtocolPreference: a provider's row expands to candidates on
// every live channel of the provider, but the client surface's protocol wins
// outright — cross-protocol candidates serve only when no same-protocol
// candidate exists. Route chains are explicit and never filtered.
func TestResolveProtocolPreference(t *testing.T) {
	snap := newTestSnapshot(t)

	// test-model's provider has both protocols: each surface gets exactly its
	// own kind.
	cands, _, ok := snap.Resolve("test-model", "anthropic")
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("anthropic surface must get the anthropic channel only: %v %v", cands, ok)
	}
	cands, _, ok = snap.Resolve("test-model", "openai")
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "openai" {
		t.Fatalf("openai surface must get the openai channel only: %v %v", cands, ok)
	}

	// oai-model's provider is openai-only: the anthropic surface falls back
	// to cross-protocol bridging.
	cands, _, ok = snap.Resolve("oai-model", "anthropic")
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "openai" {
		t.Fatalf("openai-only provider must fall back to cross-protocol: %v %v", cands, ok)
	}

	// Route targets are explicit admin configuration: no protocol filtering.
	cands, _, ok = snap.Resolve("my-route", "openai")
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("route chain must not be protocol-filtered: %v %v", cands, ok)
	}
}
