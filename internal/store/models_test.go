package store

import (
	"context"
	"errors"
	"testing"
)

func i64(v int64) *int64 { return &v }

// newProvider creates a provider for model rows to hang on
// (models.provider_id is a hard FK).
func newProvider(t *testing.T, st *Store, name string) int64 {
	t.Helper()
	pid, err := st.Providers.Create(context.Background(), name, nil)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return pid
}

func TestEnsureModelFillsAndPreserves(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pid := newProvider(t, st, "vendor")

	// Fresh insert carries the upstream limits; empty alias stays identity.
	inserted, err := st.Models.EnsureModel(ctx, &Model{
		ID: "m1", ProviderID: pid, ContextWindow: i64(131072),
	})
	if err != nil || !inserted {
		t.Fatalf("insert: inserted=%v err=%v", inserted, err)
	}
	m, err := st.Models.Get(ctx, pid, "m1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.ContextWindow == nil || *m.ContextWindow != 131072 || m.ProviderID != pid || !m.Enabled {
		t.Fatalf("unexpected row: %+v", m)
	}
	if m.UpstreamModel != "m1" || m.Upstream() != "m1" {
		t.Fatalf("identity alias expected, got %+v", m)
	}

	// A second identical refresh is a no-op: not counted, updated_at untouched.
	before := m.UpdatedAt
	inserted, err = st.Models.EnsureModel(ctx, &Model{
		ID: "m1", ProviderID: pid, ContextWindow: i64(131072),
	})
	if err != nil || inserted {
		t.Fatalf("noop refresh: inserted=%v err=%v", inserted, err)
	}
	if m, _ = st.Models.Get(ctx, pid, "m1"); m.UpdatedAt != before {
		t.Fatalf("updated_at bumped on no-op: %d -> %d", before, m.UpdatedAt)
	}

	// Existing row with NULL limits gets backfilled by a later refresh.
	if _, err := st.Models.EnsureModel(ctx, &Model{ID: "m2", ProviderID: pid}); err != nil {
		t.Fatalf("insert m2: %v", err)
	}
	inserted, err = st.Models.EnsureModel(ctx, &Model{
		ID: "m2", ProviderID: pid, ContextWindow: i64(8192), MaxOutputTokens: i64(4096),
	})
	if err != nil || !inserted {
		t.Fatalf("backfill: inserted=%v err=%v", inserted, err)
	}
	m, _ = st.Models.Get(ctx, pid, "m2")
	if m.ContextWindow == nil || *m.ContextWindow != 8192 ||
		m.MaxOutputTokens == nil || *m.MaxOutputTokens != 4096 {
		t.Fatalf("limits not backfilled: %+v", m)
	}

	// Manually-set limits, alias and enabled survive refreshes.
	m.ContextWindow = i64(999999)
	m.Enabled = false
	m.UpstreamModel = "renamed-upstream"
	if err := st.Models.Update(ctx, &m); err != nil {
		t.Fatalf("manual edit: %v", err)
	}
	inserted, err = st.Models.EnsureModel(ctx, &Model{
		ID: "m2", ProviderID: pid, ContextWindow: i64(8192), MaxOutputTokens: i64(4096),
	})
	if err != nil || inserted {
		t.Fatalf("clobbering refresh: inserted=%v err=%v", inserted, err)
	}
	m, _ = st.Models.Get(ctx, pid, "m2")
	if *m.ContextWindow != 999999 || m.Enabled || m.UpstreamModel != "renamed-upstream" {
		t.Fatalf("manual values clobbered: %+v", m)
	}
}

func TestModelKeyPerProvider(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pid1 := newProvider(t, st, "v1")
	pid2 := newProvider(t, st, "v2")

	// The same name coexists across providers, each with its own alias.
	if _, err := st.Models.EnsureModel(ctx, &Model{ID: "m", ProviderID: pid1}); err != nil {
		t.Fatalf("ensure p1: %v", err)
	}
	if _, err := st.Models.EnsureModel(ctx, &Model{ID: "m", ProviderID: pid2, UpstreamModel: "upstream-m"}); err != nil {
		t.Fatalf("ensure p2: %v", err)
	}
	all, err := st.Models.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(all), all)
	}

	// Update touches only its own key: disabling provider 1's row leaves
	// provider 2's row enabled.
	row, err := st.Models.Get(ctx, pid1, "m")
	if err != nil {
		t.Fatalf("get p1: %v", err)
	}
	row.Enabled = false
	if err := st.Models.Update(ctx, &row); err != nil {
		t.Fatalf("update p1: %v", err)
	}
	if m, err := st.Models.Get(ctx, pid2, "m"); err != nil || !m.Enabled {
		t.Fatalf("row (p2,m) must stay enabled: %+v err=%v", m, err)
	}

	// Delete removes only its own key.
	if err := st.Models.Delete(ctx, pid2, "m"); err != nil {
		t.Fatalf("delete p2: %v", err)
	}
	if _, err := st.Models.Get(ctx, pid2, "m"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("p2 row must be gone, got err=%v", err)
	}
	if _, err := st.Models.Get(ctx, pid1, "m"); err != nil {
		t.Fatalf("p1 row must survive: %v", err)
	}

	// FirstEnabled skips disabled rows.
	if _, err := st.Models.FirstEnabled(ctx, pid1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled-only provider must yield ErrNotFound, got %v", err)
	}
}

func TestDeleteProviderCleansModelRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	doomed := newProvider(t, st, "doomed")
	kept := newProvider(t, st, "kept")
	for _, m := range []*Model{
		{ID: "p-only", ProviderID: doomed},
		{ID: "shared", ProviderID: doomed},
		{ID: "shared", ProviderID: kept},
	} {
		if _, err := st.Models.EnsureModel(ctx, m); err != nil {
			t.Fatalf("ensure %s: %v", m.ID, err)
		}
	}

	if err := st.DeleteProvider(ctx, doomed); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	all, err := st.Models.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].ProviderID != kept {
		t.Fatalf("provider rows must die with the provider (FK cascade), got %+v", all)
	}
}
