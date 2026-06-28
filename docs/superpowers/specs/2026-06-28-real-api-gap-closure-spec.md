# 真实 API 联调后续缺口收口 Spec

## 背景

在 `codex/responses-routing-detection` 分支完成路由、协议透传和基础转换器后，已经做了两轮真实 API 联调：

1. 第一轮联调聚焦协议路由和流式行为。
   - 上游基址：`https://s2api.istart.uk/v1`
   - Chat 路径模型：`deepseek-v4-flash`
   - Native Responses 对照模型：`gpt-5.4-mini`（使用单独 credential group）
   - 已确认并修复：
     - `chat -> responses` 流式 reasoning 事件从旧的 `response.reasoning_text.*` 对齐到了 `response.reasoning_summary_part.added` / `response.reasoning_summary_text.*`
     - `chat -> responses` 转换流会在完成后输出 `data: [DONE]`
     - `chat` passthrough 流在收到上游 `data: [DONE]` 后会忽略后续尾包

2. 第二轮联调聚焦缓存命中。
   - `deepseek-v4-flash` 走 `responses -> chat` 转换链时，第二次大 prompt 请求可观察到缓存命中：
     - `input_tokens = 23490`
     - 第二次 `cached_tokens = 23424`
   - `gpt-5.4-mini` 走 native `responses` passthrough 时，第二次大 prompt 请求也可观察到缓存命中：
     - `input_tokens = 14293`
     - 第一次 `cached_tokens = 3840`
     - 第二次 `cached_tokens = 14080`

上述结果说明主路径已经可用，但真实联调也暴露了几个仍未收口的差距。这份 spec 只覆盖这些“已发现但未补齐”的问题，不重复描述已经完成的能力。

## 目标

- 收口真实联调已经暴露的剩余协议行为差距。
- 让 `lazy` 路由探测、非流式 thinking rectifier、Messages 流式状态语义、SSE 原生形态和缓存优化入口都具备可验证实现。
- 为下一轮实现提供清晰、可直接拆任务的 spec。

## 非目标

- 不重做已经通过真实联调验证的流式修复。
- 不新增新的上游协议族。
- 不引入供应商管理、故障转移、配额调度等超出当前代理边界的能力。

## 真实联调已确认的剩余缺口

### P0. `ROUTE_DETECTION=lazy` 仍未实现真正的首请求探测

#### 当前行为

- 配置层已经支持 `ROUTE_DETECTION=lazy|startup|off`。
- `startup` 模式会在进程启动时跑一次 `/models` 发现。
- 但请求路径上的 `resolveRoute(model)` 仍然只是查内存表；当路由 miss 时，`POST /v1/responses` 直接回退到历史 `Responses -> Chat` 路径。

#### 风险

- 对 `messages-only` upstream，冷启动首个 `responses` 请求会误落到 `chat` 回退路径。
- 对 `responses-only` upstream，冷启动首个 `responses` 请求也可能走到不必要的转换。
- 这会让“默认 lazy”与设计文档描述不一致，尤其会放大流式路径上的语义偏差。

#### 需要的目标行为

- 当 `ROUTE_DETECTION=lazy` 且某个 `(identity, model)` 没有路由项时：
  - `POST /v1/responses` 必须先尝试轻量 discovery / metadata 解析 / 受控 probe，再决定走 `responses` / `chat` / `messages`
  - 只有在 discovery 无证据、probe 被关闭、且没有明确错误时，才允许保守回退到 Chat
- `POST /v1/chat/completions` 和 `POST /v1/messages` 不应偷偷触发跨协议 fallback
  - 它们可以做 metadata refresh
  - 但如果最终仍无法确认本协议支持，应直接返回清晰错误

#### 设计要求

- 把 `resolveRoute` 提升为真正的 resolver，而不是单纯 cache lookup。
- resolver 顺序必须是：
  1. 显式配置 / model override
  2. 现有 route table
  3. `lazy` discovery / metadata refresh
  4. 受控 probe（仅在配置允许时）
  5. `responses` 入口上的保守 Chat fallback
- discovery / probe 失败要区分：
  - `404/405/unsupported-endpoint`：协议候选未命中
  - `401/403/429/timeout/network`：探测失败，不能当成“不支持”

#### 测试要求

- `TestLazyRouteDetectionResolvesResponsesBeforeFallback`
- `TestLazyRouteDetectionResolvesMessagesBeforeFallback`
- `TestLazyRouteDetectionDoesNotCrossConvertControlledEntrypoints`
- `TestLazyRouteDetectionPreservesAuthAndRateLimitErrors`

### P0. 非流式 thinking rectifier 仍未接入主链路

#### 当前行为

- thinking rectifier 相关逻辑已经存在：
  - 识别 thinking signature error
  - strip thinking blocks
  - retry
- 但主非流式 Chat 转换路径仍走普通 `forwardConvertedResponse`，没有真正使用 rectifier 分支。

#### 风险

- 某些上游在非流式工具历史、thinking block、signature 校验上会直接拒绝请求。
- 当前代理虽然有修正代码，但真实请求不会触发它，等价于 dead code。

#### 需要的目标行为

- 非流式 `responses -> chat` 路径遇到可识别的 thinking/signature 错误时：
  - 应自动 strip thinking 相关 block / signature
  - 重新构造上游请求并 retry 一次
  - 若 retry 成功，下游无感恢复
  - 若 retry 失败，返回 retry 之后的真实错误

#### 设计要求

- 不要保留两套长期分叉的“普通非流式路径”和“rectifier 路径”。
- 应把 rectifier 变成主路径上的一个 error recovery 分支。
- 需要保证 retry 不影响：
  - tool history
  - function result 映射
  - reasoning 参数映射
  - usage 转换

#### 测试要求

- `TestNonStreamingResponsesToChatUsesThinkingRectifier`
- `TestThinkingRectifierRetriesExactlyOnce`
- `TestThinkingRectifierKeepsToolHistoryAfterStrip`
- `TestThinkingRectifierReturnsOriginalErrorWhenNotApplicable`

### P0. Messages 流式完成语义仍不够保守

#### 当前行为

- Messages 流式转换的主路径已经存在。
- 但真实审计显示：
  - 当 upstream 没有给 `stop_reason` / `message_stop` 时，当前实现可能把“已有输出但未明确结束”的流标成 `completed`
  - 这与 Chat 流已经做过的 `completed / incomplete / failed` 区分不一致

#### 风险

- 上游连接截断、供应商 SSE 不完整、代理中途读 EOF 时，客户端会收到误报的 `completed`
- 这会导致下游把半截结果当成完整结果

#### 需要的目标行为

- Messages 流式收尾必须与 Chat 路径的保守性一致：
  - 没有明确 stop signal，但已有实质输出：`incomplete`
  - 没有明确 stop signal，且没有实质输出：`failed`
  - 只有收到明确可判定的结束语义，才允许 `completed`

#### 测试要求

- `TestMessagesStreamEOFWithoutStopReasonProducesIncompleteOrFailed`
- `TestMessagesStreamExplicitStopReasonProducesCompleted`
- `TestMessagesStreamNoOutputAndNoStopProducesFailed`

### P0. Messages 流式 `output_index` 仍需与最终 output 顺序完全一致

#### 当前行为

- 当前 `response.completed.output` 最终顺序是稳定的。
- 但 streaming 增量事件里的 `output_index` 分配仍存在风险：
  - reasoning / message / tool_use 的 index 分配方式与最终 assembled output 顺序不一定完全一致

#### 风险

- 严格按 `output_index` 建本地状态的客户端，可能把 reasoning、message、tool item 挂到不同槽位。
- 问题在只看最终 completed payload 时不明显，但会在边渲染边组装的客户端上暴露。

#### 需要的目标行为

- 增量 SSE 事件中的 `output_index` 必须与最终 `response.completed.output` 的 index 一一对应。
- 同一个 output item 生命周期内：
  - `added`
  - `delta`
  - `done`
  - final completed snapshot
  的 index 必须完全稳定。

#### 测试要求

- `TestConvertMessagesStreamToResponsesSSEOutputIndexesMatchCompletedOutput`
- `TestMessagesStreamReasoningMessageAndToolIndexesRemainStable`

### P1. `chat -> responses` SSE 仍未完全对齐 native Responses 形态

#### 当前行为

- 已修复：
  - `response.reasoning_summary_part.added`
  - `response.reasoning_summary_text.delta`
  - `response.reasoning_summary_text.done`
  - `data: [DONE]`
- 但与 native `responses` 实测相比，当前转换流仍缺少至少两个原生形态元素：
  - `sequence_number`
  - `response.reasoning_summary_part.done`

#### 风险

- 大多数宽松客户端可能不受影响。
- 但严格按 OpenAI Responses SSE 形态建状态机的客户端，仍可能因为缺少 `sequence_number` 或 `summary_part.done` 而丢失精确一致性。

#### 需要的目标行为

- 对 `chat -> responses` 流，进一步对齐 native `responses` SSE：
  - 全量增量事件具备单调递增 `sequence_number`
  - reasoning summary part 生命周期完整
    - `response.reasoning_summary_part.added`
    - `response.reasoning_summary_text.delta`
    - `response.reasoning_summary_text.done`
    - `response.reasoning_summary_part.done`

#### 设计要求

- `sequence_number` 应由代理本地生成，不依赖上游 chunk 序号。
- 只要求同一次响应内部单调递增，不要求跨请求全局唯一。
- 不要为了补 `sequence_number` 打乱现有 output item flush 顺序。

#### 测试要求

- `TestStreamingConverterAddsMonotonicSequenceNumbers`
- `TestStreamingConverterEmitsReasoningSummaryPartDone`
- `TestStreamingConverterEventOrderMatchesNativeResponsesShape`

### P1. passthrough 流的上游 4xx 终止契约仍未统一

#### 当前行为

- raw passthrough stream 遇到 upstream `4xx` 时，会发本地 SSE error 事件并返回。
- 但当前不会补 `data: [DONE]`，也不会补统一终止事件。

#### 风险

- 某些下游实现会把“收到 error 事件但没看到 DONE / terminal frame”视为连接异常，而不是协议内失败结束。

#### 需要的目标行为

- 需要先明确产品契约，再实现：
  - 方案 A：error 事件后立即关闭流，不补任何终止 sentinel
  - 方案 B：error 事件后补统一的 terminal sentinel，例如 `data: [DONE]`
- 本项目必须选一种，并在所有 passthrough / transformed stream 上保持一致

#### 推荐方案

- 推荐优先选方案 B：
  - 更利于客户端统一收流
  - 与现有 transformed stream 已经引入 `[DONE]` 的方向一致

#### 测试要求

- `TestPassthroughStreamRoutesMessagesSSEAndHandlesUpstream4xx`
- `TestPassthroughStream4xxEndsWithConsistentTerminalEvent`
- `TestResponsesNativeStream4xxUsesSameTerminationContract`

### P1. Cache Optimizer 还没有标准配置入口

#### 当前行为

- `Config.CacheOptimizer`、`InjectCacheBreakpoints(...)` 和相关单元测试都已经存在。
- 但标准启动路径里没有办法真正打开它：
  - `LoadConfigFromEnv` 没有读取 `CACHE_OPTIMIZER`
  - launcher 没有暴露对应配置
  - README / `.env.example` 也没有说明如何启用
- 当前真实缓存命中是上游 provider 自身行为，不是代理显式开启了 `cache_control` 注入。

#### 风险

- 用户无法稳定开启“代理主动插入 cache breakpoint”能力。
- 当前只能验证“上游自己会缓存”，不能验证“代理开启 optimizer 后是否改善命中”。

#### 需要的目标行为

- 增加显式配置：
  - `CACHE_OPTIMIZER=true|false`
  - `CACHE_OPTIMIZER_TTL=5m|1h|...`
- `go run .`、`.env` 和 interactive launcher 都能设置它
- README 明确说明：
  - 该开关只影响会经过转换器的路径
  - native `responses` passthrough 不会由代理重写 body 注入 cache_control

#### 设计要求

- 默认值保持保守关闭。
- TTL 默认值应从 `server.go` 里的硬编码 `"1h"` 抽出为配置。
- 若 upstream 不支持 cache_control，代理不得因为注入字段破坏请求成功率。
  - 需要提供显式关闭能力
  - 未来可考虑按 route feature / provider profile 决定是否注入

#### 测试要求

- `TestLoadConfigCacheOptimizerSettings`
- `TestInteractiveLauncherPersistsCacheOptimizerSettings`
- `TestForwardResponsesAsChatInjectsConfiguredCacheTTL`
- 真实联调回归：
  - 同一大 prompt 在开启 optimizer 后，第二次请求仍能成功
  - usage 中的 `cached_tokens` / `prompt_cache_hit_tokens` 能继续透出

## 建议的实现顺序

1. `lazy` route detection 真正接线
2. 非流式 thinking rectifier 接入主链路
3. Messages 流式完成语义与 `output_index` 稳定性
4. `chat -> responses` SSE 完整原生化（`sequence_number` / `summary_part.done`）
5. passthrough stream 4xx 终止契约统一
6. Cache Optimizer 配置化与文档化

## 验收标准

- `ROUTE_DETECTION=lazy` 冷启动首请求不会盲目回退 Chat。
- 非流式 thinking/signature 错误能够自动修正并 retry。
- Messages 流式在缺少 stop signal 时不会误报 `completed`。
- Messages 流式的 `output_index` 与最终 completed output 完全一致。
- `chat -> responses` 流式事件在关键结构上与 native Responses SSE 对齐，包括 `sequence_number` 和 `summary_part.done`。
- passthrough stream 的错误结束契约一致且可测试。
- Cache Optimizer 可通过标准配置路径启用，并能在真实请求里稳定验证其行为。
