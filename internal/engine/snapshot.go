// Package engine holds the in-memory routing snapshot: an immutable view of
// all routing-relevant configuration, rebuilt after every admin mutation and
// swapped atomically. Requests load it once and pin their resolution, so
// config changes never disturb in-flight traffic.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/PWZER/llm-switch/internal/store"
)

// Account is one enabled upstream credential set of a provider.
type Account struct {
	ID     int64
	Label  string
	Secret string
	Weight int
}

// Provider is a vendor account with its enabled account pool.
type Provider struct {
	ID       int64
	Name     string
	Accounts []Account
	Enabled  bool
}

// Channel is a routable protocol-specific endpoint.
type Channel struct {
	ID                  int64
	ProviderID          int64
	Provider            *Provider
	Name                string
	Protocol            string // "openai" | "anthropic"
	BaseURL             string
	ChatPath            string
	AuthStyle           string  // "bearer" | "x-api-key"
	ResponsesPath       *string // upstream Responses API endpoint; nil/"" = bridge via IR
	ExtraHeaders        map[string]string
	Priority            int
	Weight              int
	SupportsEmbeddings  bool
	Passthrough         bool
	ForceUpstreamStream bool
}

// Candidate pairs a channel with the upstream model to request. Account is
// non-nil when a model route target pins a specific account; nil means the
// channel provider's account pool rotates as usual.
type Candidate struct {
	Channel       *Channel
	UpstreamModel string
	Account       *Account
}

const (
	// Context1mSuffix is Claude Code's 1M-context marker: listed ids carry it,
	// but clients strip it before requesting, so it is never part of a
	// routing identity — Resolve normalizes it away.
	Context1mSuffix = "[1m]"
	// context1mThreshold is the registry context window from which a listed
	// model also gets a "<id>[1m]" listing entry.
	context1mThreshold int64 = 1_000_000
)

// RouteTarget is one entry of a model route's ordered failover chain.
// ProviderID owns the target; ChannelID pins one of its endpoints (0 =
// auto-select among the provider's live channels — the JSON-level nil
// flattens to 0 here). AccountID optionally pins the target to one account
// of the target's provider; 0 means the provider's account pool rotates as
// usual.
type RouteTarget struct {
	ProviderID    int64
	ChannelID     int64
	UpstreamModel string
	AccountID     int64
}

// Route is a hot-switchable client-facing model name resolving to an ordered
// target chain: the first healthy target serves, the rest are failover.
type Route struct {
	Name    string
	Targets []RouteTarget
}

// ClientKey is an authenticated gateway key (hash lookup form).
type ClientKey struct {
	ID   int64
	Name string
}

// ModelEntry is one entry of the merged GET /v1/models registry.
type ModelEntry struct {
	ID          string
	DisplayName string
	Source      string
	IsRoute     bool
	// Provider is the human-facing vendor account name emitted as the model
	// description (empty = omitted).
	Provider string
	// Limits from the models registry; nil for binding/route-only names.
	ContextWindow   *int64
	MaxOutputTokens *int64
}

// Snapshot is an immutable view of routing configuration.
type Snapshot struct {
	Version   int64
	Providers map[int64]*Provider
	Channels  map[int64]*Channel
	// channelsByProvider groups the live channels (enabled channel of an
	// enabled provider) per provider, each slice sorted priority DESC then
	// weight DESC — the same expansion order as ByModel candidates.
	channelsByProvider map[int64][]*Channel
	ByModel            map[string][]Candidate // failover-ordered
	Routes             map[string]Route
	ClientKeys         map[string]ClientKey // sha256(key) -> record
	Models             []ModelEntry         // merged registry, sorted by id
	// DiscoveryVariants holds "claude-<id>" mirrors of Models for ids lacking
	// a claude/anthropic substring. Claude Code's gateway model discovery only
	// accepts such ids, so the Anthropic-shaped /v1/models lists them; the
	// OpenAI shape never does. Resolve strips the prefix when routing.
	DiscoveryVariants []ModelEntry
	// ContextVariants holds "<id>[1m]" mirrors of anthropic-served Models
	// whose registry context window reaches context1mThreshold — Claude
	// Code's 1M-context marker. Anthropic-shaped listing only; Resolve
	// normalizes the suffix away when routing.
	ContextVariants []ModelEntry
	Settings        map[string]string // editable settings (allowlisted)
}

// SettingInt returns a settings value parsed as int with a fallback.
func (s *Snapshot) SettingInt(key string, fallback int) int {
	if s == nil {
		return fallback
	}
	if v, ok := s.Settings[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// HashKey is the storage/lookup form of a client key.
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// anthropicServed reports whether a model name is served by at least one
// live anthropic-protocol channel (via models rows or route targets). The
// claude-/[1m] listing decorations exist for Claude Code, so they apply only
// to such names; models served exclusively over the OpenAI protocol (Codex
// and friends) always list plain.
func (s *Snapshot) anthropicServed(name string) bool {
	for _, c := range s.ByModel[name] {
		if c.Channel != nil && c.Channel.Protocol == "anthropic" {
			return true
		}
	}
	if rt, ok := s.Routes[name]; ok {
		for _, tgt := range rt.Targets {
			for _, ch := range s.targetLiveChannels(tgt) {
				if ch.Protocol == "anthropic" {
					return true
				}
			}
		}
	}
	return false
}

// targetLiveChannels returns the channels one route target expands to: the
// pinned channel (nil when disabled or deleted), or every live channel of
// the target's provider in build order (priority DESC, weight DESC; empty
// when it has none). Account pins are the caller's concern.
func (s *Snapshot) targetLiveChannels(tgt RouteTarget) []*Channel {
	if tgt.ChannelID != 0 {
		ch := s.Channels[tgt.ChannelID]
		if ch == nil {
			return nil
		}
		return []*Channel{ch}
	}
	return s.channelsByProvider[tgt.ProviderID]
}

// routeChain materializes a route's target chain against the live channel
// set; empty when no target is currently routable. Channel-pinned targets
// are explicit admin configuration and are never protocol-filtered; a
// provider-scoped target expands to the provider's live channels with the
// same same-protocol preference as row-derived candidates. A target pinning
// an account is skipped when the account is not in the provider's enabled
// pool.
func (s *Snapshot) routeChain(rt Route, preferProtocol string) []Candidate {
	var cands []Candidate
	for _, tgt := range rt.Targets {
		if tgt.ChannelID != 0 {
			ch := s.Channels[tgt.ChannelID]
			if ch == nil {
				continue
			}
			cand := Candidate{Channel: ch, UpstreamModel: tgt.UpstreamModel}
			if tgt.AccountID != 0 {
				acc := ch.Provider.account(tgt.AccountID)
				if acc == nil {
					continue // pinned account disabled or gone: target unroutable
				}
				cand.Account = acc
			}
			cands = append(cands, cand)
			continue
		}
		// Provider-scoped target: auto-select among the provider's live
		// endpoints. A dead pinned account skips the whole segment — never
		// silently rotate onto accounts the admin excluded.
		var pinned *Account
		if tgt.AccountID != 0 {
			pinned = s.Providers[tgt.ProviderID].account(tgt.AccountID)
			if pinned == nil {
				continue
			}
		}
		live := s.targetLiveChannels(tgt)
		if len(live) == 0 {
			continue
		}
		seg := make([]Candidate, 0, len(live))
		for _, ch := range live {
			seg = append(seg, Candidate{Channel: ch, UpstreamModel: tgt.UpstreamModel, Account: pinned})
		}
		cands = append(cands, preferSameProtocol(seg, preferProtocol)...)
	}
	return cands
}

// DisplayName renders the account for human-facing descriptions: its label,
// or "#<id>" when unlabeled.
func (a *Account) DisplayName() string {
	if a.Label != "" {
		return a.Label
	}
	return "#" + strconv.FormatInt(a.ID, 10)
}

// account returns the provider's enabled account by id, or nil.
func (p *Provider) account(id int64) *Account {
	if p == nil {
		return nil
	}
	for i := range p.Accounts {
		if p.Accounts[i].ID == id {
			return &p.Accounts[i]
		}
	}
	return nil
}

// Resolve maps a requested model name to ordered candidates. A trailing
// "[1m]" (the 1M-context listing marker, stripped client-side by Claude
// Code) is normalized away first — routing identities are canonical.
// Precedence: explicit model route (its whole target chain, in order) ->
// enabled models rows for the name -> "claude-" prefixed name with the
// prefix stripped, routes before rows (clients and discovery caches that
// learned claude-* naming keep working; stripping never adds list entries)
// -> not found. A name with no enabled row and no route is unroutable.
//
// preferProtocol is the client surface's wire protocol ("openai" |
// "anthropic"): row-derived candidates matching it win outright, and
// cross-protocol candidates serve only when no same-protocol candidate
// exists (bridging costs a conversion). Route targets pinning an explicit
// channel are explicit admin configuration and are never filtered;
// provider-scoped route targets apply the same same-protocol preference
// within their own segment only.
//
// The second return is the canonical client-facing name that matched — the
// route's own name for route hits, the bare (marker-stripped and, for
// claude-* mirrors, prefix-stripped) row name for row hits — for attribution
// and usage logging; empty when not found.
func (s *Snapshot) Resolve(requested, preferProtocol string) ([]Candidate, string, bool) {
	requested = strings.TrimSuffix(requested, Context1mSuffix)
	if rt, ok := s.Routes[requested]; ok {
		if cands := s.routeChain(rt, preferProtocol); len(cands) > 0 {
			return cands, rt.Name, true
		}
		// Route exists but every target is disabled/deleted: fall through so
		// the model name itself can still resolve via models rows.
	}
	if cands, ok := s.ByModel[requested]; ok && len(cands) > 0 {
		return preferSameProtocol(cands, preferProtocol), requested, true
	}
	// A literal claude-* route or row always wins above; only then does
	// prefix stripping apply.
	if rest, ok := strings.CutPrefix(requested, "claude-"); ok {
		if rt, ok := s.Routes[rest]; ok {
			if cands := s.routeChain(rt, preferProtocol); len(cands) > 0 {
				return cands, rt.Name, true
			}
		}
		if cands, ok := s.ByModel[rest]; ok && len(cands) > 0 {
			return preferSameProtocol(cands, preferProtocol), rest, true
		}
	}
	return nil, "", false
}

// preferSameProtocol keeps only the candidates whose channel speaks the
// client surface's protocol when any exist; otherwise it returns the full
// list (cross-protocol bridging is the fallback, never the default).
func preferSameProtocol(cands []Candidate, protocol string) []Candidate {
	if protocol == "" {
		return cands
	}
	var same []Candidate
	for _, c := range cands {
		if c.Channel != nil && c.Channel.Protocol == protocol {
			same = append(same, c)
		}
	}
	if len(same) > 0 {
		return same
	}
	return cands
}

// Holder owns the current snapshot pointer.
type Holder struct {
	ptr     atomic.Pointer[Snapshot]
	version atomic.Int64
}

// NewHolder builds and installs the first snapshot.
func NewHolder() *Holder { return &Holder{} }

// Load returns the current snapshot; never nil after the first Rebuild.
func (h *Holder) Load() *Snapshot { return h.ptr.Load() }

// Version reports the current snapshot version.
func (h *Holder) Version() int64 { return h.version.Load() }

// Rebuild reads all routing tables and atomically installs a new snapshot.
// Called synchronously after every admin mutation.
func (h *Holder) Rebuild(ctx context.Context, st *store.Store) error {
	snap, err := buildSnapshot(ctx, st)
	if err != nil {
		return err
	}
	snap.Version = h.version.Add(1)
	h.ptr.Store(snap)
	return nil
}

func buildSnapshot(ctx context.Context, st *store.Store) (*Snapshot, error) {
	snap := &Snapshot{
		Providers:          map[int64]*Provider{},
		Channels:           map[int64]*Channel{},
		channelsByProvider: map[int64][]*Channel{},
		ByModel:            map[string][]Candidate{},
		Routes:             map[string]Route{},
		ClientKeys:         map[string]ClientKey{},
		Settings:           map[string]string{},
	}
	if all, err := st.Settings.GetAll(ctx); err == nil {
		for k, v := range all {
			snap.Settings[k] = v
		}
	}

	providers, err := st.Providers.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		np := &Provider{ID: p.ID, Name: p.Name, Enabled: p.Enabled}
		if np.Enabled {
			accounts, err := st.Accounts.EnabledAccounts(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			for _, a := range accounts {
				np.Accounts = append(np.Accounts, Account{ID: a.ID, Label: a.Label, Secret: a.APIKey, Weight: a.Weight})
			}
		}
		snap.Providers[p.ID] = np
	}

	channels, err := st.Channels.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range channels {
		if !c.Enabled {
			continue
		}
		nc := &Channel{
			ID: c.ID, ProviderID: c.ProviderID, Provider: snap.Providers[c.ProviderID],
			Name: c.Name, Protocol: c.Protocol, BaseURL: c.BaseURL, ChatPath: c.ChatPath,
			AuthStyle: c.AuthStyle, ResponsesPath: c.ResponsesPath,
			ExtraHeaders:        parseHeaders(c.ExtraHeaders),
			Priority:            c.Priority,
			Weight:              c.Weight,
			SupportsEmbeddings:  c.SupportsEmbeddings,
			Passthrough:         c.Passthrough,
			ForceUpstreamStream: c.ForceUpstreamStream,
		}
		if nc.Provider == nil || !nc.Provider.Enabled {
			continue // provider disabled or missing: channel unroutable
		}
		snap.Channels[nc.ID] = nc
		snap.channelsByProvider[nc.ProviderID] = append(snap.channelsByProvider[nc.ProviderID], nc)
	}
	// Segment order for provider-scoped route targets must match the row
	// expansion below: priority DESC, then weight DESC (the store's SQL
	// ordering breaks ties by id instead of weight).
	for _, chs := range snap.channelsByProvider {
		sort.SliceStable(chs, func(i, j int) bool {
			if chs[i].Priority != chs[j].Priority {
				return chs[i].Priority > chs[j].Priority
			}
			return chs[i].Weight > chs[j].Weight
		})
	}

	// Model rows are the routing table: each enabled row yields one candidate
	// per live channel of its provider (the registry is provider-scoped — every
	// endpoint of the provider can serve the name), carrying the row's upstream
	// alias ("" = identity). A name with no enabled row on any live provider
	// (and no model route) is neither listed nor routable.
	allModels, err := st.Models.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range allModels {
		if !m.Enabled {
			continue
		}
		for _, ch := range snap.channelsByProvider[m.ProviderID] {
			snap.ByModel[m.ID] = append(snap.ByModel[m.ID],
				Candidate{Channel: ch, UpstreamModel: m.Upstream()})
		}
	}
	for model := range snap.ByModel {
		cands := snap.ByModel[model]
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Channel.Priority != cands[j].Channel.Priority {
				return cands[i].Channel.Priority > cands[j].Channel.Priority // higher first
			}
			return cands[i].Channel.Weight > cands[j].Channel.Weight
		})
	}

	routes, err := st.Routes.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range routes {
		targets := make([]RouteTarget, 0, len(a.Targets))
		for _, tgt := range a.Targets {
			var accountID, channelID int64
			if tgt.AccountID != nil {
				accountID = *tgt.AccountID
			}
			if tgt.ChannelID != nil {
				channelID = *tgt.ChannelID
			}
			targets = append(targets, RouteTarget{
				ProviderID:    tgt.ProviderID,
				ChannelID:     channelID,
				UpstreamModel: tgt.UpstreamModel,
				AccountID:     accountID,
			})
		}
		snap.Routes[a.Name] = Route{Name: a.Name, Targets: targets}
	}

	// The merged /v1/models list dedupes by name over the enabled rows on live
	// providers: listed ⇔ routable. Rows arrive ORDER BY id, provider_id, and
	// later duplicates only fill fields the first row left blank —
	// deterministic.
	// describe renders the entry description as "{provider}/{account}" using
	// the account that would serve first: the route target's pinned account,
	// else the first enabled account of the serving provider. The channel
	// name is deliberately omitted — every live channel of the provider can
	// serve the name, so naming the first one reads as a protocol mark. A
	// provider without enabled accounts degrades to "{provider}"; names no
	// channel serves fall back to the row's provider name, then "".
	describe := func(name string, providerID int64) string {
		render := func(ch *Channel, pinned *Account) string {
			p := ch.Provider
			if p == nil {
				return ""
			}
			acc := pinned
			if acc == nil && len(p.Accounts) > 0 {
				acc = &p.Accounts[0]
			}
			if acc == nil {
				return p.Name
			}
			return p.Name + "/" + acc.DisplayName()
		}
		if rt, ok := snap.Routes[name]; ok {
			for _, tgt := range rt.Targets {
				chs := snap.targetLiveChannels(tgt)
				if len(chs) == 0 || chs[0].Provider == nil {
					continue
				}
				// chs[0] is the target's build-order top channel: the pinned
				// endpoint, or the provider's highest-priority live one.
				var pinned *Account
				if tgt.AccountID != 0 {
					pinned = chs[0].Provider.account(tgt.AccountID)
				}
				return render(chs[0], pinned)
			}
		}
		if cands := snap.ByModel[name]; len(cands) > 0 && cands[0].Channel != nil && cands[0].Channel.Provider != nil {
			return render(cands[0].Channel, nil)
		}
		if p := snap.Providers[providerID]; p != nil {
			return p.Name
		}
		return ""
	}
	merged := map[string]*ModelEntry{}
	var order []string
	for _, m := range allModels {
		if !m.Enabled {
			continue
		}
		if len(snap.channelsByProvider[m.ProviderID]) == 0 {
			continue // dead provider (disabled or no live channel): neither routable nor listed (route names still list below)
		}
		if e, ok := merged[m.ID]; ok {
			if e.ContextWindow == nil {
				e.ContextWindow = m.ContextWindow
			}
			if e.MaxOutputTokens == nil {
				e.MaxOutputTokens = m.MaxOutputTokens
			}
			if e.DisplayName == "" {
				e.DisplayName = m.DisplayName
			}
			continue
		}
		merged[m.ID] = &ModelEntry{
			ID: m.ID, DisplayName: m.DisplayName, Source: "provider",
			Provider:      describe(m.ID, m.ProviderID),
			ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
		}
		order = append(order, m.ID)
	}
	seen := map[string]bool{}
	for _, id := range order {
		snap.Models = append(snap.Models, *merged[id])
		seen[id] = true
	}
	for name := range snap.Routes {
		if seen[name] {
			// The name also has a models row: the route still wins at resolve
			// time, so the listing entry is marked as route-sourced.
			for i := range snap.Models {
				if snap.Models[i].ID == name {
					snap.Models[i].Source = "route"
					snap.Models[i].IsRoute = true
				}
			}
			continue
		}
		e := ModelEntry{ID: name, DisplayName: name, Source: "route", IsRoute: true, Provider: describe(name, 0)}
		snap.Models = append(snap.Models, e)
		seen[name] = true
	}
	sort.Slice(snap.Models, func(i, j int) bool { return snap.Models[i].ID < snap.Models[j].ID })

	// Claude Code marks 1M-context models with a "[1m]" id suffix (and strips
	// it client-side when requesting). Anthropic-served entries whose merged
	// context window reaches the threshold therefore also list a decorated
	// "<id>[1m]" id on the Anthropic surface; the plain id stays listed for
	// existing clients. Routing identities stay bare — Resolve normalizes the
	// suffix away.
	existingIDs := make(map[string]bool, len(snap.Models))
	for _, e := range snap.Models {
		existingIDs[e.ID] = true
	}
	for _, e := range snap.Models {
		if e.ContextWindow == nil || *e.ContextWindow < context1mThreshold {
			continue
		}
		if !snap.anthropicServed(e.ID) {
			continue
		}
		if strings.HasSuffix(e.ID, Context1mSuffix) {
			continue // already decorated; never double-mark
		}
		id := e.ID + Context1mSuffix
		if existingIDs[id] {
			continue
		}
		existingIDs[id] = true
		e.ID = id
		// Decorate the display name too, so the marked entry stays
		// distinguishable from the plain one in pickers that render it; an
		// empty name falls back to the (already suffixed) id at render time.
		if e.DisplayName != "" {
			e.DisplayName += Context1mSuffix
		}
		snap.ContextVariants = append(snap.ContextVariants, e)
	}
	sort.Slice(snap.ContextVariants, func(i, j int) bool {
		return snap.ContextVariants[i].ID < snap.ContextVariants[j].ID
	})

	// Claude Code's gateway model discovery silently drops ids without a
	// claude/anthropic substring, so mirror every anthropic-served listed
	// name as "claude-<id>" — including the [1m] marked entries (limits and
	// provider description copied; names that would collide with an existing
	// id are skipped). Resolve strips the prefix when routing.
	mirrored := make([]ModelEntry, 0, len(snap.Models)+len(snap.ContextVariants))
	mirrored = append(mirrored, snap.Models...)
	mirrored = append(mirrored, snap.ContextVariants...)
	baseIDs := make(map[string]bool, len(mirrored))
	for _, e := range mirrored {
		baseIDs[e.ID] = true
	}
	for _, e := range mirrored {
		// Gating keys on the bare (routable) name: marked ids are not in the
		// routing maps themselves.
		if !snap.anthropicServed(strings.TrimSuffix(e.ID, Context1mSuffix)) {
			continue
		}
		lower := strings.ToLower(e.ID)
		if strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic") {
			continue
		}
		if baseIDs["claude-"+e.ID] {
			continue
		}
		snap.DiscoveryVariants = append(snap.DiscoveryVariants, ModelEntry{
			ID: "claude-" + e.ID, DisplayName: e.DisplayName, Source: e.Source, IsRoute: e.IsRoute,
			Provider:        e.Provider,
			ContextWindow:   e.ContextWindow,
			MaxOutputTokens: e.MaxOutputTokens,
		})
	}
	sort.Slice(snap.DiscoveryVariants, func(i, j int) bool {
		return snap.DiscoveryVariants[i].ID < snap.DiscoveryVariants[j].ID
	})

	keys, err := st.APIKeys.EnabledKeyHashes(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		snap.ClientKeys[k.Hash] = ClientKey{ID: k.ID, Name: k.Name}
	}
	return snap, nil
}

func parseHeaders(raw string) map[string]string {
	// Stored as a JSON object string; parse defensively so a bad value never
	// blocks routing (it is admin input, validated at write time).
	out := map[string]string{}
	if raw == "" || raw == "{}" {
		return out
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
