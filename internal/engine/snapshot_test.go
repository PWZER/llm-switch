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
		Name: "my-route",
		// Channel-pinned target (explicit admin configuration).
		Targets: []store.ModelRouteTarget{{ProviderID: pid, ChannelID: &cid, UpstreamModel: "fake-chat"}},
	}); err != nil {
		t.Fatalf("model route: %v", err)
	}
	// Provider-scoped target: channel omitted — the gateway auto-selects
	// among the provider's live endpoints (same expansion as rows).
	if err := st.Routes.Upsert(ctx, &store.ModelRoute{
		Name:    "auto-route",
		Targets: []store.ModelRouteTarget{{ProviderID: pid, UpstreamModel: "fake-chat"}},
	}); err != nil {
		t.Fatalf("auto route: %v", err)
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

	// Listed: every enabled row's name plus both model routes.
	for _, id := range []string{"reg-model", "test-model", "claude-test", "big-model", "oai-model", "my-route", "auto-route"} {
		if findEntry(snap, id) == nil {
			t.Fatalf("missing %q in snapshot list", id)
		}
	}

	// Disabled row: hidden from clients.
	if e := findEntry(snap, "off-model"); e != nil {
		t.Fatalf("disabled row must not be listed: %+v", e)
	}

	// Description renders as "{provider}/{account}" — the account that would
	// serve first; the channel name is intentionally omitted (any live
	// channel of the provider can serve the name, so naming one reads as a
	// protocol mark). A provider without enabled accounts is bare.
	for _, id := range []string{"reg-model", "test-model", "my-route", "auto-route"} {
		if e := findEntry(snap, id); e != nil && e.Provider != "fake" {
			t.Fatalf("%s provider = %q, want %q", id, e.Provider, "fake")
		}
	}
	if e := findEntry(snap, "oai-model"); e != nil && e.Provider != "fake2" {
		t.Fatalf("oai-model provider = %q, want %q", e.Provider, "fake2")
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
	for _, want := range []string{"claude-reg-model", "claude-test-model", "claude-my-route", "claude-auto-route"} {
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
		if v.Provider != "fake" || v.ContextWindow == nil || *v.ContextWindow != 32000 {
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
	if marked.ContextWindow == nil || *marked.ContextWindow != 1048576 || marked.Provider != "fake" {
		t.Fatalf("marked entry metadata not copied: %+v", marked)
	}
	if marked.DisplayName != "big-model[1m]" {
		t.Fatalf("marked display name not decorated: %q", marked.DisplayName)
	}
	if findEntry(snap, "big-model") == nil {
		t.Fatal("plain big-model must stay listed")
	}

	// Below threshold, no limits, or openai-served: never marked.
	for _, absent := range []string{"reg-model[1m]", "test-model[1m]", "my-route[1m]", "auto-route[1m]", "oai-model[1m]", "big-model[1m][1m]"} {
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

	// Route targets pinning an explicit channel are explicit admin
	// configuration: no protocol filtering.
	cands, _, ok = snap.Resolve("my-route", "openai")
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("route chain must not be protocol-filtered: %v %v", cands, ok)
	}

	// A provider-scoped route target behaves like a row: same-protocol first.
	cands, _, ok = snap.Resolve("auto-route", "anthropic")
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("provider target must prefer same protocol: %v %v", cands, ok)
	}
	cands, _, ok = snap.Resolve("auto-route", "openai")
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "openai" {
		t.Fatalf("provider target must prefer same protocol (openai): %v %v", cands, ok)
	}
}

func idp(v int64) *int64 { return &v }

func boolp(b bool) *bool { return &b }

// TestResolveProviderTarget: a provider-scoped route target expands to one
// candidate per live channel of its provider (priority DESC, weight DESC),
// with same-protocol-first preference applied per segment only — an explicit
// channel pin in the same chain is never filtered. A dead pinned account
// skips the whole segment (fall-through), and disabled channels or providers
// never expand.
func TestResolveProviderTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(dir, filepath.Join(dir, "route.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.New(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	pid, err := st.Providers.Create(ctx, "p", nil)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	mkChannel := func(name, protocol string, priority, weight int, enabled bool) int64 {
		t.Helper()
		cid, err := st.Channels.Create(ctx, &store.Channel{
			ProviderID: pid, Name: name, Protocol: protocol,
			BaseURL: "http://127.0.0.1:1", ChatPath: "/x",
			AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: enabled, Priority: priority, Weight: weight,
		})
		if err != nil {
			t.Fatalf("channel %s: %v", name, err)
		}
		return cid
	}
	anthroLo := mkChannel("a-lo", "anthropic", 5, 1, true)  // lower priority
	oaiHi := mkChannel("o-hi", "openai", 10, 5, true)       // highest priority overall
	anthroHi := mkChannel("a-hi", "anthropic", 10, 3, true) // top anthropic by weight
	mkChannel("o-hi2", "openai", 10, 7, true)               // top openai by weight

	upsert := func(name string, targets []store.ModelRouteTarget) {
		t.Helper()
		if err := st.Routes.Upsert(ctx, &store.ModelRoute{Name: name, Targets: targets}); err != nil {
			t.Fatalf("route %s: %v", name, err)
		}
	}
	build := func() *Snapshot {
		t.Helper()
		snap, err := buildSnapshot(ctx, st)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		return snap
	}
	summary := func(cands []Candidate) string {
		out := make([]string, 0, len(cands))
		for _, c := range cands {
			out = append(out, c.Channel.Name+"("+c.UpstreamModel+")")
		}
		return strings.Join(out, ",")
	}

	upsert("auto", []store.ModelRouteTarget{{ProviderID: pid, UpstreamModel: "up"}})

	// In-segment order: priority DESC then weight DESC; protocol preference
	// beats priority (a-lo's protocol wins over o-hi's priority for the
	// anthropic surface). Candidates carry the target's upstream model.
	snap := build()
	cands, _, ok := snap.Resolve("auto", "anthropic")
	if !ok || summary(cands) != "a-hi(up),a-lo(up)" {
		t.Fatalf("anthropic segment wrong: %q %v", summary(cands), ok)
	}
	cands, _, ok = snap.Resolve("auto", "openai")
	if !ok || summary(cands) != "o-hi2(up),o-hi(up)" {
		t.Fatalf("openai segment wrong: %q %v", summary(cands), ok)
	}

	// No same-protocol channel left: the whole segment falls back to
	// cross-protocol, in priority order.
	mustDisable := func(cid int64) {
		t.Helper()
		ch, err := st.Channels.Get(ctx, cid)
		if err != nil {
			t.Fatalf("get channel: %v", err)
		}
		ch.Enabled = false
		if err := st.Channels.Update(ctx, &ch); err != nil {
			t.Fatalf("disable channel: %v", err)
		}
	}
	mustDisable(anthroLo)
	mustDisable(anthroHi)
	cands, _, ok = build().Resolve("auto", "anthropic")
	if !ok || summary(cands) != "o-hi2(up),o-hi(up)" {
		t.Fatalf("cross-protocol fallback wrong: %q %v", summary(cands), ok)
	}
	mustEnable := func(cid int64) {
		t.Helper()
		ch, err := st.Channels.Get(ctx, cid)
		if err != nil {
			t.Fatalf("get channel: %v", err)
		}
		ch.Enabled = true
		if err := st.Channels.Update(ctx, &ch); err != nil {
			t.Fatalf("enable channel: %v", err)
		}
	}
	mustEnable(anthroLo)
	mustEnable(anthroHi)

	// Mixed chain: the provider segment comes first (protocol-preferred), the
	// explicit pin follows unfiltered — an openai pin is listed for the
	// anthropic surface even though same-protocol channels exist.
	upsert("mixed", []store.ModelRouteTarget{
		{ProviderID: pid, UpstreamModel: "up"},
		{ProviderID: pid, ChannelID: idp(oaiHi), UpstreamModel: "pin"},
	})
	cands, _, ok = build().Resolve("mixed", "anthropic")
	if !ok || summary(cands) != "a-hi(up),a-lo(up),o-hi(pin)" {
		t.Fatalf("mixed chain wrong: %q %v", summary(cands), ok)
	}

	// A dead pinned account skips the whole segment; the chain falls through
	// to the next target.
	aid, err := st.Accounts.Create(ctx, pid, "a1", "k", 1, "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	upsert("pinacc", []store.ModelRouteTarget{
		{ProviderID: pid, UpstreamModel: "up", AccountID: &aid},
		{ProviderID: pid, ChannelID: idp(oaiHi), UpstreamModel: "pin"},
	})
	cands, _, ok = build().Resolve("pinacc", "anthropic")
	if !ok || len(cands) != 3 {
		t.Fatalf("live pinned account must serve the segment: %q %v", summary(cands), ok)
	}
	if err := st.Accounts.Update(ctx, aid, nil, nil, boolp(false)); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	cands, _, ok = build().Resolve("pinacc", "anthropic")
	if !ok || len(cands) != 1 || cands[0].Channel.Name != "o-hi" {
		t.Fatalf("dead pinned account must skip the segment: %q %v", summary(cands), ok)
	}

	// A provider with no live channels expands to nothing.
	if err := st.Providers.SetEnabled(ctx, pid, false); err != nil {
		t.Fatalf("disable provider: %v", err)
	}
	if _, _, ok := build().Resolve("auto", "anthropic"); ok {
		t.Fatal("disabled provider must not expand")
	}
	if err := st.Providers.SetEnabled(ctx, pid, true); err != nil {
		t.Fatalf("enable provider: %v", err)
	}

	// Legacy stored target (ProviderID 0 + channel pin) still resolves via
	// the pin, unfiltered.
	upsert("legacy", []store.ModelRouteTarget{
		{ChannelID: idp(oaiHi), UpstreamModel: "pin"},
	})
	cands, _, ok = build().Resolve("legacy", "anthropic")
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "openai" {
		t.Fatalf("legacy channel pin must resolve unfiltered: %q %v", summary(cands), ok)
	}
}
