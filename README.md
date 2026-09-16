# llm-switch

A self-hosted unified LLM API gateway. One local endpoint serves **both** OpenAI-compatible
and Anthropic-compatible APIs, routes to multiple upstream vendors (DeepSeek, Zhipu GLM,
Kimi/Moonshot, or any OpenAI/Anthropic-compatible service), converts across protocols
transparently, and records usage for every request. Ships as a single Go binary with the
admin UI embedded.

```
Agent (Claude Code, OpenAI SDK, ...)          llm-switch                    Upstream
  POST /v1/messages (Anthropic)  ──►  parse → route → adapt → bridge  ──►  DeepSeek   (OpenAI wire)
  POST /v1/chat/completions      ──►         │ passthrough fast path     ──►  Zhipu GLM  (Anthropic wire)
  GET  /v1/models                ──►         ▼ usage stats                    Kimi ...
```

## Features

- **Dual protocol surfaces** on one port: `POST /v1/chat/completions`, `POST /v1/messages`,
  `GET /v1/models`, `POST /v1/messages/count_tokens`, `POST /v1/embeddings`.
- **Cross-protocol conversion** through an internal IR: Anthropic requests can ride
  OpenAI-compatible upstreams and vice versa — including streaming SSE, tool calls
  (arguments streamed incrementally), thinking/reasoning, images, and usage/cache tokens.
- **Passthrough fast path** when the client protocol matches the channel: raw byte relay
  with only the model field rewritten, with a usage tap for stats.
- **Routing**: model → channel bindings with priority (failover order) and weighted
  round-robin; automatic failover on 429/5xx/network errors (before the first byte);
  multi-key pools per provider with cooldown honoring `Retry-After`.
- **Model aliases (hot-switching)**: stable client-facing names (e.g. `main`) re-pointable
  at runtime from the Web UI — agent configs never change. All admin config applies
  immediately via atomic snapshot reload, no restarts.
- **Dynamic model lists**: fetch each provider's `/models` from the UI, merge into
  `GET /v1/models` (OpenAI and Anthropic response shapes auto-detected).
- **Usage statistics**: per-request logs (tokens incl. cache/reasoning, latency,
  first-token latency, attempts, error types) plus dashboard aggregations. Async batched
  writes never block the hot path.
- **Client API keys**: issue gateway keys to agents; keys are stored hashed and shown once.

## Quick start

Requirements: Go 1.25+ and Node 20+ (only for building the UI).

```bash
make build          # builds the frontend, embeds it, produces bin/llm-switch
LLM_SWITCH_ADMIN_PASSWORD=secret ./bin/llm-switch -addr 127.0.0.1:8080
# open http://127.0.0.1:8080  (login with the admin password)
```

On first boot, if `LLM_SWITCH_ADMIN_PASSWORD` is unset, a random password is generated and
logged once.

### Configure in the Web UI

1. **Providers** → create a provider, add its API key(s).
2. **Channels** → add one channel per endpoint. A vendor account often speaks both
   protocols, so add both channels and pick per need:
   | Vendor | OpenAI channel | Anthropic channel |
   | --- | --- | --- |
   | DeepSeek | `https://api.deepseek.com` (`/chat/completions`) | `https://api.deepseek.com/anthropic` (`/v1/messages`) |
   | Zhipu GLM | `https://open.bigmodel.cn/api/paas/v4` | `https://open.bigmodel.cn/api/anthropic` |
   | Kimi/Moonshot | `https://api.moonshot.ai/v1` | `https://api.moonshot.ai/anthropic` |
   | Kimi For Coding | `https://api.kimi.com/coding/v1` | `https://api.kimi.com/coding/` |
   Bind client-facing model names to upstream names on each channel.
3. **Aliases** → create `main` (or override well-known names) and point it anywhere;
   flip it anytime.
4. **Client Keys** → issue a key for your agent (shown once).

### Point your agents at it

```bash
# Claude Code / Anthropic SDK
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_API_KEY=sk-lsw-...

# OpenAI SDK
base_url = "http://127.0.0.1:8080/v1"
api_key  = "sk-lsw-..."
```

Model names resolve in this order: alias → channel binding → 404.

## Development

```bash
make dev        # Go backend on :8080 + Vite dev server on :5173 (proxied, no CORS)
make test       # go test ./... -race
make mock       # canned OpenAI+Anthropic upstream on :9091 for manual e2e
make build      # production single binary
```

End-to-end without real provider keys:

```bash
make mock &                                        # fake upstream on :9091
./bin/llm-switch -addr :8080 &
# in the UI: provider (base_url http://127.0.0.1:9091) → channel → binding
#            model "mock-chat" → client key
curl -N localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-lsw-..." \
  -d '{"model":"mock-chat","stream":true}'
```

## Architecture

Pipeline per request: **parse → route → adapt → bridge** around a canonical IR
(`internal/ir`). Two codecs (`internal/protocol/openai`, `internal/protocol/anthropic`)
implement request/response/stream conversion in both directions; the IR hub means
N client protocols × M upstream protocols need N+M converters. Same-protocol traffic
skips the IR entirely (byte passthrough with a usage tap).

Routing configuration lives in SQLite (WAL) and is served from an immutable in-memory
snapshot swapped atomically after every admin mutation — in-flight requests pin their
resolution, so config changes apply to the next request without restarts or dropped
streams.

Key invariants (enforced in code, see CLAUDE.md for the full list): failover only before
the first forwarded byte; SSE parsed line-wise (no 64 KB scanner limit); thinking blocks
replayed only when signed; tool-call IDs passed through verbatim; no global HTTP client
timeout (streams live as long as needed, bounded by an idle watchdog).

## Security notes

- Provider API keys are stored in SQLite (plaintext by necessity — they are sent
  upstream). Keep the data directory permission-tight (the binary sets `0700`).
- Client gateway keys are stored as SHA-256 hashes; the plaintext is shown exactly once.
- The admin UI is a single password + bearer session. Run it on loopback or behind
  authenticated TLS; the gateway makes no attempt to be multi-tenant.

## License

MIT (or your preferred license — adjust before publishing).
