-- Initial schema: settings, admin auth, providers/accounts/channels, model
-- routing, logs. Timestamps are unix seconds unless noted otherwise.

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

-- Vendor account. One provider hosts one or more protocol endpoints
-- (channels); credentials live on the provider's accounts.
CREATE TABLE providers (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  enabled      INTEGER NOT NULL DEFAULT 1,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

-- Provider API keys: a provider pools 1:N accounts, picked by weighted RR.
-- The API key is one attribute of an account; usage probes are per-account.
CREATE TABLE accounts (
  id           INTEGER PRIMARY KEY,
  provider_id  INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  label        TEXT NOT NULL DEFAULT '',
  api_key      TEXT NOT NULL,
  weight       INTEGER NOT NULL DEFAULT 1,
  enabled      INTEGER NOT NULL DEFAULT 1,
  usage_probes TEXT NOT NULL DEFAULT '[]', -- JSON array of {type, path, name?, auth_style?} quota probes
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX idx_accounts_provider ON accounts(provider_id);

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
  responses_path        TEXT, -- OpenAI Responses endpoint relative to base_url; NULL = bridge via IR
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

-- Hot-switchable model routes: stable client-facing names -> ordered failover
-- chains of (channel, upstream_model). The first healthy target serves.
-- Names are canonical: the "[1m]" suffix is reserved for the GET /v1/models
-- 1M-context marker and is rejected at the admin API.
CREATE TABLE model_routes (
  name         TEXT PRIMARY KEY,
  targets_json TEXT NOT NULL DEFAULT '[]',
  updated_at   INTEGER NOT NULL
);

-- Client-facing model registry merged into GET /v1/models, keyed per provider:
-- the same model name may exist once per provider, each row with its own
-- enabled flag and limits. provider_id 0 is the manual (not provider-tied)
-- entry. GET /v1/models merges duplicates by name.
CREATE TABLE models (
  id                TEXT NOT NULL,
  display_name      TEXT NOT NULL DEFAULT '',
  provider_id       INTEGER NOT NULL DEFAULT 0,
  context_window    INTEGER,
  max_output_tokens INTEGER,
  enabled           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  PRIMARY KEY (provider_id, id)
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

-- Denormalized display names survive provider/channel/account deletion.
CREATE TABLE request_logs (
  id                INTEGER PRIMARY KEY,
  ts                INTEGER NOT NULL,            -- unix millis
  request_id        TEXT NOT NULL DEFAULT '',
  api_key_id        INTEGER,
  api_key_name      TEXT NOT NULL DEFAULT '',
  provider_id       INTEGER,
  provider_name     TEXT NOT NULL DEFAULT '',
  account_id        INTEGER,
  account_name      TEXT NOT NULL DEFAULT '',
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
CREATE INDEX idx_logs_ts         ON request_logs(ts);
CREATE INDEX idx_logs_model_ts   ON request_logs(model, ts);
CREATE INDEX idx_logs_key_ts     ON request_logs(api_key_id, ts);
CREATE INDEX idx_logs_prov_ts    ON request_logs(provider_id, ts);
CREATE INDEX idx_logs_account_ts ON request_logs(account_id, ts);
