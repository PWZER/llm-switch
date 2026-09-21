package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PWZER/llm-switch/internal/store"
)

func TestDefaultProbesForProvider(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.New(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := context.Background()
	s := &Server{St: st}

	pid, err := st.Providers.Create(ctx, "mixed", nil)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	// One channel per protocol (the schema allows at most one endpoint of each
	// protocol per provider); probe derivation only matches base_url hosts.
	mk := func(protocol, base string) {
		t.Helper()
		if _, err := st.Channels.Create(ctx, &store.Channel{
			ProviderID: pid, Protocol: protocol,
			BaseURL: base, ChatPath: "/chat/completions", AuthStyle: "bearer",
			ExtraHeaders: "{}", Enabled: true,
		}); err != nil {
			t.Fatalf("create channel: %v", err)
		}
	}
	mk("openai", "https://open.bigmodel.cn/api/anthropic")
	mk("anthropic", "https://api.deepseek.com/anthropic")

	probes := s.defaultProbesForProvider(ctx, pid)
	if len(probes) != 2 {
		t.Fatalf("want 2 default probes, got %+v", probes)
	}
	if probes[0].Type != "plan" || probes[0].Path != "/api/monitor/usage/quota/limit" {
		t.Fatalf("zhipu probe wrong: %+v", probes[0])
	}
	if probes[1].Type != "balance" || probes[1].Path != "/user/balance" {
		t.Fatalf("deepseek probe wrong: %+v", probes[1])
	}

	// Unknown vendor host: no defaults.
	pid2, err := st.Providers.Create(ctx, "custom", nil)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid2, Protocol: "openai",
		BaseURL: "http://127.0.0.1:9091", ChatPath: "/chat/completions", AuthStyle: "bearer",
		ExtraHeaders: "{}", Enabled: true,
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if probes := s.defaultProbesForProvider(ctx, pid2); len(probes) != 0 {
		t.Fatalf("unknown host must yield no defaults, got %+v", probes)
	}
}

func TestValidateUsageProbes(t *testing.T) {
	ok, err := validateUsageProbes([]byte(`[
		{"type":"balance","path":"https://api.deepseek.com/user/balance","name":"Balance"},
		{"type":"plan","path":"/api/monitor/usage/quota/limit","auth_style":"raw"}
	]`))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.Contains(ok, `"auth_style":"raw"`) || !strings.Contains(ok, `"auth_style":"bearer"`) {
		t.Fatalf("default auth_style not applied: %s", ok)
	}

	if _, err := validateUsageProbes([]byte(`[{"type":"quota","path":"/x"}]`)); err == nil {
		t.Fatal("bad type must fail")
	}
	if _, err := validateUsageProbes([]byte(`[{"type":"plan"}]`)); err == nil {
		t.Fatal("missing path must fail")
	}
	if _, err := validateUsageProbes([]byte(`[{"type":"plan","path":"/x","auth_style":"cookie"}]`)); err == nil {
		t.Fatal("bad auth_style must fail")
	}
	if _, err := validateUsageProbes(nil); err != nil {
		t.Fatalf("nil must be valid: %v", err)
	}
}

func TestParseBalanceDeepSeek(t *testing.T) {
	body := []byte(`{"is_available":true,"balance_infos":[
		{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","touched_balance":"100.00"}]}`)
	bal := parseBalanceBody(body)
	if bal == nil || bal.Currency != "CNY" || bal.Total == nil || *bal.Total != 110 ||
		bal.Granted == nil || *bal.Granted != 10 || bal.Available == nil || *bal.Available != 110 {
		t.Fatalf("bad deepseek balance: %+v", bal)
	}
}

func TestParseBalanceKimi(t *testing.T) {
	body := []byte(`{"code":0,"data":{"available_balance":49.59,"voucher_balance":46.59,"cash_balance":3.0},"scode":"0x0","status":true}`)
	bal := parseBalanceBody(body)
	if bal == nil || bal.Available == nil || *bal.Available != 49.59 ||
		bal.Voucher == nil || *bal.Voucher != 46.59 || bal.Cash == nil || *bal.Cash != 3.0 {
		t.Fatalf("bad kimi balance: %+v", bal)
	}
	if bal.Total == nil || *bal.Total != 49.59 {
		t.Fatalf("kimi total should default to available: %+v", bal)
	}
}

func TestParseBalanceGeneric(t *testing.T) {
	body := []byte(`{"ok":true,"data":{"total_balance":"5.50"}}`)
	bal := parseBalanceBody(body)
	if bal == nil || bal.Total == nil || *bal.Total != 5.5 {
		t.Fatalf("bad generic balance: %+v", bal)
	}
	if parseBalanceBody([]byte(`{"nothing":"here"}`)) != nil {
		t.Fatal("unparseable shape must return nil")
	}
}

func TestParsePlanGLM(t *testing.T) {
	body := []byte(`{
		"data": {
			"planName": "GLM Coding Pro",
			"limits": [
				{"type":"TOKENS_LIMIT","window":{"unit":"hour","number":5},
				 "remaining":71,"unlimited":false,"nextResetTime":1789000000000},
				{"type":"TOKENS_LIMIT","window":{"unit":"day","number":7},"value":20.5},
				{"type":"TIME_LIMIT","window":{"unit":"day","number":30},"unlimited":false,"value":60},
				{"type":"CREDIT_LIMIT","original_value":42,"remaining":58},
				{"type":"CREDIT_LIMIT","value":"33.3","nextResetTime":1789086390000},
				{"type":"MYSTERY","credit":500,"total":1000},
				{"type":"CREDIT_LIMIT","window":{"unit":"hour","number":5},
				 "usage":6000,"currentValue":1234,"remaining":4766,"percentage":20.57,
				 "unit":"CREDIT","nextResetTime":1789086390000}
			]
		}
	}`)
	windows, plan := parsePlanBody(body)
	if plan != "GLM Coding Pro" {
		t.Fatalf("plan label: %q", plan)
	}
	if len(windows) != 7 {
		t.Fatalf("want 7 windows, got %d: %+v", len(windows), windows)
	}
	w0 := windows[0]
	if w0.Window != "5h" || w0.UsedPercent == nil || *w0.UsedPercent != 29 ||
		w0.RemainingPercent == nil || *w0.RemainingPercent != 71 || w0.ResetsAt != 1789000000 {
		t.Fatalf("window 0 wrong: %+v", w0)
	}
	if windows[1].Window != "7d" || windows[1].UsedPercent == nil || *windows[1].UsedPercent != 20.5 {
		t.Fatalf("window 1 wrong: %+v", windows[1])
	}
	if windows[2].Window != "30d" || windows[2].UsedPercent == nil || *windows[2].UsedPercent != 60 {
		t.Fatalf("window 2 wrong: %+v", windows[2])
	}
	// remaining present -> used derived, both reported
	if windows[3].UsedPercent == nil || *windows[3].UsedPercent != 42 {
		t.Fatalf("window 3 wrong: %+v", windows[3])
	}
	// CREDIT_LIMIT with string percent + ms reset time
	w4 := windows[4]
	if w4.UsedPercent == nil || *w4.UsedPercent != 33.3 || w4.ResetsAt != 1789086390 {
		t.Fatalf("window 4 wrong: %+v", w4)
	}
	// unclassifiable entry keeps its numbers in Note
	if windows[5].UsedPercent != nil || !strings.Contains(windows[5].Note, "credit=500") ||
		!strings.Contains(windows[5].Note, "total=1000") {
		t.Fatalf("window 5 wrong: %+v", windows[5])
	}
	// credit-based CREDIT_LIMIT: ratio from absolute amounts + amount fields.
	w6 := windows[6]
	if w6.UsedPercent == nil || *w6.UsedPercent < 20.56 || *w6.UsedPercent > 20.58 {
		t.Fatalf("window 6 percent wrong: %+v", w6)
	}
	if w6.UsedAmount == nil || *w6.UsedAmount != 1234 ||
		w6.LimitAmount == nil || *w6.LimitAmount != 6000 || w6.Unit != "CREDIT" {
		t.Fatalf("window 6 amounts wrong: %+v", w6)
	}
	// remaining=4766 must NOT be misread as a percent.
	if w6.RemainingPercent != nil {
		t.Fatalf("absolute remaining misread as percent: %+v", w6)
	}
}

func TestResolveProbeURL(t *testing.T) {
	if u, err := resolveProbeURL("https://api.deepseek.com/user/balance", ""); err != nil || u == "" {
		t.Fatalf("absolute: %v %q", err, u)
	}
	if u, err := resolveProbeURL("/user/balance", "https://api.deepseek.com"); err != nil || u != "https://api.deepseek.com/user/balance" {
		t.Fatalf("origin join: %v %q", err, u)
	}
	if _, err := resolveProbeURL("/user/balance", ""); err == nil {
		t.Fatal("relative without origin must fail")
	}
}

func TestRunUsageProbeLive(t *testing.T) {
	gotAuth := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth[r.URL.Path] = append(gotAuth[r.URL.Path], r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/user/balance":
			w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"88.00"}]}`))
		case "/plan":
			// Bearer rejected, raw key accepted: exercises the 401 retry.
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"bad key"}`))
				return
			}
			w.Write([]byte(`{"data":{"planName":"P","limits":[{"type":"TOKENS_LIMIT","remaining":50}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probes := []usageProbe{
		{Type: "balance", Path: srv.URL + "/user/balance", Name: "Balance"},
		{Type: "plan", Path: srv.URL + "/plan", Name: "Plan"},
	}
	res := make([]probeResult, 0, len(probes))
	for _, p := range probes {
		res = append(res, runUsageProbe(context.Background(), p, "", "sk-upstream"))
	}
	if len(res) != 2 {
		t.Fatalf("want 2 probe results: %+v", res)
	}
	balRes := res[0]
	if !balRes.OK || balRes.Balance == nil || balRes.Balance.Total == nil || *balRes.Balance.Total != 88 {
		t.Fatalf("balance probe: %+v", balRes)
	}
	if balRes.Path != srv.URL+"/user/balance" {
		t.Fatalf("resolved path missing: %+v", balRes)
	}
	planRes := res[1]
	if !planRes.OK || len(planRes.Windows) != 1 {
		t.Fatalf("plan probe should succeed after raw-key retry: %+v", planRes)
	}
	if len(gotAuth["/plan"]) != 2 || gotAuth["/plan"][0] != "Bearer sk-upstream" || gotAuth["/plan"][1] != "sk-upstream" {
		t.Fatalf("expected Bearer then raw retry, got %v", gotAuth["/plan"])
	}
}
