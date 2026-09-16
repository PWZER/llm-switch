-- 0002: provider-scoped model registry — one row = one provider serving one
-- client-facing model name (registry and routing table unified). Models bind
-- to the provider, not to individual endpoints: every live channel of the
-- provider can serve the name. The models table keeps its (provider_id, id)
-- key but gains a real FK (cascade) and an upstream_model alias column; old
-- channel_models bindings fold in (alias backfill, created rows for
-- binding-only names); old manual rows (provider_id 0) have no provider and
-- are dropped. The models-list fetch URL moves from channels to providers
-- (providers.models_url, backfilled from the provider's first configured
-- channel), and channels is rebuilt without models_url and the never-consumed
-- auto_bind column.
--
-- Order matters under foreign_keys(1): stage the folded rows without FKs,
-- drop the child tables, rebuild channels (nothing references it anymore —
-- model_routes targets are JSON, not FKs), then create the final models
-- table referencing providers.

ALTER TABLE providers ADD COLUMN models_url TEXT;

-- The fetch URL was per-channel; keep the provider's first configured one.
UPDATE providers SET models_url = (
  SELECT c.models_url FROM channels c
  WHERE c.provider_id = providers.id AND c.models_url IS NOT NULL AND c.models_url <> ''
  ORDER BY c.id LIMIT 1
);

CREATE TABLE models_stage (
  id                TEXT NOT NULL,
  provider_id       INTEGER NOT NULL,
  upstream_model    TEXT NOT NULL DEFAULT '',
  display_name      TEXT NOT NULL DEFAULT '',
  context_window    INTEGER,
  max_output_tokens INTEGER,
  enabled           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  PRIMARY KEY (provider_id, id)
);

-- Registry rows bound to a real provider carry over (identity alias).
INSERT INTO models_stage (id, provider_id, upstream_model, display_name,
                          context_window, max_output_tokens, enabled, created_at, updated_at)
SELECT id, provider_id, '', display_name, context_window, max_output_tokens,
       enabled, created_at, updated_at
FROM models WHERE provider_id > 0;

-- Bindings fold in: binding-only names create rows (blank metadata), existing
-- rows backfill the alias when they have none and stay enabled if any binding
-- was. First alias wins on conflicting bindings (ordered by channel id).
INSERT INTO models_stage (id, provider_id, upstream_model, display_name,
                          context_window, max_output_tokens, enabled, created_at, updated_at)
SELECT cm.model, c.provider_id, cm.upstream_model, '', NULL, NULL, 1,
       strftime('%s', 'now'), strftime('%s', 'now')
FROM channel_models cm
JOIN channels c ON c.id = cm.channel_id
ORDER BY c.id
ON CONFLICT(provider_id, id) DO UPDATE SET
  upstream_model = CASE WHEN models_stage.upstream_model = ''
                        THEN excluded.upstream_model
                        ELSE models_stage.upstream_model END;

DROP TABLE channel_models;
DROP TABLE models;

CREATE TABLE channels_new (
  id                    INTEGER PRIMARY KEY,
  provider_id           INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  name                  TEXT NOT NULL,
  protocol              TEXT NOT NULL CHECK (protocol IN ('openai','anthropic')),
  base_url              TEXT NOT NULL,
  chat_path             TEXT NOT NULL DEFAULT '',
  auth_style            TEXT NOT NULL DEFAULT 'bearer' CHECK (auth_style IN ('bearer','x-api-key')),
  responses_path        TEXT,
  extra_headers         TEXT NOT NULL DEFAULT '{}',
  enabled               INTEGER NOT NULL DEFAULT 1,
  priority              INTEGER NOT NULL DEFAULT 0,
  weight                INTEGER NOT NULL DEFAULT 1,
  supports_embeddings   INTEGER NOT NULL DEFAULT 0,
  passthrough           INTEGER NOT NULL DEFAULT 1,
  force_upstream_stream INTEGER NOT NULL DEFAULT 0,
  created_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL
);

INSERT INTO channels_new (id, provider_id, name, protocol, base_url, chat_path, auth_style,
                          responses_path, extra_headers, enabled, priority, weight,
                          supports_embeddings, passthrough, force_upstream_stream,
                          created_at, updated_at)
SELECT id, provider_id, name, protocol, base_url, chat_path, auth_style,
       responses_path, extra_headers, enabled, priority, weight,
       supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at
FROM channels;

DROP TABLE channels;
ALTER TABLE channels_new RENAME TO channels;
CREATE INDEX idx_channels_enabled ON channels(enabled, priority DESC, weight);

CREATE TABLE models (
  id                TEXT NOT NULL,
  provider_id       INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  upstream_model    TEXT NOT NULL DEFAULT '',
  display_name      TEXT NOT NULL DEFAULT '',
  context_window    INTEGER,
  max_output_tokens INTEGER,
  enabled           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  PRIMARY KEY (provider_id, id)
);
CREATE INDEX idx_models_model ON models(id);

INSERT INTO models (id, provider_id, upstream_model, display_name,
                    context_window, max_output_tokens, enabled, created_at, updated_at)
SELECT id, provider_id, upstream_model, display_name,
       context_window, max_output_tokens, enabled, created_at, updated_at
FROM models_stage;

DROP TABLE models_stage;
