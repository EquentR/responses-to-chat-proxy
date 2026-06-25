# responses-to-chat-proxy

A small standalone Go proxy that accepts OpenAI Responses API requests, converts them to Chat Completions requests, forwards them to a Chat-compatible upstream, and converts the upstream response back to Responses API format.

## Features

- `POST /v1/responses` Responses API compatible endpoint.
- Converts Responses `input` and `instructions` to Chat `messages`.
- Converts Responses function tools to Chat Completions tools.
- Converts non-streaming Chat Completions responses back to Responses objects.
- Converts streaming Chat Completions SSE chunks to Responses SSE events.
- Optional `POST /v1/chat/completions` passthrough endpoint.
- Forwards unmatched `/v1/*` requests, such as `GET /v1/models`, to the upstream.
- Optional proxy-side bearer token authentication.
- Interactive local launcher with saved upstream settings.
- Docker image support and GitHub Actions publishing for `main` branch pushes.

## Configuration

Copy `.env.example` to `.env` and set:

```env
UPSTREAM_BASE_URL=https://api.openai.com/v1
UPSTREAM_API_KEY=sk-your-upstream-key
PROXY_API_KEY=
MODEL_OVERRIDE=
HOST=0.0.0.0
PORT=8000
REQUEST_TIMEOUT_SECONDS=120
STREAM_TIMEOUT_SECONDS=300
VERIFY_SSL=true
LOG_LEVEL=info
```

If `UPSTREAM_API_KEY` is left empty, the proxy forwards the caller's `Authorization`, `x-api-key`, or `x-goog-api-key` header upstream unchanged.

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
