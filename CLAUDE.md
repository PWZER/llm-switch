# CLAUDE.md

Guidance for Claude Code sessions working in this repository.

## Project Overview

llm-switch is a self-hosted unified LLM API gateway. It exposes OpenAI chat, OpenAI
Responses, and Anthropic API surfaces on one port under explicit protocol base_url
prefixes (`/openai/v1/*`, `/anthropic/v1/*`; unprefixed `/v1/*` kept for existing
clients), routes requests to upstream vendors
(DeepSeek, Zhipu GLM, Kimi/Moonshot, Kimi For Coding, or any generic OpenAI/Anthropic-compatible
endpoint), converts across protocols when needed, transparently passes bytes through when
wire shapes match, and records usage stats for every request. It also probes per-account
coding-plan usage / balance endpoints (preset-declared) for the admin UI. Configuration
lives in a Web UI; everything ships as a single Go binary with the frontend embedded.

## Language Rule

All code, comments, docs, commit messages, and API copy are **in English**. Keep vendor
names and protocol terms verbatim (DeepSeek, Zhipu, GLM, Kimi, Moonshot, Anthropic,
chat.completion.chunk, tool_use, reasoning_content, ...).

## Git Conventions

- Commit messages are plain text: no attribution trailers (`Co-Authored-By`,
  `Generated with ...`, etc.).

## Tech Stack (fixed by design — do not swap without strong reason)

- **Go 1.25+**, module `github.com/PWZER/llm-switch`
- Router: `github.com/go-chi/chi/v5` — plain `net/http` handlers, group middleware
- SQLite: `modernc.org/sqlite` (pure Go, no CGO), WAL + `busy_timeout`, via `database/sql`
- Migrations: embedded SQL runner (`internal/store/migrations/*.sql` + `schema_migrations` table)
- Frontend: React 18 + Vite + TypeScript + Ant Design v5, plain fetch via
  `web/src/api/client.ts` (`api.get/post/put/del`, envelope-unwrapping) called in
  per-page `useCallback`+`useEffect` loaders (no React Query), embedded via `go:embed`
  into `internal/web/dist`

Total Go deps stay minimal: chi, modernc.org/sqlite, golang.org/x/crypto (bcrypt).

## Commands

```bash
make dev       # go run ./cmd/llm-switch -web-dev  (+ cd web && npm run dev in a second terminal)
make frontend  # npm ci + build, copy web/dist → internal/web/dist
make build     # frontend + CGO_ENABLED=0 go build -o bin/llm-switch ./cmd/llm-switch
make test      # go test ./... -race
make mock      # go run ./cmd/mockupstream -addr :9091   (fake OpenAI+Anthropic upstream for e2e)
```

## Architecture Essentials

Pipeline per request: **parse → route → adapt → bridge** around an internal IR
(`internal/ir`). Three codecs (`internal/protocol/openai` chat, `internal/protocol/anthropic`,
`internal/protocol/responses`) implement the `Codec` interface; cross-protocol = client
codec → IR → upstream codec. Wire-identical traffic → **passthrough fast path** (byte
relay with a usage tap), skipping the IR: same-protocol pairs, plus `/v1/responses`
requests to openai channels with an explicit `responses_path` (upstream Responses API;
the tap protocol switches to `openai-responses` so usage parses `response.completed`).
`openai-responses` is a client-surface protocol only — `channels.protocol` stays
`'openai' | 'anthropic'` (SQL CHECK unchanged).

Data model (SQLite, `internal/store/migrations/0001_init.sql` +
`0002_provider_scoped_models.sql`):

```
providers (vendor account: name, models_url + endpoints)   accounts are shared by all its channels
  └─ accounts (label, api_key, weight, enabled, usage_probes JSON)  weighted RR + in-memory cooldown
  └─ channels (protocol, base_url, chat_path, responses_path?, auth_style, priority, weight)
  └─ models (client-facing id + upstream_model alias, PK (provider_id, id))  ← registry = routing table
model_routes (client-facing name → provider + upstream_model [+ channel pin] [+ account_id pin])  ← hot-switchable
api_keys (client-facing gateway keys, sha256-hashed, shown once)
request_logs (one row per request, account_id/account_name attribution; async batched writes)
```

One models row = one provider serving one client-facing model name
(`upstream_model` empty = identity mapping) — every live channel of the
provider can serve it. The registry and the routing table are the same table,
so "listed but unroutable" / "routable but unlisted" states cannot exist.
`providers.models_url` is the absolute upstream models-list URL, configured
explicitly — never derived from endpoint base URLs. Migration 0002 folds the
old `channel_models` bindings into provider-scoped rows (metadata inherited
from the old registry rows; first binding alias wins; unbound manual rows
dropped), backfills `models_url` from the first configured channel, and
rebuilds `channels` without the `auto_bind`/`models_url` columns. Deleting a
provider cascade-deletes its model rows (FK) through channels.

Responsibility split: providers only configure endpoints — channel **test** is pure
connectivity (any HTTP response, incl. 401, = reachable; no credential sent).
Accounts carry the API key: two-tier account test (`quick` = GET /models with the
key, zero tokens; `deep` = real `max_tokens:1` mini chat), per-account usage/balance
probes, and every key-consuming admin action (refresh-models / models-preview /
form probe) requires an **explicit `account_id` or pasted `api_key`** — no silent
first-enabled fallback. Model route targets always carry `provider_id`; `channel_id`
is an optional endpoint pin (JSON `null` = auto-select among the provider's enabled
channels), and `account_id` may pin one account (validated to belong to the target's
provider); a disabled pinned account makes the target unroutable. Deleting a pinned
channel degrades its target to provider auto-select (provider backfilled); deleting
the provider strips its targets.

Model rows reach a provider only through explicit admin action:
**refresh-models** fetches the provider's `models_url` once (auth style from
its first enabled channel, openai preferred; a missing `models_url` is a
40901 telling the admin to configure it first) and returns the fetched list
WITHOUT registering — nothing is added; **sync-models** (`POST
/providers/{id}/sync-models`, body `{register: [{id, context_length?,
max_output_tokens?}], remove: [id...]}`) commits the admin UI's selection as
an explicit diff: register entries EnsureModel provider-scoped identity rows
(insert-if-missing, NULL-limit backfill only — manual edits survive), remove
entries delete rows by (provider_id, id) including historical rows the
upstream no longer lists; **models-preview** is display-only — the provider
drawer shows the fetched ids for per-model selection and sends the checked
ones as `register_models` on provider save; the
**Models page** creates rows directly (provider multi-select: one row per
selected provider). New providers start with an empty registry by design.

Hot reload: `providers/channels/accounts/models/model_routes` are loaded into an **immutable
snapshot** swapped via `atomic.Pointer` after every admin mutation. Requests load the
snapshot once and pin their resolution; in-flight requests are never affected by config
changes. Account cooldown/rotation state lives outside the snapshot (in-memory, keyed by account ID).

Routing precedence: strip a trailing `[1m]` (reserved listing marker, never part of a
routing identity) → explicit **model route** → enabled **models rows** for the name
(each row expands to one candidate per live channel of its provider; channel priority
DESC groups, weighted RR within group; each candidate carries the row's upstream alias)
→ `claude-`-stripped name retried against routes then rows →
protocol-shaped 404. Row-derived candidates are **protocol-filtered by the client
surface** (`openai-responses` prefers `openai`): same-protocol candidates win
outright, cross-protocol bridging serves only when no same-protocol candidate
exists. Route targets pinning an explicit channel are explicit configuration and
never filtered; a provider-scoped route target (no `channel_id`) expands to one
candidate per live channel of its provider (priority DESC, weight DESC) with the
same same-protocol-first preference applied within its own segment only. A name with
no enabled row and no route is neither listed nor routable. Model routes are
listed in `GET /v1/models` on both surfaces; route
names are canonical — the admin API rejects the `[1m]` suffix.

Protocol surfaces mount under explicit base_url prefixes: `/anthropic/v1/*`
(messages, count_tokens, models) and `/openai/v1/*` (chat/completions, responses,
embeddings, models) pin both the `/v1/models` listing shape and the auth-failure
error shape to the prefix (`Gateway.MountAnthropic` / `MountOpenAI` +
`ClientKeyAuthFor` in the gateway package) — the base_url, not request headers,
is the shape control point (Claude Code only exposes `ANTHROPIC_BASE_URL`, so
its models URL is always `<base_url>/v1/models`). The unprefixed `/v1` mount
keeps serving every endpoint with the historical header-sniffed models shape
(`anthropicShapeRequest`: `x-api-key` or `anthropic-version` → Anthropic shape).

GET /v1/models visibility rules: listed ⇔ routable (the invariant is exact on
the OpenAI shape; on the Anthropic shape a mirrored name is listed — and
by-id addressable — only in its claude-prefixed form, though it stays
routable bare) — the merged list dedupes
by name over enabled models rows on live providers (later duplicates only fill
blank metadata), plus model route names. Disabling a row removes both its
list entry and its candidacy. Each entry carries `description` =
`{[route] |[model] }{provider}/{account}` — an explicit source
marker (`[route] ` for model-route entries, including names that also have a
models row since the route wins at resolve time; `[model] ` for plain
registry rows), the provider, and the account that would serve
first (route-pinned, else the first enabled account; omitted when the provider
has none). The channel name is deliberately omitted — every live channel of
the provider can serve the name, so naming the first one reads as a protocol
mark; names no channel serves fall back to the row's provider
name — Claude Code's /model picker renders it instead of
"From gateway". Claude Code's gateway discovery
silently drops ids without a claude/anthropic substring (verified on 2.1.273),
so the Anthropic-shaped /v1/models lists synthetic `claude-<id>` mirrors
(`snap.DiscoveryVariants`, provider + limits copied, collisions
skipped) **instead of** the plain ids they mirror: `anthropicListing` keeps
every mirror and drops any plain id (plain or `<id>[1m]`) whose exact
`claude-<id>` mirror exists. Ids without a mirror — already
claude/anthropic-named, openai-only, or collision-suppressed by a hand-named
`claude-<id>` row — list plainly. Both decoration families — `claude-<id>`
mirrors and `<id>[1m]`
context entries (`snap.ContextVariants`, derived from the merged
`context_window >= 1,000,000`) —
are generated only for **anthropic-served** names (>= 1 live anthropic
channel via models rows or route targets) and emitted only on the Anthropic
shape; `context_window` on the models row is the only control point. The
`claude-` mirror and the `[1m]` marker are independent at generation time,
and a 1m-capable name lists **only `claude-<id>[1m]`** — the plain id, its
plain `claude-<id>` mirror, and the bare `<id>[1m]` id are all superseded
once the `[1m]` variant exists (the marked id routes to the same identity via
suffix stripping, and by-id lookups follow the listing).
Routing accepts `claude-<name>` by stripping the prefix as the last fallback
(after literal `claude-*` routes and rows) so client caches keep
working. `request_logs.model` records the canonical resolved name (post
`[1m]`/`claude-` normalization; the route's own name for route hits), so
decorated discovery ids aggregate into one model in stats; unroutable names
log verbatim.

Admin API (`/api/v1`): envelope `{code, msg, data, request_id}` (ULID request id,
echoable via `X-Request-Id`); action endpoints use plain path suffixes
(`POST /providers/{id}/refresh-models`, `/probe`, `/models-preview`,
`POST /accounts/{id}/test`, `/usage`, `POST /channels/{id}/test`,
`POST /model-routes/{name}/switch`); segmented error codes
400xx/401xx/404xx/409xx/429xx/500xx plus 6xxxx = upstream-passthrough error preserving
the upstream body. Any non-GET admin request that returns <400 triggers a snapshot
reload (`reloadAfterMutation`) — read-only POST actions tolerate the rebuild cost.

## Critical Invariants (violating these causes real bugs)

1. **Failover only before the first byte is forwarded to the client.** Read the upstream
   status line + headers first; once streaming started, failures become in-band terminal
   error events — never retry (double-billing / corrupted stream).
2. **SSE parsing must use `bufio.Reader.ReadBytes('\n')`**, never `bufio.Scanner`
   (64 KB token limit truncates large tool-argument fragments). Handle CRLF, multi-line
   `data:`, `:`-prefixed keep-alive comments.
3. **Never wrap `/v1/*` in chi `middleware.Timeout`** — it kills long streams. Deadlines
   are per-request context only (non-stream cap; stream idle watchdog).
4. **No global `http.Client.Timeout`** on the upstream client (kills streams). Use
   transport timeouts + context deadlines.
5. **Thinking blocks are asymmetric.** Anthropic upstreams reject unsigned assistant
   `thinking` blocks on replay — pass through only signed (Anthropic-originated) blocks.
   DeepSeek/Zhipu reject `reasoning_content` in request history — always strip it when
   encoding OpenAI-shape requests. Never fabricate `signature`.
6. **Tool-call IDs pass through verbatim** (`call_…` ↔ `toolu_…`). OpenAI `arguments` is
   a JSON *string*; Anthropic `tool_use.input` is an object — convert, never rewrite IDs.
7. **Anthropic SSE strictness:** contiguous `content_block` indexes, start/stop pairing,
   `message_stop` always emitted (after `error` when applicable); usage rides
   `message_delta`, so the OpenAI→Anthropic bridge buffers the final event until the
   usage chunk or `[DONE]` arrives.
8. **`max_tokens` is required by Anthropic upstreams** — inject `default_max_tokens`
   (8192) when an OpenAI client omits it. Clamp `temperature ≤ 1` for Anthropic-bound
   requests.
9. **Header hygiene:** strip inbound `Authorization` / `x-api-key` before upstream calls;
   set upstream auth per channel `auth_style`; merge `extra_headers` last; forward
   `anthropic-beta` only to Anthropic upstreams.
10. **SQLite is single-writer:** write pool `MaxOpenConns(1)`, stats go through one
    batching goroutine (chan cap 4096, drop + counter on overflow), no per-request writes
    on the hot path.
11. **Secrets:** client gateway keys stored as SHA-256 hashes + display prefix (plaintext
    returned exactly once at creation). Provider keys are plaintext by necessity (sent
    upstream) — keep the data dir 0600 and mask keys in every admin API response.
12. **Model refresh upserts, never replaces** — refresh fetches the provider's
    configured `models_url` once (never derived/guessed from endpoint URLs) and
    upserts provider-scoped rows; existing rows keep manual edits
    (`enabled`, alias, limits — refresh only backfills NULL limits); deleting a
    provider cascade-deletes its rows; the merged `/v1/models`
    list is deduplicated by name (later rows only fill blank metadata fields).
13. **Responses surface is stateless.** `store:true` / `previous_response_id` /
    `background` / `conversation` are rejected in `serve` BEFORE routing (the
    passthrough path never decodes the body). The responses renderer must keep
    `output_index` contiguous, emit `output_item.added/done` pairs, always end with
    `response.completed`/`incomplete`/`failed`, and keep `Start()` side-effect-free
    (the relay probes it on a throwaway instance). Only the responses codec may set
    `ir.Request.ResponseFormat` — teaching the chat decoder it would make
    chat→anthropic bridging regress from silently-dropped to 400.
14. **Usage tap is protocol-gated.** `tapStreamLine` keys responses parsing on the
    `protocol` argument (`openai-responses`); never extend the shared generic usage
    struct with Responses field names — that would silently start harvesting tokens
    from anthropic passthrough bodies.

## Seeded Vendor Endpoints (docs-verified base URLs)

| Vendor | OpenAI channel | Anthropic channel |
| --- | --- | --- |
| Zhipu (GLM) | `https://open.bigmodel.cn/api/paas/v4` | `https://open.bigmodel.cn/api/anthropic` |
| DeepSeek | `https://api.deepseek.com` | `https://api.deepseek.com/anthropic` |
| Kimi/Moonshot | `https://api.moonshot.cn/v1` (cn `api.moonshot.cn/v1`) | `https://api.moonshot.cn/anthropic` (cn mirror) |
| Kimi For Coding | `https://api.kimi.com/coding/v1` | `https://api.kimi.com/coding/` (trailing slash significant) |

Generic OpenAI/Anthropic-compatible types cover everything else.
Explicit `chat_path` on channels avoids base-URL join ambiguity. The seeded OpenAI
channel carries `responses_path: /responses`.

First boot seeds six built-in vendors (Zhipu GLM, DeepSeek, Kimi/Moonshot, Kimi For
Coding, OpenAI, Anthropic) with these endpoints, their provider-level `models_url`
fetch URLs, and empty model lists —
`store.SeedDefaultProviders` in `internal/store/seed.go`, guarded by the
`seeded_providers` setting so user-deleted providers never resurrect. Vendor logos in the admin UI are inline SVGs in
`web/src/components/logo.tsx` (`vendorKeyOf` name matching; unknown names fall back
to a monogram avatar).

Usage/balance probe defaults (account-level, editable; type `balance` | `plan`,
absolute URL or origin-absolute path, auth `bearer` | `raw`):

| Vendor | Probe | Endpoint | Source |
| --- | --- | --- | --- |
| DeepSeek | balance | `GET https://api.deepseek.com/user/balance` | official docs |
| Kimi/Moonshot | balance | `GET https://api.moonshot.cn/v1/users/me/balance` | official docs |
| Zhipu GLM | plan | `GET https://open.bigmodel.cn/api/monitor/usage/quota/limit` | undocumented (Zhipu console's own endpoint; community-verified) |
| Kimi For Coding | — | none (kimi CLI exposes usage only via its local server) | — |

The GLM plan response carries `data.limits[]` (TOKENS_LIMIT / TIME_LIMIT / CREDIT_LIMIT
windows, `value`/`remaining` percentages, `nextResetTime` epoch ms) — parsed into
normalized windows with a raw-JSON fallback shown in the UI.

## Testing Conventions

- Converter/integration tests live next to the code under `internal/gateway/*_test.go`
  (package `gateway_test`): a `newHarness` builds a temp SQLite store + real engine
  holder + real gateway on `httptest`, with scripted fake upstreams from
  `internal/testutil` (`NewOpenAI`/`NewAnthropic` fakes serving `/chat/completions`,
  `/responses`, `/v1/messages`, `/models`; `FailFirstN(n)` scripts 429s for failover).
  Cross-protocol matrix tests (`conversion_test.go`), pipeline tests
  (`gateway_test.go`), Responses surface tests (`responses_test.go`), Models API tests
  (`models_test.go`).
- Codec unit tests: `internal/protocol/responses/*_test.go` (request decode,
  rejections, stream reader/renderer state machine, relay simulation); usage probe
  parsers: `internal/api/provider_usage_test.go`.
- `store_test.go` asserts the max applied migration version — bump it when adding
  `internal/store/migrations/000N_*.sql`.
- `cmd/mockupstream` backs the manual e2e recipe (see README): seed channel via admin
  API, `curl -N` all three surfaces, check logs/stats; it also serves fake balance and
  plan-quota routes for probe verification.
- Run `make test` (race detector on) before considering gateway changes done.
