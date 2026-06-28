# responses-to-chat-proxy

A small standalone Go proxy that keeps an OpenAI-compatible `Responses` endpoint on the downstream side and routes each request to the best-matching upstream protocol: native `Responses`, `Chat Completions`, or Anthropic-compatible `Messages`.

## Features

- `POST /v1/responses` OpenAI Responses-compatible downstream endpoint.
- Per-model upstream routing across `responses`, `chat/completions`, and `messages`.
- Route discovery from upstream `/models` metadata, plus in-memory route caching.
- Native Responses passthrough when the upstream supports it.
- Responses -> Chat conversion, including streaming Responses SSE normalization.
- Responses -> Messages conversion for Anthropic-compatible upstreams, including streaming SSE normalization.
- `POST /v1/chat/completions` passthrough only when the resolved upstream protocol is `chat`.
- `POST /v1/messages` passthrough only when the resolved upstream protocol is `messages`.
- `GET /v1/models` passthrough with route-table metadata refresh when authenticated upstream discovery is available.
- Controlled `/v1/chat/completions` and `/v1/messages` entrypoints never cross-convert to another upstream protocol.
- Optional proxy-side bearer token authentication.
- Interactive local launcher with saved upstream settings.
- Docker image support and GitHub Actions publishing for `main` branch pushes.

## Configuration

Copy `.env.example` to `.env` and set:

```env
UPSTREAM_BASE_URL=https://api.openai.com/v1
UPSTREAM_API_KEY=sk-your-upstream-key
UPSTREAM_MODELS_URL=
PROXY_API_KEY=
MODEL_OVERRIDE=
ROUTE_DETECTION=lazy
ROUTE_TABLE_TTL_SECONDS=1800
ROUTE_TABLE_PERSIST=false
ROUTE_PROBE_GENERATION=false
CACHE_OPTIMIZER=false
CACHE_OPTIMIZER_TTL=1h
HOST=0.0.0.0
PORT=8000
REQUEST_TIMEOUT_SECONDS=120
STREAM_TIMEOUT_SECONDS=300
VERIFY_SSL=true
LOG_LEVEL=info
REASONING_MODE=
```

If `UPSTREAM_API_KEY` is left empty, the proxy forwards the caller's `Authorization`, `x-api-key`, or `x-goog-api-key` header upstream unchanged.

## Routing behavior

- `POST /v1/responses` is the only entrypoint that can convert across upstream protocols.
- In `ROUTE_DETECTION=lazy`, a cold route miss first refreshes `/models` metadata before the proxy decides whether to use native `responses`, convert to `chat`, or convert to `messages`.
- When a model resolves to `responses`, the original request body is forwarded upstream without conversion.
- When a model resolves to `chat`, the proxy converts Responses <-> Chat.
- When a model resolves to `messages`, the proxy converts Responses <-> Messages.
- `POST /v1/chat/completions` only forwards when the resolved model route is `chat`.
- `POST /v1/messages` only forwards when the resolved model route is `messages`.
- If a controlled entrypoint resolves to a different protocol, the proxy returns a clear unsupported-protocol error instead of silently rewriting the request.
- `GET /v1/models` stays publicly reachable, mirrors the upstream models payload, and can refresh the in-memory route table.

## Route discovery settings

- `UPSTREAM_MODELS_URL`: optional explicit models endpoint. When set, discovery uses it first.
- `ROUTE_DETECTION`: `lazy`, `startup`, or `off`. Default is `lazy`.
- `ROUTE_TABLE_TTL_SECONDS`: in-memory route entry TTL. Default is `1800`.
- `ROUTE_TABLE_PERSIST`: reserved for future persistence support. Default is `false`.
- `ROUTE_PROBE_GENERATION`: whether protocol detection is allowed to fall back to minimal generation probes. Default is `false`.
- `CACHE_OPTIMIZER`: injects `cache_control` breakpoints on Responses -> Chat converted requests. Default is `false`.
- `CACHE_OPTIMIZER_TTL`: TTL used for injected cache breakpoints. Default is `1h`. Use `5m` to emit ephemeral breakpoints without an explicit TTL field.
- `REASONING_MODE`: optional explicit override for Chat/Messages reasoning parameter mapping.

## Run locally

```bash
go run . 
```

Interactive launcher:

```bash
go run . -interactive
```

## Docker

Build locally:

```bash
docker build -t responses-to-chat-proxy:latest .
```

Run:

```bash
docker run --rm -p 8000:8000 \
  -e UPSTREAM_BASE_URL=https://api.openai.com/v1 \
  -e UPSTREAM_API_KEY=sk-your-upstream-key \
  responses-to-chat-proxy:latest
```

## CI image publishing

`.github/workflows/docker.yml` builds and pushes `ghcr.io/<owner>/<repo>:latest` whenever code is pushed to the `main` branch.

## Test

```bash
go test ./...
```
