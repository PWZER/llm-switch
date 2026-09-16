package store

import (
	"context"
	"testing"
	"time"
)

func TestTimeseriesGroupsByModelAndSumsCacheRead(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	day1 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	day2 := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC).UnixMilli()
	rows := []RequestLog{
		{TS: day1, Model: "glm-4.6", Success: true, PromptTokens: 100, CompletionTokens: 10, CacheReadTokens: 40},
		{TS: day1, Model: "glm-4.6", Success: true, PromptTokens: 200, CompletionTokens: 20, CacheReadTokens: 60},
		{TS: day1, Model: "kimi-k3", Success: true, PromptTokens: 50, CompletionTokens: 5},
		{TS: day2, Model: "glm-4.6", Success: false, PromptTokens: 300, CompletionTokens: 30, CacheReadTokens: 150},
	}
	if err := st.Logs.InsertBatch(ctx, rows); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := st.Logs.Timeseries(ctx, day1-3600_000, day2+3600_000, "model")
	if err != nil {
		t.Fatalf("timeseries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 group rows, got %d: %+v", len(got), got)
	}
	// Ordered by bucket DESC, requests DESC.
	want := []GroupRow{
		{Bucket: "2026-09-16", Group: "glm-4.6", Requests: 1, Successes: 0, PromptTokens: 300, CompletionTokens: 30, CacheReadTokens: 150},
		{Bucket: "2026-09-15", Group: "glm-4.6", Requests: 2, Successes: 2, PromptTokens: 300, CompletionTokens: 30, CacheReadTokens: 100},
		{Bucket: "2026-09-15", Group: "kimi-k3", Requests: 1, Successes: 1, PromptTokens: 50, CompletionTokens: 5, CacheReadTokens: 0},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d: want %+v, got %+v", i, w, got[i])
		}
	}
}
