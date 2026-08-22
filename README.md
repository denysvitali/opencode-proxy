# OpenCode Proxy

`opencode-proxy` is a local HTTP proxy that exposes an Anthropic Messages API
on top of [OpenCode Zen](https://opencode.ai/docs/zen/). Point Claude Code (or
any Anthropic-compatible client) at it to use models that are only available
through OpenCode, such as Gemini, GPT, Grok, Kimi, MiniMax, Qwen, and the free
endpoints.

> [!IMPORTANT]
> OpenCode Zen is a paid gateway with its own terms. Check that routing your
> Claude Code traffic through Zen is permitted for your account and use case.

## Install

```bash
go build -o opencode-proxy .
```

## Authentication

Free models (`*-free`) work without any credentials. A key is only needed for
the paid catalog; it is resolved in this order:

1. `OPENCODE_API_KEY`
2. `api_key` in `~/.config/opencode-proxy/config.yaml`
3. The `opencode` entry in OpenCode's own credential store
   (`~/.local/share/opencode/auth.json`, written by `opencode auth login`)

## Start the proxy

```bash
opencode-proxy serve
```

The default listener is `127.0.0.1:8090`. The server exposes:

- `GET /` for proxy status, the full model catalog with one-click copy of each
  model ID, and ready-to-paste Claude Code and Codex launch configs
- `POST /v1/messages` for Claude Code and Anthropic Messages clients
- `POST /v1/responses` for Codex CLI (OpenAI Responses API)
- `POST /v1/chat/completions` for OpenAI chat-completions clients
- `POST /v1/messages/count_tokens` for a conservative local token estimate
- `GET /v1/models` for an OpenAI-style model list
- `GET /healthz`, `GET /readyz`, `GET /metrics`

### Claude Code

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8090 \
ANTHROPIC_AUTH_TOKEN=local \
claude --model gemini-3-flash
```

Model IDs are listed on the dashboard (`GET /`) with a copy button next to
each one. Claude model names that exist in Zen (for example
`claude-opus-5`) pass through unchanged; unknown names fall back to the
configured default model.

> [!TIP]
> Claude Code does not know the context window of third-party models and
> warns about it. Set `CLAUDE_CODE_MAX_CONTEXT_TOKENS` to the model's real
> window (or ignore the warning); a `settings.json` `model` entry overrides
> `ANTHROPIC_MODEL`, so pass `--model` explicitly when needed.

### Codex CLI

Add the provider to `~/.codex/config.toml`:

```toml
[model_providers.opencode-proxy]
name = "opencode-proxy"
base_url = "http://127.0.0.1:8090/v1"
env_key = "OPENAI_API_KEY"   # any value works unless client auth is enabled
wire_api = "responses"       # the proxy translates this to Zen for you
```

Then run:

```bash
OPENAI_API_KEY=local codex exec -c model_provider=opencode-proxy \
  -m gemini-3-flash "fix the failing test"
```

Codex's shell, edit and reasoning tools work through the translation layer;
`wire_api = "chat"` is also supported via the `/v1/chat/completions`
passthrough.

### Other commands

```bash
opencode-proxy models   # print the Zen model catalog
opencode-proxy version
```

## Configuration

Configuration is loaded in this order: command-line flags,
`OPENCODE_PROXY_*` environment variables, `~/.config/opencode-proxy/config.yaml`,
then defaults.

```yaml
base_url: https://opencode.ai/zen/v1
log_level: info
log_format: text

server:
  listen: 127.0.0.1:8090
  # api_key: require this shared key from clients
  max_body_bytes: 16777216

proxy:
  default_model: claude-sonnet-4-6
  model_map:
    claude-special: gemini-3.1-pro
  anthropic_models:
    - "claude-*"
```

Exact entries in `proxy.model_map` take priority. Any model present in the Zen
catalog passes through unchanged (a trailing `-YYYYMMDD` date suffix is
stripped before matching). Everything else uses `proxy.default_model`.

### How requests reach Zen

Zen exposes both an Anthropic (`/messages`) and an OpenAI
(`/chat/completions`) endpoint, and the proxy picks one per model:

- Models matching `proxy.anthropic_models` (`claude-*` by default) are
  forwarded to `/messages` byte-for-byte, with only the model field rewritten.
- Every other model is translated to an OpenAI chat-completions request and the
  response — streaming included — is translated back to Anthropic events.
- Codex's Responses requests are translated to chat-completions (and back),
  whichever upstream model they target.

The translation is not cosmetic. Zen hands Anthropic-shaped `tools` to
OpenAI-shaped providers without converting them, so those providers reject the
request outright (`[1210] Invalid API parameter`, or
`tools[0].function.name is invalid or missing`). Since Claude Code always sends
tools, every request to a Gemini/GPT/Kimi/free model failed before this was in
place. Anthropic-only fields with no equivalent (`context_management`,
`output_config`, `cache_control`, `metadata`) are dropped on the way out;
upstream `reasoning_content` is surfaced as Anthropic thinking blocks when the
client asked for thinking.

### Client authentication and network exposure

Set a shared key with the dedicated environment variable:

```bash
OPENCODE_PROXY_API_KEY="$(openssl rand -hex 32)" opencode-proxy serve --listen 0.0.0.0:8090
```

Clients may send the value as `Authorization: Bearer ...` or `x-api-key`. The
proxy refuses to bind to a non-loopback address without a key unless
`--allow-insecure` is explicitly provided.

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
```
