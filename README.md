# llm-switch

A self-hosted unified LLM API gateway. One local endpoint serves **both** OpenAI-compatible
and Anthropic-compatible APIs, routes to multiple upstream vendors (DeepSeek, Zhipu GLM,
Kimi/Moonshot, or any OpenAI/Anthropic-compatible service), converts across protocols
transparently, and records usage for every request. Ships as a single Go binary with the
admin UI embedded.

```
Clients                          llm-switch :8901                 Upstreams
/anthropic            ┌─────────────────────────────────┐
  Claude Code ───────►│ parse → route → adapt → bridge  │─────► DeepSeek      (OpenAI)
/openai/v1            │                                 │
  OpenAI SDK  ───────►│ same wire  → byte passthrough   │─────► Zhipu GLM     (Anthropic)
  Codex CLI   ───────►│ diff wire  → convert via IR     │─────► Kimi/Moonshot (both)
/v1/* (legacy)        │                                 │
  any client  ───────►│ usage stats on every request    │─────► OpenAI        (Responses)
                      └─────────────────────────────────┘
```

## Features

- **Three client surfaces on one port**: Anthropic `/anthropic/v1/messages`, OpenAI
  `/openai/v1/chat/completions`, OpenAI Responses `/openai/v1/responses` (stateless),
  plus `count_tokens`, `embeddings`, and `GET /v1/models` in both shapes.
- **Cross-protocol conversion** through an internal IR — Anthropic clients can ride
  OpenAI upstreams and vice versa, with streaming SSE, tool calls, thinking/reasoning,
  images, and usage/cache tokens. Wire-identical traffic takes a byte-passthrough fast path.
- **Routing & failover**: weighted round-robin over an account pool, channel priority
  ordering, automatic failover on 429/5xx/network errors (always before the first
  forwarded byte), per-account cooldowns honoring `Retry-After`.
- **Model routes (hot-switching)**: stable client-facing names (e.g. `main`) re-pointable
  at runtime from the Web UI — agent configs never change, no restarts.
- **Model discovery**: `GET /v1/models` serves the registry in OpenAI and Anthropic
  shapes with pagination and per-model lookup; `claude-<id>` discovery mirrors and
  `[1m]` context entries are generated automatically for Claude Code.
- **Account usage / balance probes**: per-account balance (DeepSeek, Kimi/Moonshot) and
  coding-plan quota (Zhipu GLM), queried on demand from the Account Pool page.
- **Usage statistics**: per-request logs (tokens, latency, first-token latency, attempts,
  errors) plus dashboard aggregations; async batched writes never block the hot path.
- **Client API keys**: issue gateway keys to agents; stored hashed, shown once.
- **Config export / import**: JSON backup of providers, model routes, client keys, and
  settings — with a dry-run preview on import (Settings page).

## Install

Prebuilt binaries are attached to every [GitHub release](https://github.com/PWZER/llm-switch/releases)
(`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`), compressed with UPX
except `darwin-arm64`, which UPX cannot pack:

```bash
curl -fsSLO https://github.com/PWZER/llm-switch/releases/latest/download/llm-switch-linux-amd64
chmod +x llm-switch-linux-amd64 && mv llm-switch-linux-amd64 ~/.local/bin/llm-switch
llm-switch --version
```

To update an installed binary in place (and restart a running instance), use the
built-in self-update — see [CLI](#cli).

## Quick start

Requirements: Go 1.27+ and Node 20+ (only for building the UI).

```bash
make build          # builds the frontend, embeds it, produces bin/llm-switch
LLM_SWITCH_ADMIN_PASSWORD=secret ./bin/llm-switch
# open http://127.0.0.1:8901  (login with the admin password)
```

If `LLM_SWITCH_ADMIN_PASSWORD` is unset on first boot, a random password is generated
and logged once. All flags (`--addr`, `--data-dir`, `--admin-password`, `--log-format`)
can also come from `LLM_SWITCH_*` env vars. `/healthz` and `/readyz` are
unauthenticated probes for load balancers.

First boot seeds six built-in providers (Zhipu GLM, DeepSeek, Kimi/Moonshot, Kimi For
Coding, OpenAI, Anthropic) with docs-verified endpoints — their model registries start
**empty**: sync models in the UI, add an API key on the Account Pool page, and routing
starts. Deleting a seeded provider is permanent (the seed runs once).

### Run with Docker

```bash
docker build -t llm-switch .
docker run -d --name llm-switch -p 8901:8901 \
  -e LLM_SWITCH_ADMIN_PASSWORD=secret \
  -v llm-switch-data:/data \
  llm-switch
# open http://127.0.0.1:8901
```

The SQLite database lives in `/data` — keep the volume. Stamp a release version with
`docker build --build-arg VERSION=1.2.3 .`.

### Run as a daemon (bare metal)

```bash
./bin/llm-switch --daemon       # detaches; logs to <data-dir>/llm-switch.log
llm-switch status               # pid, version, args of the running instance
llm-switch stop                 # graceful stop (SIGTERM, waits for drain)
kill $(cat ~/.llm-switch/llm-switch.pid)   # still works: the pid file is a plain number
```

Startup failures (port busy, already running) exit non-zero with the reason on the
terminal. A single-instance lock is held in every mode, so a second process on the
same data dir always fails with "already running".

## CLI

The binary is one command with subcommands. The root command (default) boots the
server; all flags use the standard double-dash form, each with an `LLM_SWITCH_*`
environment fallback (e.g. `--data-dir` / `LLM_SWITCH_DATA_DIR`):

| Command | Purpose |
| --- | --- |
| *(root)* | Start the gateway (`--addr`, `--data-dir`, `--daemon`, `--log-format`, `--web-dev`, `--admin-password`) |
| `--version`, `-v` | Print the stamped version |
| `upgrade` | Self-update from GitHub Releases (see below) |
| `status` | Show whether an instance is running (exit code 1 when not) |
| `stop` | Stop the running instance; `--force` escalates to SIGKILL after the grace period |

Subcommand flags: `upgrade --check` (print versions only), `upgrade --yes`
(skip confirmation), `stop --force`; every subcommand also accepts the
persistent `--data-dir`.

### Self-update (`upgrade`)

`llm-switch upgrade` resolves the latest GitHub release, downloads the asset
matching this platform, verifies its size, and atomically renames it over the
running binary:

1. The latest tag is read from the `/releases/latest` redirect (no API rate
   limit); the REST API is the fallback and honors `GITHUB_TOKEN` / `GH_TOKEN`
   (useful on shared IPs, where the 60 req/h anonymous limit runs out).
2. Version comparison is numeric semver; `dev` or commit-SHA builds always
   proceed (with a warning).
3. If an instance is running (pid file + live process in the data dir), it is
   stopped gracefully (SIGTERM, up to 45 s for in-flight LLM streams to drain)
   and restarted **detached** with its original flags — including an instance
   that was running in the foreground. With no instance running, just start
   `llm-switch` afterwards.
4. The binary is replaced before the old instance is signaled; if the stop
   times out, the new binary is already in place — stop the instance manually
   and start it again.

## Configuration (Web UI)

1. **Providers** — endpoint channels per vendor. A vendor account often speaks both
   protocols; add both channels and pick per need:

   | Vendor | OpenAI channel | Anthropic channel |
   | --- | --- | --- |
   | DeepSeek | `https://api.deepseek.com` | `https://api.deepseek.com/anthropic` |
   | Zhipu GLM | `https://open.bigmodel.cn/api/paas/v4` | `https://open.bigmodel.cn/api/anthropic` |
   | Kimi/Moonshot | `https://api.moonshot.cn/v1` | `https://api.moonshot.cn/anthropic` |
   | Kimi For Coding | `https://api.kimi.com/coding/v1` | `https://api.kimi.com/coding/` |

   Set the provider-level `models_url`, fetch the upstream list, and check the models
   to register on save. Channel **Test** is connectivity-only (no credential sent).
   Optional per channel: `responses_path` (native OpenAI Responses API, served via
   byte passthrough) and `supports_embeddings` (gates `/v1/embeddings`).
2. **Account Pool** — accounts (API key + weight + usage probes) belong to a provider;
   requests rotate across its enabled accounts. Per-account **Test** validates the key
   (quick = list models, deep = mini chat); **Usage** reports balance/quota.
3. **Models** — the provider-scoped registry: one row = one provider serving one
   client-facing name, and every live channel of that provider can serve it. Create
   rows manually or fetch/sync from the upstream list.
4. **Model Routes** — point a stable name at a provider (+ upstream model, optional
   channel/account pin); flip it anytime.
5. **Client Keys** — issue a key for your agent (shown once).

**Settings** covers log retention, failover attempt cap, injected `default_max_tokens`,
stream idle timeout, and payload recording, plus the config **Export/Import** cards.

**Payload recording** captures full request/response traffic (headers + bodies of the
client request, the upstream request, and the upstream response) for debugging. Enable
it globally with the **Record full payloads** toggle, or per request with the
`X-Debug-Trace: 1` header. Recordings are written as files under
`<data-dir>/payloads/` (never the database) with auth headers redacted, pruned after
**Payload retention** days (default 3), and viewable from the Logs page — recorded rows
carry a blue `payload` tag; click the row for the three-segment detail drawer.

## Connecting clients

The base_url prefix decides the wire shape — not request headers:

| Base URL | Surface |
| --- | --- |
| `<gw>/anthropic` | Anthropic protocol (messages, count_tokens, models) |
| `<gw>/openai/v1` | OpenAI protocol (chat/completions, responses, embeddings, models) |
| `<gw>/v1` | Legacy unprefixed mount; `/v1/models` shape sniffed from `x-api-key` / `anthropic-version` headers |

`count_tokens` is forwarded to native Anthropic upstreams and approximated for
OpenAI-only ones.

```bash
# Claude Code / Anthropic SDK
export ANTHROPIC_BASE_URL=http://127.0.0.1:8901/anthropic
export ANTHROPIC_API_KEY=sk-lsw-...

# OpenAI SDK
base_url = "http://127.0.0.1:8901/openai/v1"
api_key  = "sk-lsw-..."

# Codex CLI (OpenAI Responses API)
#   base_url http://127.0.0.1:8901/openai/v1   (POST /openai/v1/responses)
#   stateless only: store=false; previous_response_id / background are rejected
```

Model names resolve after stripping the `[1m]` context marker: model route → models
registry → `claude-`-stripped retry → 404.

`GET /v1/models` marks each entry's source in `description` (`[route] Provider` or
`[model] Provider`). On the Anthropic shape, names without a `claude`/`anthropic`
substring list only as `claude-<id>` discovery mirrors (what Claude Code's model
picker accepts), and models with `context_window ≥ 1,000,000` list only their
`[1m]`-marked forms.

## Development

```bash
make dev        # Go backend on :8901 + Vite dev server on :5173 (proxied, no CORS)
make test       # go test ./... -race
make mock       # canned OpenAI+Anthropic upstream on :9091 for manual e2e
make build      # production single binary (version stamped from git describe)
make release    # cross-compile bin/llm-switch-<goos>-<goarch> for the 4 release platforms
make compress   # UPX the release binaries (no-op where upx is missing or unsupported)
```

End-to-end without real provider keys:

```bash
make mock &        # fake upstream on :9091 (-fail-first N scripts 429s for failover)
./bin/llm-switch &
# in the UI: provider (base_url http://127.0.0.1:9091) → channel → register "fake-chat"
#            from the fetched model list → client key
curl -N localhost:8901/openai/v1/chat/completions \
  -H "Authorization: Bearer sk-lsw-..." \
  -d '{"model":"fake-chat","stream":true}'
```

## Architecture

Pipeline per request: **parse → route → adapt → bridge** around a canonical IR
(`internal/ir`). Three codecs (OpenAI chat, Anthropic, OpenAI Responses) convert in
both directions, so N client protocols × M upstream protocols need N+M converters
instead of N×M. Wire-identical traffic skips the IR entirely — byte passthrough with
a usage tap.

Routing config lives in SQLite (WAL) and is served from an immutable in-memory
snapshot swapped atomically after every admin mutation. In-flight requests keep their
resolution; changes apply to the next request, with no restarts and no dropped streams.

See [CLAUDE.md](CLAUDE.md) for detailed architecture notes and the critical invariants
enforced in code (failover only before the first forwarded byte, line-wise SSE parsing,
no global stream timeouts, thinking-block asymmetry, ...).

## Security notes

- Account API keys are stored in SQLite plaintext (they must be sent upstream). Keep
  the data directory permission-tight (`~/.llm-switch` by default; the binary sets
  `0700`).
- Client gateway keys are stored as SHA-256 hashes; the plaintext is shown exactly once.
- The admin UI is a single password + bearer session. The default listen address
  (`:8901`) binds all interfaces — restrict it with `--addr 127.0.0.1:8901` or run
  behind authenticated TLS; the gateway is not multi-tenant.

## License

MIT (or your preferred license — adjust before publishing).
