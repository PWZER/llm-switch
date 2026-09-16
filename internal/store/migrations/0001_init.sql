-- Initial schema: settings, admin auth, providers/channels/keys, model routing, logs.
-- Timestamps are unix seconds unless noted otherwise.

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

INSERT INTO settings (key, value, updated_at) VALUES
  ('retention_days', '30', strftime('%s','now')),
  ('max_failover_attempts', '3', strftime('%s','now')),
  ('default_max_tokens', '8192', strftime('%s','now')),
  ('auto_bind_new_models', '1', strftime('%s','now')),
  ('stream_idle_timeout_s', '300', strftime('%s','now')),
  ('log_bodies', '0', strftime('%s','now'));

CREATE TABLE admin_auth (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  password_hash TEXT NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
  token      TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

-- Vendor account. One account's API key typically works on both the OpenAI and
-- Anthropic endpoints, so keys live here and are shared by all of the provider's channels.
CREATE TABLE providers (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE provider_keys (
  id         INTEGER PRIMARY KEY,
  provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  label      TEXT NOT NULL DEFAULT '',
  api_key    TEXT NOT NULL,
  weight     INTEGER NOT NULL DEFAULT 1,
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_pkeys_provider ON provider_keys(provider_id);

-- Protocol-specific endpoint. One provider may expose several channels
-- (e.g. Zhipu openai + Zhipu anthropic); failover walks channels by priority.
CREATE TABLE channels (
  id                    INTEGER PRIMARY KEY,
  provider_id           INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  name                  TEXT NOT NULL,
  protocol              TEXT NOT NULL CHECK (protocol IN ('openai','anthropic')),
  base_url              TEXT NOT NULL,
  chat_path             TEXT NOT NULL DEFAULT '',
  auth_style            TEXT NOT NULL DEFAULT 'bearer' CHECK (auth_style IN ('bearer','x-api-key')),
  models_url            TEXT,
  extra_headers         TEXT NOT NULL DEFAULT '{}',
  enabled               INTEGER NOT NULL DEFAULT 1,
  priority              INTEGER NOT NULL DEFAULT 0,
  weight                INTEGER NOT NULL DEFAULT 1,
  auto_bind             INTEGER NOT NULL DEFAULT 1,
  supports_embeddings   INTEGER NOT NULL DEFAULT 0,
  passthrough           INTEGER NOT NULL DEFAULT 1,
  force_upstream_stream INTEGER NOT NULL DEFAULT 0,
  created_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL
);
CREATE INDEX idx_channels_enabled ON channels(enabled, priority DESC, weight);

-- Per-channel model mapping: client-facing name -> upstream name. The routing table.
CREATE TABLE channel_models (
  channel_id     INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  model          TEXT NOT NULL,
  upstream_model TEXT NOT NULL,
  PRIMARY KEY (channel_id, model)
);
CREATE INDEX idx_cmodel_model ON channel_models(model);

-- Hot-switchable aliases: stable client-facing names -> (channel, upstream_model).
CREATE TABLE aliases (
  name           TEXT PRIMARY KEY,
  channel_id     INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  upstream_model TEXT NOT NULL,
  updated_at     INTEGER NOT NULL
);

-- Client-facing model registry merged into GET /v1/models.
CREATE TABLE models (
  id                TEXT PRIMARY KEY,
  display_name      TEXT NOT NULL DEFAULT '',
  source            TEXT NOT NULL DEFAULT 'manual',
  context_window    INTEGER,
  max_output_tokens INTEGER,
  enabled           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

-- Gateway keys issued to Agents. `key` holds the sha256 hex of the plaintext;
-- the plaintext is shown exactly once at creation.
CREATE TABLE api_keys (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL,
  key          TEXT NOT NULL UNIQUE,
  prefix       TEXT NOT NULL DEFAULT '',
  enabled      INTEGER NOT NULL DEFAULT 1,
  token_limit  INTEGER,
  expires_at   INTEGER,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  last_used_at INTEGER
);

-- Denormalized display names survive provider/channel/key deletion.
CREATE TABLE request_logs (
  id                INTEGER PRIMARY KEY,
  ts                INTEGER NOT NULL,            -- unix millis
  request_id        TEXT NOT NULL DEFAULT '',
  api_key_id        INTEGER,
  api_key_name      TEXT NOT NULL DEFAULT '',
  provider_id       INTEGER,
  provider_name     TEXT NOT NULL DEFAULT '',
  channel_id        INTEGER,
  channel_name      TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL,               -- client-facing
  upstream_model    TEXT NOT NULL DEFAULT '',
  protocol_in       TEXT NOT NULL,               -- 'openai' | 'anthropic'
  protocol_out      TEXT NOT NULL,
  stream            INTEGER NOT NULL DEFAULT 0,
  status            INTEGER NOT NULL,            -- final HTTP status returned to the client
  success           INTEGER NOT NULL DEFAULT 0,
  error_type        TEXT,
  attempts          INTEGER NOT NULL DEFAULT 1,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  first_token_ms    INTEGER
);
CREATE INDEX idx_logs_ts       ON request_logs(ts);
CREATE INDEX idx_logs_model_ts ON request_logs(model, ts);
CREATE INDEX idx_logs_key_ts   ON request_logs(api_key_id, ts);
CREATE INDEX idx_logs_prov_ts  ON request_logs(provider_id, ts);
