# Responses 路由与 Messages 支持实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让下游继续以 `responses` 作为主入口，按上游模型能力自动选路；同时保留 `chat` 和 `messages` 的同协议透传，并把 `Responses ↔ Chat`、`Responses ↔ Messages` 的剩余转换能力补齐。

**Architecture:** 当前分支里，路由表、models 探测、Responses→Chat 主路径和受控入口分发已经有了底座，所以这份计划只拆剩下的交付项。实现上继续保持“路由层只做判定，转换器只做协议变换”的边界：`responses` 入口负责根据路由结果选择直通、Chat 转换或 Messages 转换；`chat/messages` 入口只允许同协议透传，不再做跨协议改写。Messages 方向单独放一个窄转换器，避免继续把 `server.go` 变成巨型分发器。

**Tech Stack:** Go `net/http`、`encoding/json`、`httptest`、`time`、现有 `internal/proxy` 的路由、转换和 SSE 辅助代码。

---

## 文件结构

- `internal/proxy/messages.go`：新增 `Responses ↔ Messages` 转换器。
- `internal/proxy/messages_test.go`：补 Messages 转换和 SSE 回转测试。
- `internal/proxy/adapters.go`：继续收口 `Responses ↔ Chat` 的剩余缺口。
- `internal/proxy/chat_stream.go`：修正 Chat 流式归一化的收尾语义。
- `internal/proxy/server.go`：把 `responses/chat/messages` 的边界和路由分发锁死。
- `internal/proxy/server_test.go`：补端点级回归测试，防止路由和透传回退。
- `README.md`、`.env.example`：补说明和配置示例。

---

### 当前前提

当前工作树里已经有一部分基础实现，执行这份计划时不需要重复造轮子：

- 路由表和模型发现底座已经存在。
- `POST /v1/responses` 的路由分发已经能区分 `responses`、`chat` 和 `messages`。
- `POST /v1/chat/completions` 和 `POST /v1/messages` 已经被限制为同协议透传入口。
- `GET /v1/models` 继续保持公开读取，不走下游代理认证。

这份计划只覆盖剩余的转换补齐、边界收口、文档和最终验收。

---

### Task 1: 新增 Responses ↔ Messages 转换器

**Files:**
- Create: `internal/proxy/messages.go`
- Create: `internal/proxy/messages_test.go`
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/server_test.go`

**核心设计：**
- `Responses -> Messages` 只覆盖 Codex 下游真正需要的 Anthropic 兼容能力，不扩成完整 Anthropic SDK。
- `Messages -> Responses` 负责把 Anthropic 的 `text`、`thinking`、`redacted_thinking`、`tool_use`、`tool_result`、`usage` 和流式事件转回 Responses 形态。
- `POST /v1/messages` 自身如果命中同协议透传，就不要经过转换器；只有 `responses` 入口在上游被判定为 Messages 兼容时才启用转换。
- 未识别的 Anthropic block 先按“宽容处理”执行：跳过未知 block，继续保留已知内容，不把整条响应直接判失败。

**测试先行：**
- [ ] **Step 1: 写失败测试**

  新增测试：
  - `TestConvertResponsesToMessagesHandlesInstructionsAndTools`
  - `TestConvertResponsesToMessagesHandlesImagesAndReasoning`
  - `TestConvertMessagesToResponsesHandlesTextThinkingAndTools`
  - `TestConvertMessagesToResponsesIgnoresUnknownBlocks`
  - `TestConvertMessagesStreamToResponsesSSE`
  - `TestMessagesOnlyUpstreamCanServeResponsesRequests`
  - `TestMessagesEndpointCanPassThroughWhenUpstreamProtocolMatches`

  断言要点：
  - `instructions`、输入消息和工具定义能稳定落到 Anthropic `system` / `messages` / `tools`。
  - 图片、thinking 和工具调用能在两侧互相还原。
  - 流式 Messages 事件能转成 Responses SSE。
  - `POST /v1/messages` 只在路由命中 `messages` 时直通。

- [ ] **Step 2: 运行测试确认当前会失败**

  Run: `go test ./internal/proxy -run 'TestConvertResponsesToMessagesHandlesInstructionsAndTools|TestConvertResponsesToMessagesHandlesImagesAndReasoning|TestConvertMessagesToResponsesHandlesTextThinkingAndTools|TestConvertMessagesToResponsesIgnoresUnknownBlocks|TestConvertMessagesStreamToResponsesSSE|TestMessagesOnlyUpstreamCanServeResponsesRequests|TestMessagesEndpointCanPassThroughWhenUpstreamProtocolMatches' -v`

  Expected: fail，因为 `messages.go` 还没落地，相关转换器和测试支撑还不存在。

- [ ] **Step 3: 实现最小可用转换器**

  在 `messages.go` 里先补齐：
  - `ConvertResponsesToMessages(...)`
  - `ConvertMessagesToResponses(...)`
  - 流式 Messages SSE 到 Responses SSE 的转换辅助逻辑
  - 图片、文本、thinking、工具调用、工具结果、usage 的最小映射

  原则是先打通主路径，再补细节，不把 Messages 扩成另一个通用协议层。

- [ ] **Step 4: 重新跑同一组测试**

  Run: `go test ./internal/proxy -run 'TestConvertResponsesToMessagesHandlesInstructionsAndTools|TestConvertResponsesToMessagesHandlesImagesAndReasoning|TestConvertMessagesToResponsesHandlesTextThinkingAndTools|TestConvertMessagesToResponsesIgnoresUnknownBlocks|TestConvertMessagesStreamToResponsesSSE|TestMessagesOnlyUpstreamCanServeResponsesRequests|TestMessagesEndpointCanPassThroughWhenUpstreamProtocolMatches' -v`

  Expected: PASS。

- [ ] **Step 5: 提交这个小批次**

  ```bash
  git add internal/proxy/messages.go internal/proxy/messages_test.go internal/proxy/server.go internal/proxy/server_test.go
  git commit -m "feat: add messages protocol support"
  ```

---

### Task 2: 收口 Responses-to-Chat 剩余缺口

**Files:**
- Modify: `internal/proxy/adapters.go`
- Modify: `internal/proxy/chat_stream.go`
- Modify: `internal/proxy/adapters_test.go`

**核心设计：**
- `input_file` 不能再丢字段，要保留 `file_id`、`file_data`、`filename`。
- `input_audio` 需要完整进入 Chat 消息，不再静默丢弃。
- 顶层 `input_text`、`input_image`、`input_file`、`input_audio` 作为独立 input item 时，也要能进入消息链。
- Chat 流式工具调用必须保留原始 tool context，不能让 namespace tool、custom tool、`tool_search` 在流里退化成普通 `function_call`。
- 流在没有 `finish_reason` 时结束，要按“是否已经有实质输出”区分 `incomplete` 和 `failed`。
- 非流式路径要把已有 thinking rectifier 接回主链路，而不是只存在于辅助函数里。

**测试先行：**
- [ ] **Step 1: 写失败测试**

  新增或扩展：
  - `TestConvertRequestPreservesInputFile`
  - `TestConvertRequestPreservesInputAudio`
  - `TestConvertRequestHandlesTopLevelInputItems`
  - `TestStreamingConverterKeepsToolContext`
  - `TestChatStreamEOFWithoutFinishReasonProducesIncompleteOrFailed`
  - `TestNonStreamingResponsesToChatUsesThinkingRectifier`
  - `TestReasoningExtractionCoversObjectShapeAndThinkTags`

  断言要点：
  - 文件和音频输入在转换后仍能被 Chat 上游识别。
  - 流式工具上下文不会被抹掉。
  - 响应结尾状态不会再误报 completed。
  - reasoning 提取要覆盖对象形态、字段形态和 `<think>` 标签形态。

- [ ] **Step 2: 运行测试确认当前会失败**

  Run: `go test ./internal/proxy -run 'TestConvertRequestPreservesInputFile|TestConvertRequestPreservesInputAudio|TestConvertRequestHandlesTopLevelInputItems|TestStreamingConverterKeepsToolContext|TestChatStreamEOFWithoutFinishReasonProducesIncompleteOrFailed|TestNonStreamingResponsesToChatUsesThinkingRectifier|TestReasoningExtractionCoversObjectShapeAndThinkTags' -v`

  Expected: fail，直到 `adapters.go` 和 `chat_stream.go` 的剩余缺口被补齐。

- [ ] **Step 3: 修复转换缺口**

  在 `adapters.go` / `chat_stream.go` 里补齐：
  - 文件和音频输入映射
  - 顶层输入项映射
  - 流式 tool context 保持
  - EOF 收尾语义
  - reasoning 提取和 rectifier 接入

- [ ] **Step 4: 重新跑同一组测试**

  Run: `go test ./internal/proxy -run 'TestConvertRequestPreservesInputFile|TestConvertRequestPreservesInputAudio|TestConvertRequestHandlesTopLevelInputItems|TestStreamingConverterKeepsToolContext|TestChatStreamEOFWithoutFinishReasonProducesIncompleteOrFailed|TestNonStreamingResponsesToChatUsesThinkingRectifier|TestReasoningExtractionCoversObjectShapeAndThinkTags' -v`

  Expected: PASS。

- [ ] **Step 5: 提交这个小批次**

  ```bash
  git add internal/proxy/adapters.go internal/proxy/chat_stream.go internal/proxy/adapters_test.go
  git commit -m "feat: fix responses to chat conversion gaps"
  ```

---

### Task 3: 锁定路由边界和同协议透传

**Files:**
- Modify: `internal/proxy/server.go`
- Modify: `internal/proxy/server_test.go`

**核心设计：**
- `POST /v1/responses` 是唯一允许跨协议转换的入口。
- `POST /v1/chat/completions` 和 `POST /v1/messages` 只允许同协议透传，不允许自动改写成别的协议。
- `forwardUnknownV1` 继续只处理真正没纳入支持的 `/v1/*` 路径，不再承担 `chat/messages` 这两个受控入口。
- `GET /v1/models` 继续保持公开读取，不受 proxy auth 约束。

**需要锁死的行为：**
- `handleResponses`：先走 resolver，再决定是 `responses` 直通、`chat` 转换还是 `messages` 转换。
- `handleChatCompletions`：只在 route 命中 `chat` 时直通，否则返回清晰的 `unsupported_protocol` 错误。
- `handleMessages`：只在 route 命中 `messages` 时直通，否则返回清晰的 `unsupported_protocol` 错误。

**测试先行：**
- [ ] **Step 1: 写回归测试**

  新增端点级测试：
  - `TestResponsesPassesThroughResponsesUpstream`
  - `TestResponsesRoutesToChatWhenChatOnly`
  - `TestResponsesRoutesToMessagesWhenMessagesOnly`
  - `TestChatCompletionsPassesThroughOnlyWhenRouteIsChat`
  - `TestMessagesPassesThroughOnlyWhenRouteIsMessages`
  - `TestUnsupportedProtocolReturnsClearError`
  - `TestModelsEndpointStillSkipsProxyAuth`
  - `TestControlledEntrypointsRequireProxyAuth`
  - `TestControlledEntrypointsRejectNonPostWithoutForwarding`
  - `TestMessagesStreamFalseUsesNormalPassthrough`
  - `TestPassthroughStreamRoutesRawResponsesSSEAndFlushesChunks`
  - `TestPassthroughStreamRoutesMessagesSSEAndHandlesUpstream4xx`
  - `TestJoinUpstreamEndpoint`

  断言重点：
  - `responses` 命中时 upstream 路径要正确，body 不能被偷偷改写。
  - `chat/messages` 命中时 upstream body 必须保持原样。
  - 非匹配协议要返回明确错误，而不是悄悄回退到别的协议。
  - 原始 SSE 响应要按 chunk 刷新，并能在上游 4xx 时给出合理错误事件。

- [ ] **Step 2: 运行测试确认当前行为没有回退**

  Run: `go test ./internal/proxy -run 'TestResponsesPassesThroughResponsesUpstream|TestResponsesRoutesToChatWhenChatOnly|TestResponsesRoutesToMessagesWhenMessagesOnly|TestChatCompletionsPassesThroughOnlyWhenRouteIsChat|TestMessagesPassesThroughOnlyWhenRouteIsMessages|TestUnsupportedProtocolReturnsClearError|TestModelsEndpointStillSkipsProxyAuth|TestControlledEntrypointsRequireProxyAuth|TestControlledEntrypointsRejectNonPostWithoutForwarding|TestMessagesStreamFalseUsesNormalPassthrough|TestPassthroughStreamRoutesRawResponsesSSEAndFlushesChunks|TestPassthroughStreamRoutesMessagesSSEAndHandlesUpstream4xx|TestJoinUpstreamEndpoint' -v`

  Expected: PASS；如果失败，优先修 `server.go` 的分发边界，再看转换器。

- [ ] **Step 3: 收口分发逻辑**

  在 `server.go` 里保持以下边界不动：
  - 只在 `responses` 入口做跨协议转换。
  - `chat/messages` 入口只做同协议直通。
  - 非 POST 请求直接本地返回 405，不往上游转发。
  - SSE 相关辅助函数继续区分 `responses` 和非 `responses` 的流式形态。

- [ ] **Step 4: 重新跑同一组测试**

  Run: `go test ./internal/proxy -run 'TestResponsesPassesThroughResponsesUpstream|TestResponsesRoutesToChatWhenChatOnly|TestResponsesRoutesToMessagesWhenMessagesOnly|TestChatCompletionsPassesThroughOnlyWhenRouteIsChat|TestMessagesPassesThroughOnlyWhenRouteIsMessages|TestUnsupportedProtocolReturnsClearError|TestModelsEndpointStillSkipsProxyAuth|TestControlledEntrypointsRequireProxyAuth|TestControlledEntrypointsRejectNonPostWithoutForwarding|TestMessagesStreamFalseUsesNormalPassthrough|TestPassthroughStreamRoutesRawResponsesSSEAndFlushesChunks|TestPassthroughStreamRoutesMessagesSSEAndHandlesUpstream4xx|TestJoinUpstreamEndpoint' -v`

  Expected: PASS。

- [ ] **Step 5: 提交这个小批次**

  ```bash
  git add internal/proxy/server.go internal/proxy/server_test.go
  git commit -m "feat: tighten protocol routing boundaries"
  ```

---

### Task 4: 文档、示例配置和最终验收

**Files:**
- Modify: `README.md`
- Modify: `.env.example`
- Modify: `internal/proxy/launcher.go`（只有当本地启动器需要暴露新配置时才改）

**需要更新的内容：**
- README 里要从“Responses 转 Chat”改成“Responses 自动路由 + Chat/messages 同协议透传”。
- 配置说明要补上路由探测、路由表 TTL、是否持久化、是否启动时探测等内容。
- 明确写清楚：`chat` 和 `messages` 不做跨协议转换，只有 `responses` 入口才会按上游协议转码。
- `.env.example` 要补齐新环境变量，并保持默认关闭或保守配置。
- 如果启动器会读写这些配置，再补对应字段；如果不会暴露，就不要为了文档完整性硬改启动器。

**测试和收尾：**
- [ ] **Step 1: 更新文档并跑格式检查**

  Run:
  - `gofmt -w internal/proxy/*.go`
  - `go test ./internal/proxy -run 'Test.*' -v`

  Expected: Go 文件格式正确，关键测试继续稳定通过。

- [ ] **Step 2: 跑全量测试**

  Run: `go test ./... -count=1`

  Expected: 全部 PASS，没有新的回归。

- [ ] **Step 3: 最终提交**

  ```bash
  git add README.md .env.example internal/proxy
  git commit -m "feat: add route detection and protocol passthrough"
  ```

---

## 自检清单

- `Responses ↔ Messages` 转换：Task 1
- `Responses ↔ Chat` 剩余缺口：Task 2
- `responses/chat/messages` 路由边界和同协议透传：Task 3
- 文档、示例配置和最终验证：Task 4

当前计划没有留下 `TODO` / `TBD` 占位，也没有把同一条逻辑拆成互相冲突的入口。
