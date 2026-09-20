# llm-switch

A self-hosted unified LLM API gateway. One local endpoint serves **both** OpenAI-compatible
and Anthropic-compatible APIs, routes to multiple upstream vendors (DeepSeek, Zhipu GLM,
Kimi/Moonshot, or any OpenAI/Anthropic-compatible service), converts across protocols
transparently, and records usage for every request. Ships as a single Go binary with the
admin UI embedded.

```
Agent (Claude Code, Codex CLI, OpenAI SDK, ...)   llm-switch                    Upstream
  POST /v1/messages (Anthropic)  ──►  parse → route → adapt → bridge  ──►  DeepSeek   (OpenAI wire)
  POST /v1/chat/completions      ──►         │ passthrough fast path     ──►  Zhipu GLM  (Anthropic wire)
  POST /v1/responses             ──►         ▼ usage stats                    Kimi ...
  GET  /v1/models                ──►                                          OpenAI  (Responses, passthrough)
```

## Features

- **Three client surfaces** on one port: `POST /v1/chat/completions` (OpenAI chat),
  `POST /v1/responses` (OpenAI Responses API, stateless), `POST /v1/messages` (Anthropic),
  plus `GET /v1/models` (both shapes, paginated), `POST /v1/messages/count_tokens`,
  `POST /v1/embeddings`.
- **Cross-protocol conversion** through an internal IR: Responses-only clients (e.g.
  Codex CLI) can ride chat-completions and Anthropic upstreams, Anthropic requests can
  ride OpenAI-compatible upstreams and vice versa — including streaming SSE, tool calls
  (arguments streamed incrementally), thinking/reasoning, images, and usage/cache tokens.
- **Passthrough fast path** when the wire shapes match: raw byte relay with only the
  model field rewritten, with a usage tap for stats. This covers same-protocol traffic
  and OpenAI channels with an explicit `responses_path` (upstream Responses API).
- **Routing**: model → channel bindings with priority (failover order) and weighted
  round-robin; automatic failover on 429/5xx/network errors (before the first byte);
  multi-key pools per provider with cooldown honoring `Retry-After`.
- **Model routes (hot-switching)**: stable client-facing names (e.g. `main`) re-pointable
  at runtime from the Web UI — agent configs never change. All admin config applies
  immediately via atomic snapshot reload, no restarts.
- **Dynamic model discovery**: `GET /v1/models` serves the merged registry (bindings +
  model routes + manual entries) in OpenAI and Anthropic shapes, with `limit`/`after_id`
  pagination and `GET /v1/models/{id}`; entries carry `context_length` and
  `max_output_tokens` from the model registry. Anthropic-served models
  additionally list `claude-<id>` mirrors and — when the registry context
  window reaches 1,000,000 — `<id>[1m]` entries (Claude Code's 1M-context
  marker) on the Anthropic shape.
- **Account usage / balance probes**: declared per account (preset-prefilled where an
  endpoint is known — DeepSeek and Kimi/Moonshot balance, Zhipu GLM coding-plan quota),
  queried on demand from the Account Pool page with progress bars, quota windows
  (5-hour / weekly / monthly) and reset times.
- **Usage statistics**: per-request logs (tokens incl. cache/reasoning, latency,
  first-token latency, attempts, error types) plus dashboard aggregations. Async batched
  writes never block the hot path.
- **Client API keys**: issue gateway keys to agents; keys are stored hashed and shown once.
- **Usage statistics**: per-request logs (tokens incl. cache/reasoning, latency,
  first-token latency, attempts, error types) plus dashboard aggregations. Async batched
  writes never block the hot path.
- **Client API keys**: issue gateway keys to agents; keys are stored hashed and shown once.

## Quick start

Requirements: Go 1.25+ and Node 20+ (only for building the UI).

```bash
make build          # builds the frontend, embeds it, produces bin/llm-switch
LLM_SWITCH_ADMIN_PASSWORD=secret ./bin/llm-switch
# open http://127.0.0.1:8901  (login with the admin password)
```

On first boot, if `LLM_SWITCH_ADMIN_PASSWORD` is unset, a random password is generated and
logged once. First boot also seeds six built-in providers (Zhipu GLM, DeepSeek,
Kimi/Moonshot, Kimi For Coding, OpenAI, Anthropic) with their docs-verified endpoints and
identity model bindings — just add an API key on the Account Pool page to start routing.
Deleting them is permanent (the seed runs once, tracked by the `seeded_providers` setting).

### Run with Docker

No local Go/Node toolchain needed — the image is a multi-stage build
(Node 24 → Go → `alpine:latest`):

```bash
docker build -t llm-switch .
docker run -d --name llm-switch -p 8901:8901 \
  -e LLM_SWITCH_ADMIN_PASSWORD=secret \
  -v llm-switch-data:/data \
  llm-switch
# open http://127.0.0.1:8901
```

The SQLite database lives in `/data` — keep the volume. The container runs the
gateway in the foreground; `docker run -d` is the daemonization. Stamp a
release version with `docker build --build-arg VERSION=1.2.3 .`.

### Run as a daemon (bare metal)

```bash
./bin/llm-switch -daemon        # detaches; logs to <data-dir>/llm-switch.log
kill $(cat ~/.llm-switch/llm-switch.pid)
```

Startup failures (port busy, already running) are reported on the terminal with
a non-zero exit code. A single-instance flock (`<data-dir>/llm-switch.lock`) is
held in every mode — foreground included — so a second process on the same data
dir always fails with "already running".

### Configure in the Web UI

1. **Providers** → the seeded vendors are ready; create custom providers and endpoint
   channels the same way. A vendor account often
   speaks both protocols, so add both channels and pick per need:
   | Vendor | OpenAI channel | Anthropic channel |
   | --- | --- | --- |
   | DeepSeek | `https://api.deepseek.com` (`/chat/completions`) | `https://api.deepseek.com/anthropic` (`/v1/messages`) |
   | Zhipu GLM | `https://open.bigmodel.cn/api/paas/v4` | `https://open.bigmodel.cn/api/anthropic` |
   | Kimi/Moonshot | `https://api.moonshot.cn/v1` | `https://api.moonshot.cn/anthropic` |
   | Kimi For Coding | `https://api.kimi.com/coding/v1` | `https://api.kimi.com/coding/` |
   Bind client-facing model names to upstream names on each channel. Optional per
   channel: a `responses_path` (e.g. `/responses`) marks the upstream as serving the
   OpenAI Responses API natively — `/v1/responses` traffic is then relayed byte-wise
   instead of converted. Channel **Test** is connectivity-only (URL reachability, no
   credential checked).
2. **Account Pool** → add accounts (API key + weight + usage probes) bound to a
   provider. The gateway rotates requests across a provider's enabled accounts;
   per-account **Test** (quick = list models, deep = mini chat) validates the key,
   and per-account **Usage** reports balance/quota. Model Routes can pin a target
   to a specific account.
3. **Model Routes** → create `main` (or override well-known names) and point it anywhere;
   flip it anytime.
4. **Client Keys** → issue a key for your agent (shown once).

### Point your agents at it

```bash
# Claude Code / Anthropic SDK
export ANTHROPIC_BASE_URL=http://127.0.0.1:8901
export ANTHROPIC_API_KEY=sk-lsw-...

# OpenAI SDK
base_url = "http://127.0.0.1:8901/v1"
api_key  = "sk-lsw-..."

# Codex CLI (OpenAI Responses API)
#   model provider: base_url http://127.0.0.1:8901/v1  (POST /v1/responses)
#   stateless only: store=false; previous_response_id / background are rejected
```

Model names resolve after stripping the `[1m]` context marker: model route → channel binding → `claude-`-stripped retry → 404.

## Development

```bash
make dev        # Go backend on :8901 + Vite dev server on :5173 (proxied, no CORS)
make test       # go test ./... -race
make mock       # canned OpenAI+Anthropic upstream on :9091 for manual e2e
make build      # production single binary
```

End-to-end without real provider keys:

```bash
make mock &                                        # fake upstream on :9091
./bin/llm-switch &
# in the UI: provider (base_url http://127.0.0.1:9091) → channel → binding
#            model "mock-chat" → client key
curl -N localhost:8901/v1/chat/completions \
  -H "Authorization: Bearer sk-lsw-..." \
  -d '{"model":"mock-chat","stream":true}'
```

## Architecture

Pipeline per request: **parse → route → adapt → bridge** around a canonical IR
(`internal/ir`). Three codecs (`internal/protocol/openai` chat,
`internal/protocol/anthropic`, `internal/protocol/responses`) implement
request/response/stream conversion in both directions; the IR hub means
N client protocols × M upstream protocols need N+M converters. Wire-identical traffic
skips the IR entirely (byte passthrough with a usage tap): same-protocol requests, and
`/v1/responses` requests hitting a channel whose `responses_path` is set.

Routing configuration lives in SQLite (WAL) and is served from an immutable in-memory
snapshot swapped atomically after every admin mutation — in-flight requests pin their
resolution, so config changes apply to the next request without restarts or dropped
streams.

Key invariants (enforced in code, see CLAUDE.md for the full list): failover only before
the first forwarded byte; SSE parsed line-wise (no 64 KB scanner limit); thinking blocks
replayed only when signed; tool-call IDs passed through verbatim; no global HTTP client
timeout (streams live as long as needed, bounded by an idle watchdog).

## Security notes

- Account API keys are stored in SQLite (plaintext by necessity — they are sent
  upstream). Keep the data directory permission-tight (`~/.llm-switch` by default;
  the binary sets `0700`).
- Client gateway keys are stored as SHA-256 hashes; the plaintext is shown exactly once.
- The admin UI is a single password + bearer session. The default listen address
  (`:8901`) binds all interfaces — restrict it with `-addr 127.0.0.1:8901` or run
  behind authenticated TLS; the gateway makes no attempt to be multi-tenant.

## License

MIT (or your preferred license — adjust before publishing).
