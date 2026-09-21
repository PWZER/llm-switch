package payload

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PWZER/llm-switch/internal/store"
)

func newTestWriter(t *testing.T) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.New(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	base := filepath.Join(dir, "payloads")
	w := NewWriter(base, st)
	t.Cleanup(func() { w.Close(t.Context()) })
	return w, base
}

func testRecord() *Record {
	return &Record{
		RequestID: "req-1",
		TS:        time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC).UnixMilli(),
		ClientReq: Segment{
			Headers: map[string][]string{"Content-Type": {"application/json"}},
			Body:    []byte(`{"model":"glm-4.6"}`),
		},
		UpstreamReq: &Segment{
			Headers: map[string][]string{"Authorization": {"Bearer sk-s…cret"}},
			Body:    []byte(`{"model":"fake-chat"}`),
		},
		RespStatus:  200,
		RespHeaders: map[string][]string{"Content-Type": {"application/json"}},
		RespBody:    []byte(`{"id":"chatcmpl-1"}`),
	}
}

// waitForFile polls until the record's meta.json appears (the writer is
// async) or the deadline passes.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file never appeared: %s", path)
}

func TestEnqueueWritesAndReadsBack(t *testing.T) {
	w, base := newTestWriter(t)

	rel, ok := w.Enqueue(testRecord())
	if !ok || rel == "" {
		t.Fatalf("enqueue: rel=%q ok=%v", rel, ok)
	}
	waitForFile(t, filepath.Join(base, rel, "meta.json"))

	rec, err := Read(base, rel)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rec.RequestID != "req-1" || rec.RespStatus != 200 {
		t.Fatalf("meta roundtrip wrong: %+v", rec)
	}
	if string(rec.ClientReq.Body) != `{"model":"glm-4.6"}` {
		t.Fatalf("client body wrong: %q", rec.ClientReq.Body)
	}
	if rec.UpstreamReq == nil || string(rec.UpstreamReq.Body) != `{"model":"fake-chat"}` {
		t.Fatalf("upstream body wrong: %+v", rec.UpstreamReq)
	}
	if string(rec.RespBody) != `{"id":"chatcmpl-1"}` {
		t.Fatalf("response body wrong: %q", rec.RespBody)
	}
}

func TestDirForCollisionSuffix(t *testing.T) {
	w, base := newTestWriter(t)

	rel1, ok := w.Enqueue(testRecord())
	if !ok {
		t.Fatal("first enqueue failed")
	}
	waitForFile(t, filepath.Join(base, rel1, "meta.json"))
	// Same request id again (a client may reuse X-Request-Id): the second
	// record must land in a different directory.
	rel2, ok := w.Enqueue(testRecord())
	if !ok {
		t.Fatal("second enqueue failed")
	}
	if rel1 == rel2 {
		t.Fatalf("collision not resolved: both %q", rel1)
	}
	waitForFile(t, filepath.Join(base, rel2, "meta.json"))
}

func TestSanitizeRequestID(t *testing.T) {
	for in, want := range map[string]string{
		"req-abc_123":   "req-abc_123",
		"../../etc":     "______etc",
		"a/b\\c:d":      "a_b_c_d",
		"":              "", // empty handled below (generated name)
		"with space ok": "with_space_ok",
	} {
		got := SanitizeRequestID(in)
		if in == "" {
			if !strings.HasPrefix(got, "req-") {
				t.Fatalf("empty id must get a generated name, got %q", got)
			}
			continue
		}
		if got != want {
			t.Fatalf("SanitizeRequestID(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, `/\.`) {
			t.Fatalf("unsafe name %q from %q", got, in)
		}
	}
}

func TestReadRejectsTraversal(t *testing.T) {
	_, base := newTestWriter(t)
	for _, rel := range []string{"", "..", "../x", "20260921/../../etc", "/abs"} {
		if _, err := Read(base, rel); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Read(%q): want ErrNotFound, got %v", rel, err)
		}
	}
}

func TestReadMissingRecord(t *testing.T) {
	_, base := newTestWriter(t)
	if _, err := Read(base, "20260921/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{
		"Authorization": {"Bearer sk-1234567890abcdef"},
		"X-Api-Key":     {"sk-xyz123456789"},
		"Cookie":        {"session=abcdef123456"},
		"Content-Type":  {"application/json"},
	}
	out := RedactHeaders(h)
	if got := out["Authorization"][0]; !strings.HasPrefix(got, "Bearer ") || strings.Contains(got, "1234567890") {
		t.Fatalf("authorization not masked: %q", got)
	}
	if got := out["X-Api-Key"][0]; strings.Contains(got, "xyz123456") {
		t.Fatalf("x-api-key not masked: %q", got)
	}
	if got := out["Cookie"][0]; strings.Contains(got, "abcdef123456") {
		t.Fatalf("cookie not masked: %q", got)
	}
	if out["Content-Type"][0] != "application/json" {
		t.Fatalf("non-sensitive header must pass through: %q", out["Content-Type"][0])
	}
	// The input header must stay untouched.
	if h.Get("Authorization") != "Bearer sk-1234567890abcdef" {
		t.Fatal("input header mutated")
	}
}

func TestBufferCap(t *testing.T) {
	b := NewBuffer(4)
	b.Write([]byte("ab"))
	b.Write([]byte("cdef"))
	if b.Bytes() == nil || string(b.Bytes()) != "abcd" || !b.Truncated() {
		t.Fatalf("buffer wrong: %q truncated=%v", b.Bytes(), b.Truncated())
	}
	var nilBuf *Buffer
	nilBuf.Write([]byte("x")) // must not panic
	if nilBuf.Bytes() != nil || nilBuf.Truncated() {
		t.Fatal("nil buffer must be a no-op")
	}
}

func TestPruneOlderThan(t *testing.T) {
	w, base := newTestWriter(t)
	mk := func(name string) {
		if err := os.MkdirAll(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().AddDate(0, 0, -10).UTC().Format("20060102")
	recent := time.Now().UTC().Format("20060102")
	mk(old)
	mk(recent)
	mk("not-a-date")

	removed, err := w.PruneOlderThan(3)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("want 1 removed, got %d", removed)
	}
	if _, err := os.Stat(filepath.Join(base, old)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old dir must be gone")
	}
	for _, keep := range []string{recent, "not-a-date"} {
		if _, err := os.Stat(filepath.Join(base, keep)); err != nil {
			t.Fatalf("%s must survive: %v", keep, err)
		}
	}
}
