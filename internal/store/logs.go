package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// RequestLog is one row of request_logs. Denormalized display names survive
// deletion of the referenced provider/channel/account.
type RequestLog struct {
	ID               int64   `json:"id"`
	TS               int64   `json:"ts"` // unix millis
	RequestID        string  `json:"request_id"`
	APIKeyID         *int64  `json:"api_key_id"`
	APIKeyName       string  `json:"api_key_name"`
	ProviderID       *int64  `json:"provider_id"`
	ProviderName     string  `json:"provider_name"`
	AccountID        *int64  `json:"account_id"`
	AccountName      string  `json:"account_name"`
	ChannelID        *int64  `json:"channel_id"`
	ChannelProtocol  string  `json:"channel_protocol"`
	Model            string  `json:"model"`
	UpstreamModel    string  `json:"upstream_model"`
	ProtocolIn       string  `json:"protocol_in"`
	ProtocolOut      string  `json:"protocol_out"`
	Stream           bool    `json:"stream"`
	Status           int     `json:"status"`
	Success          bool    `json:"success"`
	ErrorType        *string `json:"error_type"`
	Attempts         int     `json:"attempts"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	LatencyMS        int64   `json:"latency_ms"`
	FirstTokenMS     *int64  `json:"first_token_ms"`
	// PayloadPath is the payload directory relative to <data-dir>/payloads
	// (empty = not recorded). Doubles as the list marker and detail locator.
	PayloadPath string `json:"payload_path"`
}

// LogFilter bounds a logs query. Zero values mean "unset".
type LogFilter struct {
	From       int64 // unix millis
	To         int64
	Model      string
	APIKeyID   int64
	ProviderID int64
	AccountID  int64
	ChannelID  int64
	Status     int
	Page       int
	PageSize   int
}

const logColumns = `id, ts, request_id, api_key_id, api_key_name, provider_id, provider_name,
	account_id, account_name, channel_id, channel_protocol, model, upstream_model, protocol_in, protocol_out, stream,
	status, success, error_type, attempts, prompt_tokens, completion_tokens,
	cache_read_tokens, cache_write_tokens, reasoning_tokens, latency_ms, first_token_ms, payload_path`

// LogsRepo reads and writes request_logs. Inserts go through the batched
// stats writer; this repo provides the batch primitive and query access.
type LogsRepo struct{ db *DB }

// InsertBatch inserts rows in one transaction.
func (r *LogsRepo) InsertBatch(ctx context.Context, rows []RequestLog) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := r.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin log tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO request_logs (
			ts, request_id, api_key_id, api_key_name, provider_id, provider_name,
			account_id, account_name, channel_id, channel_protocol, model, upstream_model, protocol_in, protocol_out,
			stream, status, success, error_type, attempts, prompt_tokens, completion_tokens,
			cache_read_tokens, cache_write_tokens, reasoning_tokens, latency_ms, first_token_ms, payload_path
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare log insert: %w", err)
	}
	defer stmt.Close()
	for i := range rows {
		row := &rows[i]
		if row.TS == 0 {
			row.TS = now() * 1000
		}
		if _, err := stmt.ExecContext(ctx,
			row.TS, row.RequestID, row.APIKeyID, row.APIKeyName, row.ProviderID, row.ProviderName,
			row.AccountID, row.AccountName, row.ChannelID, row.ChannelProtocol, row.Model, row.UpstreamModel, row.ProtocolIn, row.ProtocolOut,
			row.Stream, row.Status, row.Success, row.ErrorType, row.Attempts, row.PromptTokens, row.CompletionTokens,
			row.CacheReadTokens, row.CacheWriteTokens, row.ReasoningTokens, row.LatencyMS, row.FirstTokenMS, row.PayloadPath,
		); err != nil {
			return fmt.Errorf("insert log row: %w", err)
		}
	}
	return tx.Commit()
}

// rowScanner is satisfied by both *sql.Rows and *sql.Row.
type rowScanner interface{ Scan(...any) error }

func scanLog(s rowScanner) (RequestLog, error) {
	var l RequestLog
	var errType sql.NullString
	var ftms sql.NullInt64
	err := s.Scan(&l.ID, &l.TS, &l.RequestID, &l.APIKeyID, &l.APIKeyName, &l.ProviderID, &l.ProviderName,
		&l.AccountID, &l.AccountName, &l.ChannelID, &l.ChannelProtocol, &l.Model, &l.UpstreamModel, &l.ProtocolIn, &l.ProtocolOut, &l.Stream,
		&l.Status, &l.Success, &errType, &l.Attempts, &l.PromptTokens, &l.CompletionTokens,
		&l.CacheReadTokens, &l.CacheWriteTokens, &l.ReasoningTokens, &l.LatencyMS, &l.FirstTokenMS, &l.PayloadPath)
	if err != nil {
		return l, err
	}
	if errType.Valid {
		s := errType.String
		l.ErrorType = &s
	}
	if ftms.Valid {
		v := ftms.Int64
		l.FirstTokenMS = &v
	}
	return l, nil
}

// GetLogByID returns one log row, or ErrNotFound.
func (r *LogsRepo) GetLogByID(ctx context.Context, id int64) (RequestLog, error) {
	l, err := scanLog(r.db.Read.QueryRowContext(ctx,
		`SELECT `+logColumns+` FROM request_logs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// QueryLogs returns a page of logs plus the total count for the filter.
func (r *LogsRepo) QueryLogs(ctx context.Context, f LogFilter) ([]RequestLog, int, error) {
	where, args := logWhere(f)
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 200 {
		f.PageSize = 50
	}

	var total int
	if err := r.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM request_logs `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count logs: %w", err)
	}

	q := `SELECT ` + logColumns + ` FROM request_logs ` + where +
		` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := r.db.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query logs: %w", err)
	}
	defer rows.Close()
	out := []RequestLog{}
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan log: %w", err)
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

func logWhere(f LogFilter) (string, []any) {
	var conds []string
	var args []any
	if f.From > 0 {
		conds = append(conds, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		conds = append(conds, "ts <= ?")
		args = append(args, f.To)
	}
	if f.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, f.Model)
	}
	if f.APIKeyID > 0 {
		conds = append(conds, "api_key_id = ?")
		args = append(args, f.APIKeyID)
	}
	if f.ProviderID > 0 {
		conds = append(conds, "provider_id = ?")
		args = append(args, f.ProviderID)
	}
	if f.AccountID > 0 {
		conds = append(conds, "account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.ChannelID > 0 {
		conds = append(conds, "channel_id = ?")
		args = append(args, f.ChannelID)
	}
	if f.Status > 0 {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// DeleteLogsBefore prunes rows older than cutoff (unix millis).
func (r *LogsRepo) DeleteLogsBefore(ctx context.Context, cutoffMS int64) (int64, error) {
	res, err := r.db.Write.ExecContext(ctx, `DELETE FROM request_logs WHERE ts < ?`, cutoffMS)
	if err != nil {
		return 0, fmt.Errorf("prune logs: %w", err)
	}
	return res.RowsAffected()
}

// — aggregation -----------------------------------------------------------

// Overview aggregates one time window (unix millis bounds) for the dashboard.
type Overview struct {
	Requests         int64  `json:"requests"`
	Successes        int64  `json:"successes"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	LatencyP50MS     *int64 `json:"latency_p50_ms"`
	LatencyP95MS     *int64 `json:"latency_p95_ms"`
	TTFTP50MS        *int64 `json:"ttft_p50_ms"`
	TTFTP95MS        *int64 `json:"ttft_p95_ms"`
}

// OverviewStats computes window totals and latency percentiles.
func (r *LogsRepo) OverviewStats(ctx context.Context, fromMS, toMS int64) (Overview, error) {
	var o Overview
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(success),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
			COALESCE(SUM(reasoning_tokens),0)
		FROM request_logs WHERE ts >= ? AND ts <= ?`, fromMS, toMS).
		Scan(&o.Requests, &o.Successes, &o.PromptTokens, &o.CompletionTokens,
			&o.CacheReadTokens, &o.CacheWriteTokens, &o.ReasoningTokens)
	if err != nil {
		return o, fmt.Errorf("overview totals: %w", err)
	}
	o.LatencyP50MS, err = r.percentile(ctx, "latency_ms", fromMS, toMS, 50)
	if err != nil {
		return o, err
	}
	o.LatencyP95MS, err = r.percentile(ctx, "latency_ms", fromMS, toMS, 95)
	if err != nil {
		return o, err
	}
	o.TTFTP50MS, err = r.percentile(ctx, "first_token_ms", fromMS, toMS, 50)
	if err != nil {
		return o, err
	}
	o.TTFTP95MS, err = r.percentile(ctx, "first_token_ms", fromMS, toMS, 95)
	return o, err
}

// percentile via ordered offset; NULLs excluded by the WHERE.
func (r *LogsRepo) percentile(ctx context.Context, col string, fromMS, toMS int64, pct int) (*int64, error) {
	if col != "latency_ms" && col != "first_token_ms" {
		return nil, errors.New("bad percentile column")
	}
	var n int64
	if err := r.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM request_logs WHERE ts >= ? AND ts <= ? AND `+col+` IS NOT NULL`,
		fromMS, toMS).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	rank := (n*int64(pct) + 99) / 100 // ceil
	if rank < 1 {
		rank = 1
	}
	var v sql.NullInt64
	err := r.db.Read.QueryRowContext(ctx,
		`SELECT `+col+` FROM request_logs WHERE ts >= ? AND ts <= ? AND `+col+` IS NOT NULL
		 ORDER BY `+col+` LIMIT 1 OFFSET ?`, fromMS, toMS, rank-1).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !v.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("percentile %s: %w", col, err)
	}
	out := v.Int64
	return &out, nil
}

// GroupRow is one row of a grouped timeseries.
type GroupRow struct {
	Bucket           string `json:"bucket"`
	Group            string `json:"group"`
	Requests         int64  `json:"requests"`
	Successes        int64  `json:"successes"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
}

// Timeseries groups a window by day and a dimension (model|provider|account|key).
func (r *LogsRepo) Timeseries(ctx context.Context, fromMS, toMS int64, groupBy string) ([]GroupRow, error) {
	col := map[string]string{
		"model":    "model",
		"provider": "provider_name",
		"account":  "account_name",
		"key":      "api_key_name",
	}[groupBy]
	if col == "" {
		col = "model"
	}
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT date(ts/1000, 'unixepoch') AS bucket, `+col+` AS grp,
			COUNT(*) AS requests, COALESCE(SUM(success),0) AS successes,
			COALESCE(SUM(prompt_tokens),0) AS prompt_tokens,
			COALESCE(SUM(completion_tokens),0) AS completion_tokens,
			COALESCE(SUM(cache_read_tokens),0) AS cache_read_tokens
		FROM request_logs
		WHERE ts >= ? AND ts <= ?
		GROUP BY bucket, grp ORDER BY bucket DESC, requests DESC`,
		fromMS, toMS)
	if err != nil {
		return nil, fmt.Errorf("timeseries: %w", err)
	}
	defer rows.Close()
	out := []GroupRow{}
	for rows.Next() {
		var g GroupRow
		if err := rows.Scan(&g.Bucket, &g.Group, &g.Requests, &g.Successes, &g.PromptTokens, &g.CompletionTokens, &g.CacheReadTokens); err != nil {
			return nil, fmt.Errorf("scan timeseries: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
