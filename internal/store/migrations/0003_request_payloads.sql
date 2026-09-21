-- 0003: on-demand full-payload recording. Payload bytes live as files under
-- <data-dir>/payloads/<YYYYMMDD>/<dir>/ (see internal/payload); request_logs
-- only carries the relative payload directory (empty = not recorded), which
-- doubles as the list-UI marker and the detail-endpoint locator.

ALTER TABLE request_logs ADD COLUMN payload_path TEXT NOT NULL DEFAULT '';

INSERT INTO settings (key, value, updated_at)
VALUES ('payload_retention_days', '3', strftime('%s','now'))
ON CONFLICT(key) DO NOTHING;
