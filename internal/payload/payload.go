// Package payload writes full request/response payload recordings to disk:
// files under <dir>/<YYYYMMDD>/<request-dir>/, never SQLite. Redaction
// happens at capture time; write failures never fail a request.
package payload

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PWZER/llm-switch/internal/store"
)

// MaxSegmentBytes bounds one recorded body; it matches the gateway's
// maxBodyBytes so recorded bodies are never truncated in practice.
const MaxSegmentBytes = 32 << 20

const queueCap = 256

// ErrNotFound is returned by Read when the payload is missing or pruned.
var ErrNotFound = errors.New("payload not found")

// Segment is one recorded HTTP message: redacted headers + full body.
type Segment struct {
	Headers   map[string][]string `json:"headers"`
	Body      []byte              `json:"-"` // stored as a .bin file
	Truncated bool                `json:"truncated"`
}

// Record is one request's three segments.
type Record struct {
	RequestID   string              `json:"request_id"`
	TS          int64               `json:"ts"` // unix millis; the directory date derives from it (UTC)
	ClientReq   Segment             `json:"client_request"`
	UpstreamReq *Segment            `json:"upstream_request,omitempty"` // nil on pre-dispatch failures
	RespStatus  int                 `json:"response_status,omitempty"`
	RespHeaders map[string][]string `json:"response_headers,omitempty"`
	RespBody    []byte              `json:"-"`
	RespTrunc   bool                `json:"response_truncated"`
}

// Buffer is a capped append-only body collector for streaming relays.
type Buffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

// NewBuffer returns a Buffer holding at most cap bytes.
func NewBuffer(cap int) *Buffer { return &Buffer{cap: cap} }

// Write appends up to the remaining capacity and marks the buffer truncated
// when input does not fit.
func (b *Buffer) Write(p []byte) {
	if b == nil {
		return
	}
	remaining := b.cap - b.buf.Len()
	if remaining <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return
	}
	b.buf.Write(p)
}

// Bytes returns the collected bytes.
func (b *Buffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf.Bytes()
}

// Truncated reports whether input was dropped due to the cap.
func (b *Buffer) Truncated() bool { return b != nil && b.truncated }

// SanitizeRequestID reduces a request id to a safe directory name. chi's
// reqid middleware trusts the client-supplied X-Request-Id header, so the
// raw value must never reach the filesystem.
func SanitizeRequestID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "req-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// sensitiveHeaders are redacted at capture time: plaintext provider keys ride
// the upstream request, client keys ride the inbound one.
var sensitiveHeaders = map[string]bool{
	"Authorization":       true,
	"X-Api-Key":           true,
	"Cookie":              true,
	"Proxy-Authorization": true,
}

// RedactHeaders clones h, masking sensitive values with store.MaskKey.
func RedactHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		cp := make([]string, len(vs))
		copy(cp, vs)
		if sensitiveHeaders[http.CanonicalHeaderKey(k)] {
			for i := range cp {
				cp[i] = redactValue(cp[i])
			}
		}
		out[k] = cp
	}
	return out
}

// redactValue masks a header value, keeping a scheme prefix legible
// ("Bearer sk-…ret" rather than masking the word "Bearer").
func redactValue(v string) string {
	if i := strings.IndexByte(v, ' '); i > 0 && i < len(v)-1 {
		return v[:i+1] + store.MaskKey(v[i+1:])
	}
	return store.MaskKey(v)
}

// DirFor returns the relative directory (<YYYYMMDD>/<name>) for a record,
// resolving collisions (a client may reuse X-Request-Id) with a short random
// suffix. The date derives from ts in UTC so writes and prunes agree.
func DirFor(base string, ts int64, requestID string) (string, error) {
	day := time.UnixMilli(ts).UTC().Format("20060102")
	name := SanitizeRequestID(requestID)
	for i := 0; i < 10; i++ {
		rel := filepath.Join(day, name)
		abs := filepath.Join(base, rel)
		if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
			return rel, nil
		} else if err != nil {
			return "", err
		}
		suffix := make([]byte, 3)
		if _, err := rand.Read(suffix); err != nil {
			return "", err
		}
		name = SanitizeRequestID(requestID) + "-" + hex.EncodeToString(suffix)
	}
	return "", fmt.Errorf("payload dir collision for request id %q", requestID)
}

// Read loads the record stored at relPath under dir. The resolved path must
// stay inside dir (the value originates from a DB column, but defense in
// depth is cheap). Missing or incomplete records yield ErrNotFound.
func Read(dir, relPath string) (*Record, error) {
	if relPath == "" {
		return nil, ErrNotFound
	}
	abs := filepath.Join(dir, relPath)
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.Abs(abs)
	if err != nil {
		return nil, err
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return nil, ErrNotFound
	}

	meta, err := os.ReadFile(filepath.Join(abs, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(meta, &rec); err != nil {
		return nil, fmt.Errorf("parse payload meta: %w", err)
	}
	readBody := func(name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(abs, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return b, err
	}
	if rec.ClientReq.Body, err = readBody("client_req.bin"); err != nil {
		return nil, err
	}
	if rec.UpstreamReq != nil {
		if rec.UpstreamReq.Body, err = readBody("upstream_req.bin"); err != nil {
			return nil, err
		}
	}
	if rec.RespBody, err = readBody("upstream_resp.bin"); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Writer owns the async write queue and the daily retention loop.
type Writer struct {
	dir      string
	st       *store.Store
	ch       chan *queuedRecord
	dropped  atomic.Int64
	wg       sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
}

type queuedRecord struct {
	rec *Record
	rel string
}

// NewWriter starts the write goroutine and the retention loop under dir.
func NewWriter(dir string, st *store.Store) *Writer {
	w := &Writer{
		dir:  dir,
		st:   st,
		ch:   make(chan *queuedRecord, queueCap),
		stop: make(chan struct{}),
	}
	w.wg.Add(2)
	go w.loop()
	go w.retentionLoop()
	return w
}

// Enqueue assigns the record a directory and queues the disk write without
// blocking. The returned relative path is what the caller stores on the log
// row; ok is false when the queue is full (record dropped, counted).
func (w *Writer) Enqueue(rec *Record) (relPath string, ok bool) {
	if w == nil || rec == nil {
		return "", false
	}
	rel, err := DirFor(w.dir, rec.TS, rec.RequestID)
	if err != nil {
		slog.Warn("payload dir allocation failed", "request_id", rec.RequestID, "err", err)
		return "", false
	}
	select {
	case w.ch <- &queuedRecord{rec: rec, rel: rel}:
		return rel, true
	default:
		w.dropped.Add(1)
		return "", false
	}
}

// Dropped reports records dropped due to queue backpressure.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Close drains the queue and stops the goroutines.
func (w *Writer) Close(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Writer) loop() {
	defer w.wg.Done()
	for {
		select {
		case q := <-w.ch:
			w.write(q)
		case <-w.stop:
			for {
				select {
				case q := <-w.ch:
					w.write(q)
				default:
					return
				}
			}
		}
	}
}

// write persists one record: body files first, meta.json last — meta
// presence marks the record complete for Read.
func (w *Writer) write(q *queuedRecord) {
	abs := filepath.Join(w.dir, q.rel)
	if err := os.MkdirAll(abs, 0o700); err != nil {
		slog.Warn("payload mkdir failed", "dir", abs, "err", err)
		return
	}
	writeBody := func(name string, body []byte) bool {
		if len(body) == 0 {
			return true
		}
		if err := os.WriteFile(filepath.Join(abs, name), body, 0o600); err != nil {
			slog.Warn("payload body write failed", "file", name, "err", err)
			return false
		}
		return true
	}
	ok := writeBody("client_req.bin", q.rec.ClientReq.Body)
	if q.rec.UpstreamReq != nil {
		ok = writeBody("upstream_req.bin", q.rec.UpstreamReq.Body) && ok
	}
	ok = writeBody("upstream_resp.bin", q.rec.RespBody) && ok
	meta, err := json.Marshal(q.rec)
	if err != nil {
		slog.Warn("payload meta marshal failed", "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(abs, "meta.json"), meta, 0o600); err != nil {
		slog.Warn("payload meta write failed", "dir", abs, "err", err)
		return
	}
	if !ok {
		slog.Warn("payload partially written", "dir", abs)
	}
}

// PruneOlderThan removes date-shaped directories older than days.
func (w *Writer) PruneOlderThan(days int) (removed int, err error) {
	if days <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(w.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		day, perr := time.Parse("20060102", e.Name())
		if perr != nil || !day.Before(cutoff) {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(w.dir, e.Name())); rerr != nil {
			slog.Warn("payload prune failed", "dir", e.Name(), "err", rerr)
			continue
		}
		removed++
	}
	return removed, nil
}

// retentionLoop prunes payload directories once a day, mirroring the stats
// log retention. payload_retention_days is read fresh each run.
func (w *Writer) retentionLoop() {
	defer w.wg.Done()
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		days, err := w.st.Settings.Get(ctx, "payload_retention_days")
		if err != nil {
			return // settings row missing: skip silently
		}
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return
		}
		if removed, err := w.PruneOlderThan(n); err == nil && removed > 0 {
			slog.Info("retention pruned payloads", "dirs", removed, "days", n)
		}
	}
	run() // once at boot
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			run()
		case <-w.stop:
			return
		}
	}
}
