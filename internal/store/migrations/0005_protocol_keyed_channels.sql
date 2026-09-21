-- 0005: protocol-keyed channels — one endpoint per protocol per provider.
--
-- channels loses name/priority/weight (protocol is the identity within a
-- provider) plus the never-consumed passthrough/force_upstream_stream knobs,
-- gains 'responses' as a third protocol value, and auth_style becomes
-- optional ('' = protocol default: bearer for openai/responses, x-api-key for
-- anthropic). An openai channel carrying an explicit responses_path splits
-- into two rows: the openai row keeps chat_path, the spawned responses row
-- takes responses_path as its chat_path (responses_path itself disappears).
-- Duplicate (provider_id, protocol) rows collapse to the lowest id; route
-- targets pinned to a dropped channel degrade to provider-scoped (their
-- provider_id is already on the target). request_logs.channel_name renames
-- to channel_protocol and stores the channel's protocol from here on.
--
-- Nothing references channels(id) — model_routes targets are JSON — so the
-- rebuild is a plain create/copy/drop/rename (0002 pattern).

CREATE TABLE channels_stage (
  id                  INTEGER, -- spawned responses rows get explicit ids above MAX(id)
  provider_id         INTEGER NOT NULL,
  protocol            TEXT NOT NULL,
  base_url            TEXT NOT NULL,
  chat_path           TEXT NOT NULL,
  auth_style          TEXT NOT NULL,
  extra_headers       TEXT NOT NULL,
  enabled             INTEGER NOT NULL,
  supports_embeddings INTEGER NOT NULL,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL
);

INSERT INTO channels_stage (id, provider_id, protocol, base_url, chat_path, auth_style,
                            extra_headers, enabled, supports_embeddings, created_at, updated_at)
SELECT id, provider_id, protocol, base_url, chat_path, auth_style,
       extra_headers, enabled, supports_embeddings, created_at, updated_at
FROM channels;

-- Split: an openai channel with an explicit responses_path doubles as the
-- provider's Responses endpoint — spawn a responses-protocol row for it.
-- Spawned rows get explicit ids ABOVE the current max: inserting a NULL id
-- mid-copy would autoassign max(rowid)+1 at that position and can collide
-- with a later provider's surviving explicit id.
INSERT INTO channels_stage (id, provider_id, protocol, base_url, chat_path, auth_style,
                            extra_headers, enabled, supports_embeddings, created_at, updated_at)
SELECT (SELECT MAX(id) FROM channels) + ROW_NUMBER() OVER (ORDER BY id),
       provider_id, 'responses', base_url, responses_path, auth_style,
       extra_headers, enabled, 0, created_at, updated_at
FROM channels
WHERE protocol = 'openai' AND responses_path IS NOT NULL AND responses_path <> '';

-- Explicit values equal to the protocol default normalize to '' (auto).
UPDATE channels_stage SET auth_style = ''
WHERE (protocol IN ('openai', 'responses') AND auth_style = 'bearer')
   OR (protocol = 'anthropic' AND auth_style = 'x-api-key');

CREATE TABLE dropped_channel_ids (id INTEGER PRIMARY KEY);

-- Lowest id wins within each (provider_id, protocol); spawned responses rows
-- carry the highest ids, so an original row always outranks a spawned one.
INSERT INTO dropped_channel_ids (id)
SELECT id FROM (
  SELECT id, ROW_NUMBER() OVER (
    PARTITION BY provider_id, protocol ORDER BY id
  ) AS rn FROM channels_stage
) WHERE rn > 1;

CREATE TABLE channels_new (
  id                  INTEGER PRIMARY KEY,
  provider_id         INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  protocol            TEXT NOT NULL CHECK (protocol IN ('openai','anthropic','responses')),
  base_url            TEXT NOT NULL,
  chat_path           TEXT NOT NULL DEFAULT '',
  auth_style          TEXT NOT NULL DEFAULT '' CHECK (auth_style IN ('','bearer','x-api-key')),
  extra_headers       TEXT NOT NULL DEFAULT '{}',
  enabled             INTEGER NOT NULL DEFAULT 1,
  supports_embeddings INTEGER NOT NULL DEFAULT 0,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  UNIQUE (provider_id, protocol)
);

INSERT INTO channels_new (id, provider_id, protocol, base_url, chat_path, auth_style,
                          extra_headers, enabled, supports_embeddings, created_at, updated_at)
SELECT id, provider_id, protocol, base_url, chat_path, auth_style,
       extra_headers, enabled, supports_embeddings, created_at, updated_at
FROM (
  SELECT *, ROW_NUMBER() OVER (
    PARTITION BY provider_id, protocol ORDER BY id
  ) AS rn FROM channels_stage
) WHERE rn = 1;

-- Degrade route pins pointing at dropped channels to provider auto-select.
-- json_set writes a JSON null for the SQL NULL, which the Go target struct
-- (ChannelID *int64) round-trips as a cleared pin.
UPDATE model_routes SET targets_json = (
  SELECT json_group_array(
    CASE WHEN json_extract(t.value, '$.channel_id') IN (SELECT id FROM dropped_channel_ids)
         THEN json_set(t.value, '$.channel_id', NULL)
         ELSE t.value END)
  FROM json_each(model_routes.targets_json) AS t)
WHERE EXISTS (
  SELECT 1 FROM json_each(model_routes.targets_json) AS t
  WHERE json_extract(t.value, '$.channel_id') IN (SELECT id FROM dropped_channel_ids));

DROP TABLE channels;
ALTER TABLE channels_new RENAME TO channels;
CREATE INDEX idx_channels_enabled ON channels(enabled);

DROP TABLE channels_stage;
DROP TABLE dropped_channel_ids;

ALTER TABLE request_logs RENAME COLUMN channel_name TO channel_protocol;
