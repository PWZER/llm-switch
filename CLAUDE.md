# CLAUDE.md

Guidance for Claude Code sessions working in this repository.

## Project Overview

llm-switch is a self-hosted unified LLM API gateway. It exposes **both** OpenAI-compatible
and Anthropic-compatible API surfaces on one port, routes requests to upstream vendors
(DeepSeek, Zhipu GLM, Kimi/Moonshot, Kimi For Coding, or any generic OpenAI/Anthropic-compatible
endpoint), converts across protocols when needed, transparently passes bytes through when
protocols match, and records usage stats for every request. Configuration lives in a Web UI;
everything ships as a single Go binary with the frontend embedded.

## Language Rule

All code, comments, docs, commit messages, and API copy are **in English**. Keep vendor
names and protocol terms verbatim (DeepSeek, Zhipu, GLM, Kimi, Moonshot, Anthropic,
chat.completion.chunk, tool_use, reasoning_content, ...).

## Tech Stack (fixed by design — do not swap without strong reason)

- **Go 1.25+**, module `github.com/PWZER/llm-switch`
- Router: `github.com/go-chi/chi/v5` — plain `net/http` handlers, group middleware
- SQLite: `modernc.org/sqlite` (pure Go, no CGO), WAL + `busy_timeout`, via `database/sql`
- Migrations: embedded SQL runner (`internal/store/migrations/*.sql` + `schema_migrations` table)
- Frontend: React 18 + Vite + TypeScript + Ant Design v5, plain fetch + `useApi` hook
  (no React Query), embedded via `go:embed` into `internal/web/dist`

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
(`internal/ir`). Two codecs only (`internal/protocol/openai`, `internal/protocol/anthropic`)
implement the `Codec` interface; cross-protocol = client codec → IR → upstream codec.
Same protocol → **passthrough fast path** (byte relay with a usage tap), skipping the IR.

Data model (SQLite, `internal/store/migrations/0001_init.sql`):

```
providers (vendor account)          keys are shared by all its channels
  └─ provider_keys (weight/enable)  weighted round-robin + in-memory cooldown
  └─ channels (protocol, base_url, chat_path, auth_style, priority, weight)
       └─ channel_models (model → upstream_model)   ← the routing table
aliases (stable client-facing name → channel + upstream_model)  ← hot-switchable
models   (merged registry served at GET /v1/models)
api_keys (client-facing gateway keys, sha256-hashed, shown once)
request_logs (one row per request; async batched writes)
```

Hot reload: `providers/channels/keys/bindings/aliases` are loaded into an **immutable
snapshot** swapped via `atomic.Pointer` after every admin mutation. Requests load the
snapshot once and pin their resolution; in-flight requests are never affected by config
changes. Key cooldown/rotation state lives outside the snapshot (in-memory, keyed by key ID).

Routing precedence: explicit **alias** → `channel_models` rows (priority DESC groups,
weighted RR within group) → protocol-shaped 404. Aliases are listed in `GET /v1/models`
on both surfaces.

Admin API (`/api/v1`): envelope `{code, msg, data, request_id}` (ULID request id,
echoable via `X-Request-Id`); action-suffix endpoints (`POST /providers/{id}:refresh`,
`POST /aliases/{name}:switch`); segmented error codes 400xx/401xx/404xx/409xx/429xx/500xx
plus 6xxxx = upstream-passthrough error preserving the upstream body.

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
12. **Model refresh upserts, never replaces** — manual `models` entries and `enabled`
    flags survive provider refreshes; the merged `/v1/models` list is deduplicated.

## Provider Presets (docs-verified base URLs)

| Vendor | OpenAI channel | Anthropic channel |
| --- | --- | --- |
| Zhipu (GLM) | `https://open.bigmodel.cn/api/paas/v4` | `https://open.bigmodel.cn/api/anthropic` |
| DeepSeek | `https://api.deepseek.com` | `https://api.deepseek.com/anthropic` |
| Kimi/Moonshot | `https://api.moonshot.ai/v1` (cn `api.moonshot.cn/v1`) | `https://api.moonshot.ai/anthropic` (cn mirror) |
| Kimi For Coding | `https://api.kimi.com/coding/v1` | `https://api.kimi.com/coding/` (trailing slash significant) |

Presets are declarative data in `internal/provider/presets.go` (form prefill only — all
fields stay editable). Generic OpenAI/Anthropic-compatible types cover everything else.
Explicit `chat_path` on channels avoids base-URL join ambiguity.

## Testing Conventions

- Golden converter tests in `testdata/cases/` cover the full 2×2 protocol matrix ×
  (request | response | stream): text, system, multi-turn, tool calls (single/parallel/
  truncated partial-JSON), reasoning, images, usage/cache tokens, stop-reason mapping.
- Integration tests use `httptest` + scripted fake upstreams (`testtools/`):
  429-then-200 failover, cooldown assertions, mid-stream abort → terminal error event,
  adversarial SSE (>64 KB fragments, CRLF, keep-alives, truncated `[DONE]`).
- `cmd/mockupstream` backs the manual e2e recipe (see README): seed channel via admin
  API, `curl -N` both surfaces, check logs/stats.
- Run `make test` (race detector on) before considering gateway changes done.
