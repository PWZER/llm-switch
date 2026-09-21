package gateway_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PWZER/llm-switch/internal/payload"
	"github.com/PWZER/llm-switch/internal/store"
)

// latestLogRow polls for the newest log row of a model (stats writes are
// batched) and returns it.
func latestLogRow(t *testing.T, h *harness, model string) store.RequestLog {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, total, err := h.st.Logs.QueryLogs(context.Background(), store.LogFilter{Model: model, Page: 1, PageSize: 1})
		must(t, err)
		if total > 0 && len(logs) > 0 {
			return logs[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no log row for model %q", model)
	return store.RequestLog{}
}

// latestPayloadRow polls for the newest log row of a model that carries a
// payload_path (stats writes are batched; a plain row may flush first).
func latestPayloadRow(t *testing.T, h *harness, model string) store.RequestLog {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, _, err := h.st.Logs.QueryLogs(context.Background(), store.LogFilter{Model: model, Page: 1, PageSize: 5})
		must(t, err)
		for _, l := range logs {
			if l.PayloadPath != "" {
				return l
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no payload log row for model %q", model)
	return store.RequestLog{}
}

// readPayload polls until the recorded payload for the row is readable (the
// writer is async too).
func readPayload(t *testing.T, h *harness, row store.RequestLog) *payload.Record {
	t.Helper()
	if row.PayloadPath == "" {
		t.Fatalf("log row has no payload_path: %+v", row)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec, err := payload.Read(h.payloadDir, row.PayloadPath); err == nil {
			return rec
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("payload never materialized at %q", row.PayloadPath)
	return nil
}

func TestPayloadRecordingGlobalToggle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(t, h.st.Settings.Set(ctx, "log_bodies", "1"))
	h.rebuild()

	resp, raw := h.post("/v1/chat/completions", `{"model":"test-model","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}

	row := latestPayloadRow(t, h, "test-model")
	rec := readPayload(t, h, row)

	// Client request: headers redacted, body verbatim.
	if string(rec.ClientReq.Body) != `{"model":"test-model","stream":false}` {
		t.Fatalf("client body wrong: %q", rec.ClientReq.Body)
	}
	auth := rec.ClientReq.Headers["Authorization"]
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") || strings.Contains(auth[0], h.key) {
		t.Fatalf("client authorization not redacted: %v", auth)
	}

	// Upstream request: the rewritten model and the redacted channel secret —
	// while the fake upstream saw the real one.
	if rec.UpstreamReq == nil {
		t.Fatal("upstream request segment missing")
	}
	if got := h.openai.LastAuthorization(); got != "Bearer upstream-secret" {
		t.Fatalf("upstream must get the real key, got %q", got)
	}
	upAuth := rec.UpstreamReq.Headers["Authorization"]
	if len(upAuth) != 1 || strings.Contains(upAuth[0], "upstream-secret") || !strings.HasPrefix(upAuth[0], "Bearer ") {
		t.Fatalf("upstream authorization not redacted: %v", upAuth)
	}
	if string(rec.UpstreamReq.Body) != string(h.openai.LastBody()) {
		t.Fatalf("upstream body mismatch:\nrec:  %s\nfake: %s", rec.UpstreamReq.Body, h.openai.LastBody())
	}

	// Upstream response == the bytes the client received (passthrough).
	if rec.RespStatus != 200 {
		t.Fatalf("response status wrong: %d", rec.RespStatus)
	}
	if string(rec.RespBody) != string(raw) {
		t.Fatalf("response body mismatch:\nrec:    %s\nclient: %s", rec.RespBody, raw)
	}
}

func TestPayloadRecordingHeaderTrigger(t *testing.T) {
	h := newHarness(t)

	// Global toggle off: plain requests record nothing.
	resp, raw := h.post("/v1/chat/completions", `{"model":"test-model","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	row := latestLogRow(t, h, "test-model")
	if row.PayloadPath != "" {
		t.Fatalf("payload recorded without trigger: %q", row.PayloadPath)
	}

	// X-Debug-Trace: 1 records a single request even with the toggle off.
	resp, raw = h.post("/v1/chat/completions", `{"model":"test-model","stream":false}`,
		map[string]string{"X-Debug-Trace": "1"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	row = latestPayloadRow(t, h, "test-model")
	rec := readPayload(t, h, row)
	if string(rec.RespBody) != string(raw) {
		t.Fatalf("header-triggered response body mismatch")
	}
}

func TestPayloadRecordingStream(t *testing.T) {
	h := newHarness(t)

	resp, raw := h.post("/v1/chat/completions", `{"model":"test-model","stream":true}`,
		map[string]string{"X-Debug-Trace": "1"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "data:") {
		t.Fatalf("not an SSE stream: %s", raw)
	}

	row := latestPayloadRow(t, h, "test-model")
	rec := readPayload(t, h, row)
	// OpenAI passthrough relays upstream bytes verbatim: the recording must
	// equal the exact byte stream the client received.
	if string(rec.RespBody) != string(raw) {
		t.Fatalf("streamed response mismatch:\nrec:    %q\nclient: %q", rec.RespBody, raw)
	}
}

func TestPayloadRecordingFailover(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(t, h.st.Settings.Set(ctx, "log_bodies", "1"))

	// Second provider with a lower-priority openai channel on the same fake:
	// the first attempt 429s, failover succeeds on the second.
	pid, err := h.st.Providers.Create(ctx, "fake2", nil)
	must(t, err)
	_, err = h.st.Accounts.Create(ctx, pid, "k2", "upstream-secret-2", 1, "")
	must(t, err)
	_, err = h.st.Channels.Create(ctx, &store.Channel{
		ProviderID: pid, Name: "fake2-openai", Protocol: "openai",
		BaseURL: h.openai.URL(), ChatPath: "/chat/completions",
		AuthStyle: "bearer", ExtraHeaders: "{}", Enabled: true, Priority: 5, Weight: 1,
		Passthrough: true,
	})
	must(t, err)
	_, err = h.st.Models.EnsureModel(ctx, &store.Model{
		ID: "test-model", ProviderID: pid, UpstreamModel: "fake-chat",
	})
	must(t, err)
	h.openai.FailFirstN(1)
	h.rebuild()

	resp, raw := h.post("/v1/chat/completions", `{"model":"test-model","stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("failover failed, status %d: %s", resp.StatusCode, raw)
	}

	row := latestPayloadRow(t, h, "test-model")
	if row.Attempts != 2 || !row.Success {
		t.Fatalf("want attempts=2 success row, got %+v", row)
	}
	rec := readPayload(t, h, row)
	// Only the final attempt survives: the recorded upstream request used the
	// second provider's account, and the recorded response is the 200 body.
	upAuth := rec.UpstreamReq.Headers["Authorization"]
	if len(upAuth) != 1 || !strings.HasPrefix(upAuth[0], "Bearer upst") {
		t.Fatalf("unexpected upstream auth: %v", upAuth)
	}
	if strings.Contains(upAuth[0], "upstream-secret") && !strings.Contains(upAuth[0], "…") {
		t.Fatalf("upstream secret leaked: %q", upAuth[0])
	}
	if rec.RespStatus != 200 || !strings.Contains(string(rec.RespBody), "Hello from fake OpenAI") {
		t.Fatalf("recorded response is not the successful attempt: status=%d body=%s",
			rec.RespStatus, rec.RespBody)
	}
}

// A request rejected before routing (model not found) still records the
// client segment when triggered — valuable for debugging unroutable names.
func TestPayloadRecordingEarlyFailure(t *testing.T) {
	h := newHarness(t)

	resp, _ := h.post("/v1/chat/completions", `{"model":"no-such-model"}`,
		map[string]string{"X-Debug-Trace": "1"})
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	row := latestPayloadRow(t, h, "no-such-model")
	rec := readPayload(t, h, row)
	if rec.UpstreamReq != nil {
		t.Fatalf("no upstream attempt expected: %+v", rec.UpstreamReq)
	}
	if string(rec.ClientReq.Body) != `{"model":"no-such-model"}` {
		t.Fatalf("client body wrong: %q", rec.ClientReq.Body)
	}
}
