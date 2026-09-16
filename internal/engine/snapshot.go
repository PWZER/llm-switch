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
	"sync/atomic"

	"github.com/PWZER/llm-switch/internal/store"
)

// Key is one enabled upstream credential.
type Key struct {
	ID     int64
	Secret string
	Weight int
}

// Provider is a vendor account with its enabled key pool.
type Provider struct {
	ID      int64
	Name    string
	Keys    []Key
	Enabled bool
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
	AuthStyle           string // "bearer" | "x-api-key"
	ModelsURL           *string
	ExtraHeaders        map[string]string
	Priority            int
	Weight              int
	AutoBind            bool
	SupportsEmbeddings  bool
	Passthrough         bool
	ForceUpstreamStream bool
}

// Candidate pairs a channel with the upstream model to request.
type Candidate struct {
	Channel       *Channel
	UpstreamModel string
}

// Alias is a hot-switchable client-facing name.
type Alias struct {
	Name          string
	ChannelID     int64
	UpstreamModel string
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
	IsAlias     bool
}

// Snapshot is an immutable view of routing configuration.
type Snapshot struct {
	Version    int64
	Providers  map[int64]*Provider
	Channels   map[int64]*Channel
	ByModel    map[string][]Candidate // failover-ordered
	Aliases    map[string]Alias
	ClientKeys map[string]ClientKey   // sha256(key) -> record
	Models     []ModelEntry           // merged registry, sorted by id
	Settings   map[string]string      // editable settings (allowlisted)
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

// Resolve maps a requested model name to ordered candidates.
// Precedence: explicit alias -> channel model bindings -> not found.
func (s *Snapshot) Resolve(requested string) ([]Candidate, string, bool) {
	if a, ok := s.Aliases[requested]; ok {
		if ch := s.Channels[a.ChannelID]; ch != nil {
			return []Candidate{{Channel: ch, UpstreamModel: a.UpstreamModel}}, a.Name, true
		}
	}
	if cands, ok := s.ByModel[requested]; ok && len(cands) > 0 {
		return cands, "", true
	}
	return nil, "", false
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
		Providers:  map[int64]*Provider{},
		Channels:   map[int64]*Channel{},
		ByModel:    map[string][]Candidate{},
		Aliases:    map[string]Alias{},
		ClientKeys: map[string]ClientKey{},
		Settings:   map[string]string{},
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
			keys, err := st.Keys.EnabledKeys(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			for _, k := range keys {
				np.Keys = append(np.Keys, Key{ID: k.ID, Secret: k.APIKey, Weight: k.Weight})
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
			AuthStyle: c.AuthStyle, ModelsURL: c.ModelsURL,
			ExtraHeaders:        parseHeaders(c.ExtraHeaders),
			Priority:            c.Priority,
			Weight:              c.Weight,
			AutoBind:            c.AutoBind,
			SupportsEmbeddings:  c.SupportsEmbeddings,
			Passthrough:         c.Passthrough,
			ForceUpstreamStream: c.ForceUpstreamStream,
		}
		if nc.Provider == nil || !nc.Provider.Enabled {
			continue // provider disabled or missing: channel unroutable
		}
		snap.Channels[nc.ID] = nc
		for _, b := range c.Models {
			snap.ByModel[b.Model] = append(snap.ByModel[b.Model], Candidate{Channel: nc, UpstreamModel: b.UpstreamModel})
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

	aliases, err := st.Aliases.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range aliases {
		snap.Aliases[a.Name] = Alias{Name: a.Name, ChannelID: a.ChannelID, UpstreamModel: a.UpstreamModel}
	}

	models, err := st.Models.EnabledModels(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, m := range models {
		snap.Models = append(snap.Models, ModelEntry{ID: m.ID, DisplayName: m.DisplayName, Source: m.Source})
		seen[m.ID] = true
	}
	// Channel bindings are routable, so they are servable model names.
	for model := range snap.ByModel {
		if !seen[model] {
			snap.Models = append(snap.Models, ModelEntry{ID: model, DisplayName: model, Source: "channel"})
			seen[model] = true
		}
	}
	for name := range snap.Aliases {
		if !seen[name] {
			snap.Models = append(snap.Models, ModelEntry{ID: name, DisplayName: name, Source: "alias", IsAlias: true})
			seen[name] = true
		}
	}
	sort.Slice(snap.Models, func(i, j int) bool { return snap.Models[i].ID < snap.Models[j].ID })

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
