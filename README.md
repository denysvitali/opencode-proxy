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

The proxy needs an OpenCode Zen API key. It is resolved in this order:

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
  model ID, and a ready-to-paste Claude Code launch command
- `POST /v1/messages` for Claude Code and Anthropic Messages clients
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
```

Exact entries in `proxy.model_map` take priority. Any model present in the Zen
catalog passes through unchanged (a trailing `-YYYYMMDD` date suffix is
stripped before matching). Everything else uses `proxy.default_model`.

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
