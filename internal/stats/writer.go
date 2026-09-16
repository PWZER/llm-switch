// Package stats provides the async request-log writer: request handlers push
// rows onto a buffered channel and a single goroutine batches them into
// SQLite. Overflow drops rows (counted) rather than ever blocking the hot path.
package stats

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PWZER/llm-switch/internal/store"
)

const (
	defaultQueue = 4096
	batchRows    = 100
	batchEvery   = 200 * time.Millisecond
)

// Logger batches request-log writes off the request path.
type Logger struct {
	st       *store.Store
	ch       chan store.RequestLog
	dropped  atomic.Int64
	wg       sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
}

// New starts the writer goroutine and the daily retention ticker.
func New(st *store.Store) *Logger {
	l := &Logger{
		st:   st,
		ch:   make(chan store.RequestLog, defaultQueue),
		stop: make(chan struct{}),
	}
	l.wg.Add(1)
	go l.loop()
	go l.retentionLoop()
	return l
}

// Log enqueues a row without blocking. On overflow the row is dropped and
// counted; the dashboard exposes the drop counter.
func (l *Logger) Log(entry store.RequestLog) {
	select {
	case l.ch <- entry:
	default:
		l.dropped.Add(1)
	}
}

// Dropped reports the number of log rows dropped due to backpressure.
func (l *Logger) Dropped() int64 { return l.dropped.Load() }

// Close drains and flushes remaining rows, then stops the goroutines.
func (l *Logger) Close(ctx context.Context) error {
	l.stopOnce.Do(func() { close(l.stop) })
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (l *Logger) loop() {
	defer l.wg.Done()
	buf := make([]store.RequestLog, 0, batchRows)
	tick := time.NewTicker(batchEvery)
	defer tick.Stop()
	flushCtx := context.Background()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(flushCtx, 10*time.Second)
		if err := l.st.Logs.InsertBatch(ctx, buf); err != nil {
			// Truncated rows or transient SQLite contention must not take down
			// the process: log loudly and drop the batch.
			slog.Error("stats batch insert failed", "rows", len(buf), "err", err)
		}
		cancel()
		buf = buf[:0]
	}

	for {
		select {
		case entry := <-l.ch:
			buf = append(buf, entry)
			if len(buf) >= batchRows {
				flush()
			}
		case <-tick.C:
			flush()
		case <-l.stop:
			// Drain whatever is queued, then final flush.
			for {
				select {
				case entry := <-l.ch:
					buf = append(buf, entry)
					if len(buf) >= batchRows {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// retentionLoop deletes request logs older than the configured retention once a day.
func (l *Logger) retentionLoop() {
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		days, err := l.st.Settings.Get(ctx, "retention_days")
		if err != nil {
			return // settings row missing: skip silently
		}
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return
		}
		cutoff := time.Now().AddDate(0, 0, -n).UnixMilli()
		if deleted, err := l.st.Logs.DeleteLogsBefore(ctx, cutoff); err == nil && deleted > 0 {
			slog.Info("retention pruned request logs", "rows", deleted, "days", n)
		}
	}
	run() // once at boot
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			run()
		case <-l.stop:
			return
		}
	}
}
