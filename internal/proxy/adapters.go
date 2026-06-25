package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Tool conversion context — mirrors cc-switch CodexToolContext
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
	// ReasoningPassthrough — leave reasoning field as-is (default for unknown providers).
	ReasoningPassthrough ReasoningMode = ""
	// ReasoningEffort — map to top-level reasoning_effort (OpenAI).
	ReasoningEffort ReasoningMode = "effort"
	// ReasoningThinking — map to thinking.type + reasoning_effort (DeepSeek).
	ReasoningThinking ReasoningMode = "thinking"
	// ReasoningEnableThinking — map to enable_thinking (SiliconFlow, Qwen on some platforms).
	ReasoningEnableThinking ReasoningMode = "enable_thinking"
	// ReasoningSplit — map to reasoning_split (MiniMax).
	ReasoningSplit ReasoningMode = "reasoning_split"
	// ReasoningEffortObj — map to reasoning.effort nested object (OpenRouter).
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
// ConvertRequest — responses → chat completions
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
			msg, ok := convertInputItem(item, tc)
			if ok {
				messages = append(messages, msg)
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
		chatData["tools"] = tc.chatTools
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

	mode := cfg.ReasoningMode
	if mode == "" {
		if isOSeriesModel(model) || supportsReasoningEffort(model) {
			mode = ReasoningEffort
		} else {
			return
		}
	}

	switch mode {
	case ReasoningThinking:
		if enabled {
			chatData["thinking"] = map[string]any{"type": "enabled"}
			chatData["reasoning_effort"] = mapDeepSeekEffort(effort)
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

// ===========================================================================
// ConvertResponse — chat completions → responses
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

	status, incompleteDetails := mapFinishReason(finishReason)

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
	if rc := stringValue(message["reasoning_content"]); rc != "" {
		reasoning = rc
	}

	if details, _ := message["reasoning_details"].([]any); len(details) > 0 && reasoning == "" {
		var parts []string
		for _, d := range details {
			if dm, ok := d.(map[string]any); ok {
				parts = append(parts, stringValue(dm["text"]))
			}
		}
		reasoning = strings.Join(parts, "\n")
	}

	if contentArr, ok := message["content"].([]any); ok {
		var textParts, thinkParts []string
		for _, block := range contentArr {
			bm, _ := block.(map[string]any)
			switch stringValue(bm["type"]) {
			case "thinking":
				thinkParts = append(thinkParts, stringValue(bm["thinking"]))
			case "redacted_thinking":
				thinkParts = append(thinkParts, stringValue(bm["data"]))
			case "text":
				textParts = append(textParts, stringValue(bm["text"]))
			}
		}
		if reasoning == "" && len(thinkParts) > 0 {
			reasoning = strings.Join(thinkParts, "\n")
		}
		visible = strings.Join(textParts, "")
	}

	if contentStr := stringValue(message["content"]); contentStr != "" {
		if visible == "" {
			visible = contentStr
		}
		if reasoning == "" {
			reasoning, visible = splitThinkTag(contentStr)
		}
	}

	return reasoning, visible
}

func splitThinkTag(content string) (reasoning, visible string) {
	for _, tag := range []string{"<think>", "<thinking>"} {
		after, found := strings.CutPrefix(content, tag)
		if !found {
			continue
		}
		closeTag := strings.Replace(tag, "<", "</", 1)
		reason, rest, foundClose := strings.Cut(after, closeTag)
		if foundClose {
			rest = strings.TrimSpace(rest)
			rest = strings.TrimPrefix(rest, "\n")
			return strings.TrimSpace(reason), rest
		}
		return strings.TrimSpace(after), ""
	}
	return "", content
}

func estimateReasoningTokens(text string) int {
	return len([]rune(text)) / 3
}

// ===========================================================================
// Input item conversion
// ===========================================================================

func convertInputItem(item any, tc *toolContext) (map[string]any, bool) {
	dict, ok := item.(map[string]any)
	if !ok {
		return nil, false
	}

	itemType := stringValue(dict["type"])

	if itemType == "tool_search_output" {
		if tools, _ := dict["tools"].([]any); len(tools) > 0 {
			for _, t := range tools {
				tc.addResponseTool(t)
			}
		}
		if stringValue(dict["execution"]) == "server" {
			return nil, false
		}
		return map[string]any{
			"role":    "system",
			"content": "Loaded tools: " + stringValue(dict["call_id"]),
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
			converted := make([]any, 0, len(contentList))
			var textBuilder strings.Builder
			allText := true
			for _, rawPart := range contentList {
				part, ok := rawPart.(map[string]any)
				if !ok {
					allText = false
					continue
				}
				cp, ok := convertContentPart(part)
				if !ok {
					allText = false
					continue
				}
				converted = append(converted, cp)
				if stringValue(cp["type"]) != "text" {
					allText = false
				} else {
					textBuilder.WriteString(stringValue(cp["text"]))
				}
			}
			if allText {
				content = textBuilder.String()
			} else {
				content = converted
			}
		}
		return map[string]any{"role": role, "content": content}, true
	}

	if itemType == "function_call" {
		callID := stringValue(dict["call_id"])
		if callID == "" {
			callID = stringValue(dict["id"])
		}
		return map[string]any{
			"role":    "assistant",
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
		}, true
	}

	if itemType == "function_call_output" {
		return map[string]any{
			"role":         "tool",
			"tool_call_id": stringValue(dict["call_id"]),
			"content":      stringValue(dict["output"]),
		}, true
	}

	return nil, false
}

func convertContentPart(part map[string]any) (map[string]any, bool) {
	switch stringValue(part["type"]) {
	case "input_text", "output_text":
		return map[string]any{"type": "text", "text": stringValue(part["text"])}, true
	case "input_image":
		url := extractImageURL(part)
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, true
	case "input_file":
		return nil, false
	default:
		return part, true
	}
}

func extractImageURL(part map[string]any) string {
	if url, ok := part["image_url"].(map[string]any); ok {
		return stringValue(url["url"])
	}
	return stringValue(part["image_url"])
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
						"description": customToolInputDesc,
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
	desc := stringValue(tool["description"])
	if desc == "" {
		desc = stringValue(tool["name"])
	}
	return customToolPreservedHeader + "\n" + desc
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
	suffix := strings.TrimPrefix(responseID, "resp-")
	suffix = strings.TrimPrefix(suffix, "resp_")
	itemID := "msg_" + suffix

	outputItem := map[string]any{
		"type":    "message",
		"id":      itemID,
		"status":  ternary(finishReason == "", "in_progress", "completed"),
		"role":    role,
		"content": []any{},
	}

	switch content := message["content"].(type) {
	case string:
		if content != "" {
			outputItem["content"] = []any{map[string]any{
				"type": "output_text", "text": content, "annotations": []any{},
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
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			pt := stringValue(part["type"])
			if pt == "thinking" || pt == "redacted_thinking" {
				continue
			}
			converted := convertOutputPartWithoutThinking(part)
			if converted != nil {
				textContent = append(textContent, converted)
			}
		}
		outputItem["content"] = textContent
	}

	return outputItem
}

func convertOutputPartWithoutThinking(part map[string]any) map[string]any {
	if stringValue(part["type"]) == "text" {
		return map[string]any{
			"type": "output_text", "text": stringValue(part["text"]), "annotations": []any{},
		}
	}
	return part
}

func buildToolOutputItems(message map[string]any, responseID string, tc *toolContext) []any {
	suffix := strings.TrimPrefix(responseID, "resp-")
	suffix = strings.TrimPrefix(suffix, "resp_")
	toolCalls, _ := message["tool_calls"].([]any)
	var items []any
	for _, raw := range toolCalls {
		tcMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		items = append(items, restoreToolCall(tcMap, suffix, tc))
	}
	return items
}

func restoreToolCall(toolCall map[string]any, suffix string, tc *toolContext) map[string]any {
	fn, _ := toolCall["function"].(map[string]any)
	chatName := stringValue(fn["name"])
	callID := stringValue(toolCall["id"])
	args := canonicalizeArguments(stringValue(fn["arguments"]))

	spec := tc.chatNameToSpec[chatName]
	if spec == nil {
		return map[string]any{
			"type":      "function_call",
			"id":        "fc_" + strings.TrimPrefix(callID, "call_"),
			"call_id":   callID,
			"status":    "completed",
			"name":      chatName,
			"arguments": args,
		}
	}

	switch spec.kind {
	case "namespace":
		return map[string]any{
			"type":      "function_call",
			"id":        "fc_" + strings.TrimPrefix(callID, "call_"),
			"call_id":   callID,
			"status":    "completed",
			"namespace": spec.namespace,
			"name":      spec.name,
			"arguments": args,
		}
	case "custom":
		input := extractCustomToolInput(args)
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      "ctc_" + strings.TrimPrefix(callID, "call_"),
			"call_id": callID,
			"status":  "completed",
			"name":    spec.name,
			"input":   input,
		}
	case "tool_search":
		return map[string]any{
			"type":      "tool_search_call",
			"id":        "tsc_" + strings.TrimPrefix(callID, "call_"),
			"call_id":   callID,
			"status":    "completed",
			"arguments": parseJSONObject(args),
			"execution": "client",
		}
	default:
		return map[string]any{
			"type":      "function_call",
			"id":        "fc_" + strings.TrimPrefix(callID, "call_"),
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
// StreamingConverter — chat SSE → responses SSE
// ===========================================================================

type StreamingConverter struct {
	initialized        bool
	outputItemAdded    bool
	contentPartAdded   bool
	textDone           bool
	outputItemDone     bool
	completed          bool
	reasoningDone      bool
	nextOutputIndex    int
	messageOutputIndex *int
	responseID         string
	messageID          string
	model              string
	created            int
	messageRole        string
	fullText           string
	reasoningText      string
	toolCalls          map[int]*streamToolCall
	sseBuffer          string
	usage              map[string]any
	rawReasoning       map[string]any
}

type streamToolCall struct {
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
	events = append(events, c.finishResponse("completed")...)
	return events
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
		c.reasoningText += rc
		events = append(events, c.ensureReasoningDelta()...)
	}

	// visible content delta
	if content := stringValue(delta["content"]); content != "" {
		if thinkR, visible := splitThinkTag(content); thinkR != "" {
			c.reasoningText += thinkR
			content = visible
			events = append(events, c.ensureReasoningDelta()...)
		}
		events = append(events, c.ensureContentPart()...)
		c.fullText += content
		events = append(events, sseEvent("response.output_text.delta", map[string]any{
			"item_id": c.messageID, "output_index": c.messageOutputIndexValue(),
			"content_index": 0, "delta": content,
		}))
	}

	// reasoning_details delta (MiniMax)
	if details, ok := delta["reasoning_details"].([]any); ok {
		for _, d := range details {
			if dm, ok := d.(map[string]any); ok {
				if text := stringValue(dm["text"]); text != "" {
					c.reasoningText += text
					events = append(events, c.ensureReasoningDelta()...)
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
			events = append(events, c.processToolCallDelta(tc)...)
		}
	}

	if finishReason != "" && !c.outputItemDone {
		status := "completed"
		if finishReason == "length" || finishReason == "content_filter" {
			status = "incomplete"
		}
		events = append(events, c.finishOutputItems()...)
		events = append(events, c.finishResponse(status)...)
	}

	return events
}

func (c *StreamingConverter) ensureReasoningDelta() []string {
	if c.reasoningDone || c.reasoningText == "" {
		return nil
	}
	c.reasoningDone = true
	outputIndex := c.allocateOutputIndex()
	itemID := "rs_" + strings.TrimPrefix(c.responseID, "resp_")
	if itemID == "rs_" {
		itemID = "rs_unknown"
	}
	return []string{
		sseEvent("response.output_item.added", map[string]any{
			"output_index": outputIndex,
			"item": map[string]any{
				"type":    "reasoning",
				"id":      itemID,
				"status":  "in_progress",
				"summary": []any{},
			},
		}),
		sseEvent("response.reasoning_text.delta", map[string]any{
			"item_id": itemID, "output_index": outputIndex,
			"content_index": 0, "delta": c.reasoningText,
		}),
	}
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
	if entry.ItemID == "" {
		entry.ItemID = c.toolItemID(entry.ID, index)
	}
	if entry.OutputIndex == nil {
		outputIndex := c.allocateOutputIndex()
		entry.OutputIndex = &outputIndex
	}

	var events []string
	if !entry.Added {
		entry.Added = true
		events = append(events, sseEvent("response.output_item.added", map[string]any{
			"output_index": *entry.OutputIndex,
			"item": map[string]any{
				"type": "function_call", "id": entry.ItemID, "call_id": entry.ID,
				"status": "in_progress", "name": entry.Name, "arguments": "",
			},
		}))
	}
	if arguments := stringValue(function["arguments"]); arguments != "" {
		entry.Arguments += arguments
		events = append(events, sseEvent("response.function_call_arguments.delta", map[string]any{
			"item_id": entry.ItemID, "output_index": *entry.OutputIndex,
			"delta": arguments,
		}))
	}
	return events
}

func (c *StreamingConverter) finishOutputItems() []string {
	var events []string
	if !c.outputItemDone {
		c.outputItemDone = true
		events = append(events, c.finishReasoningEvents()...)
		events = append(events, c.finishTextEvents()...)
		if c.outputItemAdded {
			var content []any
			if c.contentPartAdded {
				content = append(content, map[string]any{
					"type": "output_text", "text": c.fullText, "annotations": []any{},
				})
			}
			events = append(events, sseEvent("response.output_item.done", map[string]any{
				"output_index": c.messageOutputIndexValue(),
				"item": map[string]any{
					"type": "message", "id": c.messageID, "status": "completed",
					"role": c.messageRole, "content": content,
				},
			}))
		}
	}
	events = append(events, c.finishToolEvents()...)
	return events
}

func (c *StreamingConverter) finishReasoningEvents() []string {
	if !c.reasoningDone || c.reasoningText == "" {
		return nil
	}
	itemID := "rs_" + strings.TrimPrefix(c.responseID, "resp_")
	return []string{
		sseEvent("response.reasoning_text.done", map[string]any{
			"item_id": itemID, "output_index": 0, "content_index": 0,
			"text": c.reasoningText,
		}),
		sseEvent("response.output_item.done", map[string]any{
			"output_index": 0,
			"item": map[string]any{
				"type": "reasoning", "id": itemID, "status": "completed",
				"summary": []any{
					map[string]any{"type": "summary_text", "text": c.reasoningText},
				},
			},
		}),
	}
}

func (c *StreamingConverter) finishTextEvents() []string {
	if !c.contentPartAdded || c.textDone {
		return nil
	}
	c.textDone = true
	return []string{
		sseEvent("response.output_text.done", map[string]any{
			"item_id": c.messageID, "output_index": c.messageOutputIndexValue(),
			"content_index": 0, "text": c.fullText,
		}),
		sseEvent("response.content_part.done", map[string]any{
			"item_id": c.messageID, "output_index": c.messageOutputIndexValue(),
			"content_index": 0,
			"part": map[string]any{
				"type": "output_text", "text": c.fullText, "annotations": []any{},
			},
		}),
	}
}

func (c *StreamingConverter) finishToolEvents() []string {
	var events []string
	indexes := sortedToolIndexes(c.toolCalls)
	for _, index := range indexes {
		tc := c.toolCalls[index]
		if !tc.Added || tc.OutputIndex == nil {
			continue
		}
		events = append(events,
			sseEvent("response.function_call_arguments.done", map[string]any{
				"item_id": tc.ItemID, "output_index": *tc.OutputIndex,
				"arguments": tc.Arguments,
			}),
			sseEvent("response.output_item.done", map[string]any{
				"output_index": *tc.OutputIndex,
				"item": map[string]any{
					"type": "function_call", "id": tc.ItemID, "call_id": tc.ID,
					"status": "completed", "name": tc.Name, "arguments": tc.Arguments,
				},
			}),
		)
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
	var output []any
	if c.reasoningText != "" {
		itemID := "rs_" + strings.TrimPrefix(c.responseID, "resp_")
		output = append(output, map[string]any{
			"type": "reasoning", "id": itemID, "status": "completed",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": c.reasoningText},
			},
		})
	}
	if c.contentPartAdded {
		output = append(output, map[string]any{
			"type": "message", "id": c.messageID, "status": "completed",
			"role":    c.messageRole,
			"content": []any{map[string]any{"type": "output_text", "text": c.fullText, "annotations": []any{}}},
		})
	}
	for _, index := range sortedToolIndexes(c.toolCalls) {
		tc := c.toolCalls[index]
		if tc.Added {
			output = append(output, map[string]any{
				"type": "function_call", "id": tc.ItemID, "call_id": tc.ID,
				"status": "completed", "name": tc.Name, "arguments": tc.Arguments,
			})
		}
	}
	return output
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

func (c *StreamingConverter) toolItemID(callID string, index int) string {
	if callID != "" {
		return "fc_" + strings.TrimPrefix(callID, "call_")
	}
	suffix := strings.TrimPrefix(c.responseID, "resp-")
	suffix = strings.TrimPrefix(suffix, "resp_")
	if suffix == "" {
		suffix = "unknown"
	}
	return fmt.Sprintf("fc_%s_%d", suffix, index)
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
