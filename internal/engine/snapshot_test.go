package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PWZER/llm-switch/internal/store"
)

// The gateway's per-surface protocol preference chains (mirrors of the
// production values passed to Snapshot.Resolve).
var (
	openaiSurface    = []string{"openai", "anthropic", "responses"}
	anthropicSurface = []string{"anthropic", "openai", "responses"}
	responsesSurface = []string{"responses", "openai", "anthropic"}
)

// newTestSnapshot seeds a store with provider-scoped model rows and a model
// route, then builds a snapshot over it. Provider "fake" has both an openai
// and an anthropic channel — one endpoint per protocol (the schema's
// UNIQUE(provider_id, protocol)); rows expand to candidates on every live
// channel of their provider, in channel-id order (openai first). Provider
// "fake2" is openai-only.
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
	// The openai channel is created first (lower id); the anthropic one at the
	// higher id is the primary surface for the claude-*/[1m] listing
	// decorations, which only apply to anthropic-served names.
	if _, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Protocol: "openai",
		BaseURL: "http://127.0.0.1:2", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true,
	}); err != nil {
		t.Fatalf("oai channel: %v", err)
	}
	cid, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Protocol: "anthropic",
		BaseURL: "http://127.0.0.1:1", ChatPath: "/v1/messages",
		AuthStyle: "x-api-key", ExtraHeaders: "{}", Enabled: true,
	})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	// An OpenAI-protocol-only provider: names served only here never get the
	// claude-*/[1m] decorations.
	pid2, err := st.Providers.Create(ctx, "fake2", nil)
	if err != nil {
		t.Fatalf("provider2: %v", err)
	}
	if _, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid2, Protocol: "openai",
		BaseURL: "http://127.0.0.1:3", ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true,
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
	// row name for row hits. Route hits carry the pinned channel only; row
	// hits carry every live channel of the provider, chain-ordered.
	if cands, name, ok := snap.Resolve("my-route", anthropicSurface); !ok || name != "my-route" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("route resolve broken: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("test-model", anthropicSurface); !ok || name != "test-model" ||
		len(cands) != 2 || cands[0].UpstreamModel != "fake-chat" || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("row resolve broken: %v %q %v", cands, name, ok)
	}

	// A disabled row is unroutable: listed ⇔ routable.
	if _, _, ok := snap.Resolve("off-model", anthropicSurface); ok {
		t.Fatal("disabled row must not resolve")
	}

	// claude-* requests route to the stripped row name (client caches that
	// learned claude-* naming keep working), while claude-* must never invent
	// a route for an unknown base name.
	if cands, name, ok := snap.Resolve("claude-test", anthropicSurface); !ok || name != "claude-test" ||
		len(cands) != 2 || cands[0].UpstreamModel != "fake-chat" || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("literal claude row must win: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("claude-test-model", anthropicSurface); !ok || name != "test-model" ||
		len(cands) != 2 || cands[0].UpstreamModel != "fake-chat" || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("stripped row must route: %v %q %v", cands, name, ok)
	}
	if _, _, ok := snap.Resolve("claude-nope", anthropicSurface); ok {
		t.Fatal("stripped unknown name must not resolve")
	}
	if _, _, ok := snap.Resolve("nope", anthropicSurface); ok {
		t.Fatal("unknown model must not resolve")
	}

	// The [1m] listing marker is stripped before matching: suffixed requests
	// resolve to the bare route/row identity (Claude Code strips the marker
	// client-side, others may send it verbatim).
	if cands, name, ok := snap.Resolve("my-route[1m]", anthropicSurface); !ok || name != "my-route" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("[1m]-suffixed route resolve broken: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("test-model[1m]", anthropicSurface); !ok || name != "test-model" ||
		len(cands) != 2 || cands[0].UpstreamModel != "fake-chat" || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("[1m]-suffixed row resolve broken: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("claude-test[1m]", anthropicSurface); !ok || name != "claude-test" ||
		len(cands) != 2 || cands[0].UpstreamModel != "fake-chat" || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("literal claude row must win after marker strip: %v %q %v", cands, name, ok)
	}

	// claude- stripping now checks routes before rows, so the listed
	// claude-<route> mirror resolves to the route chain.
	if cands, name, ok := snap.Resolve("claude-my-route", anthropicSurface); !ok || name != "my-route" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("claude-<route> resolve broken: %v %q %v", cands, name, ok)
	}
	if cands, name, ok := snap.Resolve("claude-my-route[1m]", anthropicSurface); !ok || name != "my-route" ||
		len(cands) != 1 || cands[0].UpstreamModel != "fake-chat" {
		t.Fatalf("claude-<route>[1m] resolve broken: %v %q %v", cands, name, ok)
	}

	// Marker noise stays unroutable.
	for _, absent := range []string{"[1m]", "nope[1m]", "claude-nope[1m]", "my-route[1m][1m]"} {
		if _, _, ok := snap.Resolve(absent, anthropicSurface); ok {
			t.Fatalf("degenerate name %q must not resolve", absent)
		}
	}
}

// TestResolveProtocolPreference: a provider's row expands to candidates on
// every live channel of the provider (channel-id order), and Resolve stably
// sorts them by the client surface's preference chain — the same-protocol
// tier leads even when it sits at a higher id, while cross-protocol
// candidates stay in the failover list after it. Provider-scoped ROUTE
// segments behave differently: they still collapse to their single best
// tier, and explicit channel pins are never touched.
func TestResolveProtocolPreference(t *testing.T) {
	snap := newTestSnapshot(t)

	// test-model's provider has openai (id 1) and anthropic (id 2): the
	// anthropic surface leads with its own tier despite the higher id, and
	// keeps the openai candidate as cross-protocol failover.
	cands, _, ok := snap.Resolve("test-model", anthropicSurface)
	if !ok || len(cands) != 2 ||
		cands[0].Channel.Protocol != "anthropic" || cands[1].Channel.Protocol != "openai" {
		t.Fatalf("anthropic surface must order anthropic before openai: %v %v", cands, ok)
	}
	// The openai surface matches id order; both candidates survive.
	cands, _, ok = snap.Resolve("test-model", openaiSurface)
	if !ok || len(cands) != 2 ||
		cands[0].Channel.Protocol != "openai" || cands[1].Channel.Protocol != "anthropic" {
		t.Fatalf("openai surface must order openai before anthropic: %v %v", cands, ok)
	}
	// No responses channel: the empty tier is skipped, the rest keep chain
	// order (openai before anthropic).
	cands, _, ok = snap.Resolve("test-model", responsesSurface)
	if !ok || len(cands) != 2 ||
		cands[0].Channel.Protocol != "openai" || cands[1].Channel.Protocol != "anthropic" {
		t.Fatalf("responses surface must follow the chain order: %v %v", cands, ok)
	}

	// oai-model's provider is openai-only: a single cross-protocol candidate.
	cands, _, ok = snap.Resolve("oai-model", anthropicSurface)
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "openai" {
		t.Fatalf("openai-only provider must fall back to cross-protocol: %v %v", cands, ok)
	}

	// Route targets pinning an explicit channel are explicit admin
	// configuration: no protocol filtering, no reordering.
	cands, _, ok = snap.Resolve("my-route", openaiSurface)
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("route chain must not be protocol-filtered: %v %v", cands, ok)
	}

	// A provider-scoped route target collapses to its best protocol tier —
	// unlike rows, it does not keep a cross-protocol tail.
	cands, _, ok = snap.Resolve("auto-route", anthropicSurface)
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "anthropic" {
		t.Fatalf("provider target must collapse to the anthropic tier: %v %v", cands, ok)
	}
	cands, _, ok = snap.Resolve("auto-route", openaiSurface)
	if !ok || len(cands) != 1 || cands[0].Channel.Protocol != "openai" {
		t.Fatalf("provider target must collapse to the openai tier: %v %v", cands, ok)
	}
}

func idp(v int64) *int64 { return &v }

func boolp(b bool) *bool { return &b }

// TestResolveProviderTarget: a provider-scoped route target expands to one
// candidate per live channel of its provider in channel-id order, with the
// surface's protocol preference applied per segment only — the first
// preference tier with a match wins outright, so a same-protocol candidate at
// a higher id beats a lower-id cross-protocol one. A provider carries at most
// one channel per protocol, so one segment holds at most three candidates.
// An explicit channel pin in the same chain is never filtered. A dead pinned
// account skips the whole segment (fall-through), and disabled channels or
// providers never expand.
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
	mkChannel := func(protocol string, enabled bool) int64 {
		t.Helper()
		cid, err := st.Channels.Create(ctx, &store.Channel{
			ProviderID: pid, Protocol: protocol,
			BaseURL: "http://127.0.0.1:1", ChatPath: "/x",
			ExtraHeaders: "{}", Enabled: enabled,
		})
		if err != nil {
			t.Fatalf("channel %s: %v", protocol, err)
		}
		return cid
	}
	// Creation order fixes the ids: oai < anthro < resps — in-segment order
	// is channel id ASC, so this listing is the full unfiltered segment.
	oai := mkChannel("openai", true)
	anthro := mkChannel("anthropic", true)
	resps := mkChannel("responses", true)

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
			out = append(out, fmt.Sprintf("%d(%s)", c.Channel.ID, c.UpstreamModel))
		}
		return strings.Join(out, ",")
	}
	setEnabled := func(cid int64, enabled bool) {
		t.Helper()
		ch, err := st.Channels.Get(ctx, cid)
		if err != nil {
			t.Fatalf("get channel: %v", err)
		}
		ch.Enabled = enabled
		if err := st.Channels.Update(ctx, &ch); err != nil {
			t.Fatalf("set channel %d enabled=%v: %v", cid, enabled, err)
		}
	}
	fullSeg := fmt.Sprintf("%d(up),%d(up),%d(up)", oai, anthro, resps)

	upsert("auto", []store.ModelRouteTarget{{ProviderID: pid, UpstreamModel: "up"}})

	// No preference chain: the whole segment in channel-id order, candidates
	// carrying the target's upstream model.
	if cands, _, ok := build().Resolve("auto", nil); !ok || summary(cands) != fullSeg {
		t.Fatalf("unfiltered segment wrong: %q", summary(cands))
	}

	// Protocol preference beats id ordering: the anthropic channel sits at a
	// higher id than openai, yet each surface's first matching tier wins
	// outright and no cross-protocol candidate survives beside it.
	for _, tc := range []struct {
		prefer []string
		want   int64
	}{
		{anthropicSurface, anthro},
		{openaiSurface, oai},
		{responsesSurface, resps},
	} {
		cands, _, ok := build().Resolve("auto", tc.prefer)
		if !ok || len(cands) != 1 || cands[0].Channel.ID != tc.want {
			t.Fatalf("prefer %v: got %q, want only channel %d", tc.prefer, summary(cands), tc.want)
		}
	}

	// Disabled channels never expand: with responses gone the segment is the
	// two remaining ids in order.
	setEnabled(resps, false)
	wantTwo := fmt.Sprintf("%d(up),%d(up)", oai, anthro)
	if cands, _, ok := build().Resolve("auto", nil); !ok || summary(cands) != wantTwo {
		t.Fatalf("segment with a disabled channel wrong: %q", summary(cands))
	}
	setEnabled(resps, true)

	// No candidate matches any preference tier: the whole live segment falls
	// back to cross-protocol bridging, in id order.
	setEnabled(anthro, false)
	wantFallback := fmt.Sprintf("%d(up),%d(up)", oai, resps)
	if cands, _, ok := build().Resolve("auto", []string{"anthropic"}); !ok || summary(cands) != wantFallback {
		t.Fatalf("cross-protocol fallback wrong: %q", summary(cands))
	}
	setEnabled(anthro, true)

	// Mixed chain: the provider segment comes first (protocol-preferred), the
	// explicit pin follows unfiltered — an openai pin is listed for the
	// anthropic surface even though same-protocol channels exist.
	upsert("mixed", []store.ModelRouteTarget{
		{ProviderID: pid, UpstreamModel: "up"},
		{ProviderID: pid, ChannelID: idp(oai), UpstreamModel: "pin"},
	})
	cands, _, ok := build().Resolve("mixed", anthropicSurface)
	if !ok || summary(cands) != fmt.Sprintf("%d(up),%d(pin)", anthro, oai) {
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
		{ProviderID: pid, ChannelID: idp(oai), UpstreamModel: "pin"},
	})
	cands, _, ok = build().Resolve("pinacc", anthropicSurface)
	if !ok || summary(cands) != fmt.Sprintf("%d(up),%d(pin)", anthro, oai) ||
		cands[0].Account == nil || cands[0].Account.ID != aid {
		t.Fatalf("live pinned account must serve the segment: %q %v", summary(cands), ok)
	}
	if err := st.Accounts.Update(ctx, aid, nil, nil, boolp(false)); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	cands, _, ok = build().Resolve("pinacc", anthropicSurface)
	if !ok || len(cands) != 1 || cands[0].Channel.ID != oai {
		t.Fatalf("dead pinned account must skip the segment: %q %v", summary(cands), ok)
	}

	// A provider with no live channels expands to nothing.
	if err := st.Providers.SetEnabled(ctx, pid, false); err != nil {
		t.Fatalf("disable provider: %v", err)
	}
	if _, _, ok := build().Resolve("auto", anthropicSurface); ok {
		t.Fatal("disabled provider must not expand")
	}
	if err := st.Providers.SetEnabled(ctx, pid, true); err != nil {
		t.Fatalf("enable provider: %v", err)
	}

	// Legacy stored target (ProviderID 0 + channel pin) still resolves via
	// the pin, unfiltered.
	upsert("legacy", []store.ModelRouteTarget{
		{ChannelID: idp(oai), UpstreamModel: "pin"},
	})
	cands, _, ok = build().Resolve("legacy", anthropicSurface)
	if !ok || len(cands) != 1 || cands[0].Channel.ID != oai || cands[0].Channel.Protocol != "openai" {
		t.Fatalf("legacy channel pin must resolve unfiltered: %q %v", summary(cands), ok)
	}
}

// SettingBool parses both the seeded "1"/"0" form and the admin-written
// "true"/"false" form, falling back on garbage and nil snapshots.
func TestSettingBool(t *testing.T) {
	snap := &Snapshot{Settings: map[string]string{
		"on_seed":  "1",
		"off_seed": "0",
		"on_ui":    "true",
		"off_ui":   "false",
		"garbage":  "maybe",
	}}
	for _, tc := range []struct {
		key      string
		fallback bool
		want     bool
	}{
		{"on_seed", false, true},
		{"off_seed", true, false},
		{"on_ui", false, true},
		{"off_ui", true, false},
		{"garbage", true, true},
		{"missing", true, true},
		{"missing", false, false},
	} {
		if got := snap.SettingBool(tc.key, tc.fallback); got != tc.want {
			t.Errorf("SettingBool(%q, %v) = %v, want %v", tc.key, tc.fallback, got, tc.want)
		}
	}
	var nilSnap *Snapshot
	if nilSnap.SettingBool("on_seed", true) != true {
		t.Error("nil snapshot must return the fallback")
	}
}
