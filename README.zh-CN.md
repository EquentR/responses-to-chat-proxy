<h1 align="center">responses-to-chat-proxy</h1>

<p align="center">
  <a href="README.md">English</a> | 简体中文
</p>

一个小巧的独立 Go 代理，下游对外暴露兼容 OpenAI 的 `Responses` 端点，并把每一条请求路由到最匹配的上游协议：原生 `Responses`、`Chat Completions`，或 Anthropic 兼容的 `Messages`。

## 功能特性

- `POST /v1/responses` 兼容 OpenAI Responses 的下游端点。
- 按模型将请求路由到 `responses`、`chat/completions` 或 `messages` 上游。
- 通过上游 `/models` 元数据进行路由发现，并支持内存路由表缓存。
- 上游支持时直接透传原生 Responses 请求。
- Responses -> Chat 转换，包含流式 Responses SSE 规范化。
- 针对 Anthropic 兼容上游的 Responses -> Messages 转换，包含流式 SSE 规范化。
- 仅当解析出的上游协议为 `chat` 时，透传 `POST /v1/chat/completions` 请求。
- 仅当解析出的上游协议为 `messages` 时，透传 `POST /v1/messages` 请求。
- 在具备鉴权的上游路由发现可用时，透传 `GET /v1/models` 并刷新路由表元数据。
- 受控的 `/v1/chat/completions` 与 `/v1/messages` 入口永远不会被跨协议转写至其他上游协议。
- 可选的代理侧 Bearer Token 鉴权。
- 可保存上游设置的交互式本地启动器。
- 支持 Docker 镜像，并在推送到 `main` 分支时通过 GitHub Actions 发布。

## 配置

将 `.env.example` 复制为 `.env` 并设置以下变量：

```env
UPSTREAM_BASE_URL=https://api.openai.com/v1
UPSTREAM_API_KEY=sk-your-upstream-key
UPSTREAM_KEY_COOLDOWN_SECONDS=30
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

`UPSTREAM_API_KEY` 决定了上游鉴权方式：

- 在 `.env` 中，可以为同一个上游 base URL 配置单个 key，或以逗号分隔配置多个 key。
- 进程环境变量的取值同样支持以换行分隔的多个 key。
- 配置多个 key 时，生成类请求默认使用粘性调度，以提升上游 prompt 缓存的局部性。
- 粘性调度会优先使用显式的请求身份标识：`metadata.sticky_key`、`metadata.session_id`、`metadata.conversation_id`、`metadata.thread_id`、`previous_response_id` 或 `user`。
- 当不存在显式身份标识时，代理会从稳定的 prompt 内容（如 `model`、`instructions`/系统内容、`tools`，以及去掉当前尾部消息后的历史消息）派生出缓存亲和 key。
- 短小的无状态请求、路由发现、路由探测，以及缺少稳定身份标识的不匹配 `/v1/*` 透传请求，会继续使用自由轮询调度。
- 若配置的多个 key 中有某个返回 429，代理会将其冷却 `UPSTREAM_KEY_COOLDOWN_SECONDS` 秒，并改用下一个可用 key 重试请求，而不是直接把上游 429 透传到下游。
- 较大的不匹配 `/v1/*` 透传请求体会以单次尝试发送，在上游开始读取请求体后无法再为故障转移进行重放。
- 如果 `UPSTREAM_API_KEY` 为空，则调用方携带的 `Authorization`/`x-api-key`/`x-goog-api-key` 透传保持不变。

## 路由行为

- `POST /v1/responses` 是唯一能够在上游协议之间进行跨协议转换的入口。
- 在 `ROUTE_DETECTION=lazy` 模式下，冷路由未命中会先刷新 `/models` 元数据，再决定使用原生 `responses`、转换为 `chat`，还是转换为 `messages`。
- 当模型解析为 `responses` 时，原始请求体不经转换地上游转发。
- 当模型解析为 `chat` 时，代理执行 Responses <-> Chat 转换。
- 当模型解析为 `messages` 时，代理执行 Responses <-> Messages 转换。
- `POST /v1/chat/completions` 仅在解析出的模型路由为 `chat` 时才转发。
- `POST /v1/messages` 仅在解析出的模型路由为 `messages` 时才转发。
- 当受控入口解析出的协议与之不同时，代理会返回明确的“不支持该协议”错误，而非静默改写请求。
- `GET /v1/models` 始终公开可访问，镜像上游的 models 返回内容，并可刷新内存中的路由表。

## 路由发现设置

- `UPSTREAM_MODELS_URL`：可选的显式 models 端点。设置后，路由发现会优先使用它。
- `ROUTE_DETECTION`：`lazy`、`startup` 或 `off`。默认为 `lazy`。
- `ROUTE_TABLE_TTL_SECONDS`：内存路由表条目的 TTL。默认为 `1800`。
- `ROUTE_TABLE_PERSIST`：为将来的持久化支持预留。默认为 `false`。
- `ROUTE_PROBE_GENERATION`：当元数据不足以判断协议时，是否允许回退到最小化的生成探测。默认为 `false`。
- `CACHE_OPTIMIZER`：在 Responses -> Chat 转换链路上注入 `cache_control` 断点。默认为 `false`。
- `CACHE_OPTIMIZER_TTL`：注入缓存断点所使用的 TTL。默认为 `1h`。设为 `5m` 可发出不带显式 TTL 字段的临时断点。
- `REASONING_MODE`：可选的显式覆盖，用于 Chat/Messages 上的 reasoning 参数映射。

## 本地运行

```bash
go run .
```

交互式启动器：

```bash
go run . -interactive
```

## Docker

本地构建：

```bash
docker build -t responses-to-chat-proxy:latest .
```

运行：

```bash
docker run --rm -p 8000:8000 \
  -e UPSTREAM_BASE_URL=https://api.openai.com/v1 \
  -e UPSTREAM_API_KEY=sk-your-upstream-key \
  -e UPSTREAM_KEY_COOLDOWN_SECONDS=30 \
  responses-to-chat-proxy:latest
```

## CI 镜像发布

每当有代码推送到 `main` 分支时，`.github/workflows/docker.yml` 会构建并推送 `ghcr.io/<owner>/<repo>:latest`。

## 测试

```bash
go test ./...
```