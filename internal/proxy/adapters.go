package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// ---------------------------------------------------------------------------
// Tool conversion context -- mirrors cc-switch CodexToolContext
// ---------------------------------------------------------------------------

type toolSpec struct {
	kind      string // "function", "namespace", "custom", "tool_search"
	name      string // original response-side name
	namespace string // empty for non-namespace tools
}

type toolContext struct {
	chatTools               []map[string]any
	seenChatNames           map[string]struct{}
	chatNameToSpec          map[string]*toolSpec
	namespaceNameToChatName map[string]string
}

type normalizedInputMessage struct {
	role    string
	content any
}

type normalizedContentPart struct {
	kind string
	data map[string]any
}

type messageTextParts struct {
	reasoningParts []string
	visibleParts   []string
	visibleThink   thinkTextAccumulator
}

func (p *messageTextParts) addReasoning(text string) {
	if strings.TrimSpace(text) != "" {
		p.reasoningParts = append(p.reasoningParts, text)
	}
}

func (p *messageTextParts) addVisible(text string) {
	if text != "" {
		p.visibleParts = append(p.visibleParts, text)
	}
}

func (p *messageTextParts) collectReasoning(value any) {
	switch typed := value.(type) {
	case nil:
		return
	case string:
		reasoning, _, found := splitThinkText(typed)
		if found {
			p.addReasoning(reasoning)
			return
		}
		p.addReasoning(typed)
	case []string:
		for _, item := range typed {
			p.addReasoning(item)
		}
	case []any:
		for _, item := range typed {
			p.collectReasoning(item)
		}
	case map[string]any:
		p.collectReasoningMap(typed)
	default:
		p.addReasoning(stringValue(typed))
	}
}

func (p *messageTextParts) collectReasoningDetails(value any) {
	p.collectReasoning(value)
}

func (p *messageTextParts) collectReasoningMap(value map[string]any) {
	switch stringValue(value["type"]) {
	case "thinking":
		p.collectReasoning(value["thinking"])
		p.collectReasoning(value["text"])
		p.collectReasoning(value["content"])
		p.collectReasoning(value["data"])
		p.collectReasoning(value["summary"])
		p.collectReasoning(value["reasoning"])
		return
	case "redacted_thinking":
		p.collectReasoning(value["data"])
		p.collectReasoning(value["text"])
		p.collectReasoning(value["content"])
		p.collectReasoning(value["thinking"])
		return
	case "summary_text", "reasoning_text":
		p.collectReasoning(value["text"])
		p.collectReasoning(value["content"])
		return
	case "reasoning":
		p.collectReasoning(value["summary"])
		p.collectReasoning(value["content"])
		p.collectReasoning(value["details"])
		p.collectReasoning(value["reasoning_details"])
		p.collectReasoning(value["text"])
		p.collectReasoning(value["thinking"])
		p.collectReasoning(value["data"])
		return
	}

	for _, key := range []string{"summary", "content", "details", "reasoning_details", "items", "blocks"} {
		if child, ok := value[key]; ok {
			p.collectReasoning(child)
		}
	}

	for _, key := range []string{"thinking", "text", "data", "reasoning"} {
		if child, ok := value[key]; ok {
			p.collectReasoning(child)
		}
	}
}

func (p *messageTextParts) collectVisible(value any) {
	switch typed := value.(type) {
	case nil:
		return
	case string:
		reasoning, visible := p.visibleThink.consume(typed)
		p.addReasoning(reasoning)
		p.addVisible(visible)
	case []string:
		for _, item := range typed {
			p.addVisible(item)
		}
	case []any:
		for _, item := range typed {
			p.collectVisible(item)
		}
	case map[string]any:
		switch stringValue(typed["type"]) {
		case "thinking", "redacted_thinking", "summary_text", "reasoning_text", "reasoning":
			p.collectReasoning(typed)
			return
		case "text", "output_text", "input_text":
			if text := stringValue(typed["text"]); text != "" {
				p.collectVisible(text)
				return
			}
			if text := stringValue(typed["content"]); text != "" {
				p.collectVisible(text)
				return
			}
		}

		if text := stringValue(typed["text"]); text != "" {
			p.collectVisible(text)
		}
		if content, ok := typed["content"]; ok {
			p.collectVisible(content)
		}
		if output := stringValue(typed["output"]); output != "" {
			p.collectVisible(output)
		}
	default:
		p.addVisible(stringValue(typed))
	}
}

func (p *messageTextParts) finishVisibleThinking() {
	reasoning, visible := p.visibleThink.finish()
	p.addReasoning(reasoning)
	p.addVisible(visible)
}

func newToolContext() *toolContext {
	return &toolContext{
		seenChatNames:           map[string]struct{}{},
		chatNameToSpec:          map[string]*toolSpec{},
		namespaceNameToChatName: map[string]string{},
	}
}

func (tc *toolContext) addChatTool(chatName string, spec *toolSpec, chatTool map[string]any) {
	if chatName == "" {
		return
	}
	if _, exists := tc.seenChatNames[chatName]; exists {
		return
	}
	tc.seenChatNames[chatName] = struct{}{}
	if spec.namespace != "" {
		tc.namespaceNameToChatName[spec.namespace+"\x00"+spec.name] = chatName
	}
	tc.chatNameToSpec[chatName] = spec
	tc.chatTools = append(tc.chatTools, chatTool)
}

var codexInternalTools = map[string]struct{}{
	"update_plan":                 {},
	"request_user_input":          {},
	"view_image":                  {},
	"spawn_agent":                 {},
	"send_input":                  {},
	"resume_agent":                {},
	"wait_agent":                  {},
	"close_agent":                 {},
	"read_thread_terminal":        {},
	"load_workspace_dependencies": {},
}

const (
	toolSearchProxyName       = "tool_search"
	customToolInputField      = "input"
	chatToolNameMaxLen        = 64
	customToolInputDesc       = "Raw string input for the original custom tool. Preserve formatting exactly and follow the original tool definition embedded in the description."
	customToolPreservedHeader = "Original tool definition:"
)

// ReasoningMode controls how reasoning.effort is mapped to upstream parameters.
type ReasoningMode string

const (
	// ReasoningPassthrough --leave reasoning field as-is (default for unknown providers).
	ReasoningPassthrough ReasoningMode = ""
	// ReasoningEffort -- map to top-level reasoning_effort (OpenAI / GPT-5).
	ReasoningEffort ReasoningMode = "effort"
	// ReasoningThinking -- map to thinking.type + reasoning_effort (DeepSeek).
	ReasoningThinking ReasoningMode = "thinking"
	// ReasoningThinkingOnly -- map to thinking.type only, no reasoning_effort.
	// Used for GLM / Kimi / MiMo and other providers whose upstream accepts the
	// "thinking" object but rejects a top-level "reasoning_effort".
	ReasoningThinkingOnly ReasoningMode = "thinking_only"
	// ReasoningEnableThinking --map to enable_thinking (SiliconFlow, Qwen on some platforms).
	ReasoningEnableThinking ReasoningMode = "enable_thinking"
	// ReasoningSplit --map to reasoning_split (MiniMax).
	ReasoningSplit ReasoningMode = "reasoning_split"
	// ReasoningEffortObj --map to reasoning.effort nested object (OpenRouter).
	ReasoningEffortObj ReasoningMode = "effort_obj"
)

var extraChatPassthroughFields = []string{
	"frequency_penalty",
	"logit_bias",
	"logprobs",
	"metadata",
	"n",
	"parallel_tool_calls",
	"presence_penalty",
	"response_format",
	"seed",
	"service_tier",
	"stop",
	"stream_options",
	"top_logprobs",
	"user",
}

// ===========================================================================
// ConvertRequest --responses --chat completions
// ===========================================================================

func ConvertRequest(data map[string]any, cfg Config) map[string]any {
	model := stringValue(data["model"])
	if cfg.ModelOverride != "" {
		model = cfg.ModelOverride
	}

	tc := buildToolContextFromRequest(data)

	chatData := map[string]any{"model": model}

	// ---- messages ----
	var messages []any
	if instructions := stringValue(data["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	switch input := data["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": input})
	case []any:
		for _, item := range input {
			msg, ok := normalizeInputItem(item, tc)
			if ok {
				messages = append(messages, msg.toChatMessage())
			}
		}
	}
	messages = collapseSystemMessages(messages)
	chatData["messages"] = messages

	// ---- token limit ----
	if maxTokens, ok := data["max_output_tokens"]; ok {
		if isOSeriesModel(model) {
			chatData["max_completion_tokens"] = maxTokens
		} else {
			chatData["max_tokens"] = maxTokens
		}
	}
	if v, ok := data["max_tokens"]; ok {
		chatData["max_tokens"] = v
	}
	if v, ok := data["max_completion_tokens"]; ok {
		chatData["max_completion_tokens"] = v
	}

	// ---- scalar passthrough ----
	for _, key := range []string{"stream", "temperature", "top_p"} {
		if v, ok := data[key]; ok {
			chatData[key] = v
		}
	}

	// ---- tools ----
	if len(tc.chatTools) > 0 {
		chatData["tools"] = mapSliceToAny(tc.chatTools)
	}

	// ---- tool_choice ----
	if rawTC, ok := data["tool_choice"]; ok {
		chatData["tool_choice"] = mapToolChoice(rawTC, tc)
	}
	if len(tc.chatTools) == 0 {
		delete(chatData, "tool_choice")
		delete(chatData, "parallel_tool_calls")
	}

	// ---- reasoning ----
	applyReasoningOptions(chatData, data, model, cfg)

	// ---- extra passthrough fields ----
	for _, key := range extraChatPassthroughFields {
		if v, ok := data[key]; ok {
			chatData[key] = v
		}
	}

	// ---- stream_options injection ----
	injectStreamIncludeUsage(chatData)

	return chatData
}

func applyReasoningOptions(chatData, data map[string]any, model string, cfg Config) {
	reasoning, _ := data["reasoning"].(map[string]any)
	effort := strings.ToLower(stringValue(reasoning["effort"]))
	enabled := effort != "" && effort != "none" && effort != "off" && effort != "disabled"

	// Resolve the reasoning parameter form for this upstream.
	// An explicit REASONING_MODE (cfg.ReasoningMode) always wins; this mirrors
	// cc-switch's provider-level codexChatReasoning declaration and lets users
	// with a custom Chat-Completions-compatible upstream tell the proxy which
	// field the upstream actually accepts. When unset, fall back to inference
	// from model name / base URL.
	mode := cfg.ReasoningMode
	if mode == "" {
		mode = inferReasoningMode(model, cfg.UpstreamBaseURL)
	}
	if mode == ReasoningPassthrough {
		return
	}

	switch mode {
	case ReasoningThinking:
		if enabled {
			chatData["thinking"] = map[string]any{"type": "enabled"}
			chatData["reasoning_effort"] = mapDeepSeekEffort(effort)
		} else if effort != "" {
			chatData["thinking"] = map[string]any{"type": "disabled"}
		}
	case ReasoningThinkingOnly:
		// Only emit the `thinking` object; never a top-level reasoning_effort.
		// Real GLM / Kimi / MiMo upstreams reject reasoning_effort but read
		// thinking.type to gate reasoning on/off.
		if enabled {
			chatData["thinking"] = map[string]any{"type": "enabled"}
		} else if effort != "" {
			chatData["thinking"] = map[string]any{"type": "disabled"}
		}
	case ReasoningEnableThinking:
		chatData["enable_thinking"] = enabled
	case ReasoningSplit:
		chatData["reasoning_split"] = enabled
	case ReasoningEffort:
		if effort != "" {
			chatData["reasoning_effort"] = mapReasoningEffort(effort)
		}
	case ReasoningEffortObj:
		if effort != "" {
			if enabled {
				chatData["reasoning"] = map[string]any{"effort": mapOpenRouterEffort(effort)}
			} else {
				chatData["reasoning"] = map[string]any{"effort": "none"}
			}
		}
	}
}

// inferReasoningMode maps model/baseURL to the upstream reasoning parameter
// form (top-level reasoning_effort, nested reasoning.effort object,
// enable_thinking, reasoning_split, thinking_only). Returns
// ReasoningPassthrough when no compatibility signal is detected, in which case
// applyReasoningOptions leaves reasoning untouched and forwards no reasoning
// field upstream.
//
// The vendor/platform set mirrors cc-switch's
// infer_codex_chat_reasoning_config (codex.rs): platform detection via URL is
// preferred over model name since aggregator platforms may proxy vendor model
// names that no longer reveal the platform's own reasoning contract.
func inferReasoningMode(model, baseURL string) ReasoningMode {
	platform := strings.ToLower(baseURL + " " + model)
	switch {
	case strings.Contains(platform, "openrouter"):
		return ReasoningEffortObj
	case strings.Contains(platform, "siliconflow"):
		return ReasoningEnableThinking
	}

	mLower := strings.ToLower(model)
	switch {
	case strings.Contains(mLower, "glm") || strings.Contains(mLower, "zhipu") || strings.Contains(mLower, "z.ai") ||
		strings.Contains(mLower, "kimi") || strings.Contains(mLower, "moonshot") ||
		strings.Contains(mLower, "mimo"):
		return ReasoningThinkingOnly
	case strings.Contains(mLower, "qwen") || strings.Contains(mLower, "dashscope") || strings.Contains(mLower, "bailian"):
		return ReasoningEnableThinking
	case strings.Contains(mLower, "minimax"):
		return ReasoningSplit
	case strings.Contains(mLower, "deepseek"):
		return ReasoningThinking
	case isOSeriesModel(model) || supportsReasoningEffort(model):
		return ReasoningEffort
	}
	return ReasoningPassthrough
}

// ===========================================================================
// ConvertResponse --chat completions --responses
// ===========================================================================

func ConvertResponse(chatResp, originalReq map[string]any) map[string]any {
	chatID := stringValue(chatResp["id"])
	responseID := convertID(chatID)
	created := intValue(chatResp["created"])
	model := stringValue(chatResp["model"])
	if model == "" {
		model = stringValue(originalReq["model"])
	}

	tc := buildToolContextFromRequest(originalReq)

	var choice map[string]any
	if choices, _ := chatResp["choices"].([]any); len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	if choice == nil {
		choice = map[string]any{}
	}

	message, _ := choice["message"].(map[string]any)
	finishReason := stringValue(choice["finish_reason"])

	var output []any
	reasoningText, messageText := extractReasoningAndMessage(message)
	status, incompleteDetails := mapFinishReasonForOutput(finishReason, nil)

	if reasoningText != "" {
		suffix := strings.TrimPrefix(responseID, "resp-")
		suffix = strings.TrimPrefix(suffix, "resp_")
		output = append(output, map[string]any{
			"type":   "reasoning",
			"id":     "rs_" + suffix,
			"status": "completed",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": reasoningText},
			},
		})
	}

	if len(message) > 0 {
		item := buildOutputItem(message, responseID, finishReason, tc, messageText)
		if item != nil {
			output = append(output, item)
		}
	}

	output = append(output, buildToolOutputItems(message, responseID, tc)...)

	status, incompleteDetails = mapFinishReasonForOutput(finishReason, output)
	output = finalizeOutputItems(output, status)

	usage := convertUsage(chatResp["usage"])
	if reasoningText != "" {
		if ud, ok := usage["output_tokens_details"].(map[string]any); ok {
			if intValue(ud["reasoning_tokens"]) == 0 {
				ud["reasoning_tokens"] = estimateReasoningTokens(reasoningText)
			}
		}
	}

	return map[string]any{
		"id":                  responseID,
		"object":              "response",
		"created_at":          created,
		"status":              status,
		"error":               nil,
		"incomplete_details":  incompleteDetails,
		"instructions":        originalReq["instructions"],
		"max_output_tokens":   originalReq["max_output_tokens"],
		"model":               model,
		"output":              output,
		"parallel_tool_calls": valueOrDefault(originalReq["parallel_tool_calls"], true),
		"temperature":         valueOrDefault(originalReq["temperature"], 1.0),
		"tool_choice":         valueOrDefault(originalReq["tool_choice"], "auto"),
		"tools":               valueOrDefault(originalReq["tools"], []any{}),
		"top_p":               valueOrDefault(originalReq["top_p"], 1.0),
		"truncation":          "disabled",
		"usage":               usage,
		"user":                originalReq["user"],
		"metadata":            valueOrDefault(originalReq["metadata"], map[string]any{}),
	}
}

func extractReasoningAndMessage(message map[string]any) (reasoning, visible string) {
	parts := messageTextParts{}
	parts.collectReasoning(message["reasoning_content"])
	parts.collectReasoning(message["reasoning"])
	parts.collectReasoningDetails(message["reasoning_details"])
	parts.collectVisible(message["content"])
	parts.finishVisibleThinking()

	return strings.Join(parts.reasoningParts, "\n"), strings.Join(parts.visibleParts, "")
}

func finalizeOutputItems(output []any, responseStatus string) []any {
	if responseStatus == "failed" {
		var kept []any
		for _, raw := range output {
			item, ok := raw.(map[string]any)
			if !ok || hasSubstantiveResponseOutput([]any{item}) {
				kept = append(kept, raw)
			}
		}
		return kept
	}
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(item["status"]) == "in_progress" {
			item["status"] = "completed"
		}
	}
	return output
}

type thinkTextAccumulator struct {
	insideThink                  bool
	expectedClose                string
	carry                        string
	foundTag                     bool
	skipLeadingVisibleWhitespace bool
}

var thinkOpenTags = []string{"<think>", "<thinking>"}

func splitThinkText(text string) (reasoning, visible string, found bool) {
	var acc thinkTextAccumulator
	reasoning, visible = acc.consume(text)
	reasoning2, visible2 := acc.finish()
	return reasoning + reasoning2, visible + visible2, acc.foundTag
}

func splitThinkTag(content string) (reasoning, visible string) {
	reasoning, visible, _ = splitThinkText(content)
	return reasoning, visible
}

func (a *thinkTextAccumulator) consume(text string) (reasoning, visible string) {
	buf := a.carry + text
	a.carry = ""
	var reasoningParts, visibleParts strings.Builder

	for len(buf) > 0 {
		if a.skipLeadingVisibleWhitespace && !a.insideThink {
			buf = strings.TrimLeftFunc(buf, unicode.IsSpace)
			if buf == "" {
				break
			}
			a.skipLeadingVisibleWhitespace = false
		}

		if !a.insideThink {
			idx, tag := findThinkTag(buf, thinkOpenTags)
			if idx >= 0 {
				if idx > 0 {
					visibleParts.WriteString(buf[:idx])
				}
				buf = buf[idx+len(tag):]
				a.insideThink = true
				a.expectedClose = matchingThinkCloseTag(tag)
				a.foundTag = true
				continue
			}

			keep := longestThinkPrefixSuffix(buf, thinkOpenTags)
			if keep > 0 {
				if len(buf) > keep {
					visibleParts.WriteString(buf[:len(buf)-keep])
				}
				a.carry = buf[len(buf)-keep:]
			} else {
				visibleParts.WriteString(buf)
			}
			break
		}

		idx, _ := findThinkTag(buf, []string{a.expectedClose})
		if idx >= 0 {
			if idx > 0 {
				reasoningParts.WriteString(buf[:idx])
			}
			buf = buf[idx+len(a.expectedClose):]
			a.insideThink = false
			a.expectedClose = ""
			a.foundTag = true
			a.skipLeadingVisibleWhitespace = true
			continue
		}

		keep := longestThinkPrefixSuffix(buf, []string{a.expectedClose})
		if keep > 0 {
			if len(buf) > keep {
				reasoningParts.WriteString(buf[:len(buf)-keep])
			}
			a.carry = buf[len(buf)-keep:]
		} else {
			reasoningParts.WriteString(buf)
		}
		break
	}

	return reasoningParts.String(), visibleParts.String()
}

func (a *thinkTextAccumulator) finish() (reasoning, visible string) {
	if a.carry == "" {
		return "", ""
	}
	if a.insideThink {
		reasoning = a.carry
	} else {
		visible = a.carry
	}
	a.carry = ""
	return reasoning, visible
}

func findThinkTag(text string, tags []string) (index int, tag string) {
	index = -1
	for _, candidate := range tags {
		if i := strings.Index(text, candidate); i >= 0 {
			if index < 0 || i < index || (i == index && len(candidate) > len(tag)) {
				index = i
				tag = candidate
			}
		}
	}
	return index, tag
}

func matchingThinkCloseTag(openTag string) string {
	switch openTag {
	case "<thinking>":
		return "</thinking>"
	default:
		return "</think>"
	}
}

func longestThinkPrefixSuffix(text string, tags []string) int {
	maxKeep := 0
	for _, tag := range tags {
		for keep := 1; keep < len(tag) && keep <= len(text); keep++ {
			if strings.HasSuffix(text, tag[:keep]) && keep > maxKeep {
				maxKeep = keep
			}
		}
	}
	return maxKeep
}

func estimateReasoningTokens(text string) int {
	return len([]rune(text)) / 3
}

// ===========================================================================
// Input item conversion
// ===========================================================================

func normalizeInputItem(item any, tc *toolContext) (normalizedInputMessage, bool) {
	dict, ok := item.(map[string]any)
	if !ok {
		return normalizedInputMessage{}, false
	}

	itemType := stringValue(dict["type"])

	if itemType == "tool_search_output" {
		if tools, _ := dict["tools"].([]any); len(tools) > 0 {
			for _, t := range tools {
				tc.addResponseTool(t)
			}
		}
		if stringValue(dict["execution"]) == "server" {
			return normalizedInputMessage{}, false
		}
		return normalizedInputMessage{
			role:    "system",
			content: "Loaded tools: " + stringValue(dict["call_id"]),
		}, true
	}

	switch itemType {
	case "input_text":
		text := stringValue(dict["text"])
		if text == "" {
			text = stringValue(dict["content"])
		}
		return normalizedInputMessage{role: "user", content: text}, true
	case "input_image", "input_file", "input_audio":
		part, ok := normalizeStandaloneContentPart(dict)
		if !ok {
			return normalizedInputMessage{}, false
		}
		return normalizedInputMessage{
			role:    "user",
			content: []normalizedContentPart{part},
		}, true
	}

	_, hasRole := dict["role"]
	isMessage := itemType == "message" || (itemType == "" && hasRole)
	if isMessage {
		role := stringValue(dict["role"])
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			role = "system"
		}

		content := dict["content"]
		if contentList, ok := content.([]any); ok {
			normalized, ok := normalizeContentList(contentList)
			if ok {
				content = normalized
			}
		}
		return normalizedInputMessage{role: role, content: content}, true
	}

	if itemType == "function_call" {
		callID := stringValue(dict["call_id"])
		if callID == "" {
			callID = stringValue(dict["id"])
		}
		return normalizedInputMessage{
			role: "assistant",
			content: map[string]any{
				"content": nil,
				"tool_calls": []any{
					map[string]any{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      stringValue(dict["name"]),
							"arguments": valueOrDefault(dict["arguments"], "{}"),
						},
					},
				},
			},
		}, true
	}

	if itemType == "function_call_output" {
		return normalizedInputMessage{
			role: "tool",
			content: map[string]any{
				"tool_call_id": stringValue(dict["call_id"]),
				"content":      stringValue(dict["output"]),
			},
		}, true
	}

	return normalizedInputMessage{}, false
}

func (m normalizedInputMessage) toChatMessage() map[string]any {
	message := map[string]any{"role": m.role}
	switch content := m.content.(type) {
	case nil:
		message["content"] = nil
	case string:
		message["content"] = content
	case []normalizedContentPart:
		message["content"] = renderNormalizedContentParts(content)
	case map[string]any:
		for k, v := range content {
			message[k] = v
		}
	default:
		message["content"] = content
	}
	return message
}

func renderNormalizedContentParts(parts []normalizedContentPart) any {
	rendered := make([]any, 0, len(parts))
	var textBuilder strings.Builder
	allText := true
	for _, part := range parts {
		cp := part.toChatPart()
		rendered = append(rendered, cp)
		if stringValue(cp["type"]) != "text" {
			allText = false
			continue
		}
		textBuilder.WriteString(stringValue(cp["text"]))
	}
	if len(rendered) == 0 {
		return []any{}
	}
	if allText {
		return textBuilder.String()
	}
	return rendered
}

func normalizeContentList(contentList []any) ([]normalizedContentPart, bool) {
	normalized := make([]normalizedContentPart, 0, len(contentList))
	for _, rawPart := range contentList {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, false
		}
		cp, ok := normalizeContentPart(part)
		if !ok {
			return nil, false
		}
		normalized = append(normalized, cp)
	}
	return normalized, true
}

func normalizeStandaloneContentPart(part map[string]any) (normalizedContentPart, bool) {
	return normalizeContentPart(part)
}

func normalizeContentPart(part map[string]any) (normalizedContentPart, bool) {
	switch stringValue(part["type"]) {
	case "input_text", "output_text":
		return normalizedContentPart{
			kind: "text",
			data: map[string]any{"text": stringValue(part["text"])},
		}, true
	case "input_image":
		image := extractImageURLPart(part)
		if len(image) == 0 {
			return normalizedContentPart{}, false
		}
		return normalizedContentPart{
			kind: "image_url",
			data: map[string]any{"image_url": image},
		}, true
	case "input_file":
		file := extractFilePart(part)
		if len(file) == 0 {
			return normalizedContentPart{}, false
		}
		return normalizedContentPart{
			kind: "file",
			data: map[string]any{"file": file},
		}, true
	case "input_audio":
		audio := extractAudioPart(part)
		if len(audio) == 0 {
			return normalizedContentPart{}, false
		}
		return normalizedContentPart{
			kind: "input_audio",
			data: map[string]any{"input_audio": audio},
		}, true
	default:
		return normalizedContentPart{
			kind: stringValue(part["type"]),
			data: cloneMap(part),
		}, true
	}
}

func (p normalizedContentPart) toChatPart() map[string]any {
	part := cloneMap(p.data)
	part["type"] = p.kind
	return part
}

func extractImageURLPart(part map[string]any) map[string]any {
	if image, ok := part["image_url"].(map[string]any); ok {
		return cloneMap(image)
	}
	if url := stringValue(part["image_url"]); url != "" {
		image := map[string]any{"url": url}
		if detail := stringValue(part["detail"]); detail != "" {
			image["detail"] = detail
		}
		return image
	}
	if url := stringValue(part["url"]); url != "" {
		image := map[string]any{"url": url}
		if detail := stringValue(part["detail"]); detail != "" {
			image["detail"] = detail
		}
		return image
	}
	return nil
}

func extractFilePart(part map[string]any) map[string]any {
	if file, ok := part["file"].(map[string]any); ok {
		return cloneMap(file)
	}
	file := map[string]any{}
	for _, key := range []string{"file_id", "file_data", "file_url", "filename", "mime_type"} {
		if value, ok := part[key]; ok {
			file[key] = value
		}
	}
	if len(file) == 0 {
		return nil
	}
	return file
}

func extractAudioPart(part map[string]any) map[string]any {
	if audio, ok := part["input_audio"].(map[string]any); ok {
		return cloneMap(audio)
	}
	audio := map[string]any{}
	for _, key := range []string{"data", "format", "transcript"} {
		if value, ok := part[key]; ok {
			audio[key] = value
		}
	}
	if len(audio) == 0 {
		return nil
	}
	return audio
}

// ===========================================================================
// Tool context building
// ===========================================================================

func buildToolContextFromRequest(data map[string]any) *toolContext {
	tc := newToolContext()
	if tools, _ := data["tools"].([]any); len(tools) > 0 {
		for _, t := range tools {
			tc.addResponseTool(t)
		}
	}
	if input, _ := data["input"].([]any); len(input) > 0 {
		for _, item := range input {
			dict, _ := item.(map[string]any)
			if stringValue(dict["type"]) == "tool_search_output" {
				if tools, _ := dict["tools"].([]any); len(tools) > 0 {
					for _, t := range tools {
						tc.addResponseTool(t)
					}
				}
			}
		}
	}
	return tc
}

func (tc *toolContext) addResponseTool(raw any) {
	switch t := raw.(type) {
	case string:
		tc.addCustomTool(t, "")
	case map[string]any:
		switch stringValue(t["type"]) {
		case "function":
			tc.addFunctionTool(t, "")
		case "custom":
			tc.addCustomTool(stringValue(t["name"]), buildCustomToolDesc(t))
		case "tool_search":
			tc.addToolSearchTool()
		case "namespace":
			tc.addNamespaceTool(t)
		}
	}
}

func (tc *toolContext) addFunctionTool(tool map[string]any, namespace string) {
	name := responsesToolName(tool)
	if name == "" {
		return
	}
	chatName := name
	kind := "function"
	if namespace != "" {
		chatName = flattenNamespaceName(namespace, name)
		kind = "namespace"
	}
	chatTool := responsesToChatFunctionTool(tool, chatName)
	if chatTool == nil {
		return
	}
	tc.addChatTool(chatName, &toolSpec{kind: kind, name: name, namespace: namespace}, chatTool)
}

func (tc *toolContext) addCustomTool(name, desc string) {
	if name == "" {
		return
	}
	if desc == "" {
		desc = customToolInputDesc
	}
	inputDesc := customToolInputDesc
	if desc != customToolInputDesc {
		inputDesc += "\n\n" + desc
	}
	chatTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					customToolInputField: map[string]any{
						"type":        "string",
						"description": inputDesc,
					},
				},
				"required": []any{customToolInputField},
			},
		},
	}
	tc.addChatTool(name, &toolSpec{kind: "custom", name: name}, chatTool)
}

func (tc *toolContext) addToolSearchTool() {
	const name = toolSearchProxyName
	chatTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Search and load Codex tools, plugins, connectors, and MCP namespaces for the current task.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query for tools or connectors to load."},
					"limit": map[string]any{"type": "integer", "description": "Maximum number of tool groups to return."},
				},
				"required": []any{"query"},
			},
		},
	}
	tc.addChatTool(name, &toolSpec{kind: "tool_search", name: name}, chatTool)
}

func (tc *toolContext) addNamespaceTool(tool map[string]any) {
	ns := stringValue(tool["name"])
	if ns == "" {
		return
	}
	children, _ := tool["tools"].([]any)
	if children == nil {
		children, _ = tool["children"].([]any)
	}
	for _, child := range children {
		cm, _ := child.(map[string]any)
		if stringValue(cm["type"]) == "function" {
			tc.addFunctionTool(cm, ns)
		}
	}
}

func responsesToolName(tool map[string]any) string {
	if name := stringValue(tool["name"]); name != "" {
		return name
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		return stringValue(fn["name"])
	}
	return ""
}

func flattenNamespaceName(namespace, name string) string {
	raw := namespace + "__" + name
	if len(raw) > chatToolNameMaxLen {
		raw = raw[:chatToolNameMaxLen]
	}
	return raw
}

func responsesToChatFunctionTool(tool map[string]any, chatName string) map[string]any {
	if fn, ok := tool["function"].(map[string]any); ok {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        chatName,
				"description": valueOrDefault(fn["description"], ""),
				"parameters":  valueOrDefault(fn["parameters"], map[string]any{}),
			},
		}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        chatName,
			"description": valueOrDefault(tool["description"], ""),
			"parameters":  valueOrDefault(tool["parameters"], map[string]any{}),
		},
	}
}

func buildCustomToolDesc(tool map[string]any) string {
	definition, err := json.MarshalIndent(tool, "", "  ")
	if err == nil && len(definition) > 0 {
		return customToolPreservedHeader + "\n" + string(definition)
	}
	desc := stringValue(tool["description"])
	if desc == "" {
		desc = stringValue(tool["name"])
	}
	return customToolPreservedHeader + "\n" + desc
}

func mapSliceToAny(values []map[string]any) []any {
	if len(values) == 0 {
		return []any{}
	}
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

// ---- legacy convertTools (used by tests) ----

func convertTools(tools []any) ([]any, bool) {
	tc := newToolContext()
	for _, t := range tools {
		tc.addResponseTool(t)
	}
	converted := make([]any, len(tc.chatTools))
	for i, ct := range tc.chatTools {
		converted[i] = ct
	}
	hasWebSearch := false
	for _, t := range tools {
		if tm, ok := t.(map[string]any); ok && stringValue(tm["type"]) == "web_search" && boolValue(tm["external_web_access"]) {
			hasWebSearch = true
		}
	}
	return converted, hasWebSearch
}

// ---- tool_choice mapping ----

func mapToolChoice(raw any, tc *toolContext) any {
	if s, ok := raw.(string); ok {
		return s
	}
	dict, ok := raw.(map[string]any)
	if !ok || stringValue(dict["type"]) != "function" {
		return raw
	}
	name := stringValue(dict["name"])
	chatName := tc.chatNameForResponseFunction(name, "")
	return map[string]any{"type": "function", "function": map[string]any{"name": chatName}}
}

func (tc *toolContext) chatNameForResponseFunction(name, namespace string) string {
	if namespace != "" {
		if chatName, ok := tc.namespaceNameToChatName[namespace+"\x00"+name]; ok {
			return chatName
		}
		return flattenNamespaceName(namespace, name)
	}
	return name
}

// ===========================================================================
// Response output item building
// ===========================================================================

func buildOutputItem(message map[string]any, responseID, finishReason string, tc *toolContext, messageText string) map[string]any {
	role := stringValue(message["role"])
	if role == "" {
		role = "assistant"
	}
	itemID := "msg_" + responseSuffix(responseID)

	outputItem := map[string]any{
		"type":    "message",
		"id":      itemID,
		"status":  ternary(finishReason == "", "in_progress", "completed"),
		"role":    role,
		"content": []any{},
	}

	switch content := message["content"].(type) {
	case string:
		if messageText != "" {
			outputItem["content"] = []any{map[string]any{
				"type": "output_text", "text": messageText, "annotations": []any{},
			}}
		}
	case nil:
		if messageText != "" {
			outputItem["content"] = []any{map[string]any{
				"type": "output_text", "text": messageText, "annotations": []any{},
			}}
		}
	case []any:
		var textContent []any
		var contentThink thinkTextAccumulator
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			pt := stringValue(part["type"])
			if isReasoningContentPartType(pt) {
				continue
			}
			converted := convertOutputPartWithoutThinking(part, &contentThink)
			if converted != nil {
				textContent = append(textContent, converted)
			}
		}
		if _, visible := contentThink.finish(); visible != "" {
			textContent = append(textContent, map[string]any{
				"type": "output_text", "text": visible, "annotations": []any{},
			})
		}
		outputItem["content"] = textContent
	}

	return outputItem
}

func convertOutputPartWithoutThinking(part map[string]any, acc *thinkTextAccumulator) map[string]any {
	if visible, ok := visibleOutputText(part, acc); ok {
		if visible == "" {
			return nil
		}
		return map[string]any{
			"type": "output_text", "text": visible, "annotations": []any{},
		}
	}
	return part
}

func visibleOutputText(part map[string]any, acc *thinkTextAccumulator) (string, bool) {
	for _, key := range []string{"text", "content", "output"} {
		if visible, ok := splitThinkVisibleString(part[key], acc); ok {
			return visible, true
		}
	}
	return "", false
}

func splitThinkVisibleString(value any, acc *thinkTextAccumulator) (string, bool) {
	text := stringValue(value)
	if text == "" {
		return "", false
	}
	if acc == nil {
		_, visible, _ := splitThinkText(text)
		return visible, true
	}
	_, visible := acc.consume(text)
	return visible, true
}

func isReasoningContentPartType(partType string) bool {
	switch partType {
	case "thinking", "redacted_thinking", "summary_text", "reasoning_text", "reasoning":
		return true
	default:
		return false
	}
}

func buildToolOutputItems(message map[string]any, responseID string, tc *toolContext) []any {
	suffix := strings.TrimPrefix(responseID, "resp-")
	suffix = strings.TrimPrefix(suffix, "resp_")
	toolCalls, _ := message["tool_calls"].([]any)
	var items []any
	for index, raw := range toolCalls {
		tcMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		items = append(items, restoreToolCall(tcMap, suffix, index, tc))
	}
	return items
}

func restoreToolCall(toolCall map[string]any, suffix string, index int, tc *toolContext) map[string]any {
	fn, _ := toolCall["function"].(map[string]any)
	chatName := stringValue(fn["name"])
	callID := stringValue(toolCall["id"])
	args := canonicalizeArguments(stringValue(fn["arguments"]))

	spec := tc.chatNameToSpec[chatName]
	if spec == nil {
		id := toolCallFallbackID("fc_", suffix, index)
		if callID != "" {
			id = "fc_" + strings.TrimPrefix(callID, "call_")
		}
		return map[string]any{
			"type":      "function_call",
			"id":        id,
			"call_id":   callID,
			"status":    "completed",
			"name":      chatName,
			"arguments": args,
		}
	}

	switch spec.kind {
	case "namespace":
		id := toolCallFallbackID("fc_", suffix, index)
		if callID != "" {
			id = "fc_" + strings.TrimPrefix(callID, "call_")
		}
		return map[string]any{
			"type":      "function_call",
			"id":        id,
			"call_id":   callID,
			"status":    "completed",
			"namespace": spec.namespace,
			"name":      spec.name,
			"arguments": args,
		}
	case "custom":
		id := toolCallFallbackID("ctc_", suffix, index)
		if callID != "" {
			id = "ctc_" + strings.TrimPrefix(callID, "call_")
		}
		input := extractCustomToolInput(args)
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      id,
			"call_id": callID,
			"status":  "completed",
			"name":    spec.name,
			"input":   input,
		}
	case "tool_search":
		id := toolCallFallbackID("tsc_", suffix, index)
		if callID != "" {
			id = "tsc_" + strings.TrimPrefix(callID, "call_")
		}
		return map[string]any{
			"type":      "tool_search_call",
			"id":        id,
			"call_id":   callID,
			"status":    "completed",
			"arguments": parseJSONObject(args),
			"execution": "client",
		}
	default:
		id := toolCallFallbackID("fc_", suffix, index)
		if callID != "" {
			id = "fc_" + strings.TrimPrefix(callID, "call_")
		}
		return map[string]any{
			"type":      "function_call",
			"id":        id,
			"call_id":   callID,
			"status":    "completed",
			"name":      spec.name,
			"arguments": args,
		}
	}
}

func extractCustomToolInput(args string) string {
	obj := parseJSONObject(args)
	if obj == nil {
		return args
	}
	if input, ok := obj[customToolInputField]; ok {
		return stringValue(input)
	}
	return args
}

func canonicalizeArguments(args string) string {
	if args == "" {
		return "{}"
	}
	obj := parseJSONObject(args)
	if obj == nil {
		return args
	}
	canonical, err := json.Marshal(canonicalizeOrder(obj))
	if err != nil {
		return args
	}
	return string(canonical)
}

var _muCanonicalize sync.Mutex

func canonicalizeOrder(v any) any {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make(map[string]any, len(val))
		for _, k := range keys {
			ordered[k] = canonicalizeOrder(val[k])
		}
		return ordered
	case []any:
		result := make([]any, len(val))
		for i, item := range val {
			result[i] = canonicalizeOrder(item)
		}
		return result
	default:
		return val
	}
}

// ---- legacy convertToolCall (used by tests) ----

func convertToolCall(toolCall map[string]any) map[string]any {
	function, _ := toolCall["function"].(map[string]any)
	args := canonicalizeArguments(stringValue(function["arguments"]))
	return map[string]any{
		"type":      "tool_call",
		"id":        toolCall["id"],
		"call_type": valueOrDefault(toolCall["type"], "function"),
		"status":    "completed",
		"name":      stringValue(function["name"]),
		"arguments": args,
	}
}

// ===========================================================================
// Common helpers
// ===========================================================================

func convertID(chatID string) string {
	switch {
	case strings.HasPrefix(chatID, "chatcmpl-"):
		return strings.Replace(chatID, "chatcmpl-", "resp-", 1)
	case strings.HasPrefix(chatID, "chatcmpl"):
		return strings.Replace(chatID, "chatcmpl", "resp", 1)
	case chatID != "":
		return "resp_" + chatID
	default:
		return "resp_unknown"
	}
}

func mapFinishReason(finishReason string) (string, map[string]any) {
	switch finishReason {
	case "stop", "tool_calls":
		return "completed", nil
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	case "":
		return "in_progress", nil
	default:
		return "completed", nil
	}
}

func mapFinishReasonForOutput(finishReason string, output []any) (string, map[string]any) {
	if finishReason != "" {
		return mapFinishReason(finishReason)
	}
	if hasSubstantiveResponseOutput(output) {
		return "incomplete", nil
	}
	return "failed", nil
}

func hasSubstantiveResponseOutput(output []any) bool {
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(item["type"]) {
		case "message":
			if content, ok := item["content"].([]any); ok {
				for _, rawPart := range content {
					part, _ := rawPart.(map[string]any)
					if strings.TrimSpace(stringValue(part["text"])) != "" {
						return true
					}
				}
			}
		case "reasoning":
			if summary, ok := item["summary"].([]any); ok {
				for _, rawPart := range summary {
					part, _ := rawPart.(map[string]any)
					if strings.TrimSpace(stringValue(part["text"])) != "" {
						return true
					}
				}
			}
		case "function_call", "custom_tool_call", "tool_search_call":
			return true
		}
	}
	return false
}

func convertUsage(rawUsage any) map[string]any {
	usage, _ := rawUsage.(map[string]any)
	promptTokens := intValue(usage["prompt_tokens"])
	completionTokens := intValue(usage["completion_tokens"])
	totalTokens := intValue(usage["total_tokens"])

	cachedTokens := 0
	if promptDetails, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		cachedTokens = intValue(promptDetails["cached_tokens"])
	}
	if cachedTokens == 0 {
		cachedTokens = intValue(usage["cache_read_input_tokens"])
	}
	if cc := intValue(usage["cache_creation_input_tokens"]); cc > 0 && cachedTokens == 0 {
		cachedTokens = cc
	}

	reasoningTokens := 0
	if completionDetails, ok := usage["completion_tokens_details"].(map[string]any); ok {
		reasoningTokens = intValue(completionDetails["reasoning_tokens"])
	}

	return map[string]any{
		"input_tokens":  promptTokens,
		"output_tokens": completionTokens,
		"total_tokens":  totalTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": cachedTokens,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoningTokens,
		},
	}
}

// ===========================================================================
// system message collapse & model detection
// ===========================================================================

func collapseSystemMessages(messages []any) []any {
	var systemTexts []string
	var rest []any
	for _, msg := range messages {
		m, _ := msg.(map[string]any)
		if stringValue(m["role"]) == "system" {
			if text := stringValue(m["content"]); text != "" {
				systemTexts = append(systemTexts, text)
			}
			continue
		}
		rest = append(rest, msg)
	}
	var result []any
	if len(systemTexts) > 0 {
		result = append(result, map[string]any{
			"role": "system", "content": strings.Join(systemTexts, "\n\n"),
		})
	}
	result = append(result, rest...)
	return result
}

func isOSeriesModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4")
}

func supportsReasoningEffort(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "deepseek") || strings.Contains(lower, "gpt-5") || isOSeriesModel(model)
}

func injectStreamIncludeUsage(data map[string]any) {
	if !boolValue(data["stream"]) {
		return
	}
	if _, ok := data["stream_options"]; ok {
		return
	}
	data["stream_options"] = map[string]any{"include_usage": true}
}

func mapReasoningEffort(effort string) string {
	switch strings.ToLower(effort) {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max":
		return "high"
	default:
		return effort
	}
}

func mapDeepSeekEffort(effort string) string {
	switch strings.ToLower(effort) {
	case "max", "xhigh":
		return "max"
	default:
		return "high"
	}
}

func mapOpenRouterEffort(effort string) string {
	switch strings.ToLower(effort) {
	case "max", "xhigh":
		return "xhigh"
	case "high":
		return "high"
	case "medium":
		return "medium"
	case "low", "minimal":
		return "minimal"
	default:
		return "high"
	}
}

func parseJSONObject(s string) map[string]any {
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// ===========================================================================
// Error normalization
// ===========================================================================

func NormalizeUpstreamError(body map[string]any) map[string]any {
	if body == nil {
		return map[string]any{
			"error": map[string]any{
				"message": "Upstream returned an empty error response",
				"type":    "upstream_error",
			},
		}
	}
	if errObj, ok := body["error"].(map[string]any); ok {
		return map[string]any{
			"error": map[string]any{
				"message": stringValue(errObj["message"]),
				"type":    valueOrDefaultString(errObj["type"], "upstream_error"),
				"code":    errObj["code"],
				"param":   errObj["param"],
			},
		}
	}
	if baseResp, ok := body["base_resp"].(map[string]any); ok {
		return map[string]any{
			"error": map[string]any{
				"message": stringValue(baseResp["status_msg"]),
				"type":    "upstream_error",
				"code":    baseResp["status_code"],
			},
		}
	}
	if msg := stringValue(body["error"]); msg != "" {
		return map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}}
	}
	if detail := stringValue(body["detail"]); detail != "" {
		return map[string]any{"error": map[string]any{"message": detail, "type": "upstream_error"}}
	}
	if msg := stringValue(body["message"]); msg != "" {
		return map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}}
	}
	return map[string]any{
		"error": map[string]any{
			"message": fmt.Sprintf("Upstream error: %v", body),
			"type":    "upstream_error",
		},
	}
}

// ===========================================================================
// StreamingConverter --chat SSE --responses SSE
// ===========================================================================

type StreamingConverter struct {
	initialized          bool
	outputItemAdded      bool
	contentPartAdded     bool
	textDone             bool
	outputItemDone       bool
	completed            bool
	reasoningDone        bool
	sawSubstantiveOutput bool
	nextOutputIndex      int
	messageOutputIndex   *int
	reasoningOutputIndex *int
	responseID           string
	messageID            string
	model                string
	created              int
	messageRole          string
	fullText             string
	reasoningText        string
	contentThink         thinkTextAccumulator
	finishReason         string
	toolCalls            map[int]*streamToolCall
	sseBuffer            string
	usage                map[string]any
	rawReasoning         map[string]any
}

type streamToolCallKind string

const (
	streamToolCallKindUnknown    streamToolCallKind = ""
	streamToolCallKindFunction   streamToolCallKind = "function"
	streamToolCallKindNamespace  streamToolCallKind = "namespace"
	streamToolCallKindCustom     streamToolCallKind = "custom"
	streamToolCallKindToolSearch streamToolCallKind = "tool_search"
)

type normalizedStreamToolCall struct {
	kind      streamToolCallKind
	namespace string
	name      string
	input     string
	execution string
}

type streamToolCall struct {
	spec        normalizedStreamToolCall
	ID          string
	ItemID      string
	Name        string
	Arguments   string
	Added       bool
	OutputIndex *int
}

func NewStreamingConverter() *StreamingConverter {
	return &StreamingConverter{
		messageRole:  "assistant",
		toolCalls:    map[int]*streamToolCall{},
		rawReasoning: map[string]any{},
		usage:        map[string]any{},
	}
}

type finalStreamingOutputItem struct {
	outputIndex int
	addedEvent  string
	doneEvents  []string
	output      map[string]any
}

func (c *StreamingConverter) Feed(chunk []byte) []string {
	c.sseBuffer += strings.ReplaceAll(strings.ReplaceAll(string(chunk), "\r\n", "\n"), "\r", "\n")
	var events []string

	for {
		index := strings.Index(c.sseBuffer, "\n\n")
		if index < 0 {
			break
		}
		rawEvent := c.sseBuffer[:index]
		c.sseBuffer = c.sseBuffer[index+2:]

		var dataLines []string
		for _, line := range strings.Split(rawEvent, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(dataLines) == 0 {
			continue
		}

		payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
		if payload == "[DONE]" {
			events = append(events, c.Finish()...)
			continue
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			continue
		}
		events = append(events, c.processChunk(data)...)
	}
	return events
}

func (c *StreamingConverter) Finish() []string {
	events := c.finishOutputItems()
	events = append(events, c.finishResponse(c.finalStatus())...)
	return events
}

func (c *StreamingConverter) finalStatus() string {
	if c.finishReason == "" {
		if c.sawSubstantiveOutput {
			return "incomplete"
		}
		return "failed"
	}
	switch c.finishReason {
	case "length", "content_filter":
		return "incomplete"
	default:
		return "completed"
	}
}

func (c *StreamingConverter) processChunk(data map[string]any) []string {
	events := c.ensureInitialized(data)

	if usage, ok := data["usage"].(map[string]any); ok && len(usage) > 0 {
		c.usage = usage
	}

	choices, ok := data["choices"].([]any)
	if !ok || len(choices) == 0 {
		return events
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		delta = map[string]any{}
	}
	finishReason := stringValue(choice["finish_reason"])

	if role := stringValue(delta["role"]); role != "" {
		c.messageRole = role
	}

	// reasoning_content (DeepSeek, Kimi, GLM)
	if rc := stringValue(delta["reasoning_content"]); rc != "" {
		events = append(events, c.appendReasoningDelta(rc)...)
	}

	if reasoningDelta := extractReasoningDelta(delta["reasoning"]); reasoningDelta != "" {
		events = append(events, c.appendReasoningDelta(reasoningDelta)...)
	}

	// visible content delta
	if content := stringValue(delta["content"]); content != "" {
		reasoning, visible := c.contentThink.consume(content)
		if reasoning != "" {
			events = append(events, c.appendReasoningDelta(reasoning)...)
		}
		if visible != "" {
			events = append(events, c.appendVisibleDelta(visible)...)
		}
	}

	// reasoning_details delta (MiniMax)
	if details, ok := delta["reasoning_details"].([]any); ok {
		for _, d := range details {
			if dm, ok := d.(map[string]any); ok {
				if text := stringValue(dm["text"]); text != "" {
					events = append(events, c.appendReasoningDelta(text)...)
				}
			}
		}
	}

	// tool_calls delta
	if rawToolCalls, ok := delta["tool_calls"].([]any); ok {
		for _, rawTC := range rawToolCalls {
			tc, ok := rawTC.(map[string]any)
			if !ok {
				continue
			}
			c.sawSubstantiveOutput = true
			events = append(events, c.processToolCallDelta(tc)...)
		}
	}

	if finishReason != "" {
		c.finishReason = finishReason
	}
	if finishReason != "" && !c.outputItemDone {
		status := c.finalStatus()
		events = append(events, c.finishOutputItems()...)
		events = append(events, c.finishResponse(status)...)
	}

	return events
}

func extractReasoningDelta(value any) string {
	parts := messageTextParts{}
	parts.collectReasoning(value)
	return strings.Join(parts.reasoningParts, "")
}

func (c *StreamingConverter) appendReasoningDelta(delta string) []string {
	if delta == "" {
		return nil
	}
	c.reasoningText += delta
	c.sawSubstantiveOutput = true
	events := c.ensureReasoningItem()
	events = append(events, sseEvent("response.reasoning_text.delta", map[string]any{
		"item_id": c.reasoningItemID(), "output_index": c.reasoningOutputIndexValue(),
		"content_index": 0, "delta": delta,
	}))
	return events
}

func (c *StreamingConverter) appendVisibleDelta(delta string) []string {
	if delta == "" {
		return nil
	}
	c.fullText += delta
	c.sawSubstantiveOutput = true
	events := c.ensureContentPart()
	events = append(events, sseEvent("response.output_text.delta", map[string]any{
		"item_id": c.messageID, "output_index": c.messageOutputIndexValue(),
		"content_index": 0, "delta": delta,
	}))
	return events
}

func (c *StreamingConverter) flushThinkContent() []string {
	reasoning, visible := c.contentThink.finish()
	var events []string
	if reasoning != "" {
		events = append(events, c.appendReasoningDelta(reasoning)...)
	}
	if visible != "" {
		events = append(events, c.appendVisibleDelta(visible)...)
	}
	return events
}

func (c *StreamingConverter) ensureReasoningItem() []string {
	if c.reasoningDone {
		return nil
	}
	c.reasoningDone = true
	outputIndex := c.reasoningOutputIndexValue()
	return []string{sseEvent("response.output_item.added", map[string]any{
		"output_index": outputIndex,
		"item": map[string]any{
			"type":    "reasoning",
			"id":      c.reasoningItemID(),
			"status":  "in_progress",
			"summary": []any{},
		},
	})}
}

func (c *StreamingConverter) ensureReasoningDelta() []string {
	if c.reasoningText == "" {
		return nil
	}
	events := c.ensureReasoningItem()
	events = append(events, sseEvent("response.reasoning_text.delta", map[string]any{
		"item_id": c.reasoningItemID(), "output_index": c.reasoningOutputIndexValue(),
		"content_index": 0, "delta": c.reasoningText,
	}))
	return events
}

func (c *StreamingConverter) ensureInitialized(data map[string]any) []string {
	if c.initialized {
		return nil
	}
	c.initialized = true
	c.responseID = convertID(stringValue(data["id"]))
	suffix := strings.TrimPrefix(c.responseID, "resp-")
	suffix = strings.TrimPrefix(suffix, "resp_")
	if suffix == "" {
		suffix = "unknown"
	}
	c.messageID = "msg_" + suffix
	c.model = stringValue(data["model"])
	c.created = intValue(data["created"])

	return []string{
		sseEvent("response.created", map[string]any{
			"response": buildResponseStub(c.responseID, c.created, "in_progress", c.model),
		}),
		sseEvent("response.in_progress", map[string]any{
			"response": map[string]any{
				"id": c.responseID, "object": "response", "status": "in_progress",
			},
		}),
	}
}

func (c *StreamingConverter) ensureMessageItem(role ...string) []string {
	if c.outputItemAdded {
		return nil
	}
	c.outputItemAdded = true
	outputIndex := c.messageOutputIndexValue()
	itemRole := c.messageRole
	if len(role) > 0 && role[0] != "" {
		itemRole = role[0]
	}
	return []string{sseEvent("response.output_item.added", map[string]any{
		"output_index": outputIndex,
		"item": map[string]any{
			"type": "message", "id": c.messageID, "status": "in_progress",
			"role": itemRole, "content": []any{},
		},
	})}
}

func (c *StreamingConverter) ensureContentPart() []string {
	if c.contentPartAdded {
		return nil
	}
	c.contentPartAdded = true
	events := c.ensureMessageItem()
	events = append(events, sseEvent("response.content_part.added", map[string]any{
		"item_id": c.messageID, "output_index": c.messageOutputIndexValue(),
		"content_index": 0,
		"part": map[string]any{
			"type": "output_text", "text": "", "annotations": []any{},
		},
	}))
	return events
}

func (c *StreamingConverter) processToolCallDelta(toolCall map[string]any) []string {
	index := intValue(toolCall["index"])
	entry, ok := c.toolCalls[index]
	if !ok {
		entry = &streamToolCall{}
		c.toolCalls[index] = entry
	}
	if id := stringValue(toolCall["id"]); id != "" {
		entry.ID = id
	}
	function, _ := toolCall["function"].(map[string]any)
	if name := stringValue(function["name"]); name != "" {
		entry.Name = name
	}
	if entry.OutputIndex == nil {
		outputIndex := c.allocateOutputIndex()
		entry.OutputIndex = &outputIndex
	}
	entry.spec = normalizeStreamToolCall(entry.Name, entry.Arguments)
	if entry.spec.kind != streamToolCallKindUnknown && entry.ItemID == "" {
		entry.ItemID = c.toolItemIDForSpec(entry.spec, entry.ID, index)
	}

	var events []string
	if !entry.Added && entry.spec.kind != streamToolCallKindUnknown {
		entry.Added = true
		events = append(events, sseEvent("response.output_item.added", map[string]any{
			"output_index": *entry.OutputIndex,
			"item":         renderStreamToolCallItem(entry.spec, entry.ItemID, entry.ID, "", "in_progress"),
		}))
	}
	if arguments := stringValue(function["arguments"]); arguments != "" {
		entry.Arguments += arguments
		entry.spec = normalizeStreamToolCall(entry.Name, entry.Arguments)
		if entry.spec.kind != streamToolCallKindUnknown {
			if entry.ItemID == "" {
				entry.ItemID = c.toolItemIDForSpec(entry.spec, entry.ID, index)
			}
			if !entry.Added {
				entry.Added = true
				events = append(events, sseEvent("response.output_item.added", map[string]any{
					"output_index": *entry.OutputIndex,
					"item":         renderStreamToolCallItem(entry.spec, entry.ItemID, entry.ID, "", "in_progress"),
				}))
			}
			events = append(events, sseEvent("response.function_call_arguments.delta", map[string]any{
				"item_id": entry.ItemID, "output_index": *entry.OutputIndex,
				"delta": arguments,
			}))
		}
	}
	return events
}

func (c *StreamingConverter) finishOutputItems() []string {
	if c.outputItemDone {
		return nil
	}
	c.outputItemDone = true

	events := c.flushThinkContent()
	for _, item := range c.orderedFinalOutputItems() {
		if item.addedEvent != "" {
			events = append(events, item.addedEvent)
		}
		events = append(events, item.doneEvents...)
	}
	return events
}

func (c *StreamingConverter) finishResponse(status string) []string {
	if c.completed {
		return nil
	}
	c.completed = true
	return []string{sseEvent("response.completed", map[string]any{
		"response": map[string]any{
			"id":     valueOrDefault(c.responseID, "resp_unknown"),
			"object": "response", "created_at": c.created, "status": status,
			"output": c.buildOutput(), "usage": c.buildUsage(),
		},
	})}
}

func (c *StreamingConverter) buildOutput() []any {
	items := c.orderedFinalOutputItems()
	output := make([]any, 0, len(items))
	for _, item := range items {
		output = append(output, item.output)
	}
	return output
}

func (c *StreamingConverter) orderedFinalOutputItems() []finalStreamingOutputItem {
	items := make([]finalStreamingOutputItem, 0, 1+len(c.toolCalls))

	if c.reasoningText != "" {
		outputIndex := c.reasoningOutputIndexValue()
		items = append(items, finalStreamingOutputItem{
			outputIndex: outputIndex,
			output: map[string]any{
				"type": "reasoning", "id": c.reasoningItemID(), "status": "completed",
				"summary": []any{
					map[string]any{"type": "summary_text", "text": c.reasoningText},
				},
			},
			doneEvents: []string{
				sseEvent("response.reasoning_text.done", map[string]any{
					"item_id": c.reasoningItemID(), "output_index": outputIndex, "content_index": 0,
					"text": c.reasoningText,
				}),
				sseEvent("response.output_item.done", map[string]any{
					"output_index": outputIndex,
					"item": map[string]any{
						"type": "reasoning", "id": c.reasoningItemID(), "status": "completed",
						"summary": []any{
							map[string]any{"type": "summary_text", "text": c.reasoningText},
						},
					},
				}),
			},
		})
	}

	if c.contentPartAdded {
		outputIndex := c.messageOutputIndexValue()
		items = append(items, finalStreamingOutputItem{
			outputIndex: outputIndex,
			output: map[string]any{
				"type": "message", "id": c.messageID, "status": "completed",
				"role":    c.messageRole,
				"content": []any{map[string]any{"type": "output_text", "text": c.fullText, "annotations": []any{}}},
			},
			doneEvents: []string{
				sseEvent("response.output_text.done", map[string]any{
					"item_id": c.messageID, "output_index": outputIndex,
					"content_index": 0, "text": c.fullText,
				}),
				sseEvent("response.content_part.done", map[string]any{
					"item_id": c.messageID, "output_index": outputIndex,
					"content_index": 0,
					"part": map[string]any{
						"type": "output_text", "text": c.fullText, "annotations": []any{},
					},
				}),
				sseEvent("response.output_item.done", map[string]any{
					"output_index": outputIndex,
					"item": map[string]any{
						"type": "message", "id": c.messageID, "status": "completed",
						"role": c.messageRole, "content": []any{map[string]any{"type": "output_text", "text": c.fullText, "annotations": []any{}}},
					},
				}),
			},
		})
	}

	for _, index := range sortedToolIndexes(c.toolCalls) {
		tc := c.toolCalls[index]
		if tc.OutputIndex == nil {
			continue
		}
		tc.spec = normalizeStreamToolCall(tc.Name, tc.Arguments)
		if tc.spec.kind == streamToolCallKindUnknown {
			tc.spec = streamToolCallSpecFromArguments(tc.Name, tc.Arguments)
		}
		if tc.ItemID == "" {
			tc.ItemID = c.toolItemIDForSpec(tc.spec, tc.ID, index)
		}
		outputIndex := *tc.OutputIndex
		item := renderStreamToolCallItem(tc.spec, tc.ItemID, tc.ID, tc.Arguments, "completed")
		addedEvent := ""
		if !tc.Added {
			tc.Added = true
			addedEvent = sseEvent("response.output_item.added", map[string]any{
				"output_index": outputIndex,
				"item":         renderStreamToolCallItem(tc.spec, tc.ItemID, tc.ID, tc.Arguments, "in_progress"),
			})
		}
		items = append(items, finalStreamingOutputItem{
			outputIndex: outputIndex,
			addedEvent:  addedEvent,
			output:      item,
			doneEvents: []string{
				sseEvent("response.function_call_arguments.done", map[string]any{
					"item_id": tc.ItemID, "output_index": outputIndex,
					"arguments": tc.Arguments,
				}),
				sseEvent("response.output_item.done", map[string]any{
					"output_index": outputIndex,
					"item":         item,
				}),
			},
		})
	}

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].outputIndex < items[j].outputIndex
	})
	return items
}

func (c *StreamingConverter) buildUsage() map[string]any {
	return map[string]any{
		"input_tokens":  intValue(c.usage["prompt_tokens"]),
		"output_tokens": intValue(c.usage["completion_tokens"]),
		"total_tokens":  intValue(c.usage["total_tokens"]),
	}
}

func (c *StreamingConverter) allocateOutputIndex() int {
	idx := c.nextOutputIndex
	c.nextOutputIndex++
	return idx
}

func (c *StreamingConverter) messageOutputIndexValue() int {
	if c.messageOutputIndex == nil {
		idx := c.allocateOutputIndex()
		c.messageOutputIndex = &idx
	}
	return *c.messageOutputIndex
}

func (c *StreamingConverter) reasoningOutputIndexValue() int {
	if c.reasoningOutputIndex == nil {
		idx := c.allocateOutputIndex()
		c.reasoningOutputIndex = &idx
	}
	return *c.reasoningOutputIndex
}

func (c *StreamingConverter) reasoningItemID() string {
	suffix := responseSuffix(c.responseID)
	return "rs_" + suffix
}

func responseSuffix(responseID string) string {
	suffix := strings.TrimPrefix(responseID, "resp-")
	suffix = strings.TrimPrefix(suffix, "resp_")
	if suffix == "" {
		return "unknown"
	}
	return suffix
}

func toolCallFallbackID(prefix, suffix string, index int) string {
	if suffix == "" {
		suffix = "unknown"
	}
	return fmt.Sprintf("%s%s_%d", prefix, suffix, index)
}

func (c *StreamingConverter) toolItemIDForSpec(spec normalizedStreamToolCall, callID string, index int) string {
	prefix := "fc_"
	switch spec.kind {
	case streamToolCallKindCustom:
		prefix = "ctc_"
	case streamToolCallKindToolSearch:
		prefix = "tsc_"
	}
	if callID != "" {
		return prefix + strings.TrimPrefix(callID, "call_")
	}
	suffix := strings.TrimPrefix(c.responseID, "resp-")
	suffix = strings.TrimPrefix(suffix, "resp_")
	if suffix == "" {
		suffix = "unknown"
	}
	return fmt.Sprintf("%s%s_%d", prefix, suffix, index)
}

func normalizeStreamToolCall(name, arguments string) normalizedStreamToolCall {
	spec := normalizedStreamToolCall{name: name}
	switch {
	case name == "":
		return spec
	case name == toolSearchProxyName:
		spec.kind = streamToolCallKindToolSearch
		spec.execution = "client"
		return spec
	case strings.Contains(name, "__"):
		namespace, child, found := strings.Cut(name, "__")
		if found && namespace != "" && child != "" {
			spec.kind = streamToolCallKindNamespace
			spec.namespace = namespace
			spec.name = child
			return spec
		}
	}

	obj := parseJSONObject(arguments)
	if obj == nil {
		return spec
	}
	if len(obj) == 1 {
		if input, ok := obj[customToolInputField]; ok {
			spec.kind = streamToolCallKindCustom
			spec.input = stringValue(input)
			return spec
		}
	}
	spec.kind = streamToolCallKindFunction
	return spec
}

func streamToolCallSpecFromArguments(name, arguments string) normalizedStreamToolCall {
	return normalizeStreamToolCall(name, arguments)
}

func renderStreamToolCallItem(spec normalizedStreamToolCall, itemID, callID, arguments, status string) map[string]any {
	switch spec.kind {
	case streamToolCallKindNamespace:
		return map[string]any{
			"type":      "function_call",
			"id":        itemID,
			"call_id":   callID,
			"status":    status,
			"namespace": spec.namespace,
			"name":      spec.name,
			"arguments": arguments,
		}
	case streamToolCallKindCustom:
		input := spec.input
		if input == "" {
			input = extractCustomToolInput(arguments)
		}
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      itemID,
			"call_id": callID,
			"status":  status,
			"name":    spec.name,
			"input":   input,
		}
	case streamToolCallKindToolSearch:
		return map[string]any{
			"type":      "tool_search_call",
			"id":        itemID,
			"call_id":   callID,
			"status":    status,
			"arguments": parseJSONObject(arguments),
			"execution": valueOrDefault(spec.execution, "client"),
		}
	default:
		return map[string]any{
			"type":      "function_call",
			"id":        itemID,
			"call_id":   callID,
			"status":    status,
			"name":      spec.name,
			"arguments": arguments,
		}
	}
}

func buildResponseStub(responseID string, created int, status, model string) map[string]any {
	return map[string]any{
		"id": responseID, "object": "response", "created_at": created, "status": status,
		"error": nil, "incomplete_details": nil, "instructions": nil, "max_output_tokens": nil,
		"model": model, "output": []any{},
		"parallel_tool_calls": true, "temperature": 1.0, "tool_choice": "auto",
		"tools": []any{}, "top_p": 1.0, "truncation": "disabled",
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"user":  nil, "metadata": map[string]any{},
	}
}

func sseEvent(eventType string, data map[string]any) string {
	payload := map[string]any{"type": eventType}
	for k, v := range data {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, body)
}

func sortedToolIndexes(toolCalls map[int]*streamToolCall) []int {
	indexes := make([]int, 0, len(toolCalls))
	for index := range toolCalls {
		indexes = append(indexes, index)
	}
	for i := 0; i < len(indexes); i++ {
		for j := i + 1; j < len(indexes); j++ {
			if indexes[j] < indexes[i] {
				indexes[i], indexes[j] = indexes[j], indexes[i]
			}
		}
	}
	return indexes
}

// ===========================================================================
// Generic value helpers
// ===========================================================================

func valueOrDefaultString(value any, fallback string) string {
	if value == nil {
		return fallback
	}
	return stringValue(value)
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		if typed == nil {
			return ""
		}
		return fmt.Sprint(typed)
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func boolValue(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
}

func valueOrDefault[T any](value any, fallback T) any {
	if value == nil {
		return fallback
	}
	return value
}

func ternary[T any](condition bool, whenTrue, whenFalse T) T {
	if condition {
		return whenTrue
	}
	return whenFalse
}
