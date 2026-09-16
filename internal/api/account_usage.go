package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PWZER/llm-switch/internal/httpx"
)

// Usage probes query upstream quota/balance endpoints per account.
// Whether an account is pay-as-you-go or a coding plan is NOT detectable from
// the protocol — vendors expose different endpoints per account type (some
// unofficial), so the probes are declared per account (preset-prefilled,
// editable in the UI) and executed on demand with a short TTL cache.

// usageProbe is one declared upstream quota endpoint.
type usageProbe struct {
	Type      string `json:"type"`                 // "balance" | "plan"
	Path      string `json:"path"`                 // absolute URL or origin-absolute path
	Name      string `json:"name,omitempty"`       // display label
	AuthStyle string `json:"auth_style,omitempty"` // "bearer" (default) | "raw"
}

// usageWindow is one quota window of a plan (5-hour, weekly, monthly, ...).
type usageWindow struct {
	Name             string   `json:"name"`                   // e.g. "TOKENS_LIMIT 5h"
	Window           string   `json:"window,omitempty"`       // duration label, e.g. "5h"
	UsedPercent      *float64 `json:"used_percent,omitempty"` // 0-100
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
	Unlimited        bool     `json:"unlimited,omitempty"`
	ResetsAt         int64    `json:"resets_at,omitempty"` // unix seconds
	// Absolute quota fields (CREDIT_LIMIT entries on newer plans carry
	// credit counts, not percentages).
	Unit        string   `json:"unit,omitempty"`         // e.g. "CREDIT"
	UsedAmount  *float64 `json:"used_amount,omitempty"`  // currentValue
	LimitAmount *float64 `json:"limit_amount,omitempty"` // usage (total quota)
	// Note carries unrecognized numeric fields verbatim when the percent
	// semantics of an entry could not be classified — the endpoint is
	// undocumented and plan generations drift; showing the raw numbers beats
	// showing nothing.
	Note string `json:"note,omitempty"`
}

// usageBalance is the normalized balance of one account.
type usageBalance struct {
	Currency  string   `json:"currency,omitempty"`
	Total     *float64 `json:"total,omitempty"`
	Available *float64 `json:"available,omitempty"`
	Granted   *float64 `json:"granted,omitempty"` // free/grant portion
	Voucher   *float64 `json:"voucher,omitempty"`
	Cash      *float64 `json:"cash,omitempty"`
}

// probeResult is the outcome of one probe against one account.
type probeResult struct {
	Probe   string        `json:"probe"`          // display name
	Type    string        `json:"type"`           // "balance" | "plan"
	Path    string        `json:"path,omitempty"` // resolved endpoint actually queried
	OK      bool          `json:"ok"`
	Status  int           `json:"status,omitempty"`
	Error   string        `json:"error,omitempty"`
	Plan    string        `json:"plan,omitempty"`
	Balance *usageBalance `json:"balance,omitempty"`
	Windows []usageWindow `json:"windows,omitempty"`
	Raw     string        `json:"raw,omitempty"` // truncated upstream body (always useful for unknown shapes)
}

const usageProbesMax = 8

// validateUsageProbes normalizes an account's usage_probes JSON payload and
// returns the canonical compact form for storage.
func validateUsageProbes(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" {
		return "[]", nil
	}
	var probes []usageProbe
	if err := json.Unmarshal(raw, &probes); err != nil {
		return "", fmt.Errorf("usage_probes must be an array of {type, path}")
	}
	if len(probes) > usageProbesMax {
		return "", fmt.Errorf("at most %d usage probes per account", usageProbesMax)
	}
	out := make([]usageProbe, 0, len(probes))
	for _, p := range probes {
		p.Path = strings.TrimSpace(p.Path)
		switch p.Type {
		case "balance", "plan":
		default:
			return "", fmt.Errorf(`usage probe type must be "balance" or "plan", got %q`, p.Type)
		}
		if p.Path == "" {
			return "", fmt.Errorf("usage probe path is required")
		}
		p.AuthStyle = strings.TrimSpace(p.AuthStyle)
		if p.AuthStyle == "" {
			p.AuthStyle = "bearer"
		}
		if p.AuthStyle != "bearer" && p.AuthStyle != "raw" {
			return "", fmt.Errorf(`usage probe auth_style must be "bearer" or "raw"`)
		}
		out = append(out, p)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// — admin endpoint ---------------------------------------------------------

// handleAccountUsage runs the account's declared probes against its key.
// POST /accounts/{id}/usage?refresh=1 (refresh bypasses the 60s TTL cache).
func (s *Server) handleAccountUsage(w http.ResponseWriter, req *http.Request) {
	id, ok := pathID(req)
	if !ok {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "invalid id")
		return
	}
	if req.URL.Query().Get("refresh") != "1" {
		if cached, ok := usageCache.get(id); ok {
			httpx.WriteEnvelope(w, req, cached)
			return
		}
	}
	account, err := s.St.Accounts.Get(req.Context(), id)
	if err != nil {
		mapStoreErr(w, req, err)
		return
	}
	probes, err := parseStoredUsageProbes(string(account.UsageProbes))
	if err != nil {
		httpx.WriteEnvelopeError(w, req, http.StatusBadRequest, 40002, "usage_probes: "+err.Error())
		return
	}
	// Accounts created before probes existed (or custom ones) carry an empty
	// config: fall back to built-in defaults matched by channel host, so known
	// vendors work out of the box. Explicit config always wins. `configured`
	// reports whether the USER configured probes (drives the UI hint), not
	// whether probes ran.
	userConfigured := len(probes) > 0
	if len(probes) == 0 {
		probes = s.defaultProbesForProvider(req.Context(), account.ProviderID)
	}
	out := accountUsageReport{
		AccountID:  account.ID,
		ProviderID: account.ProviderID,
		Label:      account.Label,
		KeyMask:    maskForDisplay(account.APIKey),
		QueriedAt:  time.Now().Unix(),
		Configured: userConfigured,
		Results:    []probeResult{}, // never nil: JSON null breaks the UI's .length
	}
	if len(probes) > 0 {
		// Origin of the provider's first enabled channel resolves relative
		// probe paths.
		origin := s.providerOrigin(req.Context(), account.ProviderID)
		for _, p := range probes {
			out.Results = append(out.Results, runUsageProbe(req.Context(), p, origin, account.APIKey))
		}
	}
	usageCache.set(id, out)
	httpx.WriteEnvelope(w, req, out)
}

type accountUsageReport struct {
	AccountID  int64         `json:"account_id"`
	ProviderID int64         `json:"provider_id"`
	Label      string        `json:"label"`
	KeyMask    string        `json:"key_mask"`
	QueriedAt  int64         `json:"queried_at"` // unix seconds
	Configured bool          `json:"configured"`
	Results    []probeResult `json:"results"`
}

// providerOrigin returns scheme://host of the provider's first enabled
// channel, used to resolve origin-absolute probe paths (avoids /v1 prefixes
// in base_url leaking into probe URLs).
func (s *Server) providerOrigin(ctx context.Context, providerID int64) string {
	channels, err := s.St.Channels.List(ctx)
	if err != nil {
		return ""
	}
	for _, ch := range channels {
		if ch.ProviderID == providerID && ch.Enabled && ch.BaseURL != "" {
			if u, err := url.Parse(ch.BaseURL); err == nil && u.Scheme != "" && u.Host != "" {
				return u.Scheme + "://" + u.Host
			}
		}
	}
	return ""
}

// knownProbeDefaults maps channel base_url hosts to the vendor's quota/balance
// endpoint. Paths are origin-relative (resolved against the channel origin),
// which keeps both protocol channels (e.g. api.deepseek.com and
// api.deepseek.com/anthropic) on the same probe endpoint.
var knownProbeDefaults = []struct {
	hostContains string
	probe        usageProbe
}{
	{"bigmodel.cn", usageProbe{Type: "plan", Name: "Coding Plan", AuthStyle: "bearer", Path: "/api/monitor/usage/quota/limit"}},
	{"z.ai", usageProbe{Type: "plan", Name: "Coding Plan", AuthStyle: "bearer", Path: "/api/monitor/usage/quota/limit"}},
	{"deepseek.com", usageProbe{Type: "balance", Name: "Balance", AuthStyle: "bearer", Path: "/user/balance"}},
	{"moonshot.cn", usageProbe{Type: "balance", Name: "Balance", AuthStyle: "bearer", Path: "/v1/users/me/balance"}},
	{"moonshot.ai", usageProbe{Type: "balance", Name: "Balance", AuthStyle: "bearer", Path: "/v1/users/me/balance"}},
	{"anthropic.com", usageProbe{Type: "plan", Name: "Plan usage", AuthStyle: "bearer", Path: "/v1/organizations/usage_report/messages"}},
}

// defaultProbesForProvider derives built-in probes from the hosts of the
// provider's enabled channels when nothing is explicitly configured.
func (s *Server) defaultProbesForProvider(ctx context.Context, providerID int64) []usageProbe {
	channels, err := s.St.Channels.List(ctx)
	if err != nil {
		return nil
	}
	var out []usageProbe
	seen := map[string]bool{}
	for _, ch := range channels {
		if ch.ProviderID != providerID || !ch.Enabled || ch.BaseURL == "" {
			continue
		}
		u, err := url.Parse(ch.BaseURL)
		if err != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		for _, kd := range knownProbeDefaults {
			if !strings.Contains(host, kd.hostContains) || seen[kd.probe.Type+kd.probe.Path] {
				continue
			}
			seen[kd.probe.Type+kd.probe.Path] = true
			out = append(out, kd.probe)
		}
	}
	return out
}

// runUsageProbe executes one probe: GET the resolved URL, classify, parse.
// Bearer-auth probes retry once with the raw key on 401/403 — vendors are
// inconsistent about the Authorization form on their quota endpoints
// (e.g. bigmodel.cn console endpoints accept a bare key).
func runUsageProbe(ctx context.Context, p usageProbe, origin, secret string) probeResult {
	res := probeResult{Probe: p.Name, Type: p.Type}
	if res.Probe == "" {
		res.Probe = p.Type
	}
	target, err := resolveProbeURL(p.Path, origin)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Path = target

	do := func(authStyle string) (int, []byte, error) {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return 0, nil, err
		}
		hreq.Header.Set("Accept", "application/json")
		if authStyle == "raw" {
			hreq.Header.Set("Authorization", secret)
		} else {
			hreq.Header.Set("Authorization", "Bearer "+secret)
		}
		hresp, err := usageHTTPClient.Do(hreq)
		if err != nil {
			return 0, nil, err
		}
		defer hresp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(hresp.Body, 64<<10))
		return hresp.StatusCode, body, nil
	}

	status, body, err := do(p.AuthStyle)
	if err != nil {
		res.Error = "request failed: " + err.Error()
		return res
	}
	if (status == http.StatusUnauthorized || status == http.StatusForbidden) && p.AuthStyle != "raw" {
		if status2, body2, err2 := do("raw"); err2 == nil {
			status, body = status2, body2
		}
	}
	res.Status = status
	if len(body) > 4096 {
		res.Raw = string(body[:4096]) + "…"
	} else {
		res.Raw = string(body)
	}
	if status < 200 || status >= 300 {
		res.Error = fmt.Sprintf("HTTP %d", status)
		return res
	}
	switch p.Type {
	case "plan":
		windows, plan := parsePlanBody(body)
		res.Windows, res.Plan = windows, plan
		res.OK = len(windows) > 0
		if !res.OK {
			res.Error = "no quota windows found in response"
		}
	default:
		bal := parseBalanceBody(body)
		res.Balance = bal
		res.OK = bal != nil
		if !res.OK {
			res.Error = "no balance fields found in response"
		}
	}
	return res
}

// resolveProbeURL accepts an absolute URL or an origin-absolute path.
func resolveProbeURL(path, origin string) (string, error) {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path, nil
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if origin == "" {
		return "", fmt.Errorf("probe path %q is relative but the provider has no enabled channel to derive an origin from", path)
	}
	return origin + path, nil
}

var usageHTTPClient = &http.Client{Timeout: 15 * time.Second}

// — TTL cache (manual refresh + short TTL so clicks never hammer upstreams) -

const usageCacheTTL = 60 * time.Second

var usageCache = newUsageCacheStore()

type usageCacheStore struct {
	mu sync.Mutex
	m  map[int64]usageCacheEntry
}
type usageCacheEntry struct {
	at  time.Time
	val accountUsageReport
}

func newUsageCacheStore() *usageCacheStore {
	return &usageCacheStore{m: map[int64]usageCacheEntry{}}
}

func (c *usageCacheStore) get(accountID int64) (accountUsageReport, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[accountID]
	if !ok || time.Since(e.at) > usageCacheTTL {
		return accountUsageReport{}, false
	}
	return e.val, true
}

func (c *usageCacheStore) set(accountID int64, val accountUsageReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[accountID] = usageCacheEntry{at: time.Now(), val: val}
}

// parseStoredUsageProbes parses the stored JSON; empty/'[]' → nil.
func parseStoredUsageProbes(raw string) ([]usageProbe, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var probes []usageProbe
	if err := json.Unmarshal([]byte(raw), &probes); err != nil {
		return nil, err
	}
	return probes, nil
}

// — response parsers -------------------------------------------------------

// parseBalanceBody recognizes the documented balance shapes (DeepSeek
// balance_infos, Kimi data.{available,voucher,cash}_balance) and falls back
// to a generic *_balance scan.
func parseBalanceBody(body []byte) *usageBalance {
	var wire struct {
		BalanceInfos []struct {
			Currency       string `json:"currency"`
			TotalBalance   string `json:"total_balance"`
			GrantedBalance string `json:"granted_balance"`
			TouchedBalance string `json:"touched_balance"`
		} `json:"balance_infos"`
		Data struct {
			AvailableBalance json.RawMessage `json:"available_balance"`
			VoucherBalance   json.RawMessage `json:"voucher_balance"`
			CashBalance      json.RawMessage `json:"cash_balance"`
			TotalBalance     json.RawMessage `json:"total_balance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil
	}
	// DeepSeek: balance_infos[] with string amounts.
	if len(wire.BalanceInfos) > 0 {
		info := wire.BalanceInfos[0]
		bal := &usageBalance{Currency: info.Currency}
		bal.Total = flexFloat(info.TotalBalance, nil)
		bal.Granted = flexFloat(info.GrantedBalance, nil)
		if bal.Total != nil || bal.Granted != nil {
			bal.Available = bal.Total
			return bal
		}
	}
	// Kimi (Moonshot): data.{available,voucher,cash}_balance as numbers
	// (some vendors emit strings, hence the RawMessage detour).
	d := wire.Data
	avail, voucher, cash, total := rawToFloat(d.AvailableBalance), rawToFloat(d.VoucherBalance), rawToFloat(d.CashBalance), rawToFloat(d.TotalBalance)
	if avail != nil || voucher != nil || cash != nil || total != nil {
		bal := &usageBalance{Available: avail, Voucher: voucher, Cash: cash, Total: total}
		if bal.Total == nil {
			bal.Total = avail // Kimi: available already includes voucher+cash
		}
		return bal
	}
	// Generic fallback: first recognizable *_balance field.
	return scanGenericBalance(body)
}

func scanGenericBalance(body []byte) *usageBalance {
	var flat map[string]any
	if err := json.Unmarshal(body, &flat); err != nil {
		return nil
	}
	if d, ok := flat["data"].(map[string]any); ok {
		for k, v := range d {
			flat[k] = v // merge data.* up one level for the scan
		}
	}
	bal := &usageBalance{}
	pick := func(name string) *float64 {
		for k, v := range flat {
			if strings.HasSuffix(k, "balance") && strings.Contains(k, name) {
				if f, ok := toFloat(v); ok {
					return &f
				}
			}
		}
		return nil
	}
	bal.Total = pick("total")
	bal.Available = pick("available")
	bal.Granted = pick("granted")
	bal.Voucher = pick("voucher")
	bal.Cash = pick("cash")
	if bal.Total == nil && bal.Available == nil && bal.Granted == nil {
		return nil
	}
	return bal
}

// parsePlanBody recognizes the GLM/Z.ai coding-plan quota shape:
// data.limits[] with typed windows (TOKENS_LIMIT / TIME_LIMIT / CREDIT_LIMIT —
// field conventions drift between plan generations), per-window percentages
// (value / original_value / remaining, numbers or numeric strings), and
// epoch-ms reset times. Percent semantics vary: an explicit `remaining` wins;
// otherwise value/original_value is treated as used. Entries that cannot be
// classified keep their numeric fields in Note instead of being dropped.
func parsePlanBody(body []byte) ([]usageWindow, string) {
	var wire struct {
		Data struct {
			PlanName string          `json:"planName"`
			Plan     string          `json:"plan"`
			Limits   json.RawMessage `json:"limits"`
		} `json:"data"`
		Plan        string          `json:"plan"`
		PlanType    string          `json:"plan_type"`
		PackageName string          `json:"packageName"`
		Limits      json.RawMessage `json:"limits"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, ""
	}
	limits := wire.Data.Limits
	if len(limits) == 0 {
		limits = wire.Limits
	}
	plan := wire.Data.PlanName
	if plan == "" {
		plan = firstNonEmpty(wire.Data.Plan, wire.Plan, wire.PlanType, wire.PackageName)
	}
	if len(limits) == 0 {
		return nil, plan
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(limits, &entries); err != nil {
		return nil, plan
	}
	var out []usageWindow
	now := time.Now().Unix()
	for _, re := range entries {
		var m map[string]any
		if err := json.Unmarshal(re, &m); err != nil {
			continue
		}
		name := "limit"
		if s, ok := m["type"].(string); ok && s != "" {
			name = s
		}
		w := usageWindow{
			Name:      name,
			Window:    windowLabel(mustJSON(m["window"])),
			Unlimited: boolOf(m["unlimited"]),
			ResetsAt:  epochSeconds(m["nextResetTime"]),
		}
		// Some entries (CREDIT_LIMIT on newer plans) carry no window field:
		// infer the duration from how far out the reset time is.
		if w.Window == "" && w.ResetsAt > 0 {
			w.Window = inferWindowLabel(w.ResetsAt, now)
		}
		w.UsedPercent, w.RemainingPercent, w.UsedAmount, w.LimitAmount, w.Unit = classifyLimit(m)
		if w.UsedPercent == nil && w.RemainingPercent == nil {
			w.Note = numericSummary(m)
		}
		out = append(out, w)
	}
	return out, plan
}

// inferWindowLabel guesses the window duration from the reset distance:
// sub-day rolls (5-hour), multi-day (weekly), ~month (monthly).
func inferWindowLabel(resetsAt, now int64) string {
	d := resetsAt - now
	switch {
	case d <= 0:
		return ""
	case d <= 6*3600:
		return "5h"
	case d <= 8*24*3600:
		return "7d"
	case d <= 32*24*3600:
		return "30d"
	default:
		return ""
	}
}

// classifyLimit maps one limit entry to percent + absolute quota fields.
// Field conventions drift between plan generations:
//   - TOKENS_LIMIT (older): percentage via `remaining` (0-100) or
//     `value`/`original_value`.
//   - CREDIT_LIMIT (newer, credit-based): absolute `usage` (total),
//     `currentValue` (used), `remaining` (credit count, NOT a percent), and
//     an explicit `percentage` plus `unit`.
//
// Resolution order: explicit `percentage` > ratio from absolute amounts >
// `remaining` as a percent (only when plausibly 0-100) > legacy percent
// fields. Numbers and numeric strings pass.
func classifyLimit(m map[string]any) (used, remaining, usedAmt, limitAmt *float64, unit string) {
	if s, ok := m["unit"].(string); ok {
		unit = s
	}
	num := func(keys ...string) *float64 {
		for _, k := range keys {
			if v, ok := toFloat(m[k]); ok {
				return &v
			}
		}
		return nil
	}
	inPct := func(v *float64) *float64 {
		if v == nil || *v < 0 || *v > 100 {
			return nil
		}
		return v
	}
	pct := inPct(num("percentage"))
	rem := num("remaining")
	remPct := inPct(rem)
	usedAmt = num("currentValue", "used")
	limitAmt = num("usage", "total", "limit")

	switch {
	case pct != nil:
		used = pct
	case limitAmt != nil && *limitAmt > 0 && usedAmt != nil:
		u := *usedAmt / *limitAmt * 100
		u = math.Max(0, math.Min(100, u))
		used = &u
	case remPct != nil:
		u := 100 - *remPct
		used = &u
		remaining = remPct
	default:
		used = inPct(num("original_value", "value", "used_percent", "percent", "ratio"))
	}
	return used, remaining, usedAmt, limitAmt, unit
}

// numericSummary renders the recognizable numeric fields of an unclassified
// limit entry as "k=v" pairs for display.
func numericSummary(m map[string]any) string {
	var parts []string
	for _, k := range []string{"value", "original_value", "remaining", "used", "currentValue", "total", "limit", "usage", "credit", "credits", "percentage"} {
		if v, ok := toFloat(m[k]); ok {
			parts = append(parts, k+"="+trimNum(v))
		}
	}
	return strings.Join(parts, " ")
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// mustJSON marshals v, returning null on failure (best-effort display data).
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// epochSeconds accepts epoch seconds or milliseconds.
func epochSeconds(v any) int64 {
	f, ok := toFloat(v)
	if !ok || f <= 0 {
		return 0
	}
	if f > 1e12 { // epoch ms
		f /= 1000
	}
	return int64(f)
}

// windowLabel renders the window duration ("5h", "7d", ...) from the loose
// wire forms vendors use: "5h" | 5 | {"unit":"hour","number":5} | {"unit":"h","value":5}.
func windowLabel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return trimNum(n)
	}
	var obj struct {
		Unit   string   `json:"unit"`
		Number *float64 `json:"number"`
		Value  *float64 `json:"value"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		v := obj.Number
		if v == nil {
			v = obj.Value
		}
		if v == nil {
			return ""
		}
		unit := obj.Unit
		switch strings.ToLower(unit) {
		case "minute", "minutes", "min":
			unit = "m"
		case "hour", "hours", "h":
			unit = "h"
		case "day", "days", "d":
			unit = "d"
		case "week", "weeks", "w":
			unit = "w"
		case "month", "months":
			unit = "mo"
		}
		return trimNum(*v) + unit
	}
	return ""
}

func trimNum(n float64) string {
	if n == float64(int64(n)) {
		return fmt.Sprintf("%d", int64(n))
	}
	return fmt.Sprintf("%g", n)
}

func flexFloat(s string, fb *float64) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return fb
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return fb
	}
	return &f
}

// rawToFloat accepts a JSON number or numeric string.
func rawToFloat(raw json.RawMessage) *float64 {
	if len(raw) == 0 {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return &f
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return flexFloat(s, nil)
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		if f := flexFloat(n, nil); f != nil {
			return *f, true
		}
	}
	return 0, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
