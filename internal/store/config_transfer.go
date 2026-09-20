package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ExportVersion is the only config document version this build reads.
const ExportVersion = 1

// Section names accepted by ExportConfig and recognized by ImportConfig.
const (
	SectionProviders = "providers"
	SectionRoutes    = "model_routes"
	SectionAPIKeys   = "api_keys"
	SectionSettings  = "settings"
)

// ExportSections lists every exportable section.
var ExportSections = []string{SectionProviders, SectionRoutes, SectionAPIKeys, SectionSettings}

// ErrBadDocument marks a structurally invalid export document (unsupported
// version, duplicate natural keys inside the document, malformed key hash,
// invalid allowlisted setting value). Referential problems (unknown route
// targets, missing pins) are not errors: they degrade with warnings.
var ErrBadDocument = errors.New("invalid config document")

// ConfigExport is the whole configuration document. It is deliberately
// ID-free: SQLite rowids are DB-local, so every row is keyed by its natural
// name and import remaps route-target pins to the destination DB's IDs.
type ConfigExport struct {
	Version     int               `json:"version"`
	ExportedAt  string            `json:"exported_at,omitempty"` // RFC3339, informational; ignored on import
	Providers   []ExportProvider  `json:"providers,omitempty"`
	ModelRoutes []ExportRoute     `json:"model_routes,omitempty"`
	APIKeys     []ExportAPIKey    `json:"api_keys,omitempty"`
	Settings    map[string]string `json:"settings,omitempty"`
}

// ExportProvider is one provider with its full subtree. Accounts, channels
// and models always travel with their provider — the registry is only
// routable when the whole unit lands together.
type ExportProvider struct {
	Name      string          `json:"name"`
	ModelsURL *string         `json:"models_url"`
	Enabled   bool            `json:"enabled"`
	Accounts  []ExportAccount `json:"accounts,omitempty"`
	Channels  []ExportChannel `json:"channels,omitempty"`
	Models    []ExportModel   `json:"models,omitempty"`
}

// ExportAccount: APIKey is empty (and omitted) when the export excluded
// account keys. UsageProbes is the stored JSON array verbatim.
type ExportAccount struct {
	Label       string          `json:"label"`
	APIKey      string          `json:"api_key,omitempty"`
	Weight      int             `json:"weight"`
	Enabled     bool            `json:"enabled"`
	UsageProbes json.RawMessage `json:"usage_probes"`
}

// ExportChannel mirrors a channel row minus IDs/timestamps. ExtraHeaders is
// the stored JSON-object string verbatim (lossless pass-through).
type ExportChannel struct {
	Name                string  `json:"name"`
	Protocol            string  `json:"protocol"` // "openai" | "anthropic"
	BaseURL             string  `json:"base_url"`
	ChatPath            string  `json:"chat_path"`
	ResponsesPath       *string `json:"responses_path"`
	AuthStyle           string  `json:"auth_style"` // "bearer" | "x-api-key"
	ExtraHeaders        string  `json:"extra_headers"`
	Enabled             bool    `json:"enabled"`
	Priority            int     `json:"priority"`
	Weight              int     `json:"weight"`
	SupportsEmbeddings  bool    `json:"supports_embeddings"`
	Passthrough         bool    `json:"passthrough"`
	ForceUpstreamStream bool    `json:"force_upstream_stream"`
}

type ExportModel struct {
	ID              string `json:"id"`
	UpstreamModel   string `json:"upstream_model"`
	DisplayName     string `json:"display_name"`
	ContextWindow   *int64 `json:"context_window"`
	MaxOutputTokens *int64 `json:"max_output_tokens"`
	Enabled         bool   `json:"enabled"`
}

// ExportRouteTarget references its provider by name. Channel pins reference
// a channel name and account pins an account label within that provider —
// neither is unique in the schema, so import resolves first-match and
// degrades a missing pin to provider auto-select with a warning. A dead
// stored pin (row already gone) exports as nil for the same reason.
type ExportRouteTarget struct {
	Provider      string  `json:"provider"`
	Channel       *string `json:"channel"`
	AccountLabel  *string `json:"account_label"`
	UpstreamModel string  `json:"upstream_model"`
}

type ExportRoute struct {
	Name    string              `json:"name"`
	Targets []ExportRouteTarget `json:"targets"`
}

// ExportAPIKey carries the stored sha256 hex so the client key stays valid
// on the destination instance. The hash of a random gateway key is not
// brute-forceable, but documents containing them still deserve care.
type ExportAPIKey struct {
	Name       string `json:"name"`
	KeyHash    string `json:"key_hash"`
	Prefix     string `json:"prefix"`
	Enabled    bool   `json:"enabled"`
	TokenLimit *int64 `json:"token_limit"`
	ExpiresAt  *int64 `json:"expires_at"`
}

// ImportCounts tallies one section of an import.
type ImportCounts struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
}

// ImportResult is the response of both the dry-run plan and the applied
// import: per-section counts plus non-fatal warnings.
type ImportResult struct {
	Providers   ImportCounts `json:"providers"`
	Accounts    ImportCounts `json:"accounts"`
	Channels    ImportCounts `json:"channels"`
	Models      ImportCounts `json:"models"`
	ModelRoutes ImportCounts `json:"model_routes"`
	APIKeys     ImportCounts `json:"api_keys"`
	Settings    ImportCounts `json:"settings"`
	Warnings    []string     `json:"warnings"`
}

// ExportConfig assembles the document for the requested sections. Selecting
// model_routes auto-includes the full subtree of every provider its targets
// reference (dependency closure). Account plaintext keys are populated only
// when includeAccountKeys is set. Read-only: runs on the read pool.
func (s *Store) ExportConfig(ctx context.Context, sections []string, includeAccountKeys bool) (*ConfigExport, error) {
	want := map[string]bool{}
	for _, sec := range sections {
		switch sec {
		case SectionProviders, SectionRoutes, SectionAPIKeys, SectionSettings:
			want[sec] = true
		}
	}
	doc := &ConfigExport{Version: ExportVersion, ExportedAt: time.Now().UTC().Format(time.RFC3339)}
	if len(want) == 0 {
		return doc, nil
	}

	providers, err := s.Providers.List(ctx)
	if err != nil {
		return nil, err
	}
	channels, err := s.Channels.List(ctx)
	if err != nil {
		return nil, err
	}
	models, err := s.Models.List(ctx)
	if err != nil {
		return nil, err
	}
	routes, err := s.Routes.List(ctx)
	if err != nil {
		return nil, err
	}
	allSettings, err := s.Settings.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := s.accountsForExport(ctx)
	if err != nil {
		return nil, err
	}
	apiKeys, err := s.apiKeysForExport(ctx)
	if err != nil {
		return nil, err
	}

	providerName := make(map[int64]string, len(providers))
	for _, p := range providers {
		providerName[p.ID] = p.Name
	}
	channelName := make(map[int64]string, len(channels))
	for _, c := range channels {
		channelName[c.ID] = c.Name
	}
	accountLabel := make(map[int64]string, len(accounts))
	for _, a := range accounts {
		accountLabel[a.id] = a.label
	}

	// Dependency closure: route targets pull in their provider's subtree.
	selected := map[int64]bool{}
	if want[SectionProviders] {
		for _, p := range providers {
			selected[p.ID] = true
		}
	}
	if want[SectionRoutes] {
		for _, r := range routes {
			for _, tgt := range r.Targets {
				if tgt.ProviderID != 0 {
					selected[tgt.ProviderID] = true
				}
			}
		}
	}

	if want[SectionProviders] || want[SectionRoutes] {
		for _, p := range providers {
			if !selected[p.ID] {
				continue
			}
			ep := ExportProvider{Name: p.Name, ModelsURL: p.ModelsURL, Enabled: p.Enabled}
			for _, a := range accounts {
				if a.providerID != p.ID {
					continue
				}
				ea := ExportAccount{
					Label:       a.label,
					Weight:      a.weight,
					Enabled:     a.enabled,
					UsageProbes: json.RawMessage(a.probes),
				}
				if includeAccountKeys {
					ea.APIKey = a.apiKey
				}
				ep.Accounts = append(ep.Accounts, ea)
			}
			for _, c := range channels {
				if c.ProviderID != p.ID {
					continue
				}
				ep.Channels = append(ep.Channels, ExportChannel{
					Name: c.Name, Protocol: c.Protocol, BaseURL: c.BaseURL,
					ChatPath: c.ChatPath, ResponsesPath: c.ResponsesPath,
					AuthStyle: c.AuthStyle, ExtraHeaders: c.ExtraHeaders,
					Enabled: c.Enabled, Priority: c.Priority, Weight: c.Weight,
					SupportsEmbeddings: c.SupportsEmbeddings, Passthrough: c.Passthrough,
					ForceUpstreamStream: c.ForceUpstreamStream,
				})
			}
			for _, m := range models {
				if m.ProviderID != p.ID {
					continue
				}
				ep.Models = append(ep.Models, ExportModel{
					ID: m.ID, UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName,
					ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
					Enabled: m.Enabled,
				})
			}
			doc.Providers = append(doc.Providers, ep)
		}
	}

	if want[SectionRoutes] {
		for _, r := range routes {
			er := ExportRoute{Name: r.Name}
			for _, tgt := range r.Targets {
				ert := ExportRouteTarget{
					Provider:      providerName[tgt.ProviderID],
					UpstreamModel: tgt.UpstreamModel,
				}
				if tgt.ChannelID != nil {
					if name := channelName[*tgt.ChannelID]; name != "" {
						ert.Channel = &name // a dead pin cannot be named; export as nil
					}
				}
				if tgt.AccountID != nil {
					if label, ok := accountLabel[*tgt.AccountID]; ok {
						ert.AccountLabel = &label
					}
				}
				er.Targets = append(er.Targets, ert)
			}
			doc.ModelRoutes = append(doc.ModelRoutes, er)
		}
	}

	if want[SectionAPIKeys] {
		for _, k := range apiKeys {
			doc.APIKeys = append(doc.APIKeys, ExportAPIKey{
				Name: k.name, KeyHash: k.hash, Prefix: k.prefix, Enabled: k.enabled,
				TokenLimit: k.tokenLimit, ExpiresAt: k.expiresAt,
			})
		}
	}

	if want[SectionSettings] {
		doc.Settings = map[string]string{}
		for k := range AdminSettingKeys {
			if v, ok := allSettings[k]; ok {
				doc.Settings[k] = v
			}
		}
	}
	return doc, nil
}

// ImportConfig validates the document, then applies every section present in
// ONE transaction on the write pool and returns per-section counts plus
// non-fatal warnings. Absent sections are untouched — upsert merge, never
// wipe. Internal settings (seeded_providers and friends) are not in the
// admin allowlist and can never pass through.
func (s *Store) ImportConfig(ctx context.Context, doc *ConfigExport) (*ImportResult, error) {
	return s.runImport(ctx, doc, true)
}

// PlanImport (dry run) executes the exact same transactional path as
// ImportConfig but rolls back instead of committing, so its counts and
// warnings are what the real import would produce — with zero side effects.
func (s *Store) PlanImport(ctx context.Context, doc *ConfigExport) (*ImportResult, error) {
	return s.runImport(ctx, doc, false)
}

func (s *Store) runImport(ctx context.Context, doc *ConfigExport, commit bool) (*ImportResult, error) {
	res := &ImportResult{Warnings: []string{}}
	providers, routes, settings, err := prepareImport(doc, res)
	if err != nil {
		return nil, err
	}
	// The write pool is required even for the plan: the plan must see the
	// same intermediate state as the import (rows inserted earlier in the
	// transaction resolve later references).
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin import tx: %w", err)
	}
	defer tx.Rollback()

	state := &importState{providerIDs: map[string]int64{}}
	if providers != nil {
		if err := importProviders(ctx, tx, providers, res, state); err != nil {
			return nil, err
		}
	}
	if routes != nil {
		if err := importRoutes(ctx, tx, routes, res, state); err != nil {
			return nil, err
		}
	}
	if len(doc.APIKeys) > 0 {
		if err := importAPIKeys(ctx, tx, doc.APIKeys, res); err != nil {
			return nil, err
		}
	}
	if len(settings) > 0 {
		if err := importSettings(ctx, tx, settings, res); err != nil {
			return nil, err
		}
	}
	if commit {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit import: %w", err)
		}
	}
	sort.Strings(res.Warnings)
	return res, nil
}

var keyHashRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// prepareImport is phase A: structural validation with hard errors wrapped
// in ErrBadDocument, in-place normalization (trimmed names, clamped weights,
// lowercase hashes, "{}" headers) and soft warnings for problems that resolve
// by dropping content (duplicate channel names, reserved route suffixes,
// structurally empty routes/targets, non-importable settings).
func prepareImport(doc *ConfigExport, res *ImportResult) ([]ExportProvider, []ExportRoute, map[string]string, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrBadDocument}, args...)...)
	}
	if doc == nil {
		return nil, nil, nil, bad("document is nil")
	}
	if doc.Version != ExportVersion {
		return nil, nil, nil, bad("unsupported version %d (want %d)", doc.Version, ExportVersion)
	}
	if len(doc.Providers) == 0 && len(doc.ModelRoutes) == 0 && len(doc.APIKeys) == 0 && len(doc.Settings) == 0 {
		return nil, nil, nil, bad("no sections to import")
	}

	providers := make([]ExportProvider, 0, len(doc.Providers))
	seenProvider := map[string]bool{}
	for _, p := range doc.Providers {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			return nil, nil, nil, bad("provider name is required")
		}
		if seenProvider[p.Name] {
			return nil, nil, nil, bad("duplicate provider %q", p.Name)
		}
		seenProvider[p.Name] = true

		channels := make([]ExportChannel, 0, len(p.Channels))
		seenChannel := map[string]bool{}
		for _, c := range p.Channels {
			c.Name = strings.TrimSpace(c.Name)
			if c.Name == "" {
				return nil, nil, nil, bad("provider %q channel name is required", p.Name)
			}
			switch c.Protocol {
			case "openai", "anthropic":
			default:
				return nil, nil, nil, bad("provider %q channel %q: protocol must be openai or anthropic", p.Name, c.Name)
			}
			switch c.AuthStyle {
			case "bearer", "x-api-key":
			default:
				return nil, nil, nil, bad("provider %q channel %q: auth_style must be bearer or x-api-key", p.Name, c.Name)
			}
			c.ExtraHeaders = strings.TrimSpace(c.ExtraHeaders)
			if c.ExtraHeaders == "" {
				c.ExtraHeaders = "{}"
			} else if !json.Valid([]byte(c.ExtraHeaders)) || !strings.HasPrefix(c.ExtraHeaders, "{") {
				return nil, nil, nil, bad("provider %q channel %q: extra_headers must be a JSON object", p.Name, c.Name)
			}
			if seenChannel[c.Name] {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"provider %q: duplicate channel %q — first entry wins", p.Name, c.Name))
				continue
			}
			seenChannel[c.Name] = true
			channels = append(channels, c)
		}

		models := make([]ExportModel, 0, len(p.Models))
		for _, m := range p.Models {
			m.ID = strings.TrimSpace(m.ID)
			if m.ID == "" {
				return nil, nil, nil, bad("provider %q model id is required", p.Name)
			}
			models = append(models, m)
		}

		accounts := make([]ExportAccount, 0, len(p.Accounts))
		for _, a := range p.Accounts {
			if a.Weight < 1 {
				a.Weight = 1
			}
			accounts = append(accounts, a)
		}

		p.Accounts, p.Channels, p.Models = accounts, channels, models
		providers = append(providers, p)
	}

	routes := make([]ExportRoute, 0, len(doc.ModelRoutes))
	for _, r := range doc.ModelRoutes {
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			return nil, nil, nil, bad("model route name is required")
		}
		if strings.HasSuffix(r.Name, "[1m]") {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"route %q: the [1m] suffix is reserved — route skipped", r.Name))
			continue
		}
		if len(r.Targets) == 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"route %q: no targets — route skipped", r.Name))
			continue
		}
		targets := make([]ExportRouteTarget, 0, len(r.Targets))
		for i, tgt := range r.Targets {
			tgt.Provider = strings.TrimSpace(tgt.Provider)
			tgt.UpstreamModel = strings.TrimSpace(tgt.UpstreamModel)
			if tgt.Provider == "" || tgt.UpstreamModel == "" {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"route %q target %d: provider and upstream model are required — target dropped", r.Name, i+1))
				continue
			}
			targets = append(targets, tgt)
		}
		if len(targets) == 0 {
			continue // already warned per target
		}
		r.Targets = targets
		routes = append(routes, r)
	}

	apiKeys := make([]ExportAPIKey, 0, len(doc.APIKeys))
	seenHash := map[string]bool{}
	for _, k := range doc.APIKeys {
		k.Name = strings.TrimSpace(k.Name)
		if k.Name == "" {
			return nil, nil, nil, bad("api key name is required")
		}
		k.KeyHash = strings.ToLower(strings.TrimSpace(k.KeyHash))
		if !keyHashRe.MatchString(k.KeyHash) {
			return nil, nil, nil, bad("api key %q: key_hash must be 64 hex characters", k.Name)
		}
		if seenHash[k.KeyHash] {
			return nil, nil, nil, bad("duplicate api key hash (%q)", k.Name)
		}
		seenHash[k.KeyHash] = true
		apiKeys = append(apiKeys, k)
	}

	settings, warn, err := SanitizeAdminSettings(doc.Settings)
	if err != nil {
		return nil, nil, nil, bad("%s", err.Error())
	}
	res.Warnings = append(res.Warnings, warn...)

	if len(providers) == 0 && len(routes) == 0 && len(apiKeys) == 0 && len(settings) == 0 {
		return nil, nil, nil, bad("no sections to import")
	}
	return providers, routes, settings, nil
}

// importState carries ID resolution across the import steps.
type importState struct {
	providerIDs map[string]int64 // provider name -> id (created or looked up)
}

// importProviders upserts providers by name, then reconciles each provider's
// accounts, channels and model rows (B1/B2).
func importProviders(ctx context.Context, tx *sql.Tx, providers []ExportProvider, res *ImportResult, st *importState) error {
	for _, p := range providers {
		id, found, err := txProviderByName(ctx, tx, p.Name)
		if err != nil {
			return err
		}
		if found {
			if _, err := tx.ExecContext(ctx, `
				UPDATE providers SET models_url = ?, enabled = ?, updated_at = ? WHERE id = ?`,
				p.ModelsURL, p.Enabled, now(), id); err != nil {
				return fmt.Errorf("import provider %q: %w", p.Name, err)
			}
			res.Providers.Updated++
		} else {
			r, err := tx.ExecContext(ctx, `
				INSERT INTO providers (name, models_url, enabled, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?)`,
				p.Name, p.ModelsURL, p.Enabled, now(), now())
			if err != nil {
				return fmt.Errorf("import provider %q: %w", p.Name, err)
			}
			if id, err = r.LastInsertId(); err != nil {
				return fmt.Errorf("import provider %q: %w", p.Name, err)
			}
			res.Providers.Created++
		}
		st.providerIDs[p.Name] = id

		if err := importAccounts(ctx, tx, id, p, res); err != nil {
			return err
		}
		if err := importChannels(ctx, tx, id, p, res); err != nil {
			return err
		}
		if err := importModels(ctx, tx, id, p, res); err != nil {
			return err
		}
	}
	return nil
}

// importAccounts matches document accounts to existing rows: identical
// plaintext key first, then a non-empty label (first/lowest id wins, with an
// ambiguity warning). A keyless document entry that cannot match is skipped —
// accounts.api_key is NOT NULL and the admin UI cannot backfill a key, so
// inserting it would create a permanently broken account.
func importAccounts(ctx context.Context, tx *sql.Tx, providerID int64, p ExportProvider, res *ImportResult) error {
	existing, err := txAccountsByProvider(ctx, tx, providerID)
	if err != nil {
		return err
	}
	used := map[int64]bool{}
	for _, ea := range p.Accounts {
		match := -1
		ambiguous := false
		if ea.APIKey != "" {
			for _, ex := range existing {
				if !used[ex.id] && ex.apiKey == ea.APIKey {
					match = int(ex.id)
					break
				}
			}
		}
		if match < 0 && ea.Label != "" {
			for _, ex := range existing {
				if used[ex.id] || ex.label != ea.Label {
					continue
				}
				if match < 0 {
					match = int(ex.id)
				} else {
					ambiguous = true
				}
			}
		}
		if match >= 0 {
			exID := int64(match)
			if ambiguous {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"provider %q: account label %q matches multiple accounts — using the first", p.Name, ea.Label))
			}
			used[exID] = true
			if ea.APIKey != "" {
				if _, err := tx.ExecContext(ctx, `
					UPDATE accounts SET label = ?, api_key = ?, weight = ?, enabled = ?, usage_probes = ?, updated_at = ?
					WHERE id = ?`,
					ea.Label, ea.APIKey, ea.Weight, ea.Enabled, string(ea.UsageProbes), now(), exID); err != nil {
					return fmt.Errorf("import provider %q account %q: %w", p.Name, ea.Label, err)
				}
			} else {
				// Never overwrite a stored key with an empty document field.
				if _, err := tx.ExecContext(ctx, `
					UPDATE accounts SET label = ?, weight = ?, enabled = ?, usage_probes = ?, updated_at = ?
					WHERE id = ?`,
					ea.Label, ea.Weight, ea.Enabled, string(ea.UsageProbes), now(), exID); err != nil {
					return fmt.Errorf("import provider %q account %q: %w", p.Name, ea.Label, err)
				}
			}
			res.Accounts.Updated++
			continue
		}
		if ea.APIKey == "" {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"provider %q: account %q has no key in the document and cannot be matched — skipped", p.Name, ea.Label))
			res.Accounts.Skipped++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO accounts (provider_id, label, api_key, weight, enabled, usage_probes, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			providerID, ea.Label, ea.APIKey, ea.Weight, ea.Enabled, string(ea.UsageProbes), now(), now()); err != nil {
			return fmt.Errorf("import provider %q account %q: %w", p.Name, ea.Label, err)
		}
		res.Accounts.Created++
	}
	return nil
}

// importChannels matches by (provider, name) first occurrence and updates the
// full endpoint shape, or inserts.
func importChannels(ctx context.Context, tx *sql.Tx, providerID int64, p ExportProvider, res *ImportResult) error {
	type chRef struct {
		id   int64
		name string
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM channels WHERE provider_id = ? ORDER BY id`, providerID)
	if err != nil {
		return fmt.Errorf("import provider %q channels: %w", p.Name, err)
	}
	defer rows.Close()
	var existing []chRef
	for rows.Next() {
		var c chRef
		if err := rows.Scan(&c.id, &c.name); err != nil {
			return fmt.Errorf("import provider %q channels: %w", p.Name, err)
		}
		existing = append(existing, c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import provider %q channels: %w", p.Name, err)
	}
	matched := map[int64]bool{}
	for _, c := range p.Channels {
		var id int64
		found := false
		for _, ex := range existing {
			if !matched[ex.id] && ex.name == c.Name {
				id, found = ex.id, true
				break
			}
		}
		if found {
			matched[id] = true
			if _, err := tx.ExecContext(ctx, `
				UPDATE channels SET protocol = ?, base_url = ?, chat_path = ?, auth_style = ?,
					responses_path = ?, extra_headers = ?, enabled = ?, priority = ?, weight = ?,
					supports_embeddings = ?, passthrough = ?, force_upstream_stream = ?, updated_at = ?
				WHERE id = ?`,
				c.Protocol, c.BaseURL, c.ChatPath, c.AuthStyle, c.ResponsesPath, c.ExtraHeaders,
				c.Enabled, c.Priority, c.Weight, c.SupportsEmbeddings, c.Passthrough,
				c.ForceUpstreamStream, now(), id); err != nil {
				return fmt.Errorf("import provider %q channel %q: %w", p.Name, c.Name, err)
			}
			res.Channels.Updated++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channels (provider_id, name, protocol, base_url, chat_path, auth_style,
				responses_path, extra_headers, enabled, priority, weight,
				supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			providerID, c.Name, c.Protocol, c.BaseURL, c.ChatPath, c.AuthStyle, c.ResponsesPath,
			c.ExtraHeaders, c.Enabled, c.Priority, c.Weight, c.SupportsEmbeddings,
			c.Passthrough, c.ForceUpstreamStream, now(), now()); err != nil {
			return fmt.Errorf("import provider %q channel %q: %w", p.Name, c.Name, err)
		}
		res.Channels.Created++
	}
	return nil
}

// importModels upserts by the composite PK (provider_id, id) with a FULL
// field update — the document is authoritative for the rows it carries,
// unlike EnsureModel's insert-if-missing/NULL-backfill semantics.
func importModels(ctx context.Context, tx *sql.Tx, providerID int64, p ExportProvider, res *ImportResult) error {
	existing := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM models WHERE provider_id = ?`, providerID)
	if err != nil {
		return fmt.Errorf("import provider %q models: %w", p.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("import provider %q models: %w", p.Name, err)
		}
		existing[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import provider %q models: %w", p.Name, err)
	}
	for _, m := range p.Models {
		if existing[m.ID] {
			if _, err := tx.ExecContext(ctx, `
				UPDATE models SET upstream_model = ?, display_name = ?, context_window = ?,
					max_output_tokens = ?, enabled = ?, updated_at = ?
				WHERE provider_id = ? AND id = ?`,
				m.UpstreamModel, m.DisplayName, m.ContextWindow, m.MaxOutputTokens, m.Enabled,
				now(), providerID, m.ID); err != nil {
				return fmt.Errorf("import provider %q model %q: %w", p.Name, m.ID, err)
			}
			res.Models.Updated++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO models (id, provider_id, upstream_model, display_name, context_window,
				max_output_tokens, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, providerID, m.UpstreamModel, m.DisplayName, m.ContextWindow,
			m.MaxOutputTokens, m.Enabled, now(), now()); err != nil {
			return fmt.Errorf("import provider %q model %q: %w", p.Name, m.ID, err)
		}
		res.Models.Created++
	}
	return nil
}

// importRoutes resolves target references by natural name (the provider map
// from this document first, then the live DB — a routes-only document against
// an already-configured destination must still resolve), drops unresolvable
// pins with warnings, dedupes degraded chains and upserts by route name.
func importRoutes(ctx context.Context, tx *sql.Tx, routes []ExportRoute, res *ImportResult, st *importState) error {
	for _, r := range routes {
		resolved := make([]ModelRouteTarget, 0, len(r.Targets))
		for i, tgt := range r.Targets {
			providerID, ok := st.providerIDs[tgt.Provider]
			if !ok {
				id, found, err := txProviderByName(ctx, tx, tgt.Provider)
				if err != nil {
					return err
				}
				if found {
					providerID, ok = id, true
				}
			}
			if !ok {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"route %q target %d: provider %q not found — target dropped", r.Name, i+1, tgt.Provider))
				continue
			}
			rt := ModelRouteTarget{ProviderID: providerID, UpstreamModel: tgt.UpstreamModel}
			if tgt.Channel != nil {
				cid, found, err := txChannelIDByName(ctx, tx, providerID, *tgt.Channel)
				if err != nil {
					return err
				}
				if found {
					rt.ChannelID = &cid
				} else {
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"route %q target %d: channel %q not found on provider %q — pin dropped (provider auto-select)",
						r.Name, i+1, *tgt.Channel, tgt.Provider))
				}
			}
			if tgt.AccountLabel != nil {
				aid, found, err := txAccountIDByLabel(ctx, tx, providerID, *tgt.AccountLabel)
				if err != nil {
					return err
				}
				if found {
					rt.AccountID = &aid
				} else {
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"route %q target %d: account %q not found on provider %q — pin dropped (pool rotation)",
						r.Name, i+1, *tgt.AccountLabel, tgt.Provider))
				}
			}
			resolved = append(resolved, rt)
		}
		if len(resolved) == 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"route %q: no resolvable targets — route skipped", r.Name))
			res.ModelRoutes.Skipped++
			continue
		}
		resolved = dedupeRouteTargets(resolved)
		targetsJSON, err := json.Marshal(resolved)
		if err != nil {
			return fmt.Errorf("import route %q: %w", r.Name, err)
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_routes WHERE name = ?`, r.Name).Scan(&exists); err != nil {
			return fmt.Errorf("import route %q: %w", r.Name, err)
		}
		if exists > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE model_routes SET targets_json = ?, updated_at = ? WHERE name = ?`,
				string(targetsJSON), now(), r.Name); err != nil {
				return fmt.Errorf("import route %q: %w", r.Name, err)
			}
			res.ModelRoutes.Updated++
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO model_routes (name, targets_json, updated_at) VALUES (?, ?, ?)`,
				r.Name, string(targetsJSON), now()); err != nil {
				return fmt.Errorf("import route %q: %w", r.Name, err)
			}
			res.ModelRoutes.Created++
		}
	}
	return nil
}

// importAPIKeys upserts by the stored sha256 hash (UNIQUE): an existing hash
// keeps the same plaintext client key working on this instance.
func importAPIKeys(ctx context.Context, tx *sql.Tx, keys []ExportAPIKey, res *ImportResult) error {
	existing := map[string]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT id, key FROM api_keys`)
	if err != nil {
		return fmt.Errorf("import api keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return fmt.Errorf("import api keys: %w", err)
		}
		existing[hash] = id
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import api keys: %w", err)
	}
	for _, k := range keys {
		if id, ok := existing[k.KeyHash]; ok {
			if _, err := tx.ExecContext(ctx, `
				UPDATE api_keys SET name = ?, enabled = ?, token_limit = ?, expires_at = ?, updated_at = ?
				WHERE id = ?`,
				k.Name, k.Enabled, k.TokenLimit, k.ExpiresAt, now(), id); err != nil {
				return fmt.Errorf("import api key %q: %w", k.Name, err)
			}
			res.APIKeys.Updated++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO api_keys (name, key, prefix, enabled, token_limit, expires_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			k.Name, k.KeyHash, k.Prefix, k.Enabled, k.TokenLimit, k.ExpiresAt, now(), now()); err != nil {
			return fmt.Errorf("import api key %q: %w", k.Name, err)
		}
		res.APIKeys.Created++
	}
	return nil
}

// importSettings upserts the sanitized (allowlisted) settings on the tx.
func importSettings(ctx context.Context, tx *sql.Tx, settings map[string]string, res *ImportResult) error {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, settings[k], now()); err != nil {
			return fmt.Errorf("import setting %q: %w", k, err)
		}
		res.Settings.Updated++
	}
	return nil
}

func txProviderByName(ctx context.Context, tx *sql.Tx, name string) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM providers WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup provider %q: %w", name, err)
	}
	return id, true, nil
}

type exportAccountRow struct {
	id        int64
	providerID int64
	label     string
	apiKey    string
	weight    int
	enabled   bool
	probes    string
}

// accountsForExport reads every account including plaintext keys (list
// paths mask them; export with keys enabled is the one legitimate reader).
func (s *Store) accountsForExport(ctx context.Context) ([]exportAccountRow, error) {
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT id, provider_id, label, api_key, weight, enabled, usage_probes
		FROM accounts ORDER BY provider_id, id`)
	if err != nil {
		return nil, fmt.Errorf("export accounts: %w", err)
	}
	defer rows.Close()
	out := []exportAccountRow{}
	for rows.Next() {
		var a exportAccountRow
		if err := rows.Scan(&a.id, &a.providerID, &a.label, &a.apiKey, &a.weight, &a.enabled, &a.probes); err != nil {
			return nil, fmt.Errorf("export accounts: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type txAccount struct {
	id      int64
	label   string
	apiKey  string
	weight  int
	enabled bool
	probes  string
}

func txAccountsByProvider(ctx context.Context, tx *sql.Tx, providerID int64) ([]txAccount, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, label, api_key, weight, enabled, usage_probes
		FROM accounts WHERE provider_id = ? ORDER BY id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("load provider %d accounts: %w", providerID, err)
	}
	defer rows.Close()
	out := []txAccount{}
	for rows.Next() {
		var a txAccount
		if err := rows.Scan(&a.id, &a.label, &a.apiKey, &a.weight, &a.enabled, &a.probes); err != nil {
			return nil, fmt.Errorf("load provider %d accounts: %w", providerID, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func txChannelIDByName(ctx context.Context, tx *sql.Tx, providerID int64, name string) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM channels WHERE provider_id = ? AND name = ? ORDER BY id LIMIT 1`,
		providerID, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup channel %q: %w", name, err)
	}
	return id, true, nil
}

func txAccountIDByLabel(ctx context.Context, tx *sql.Tx, providerID int64, label string) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE provider_id = ? AND label = ? ORDER BY id LIMIT 1`,
		providerID, label).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup account %q: %w", label, err)
	}
	return id, true, nil
}

type exportAPIKeyRow struct {
	id         int64
	name       string
	hash       string
	prefix     string
	enabled    bool
	tokenLimit *int64
	expiresAt  *int64
}

// apiKeysForExport reads every key including its hash (APIKeys.List never
// selects the hash; export is the one legitimate reader).
func (s *Store) apiKeysForExport(ctx context.Context) ([]exportAPIKeyRow, error) {
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT id, name, key, prefix, enabled, token_limit, expires_at
		FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("export api keys: %w", err)
	}
	defer rows.Close()
	out := []exportAPIKeyRow{}
	for rows.Next() {
		var k exportAPIKeyRow
		if err := rows.Scan(&k.id, &k.name, &k.hash, &k.prefix, &k.enabled, &k.tokenLimit, &k.expiresAt); err != nil {
			return nil, fmt.Errorf("export api keys: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
