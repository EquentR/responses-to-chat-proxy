package proxy

import (
	"encoding/json"
	"sort"
	"strings"
)

// -----------------------------------------------------------------------------
// Responses -> Messages
// -----------------------------------------------------------------------------

func ConvertResponsesToMessages(data map[string]any, cfg Config) map[string]any {
	model := stringValue(data["model"])
	if cfg.ModelOverride != "" {
		model = cfg.ModelOverride
	}

	result := map[string]any{
		"model": model,
	}

	if system := stringValue(data["instructions"]); system != "" {
		result["system"] = system
	}
	if thinking := convertResponsesThinking(data["reasoning"], cfg); thinking != nil {
		result["thinking"] = thinking
	}
	if maxOutput := data["max_output_tokens"]; maxOutput != nil {
		result["max_tokens"] = maxOutput
	} else if maxTokens := data["max_tokens"]; maxTokens != nil {
		result["max_tokens"] = maxTokens
	}
	if tools := convertResponsesToolsToMessages(data["tools"]); len(tools) > 0 {
		result["tools"] = tools
	}
	result["messages"] = convertResponsesInputToMessages(data["input"])
	if stream, ok := data["stream"].(bool); ok {
		result["stream"] = stream
	}
	return result
}

func convertResponsesThinking(raw any, cfg Config) map[string]any {
	reasoning, _ := raw.(map[string]any)
	if len(reasoning) == 0 {
		return nil
	}
	if cfg.ReasoningMode != ReasoningThinking && cfg.ReasoningMode != ReasoningThinkingOnly {
		return nil
	}
	effort := strings.ToLower(stringValue(reasoning["effort"]))
	if effort == "" {
		return nil
	}
	if effort == "off" || effort == "none" || effort == "disabled" {
		return map[string]any{"type": "disabled"}
	}
	thinking := map[string]any{"type": "enabled"}
	if budget := reasoning["budget_tokens"]; budget != nil {
		thinking["budget_tokens"] = budget
	}
	return thinking
}

func convertResponsesInputToMessages(input any) []any {
	switch typed := input.(type) {
	case string:
		return []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": typed},
			},
		}}
	case []any:
		var messages []any
		for _, raw := range typed {
			msg, ok := convertResponsesInputItemToMessage(raw)
			if ok {
				messages = append(messages, msg)
			}
		}
		return messages
	default:
		return []any{}
	}
}

func convertResponsesInputItemToMessage(raw any) (map[string]any, bool) {
	item, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}

	switch stringValue(item["type"]) {
	case "input_text":
		text := stringValue(item["text"])
		if text == "" {
			text = stringValue(item["content"])
		}
		return map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": text},
			},
		}, text != ""
	case "input_image":
		image := buildAnthropicImageSource(item)
		if len(image) == 0 {
			return nil, false
		}
		return map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "image", "source": image},
			},
		}, true
	case "input_file":
		file := buildAnthropicDocumentSource(item)
		if len(file) == 0 {
			return nil, false
		}
		return map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "document", "source": file},
			},
		}, true
	case "input_audio":
		audio := buildAnthropicAudioSource(item)
		if len(audio) == 0 {
			return nil, false
		}
		return map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "audio", "source": audio},
			},
		}, true
	case "message":
		return normalizeAnthropicMessage(item), true
	case "reasoning":
		text := extractMessagesReasoningText(item)
		if text == "" {
			return nil, false
		}
		return map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": text},
			},
		}, true
	case "function_call":
		return convertFunctionCallToAssistantMessage(item), true
	case "function_call_output":
		return convertFunctionCallOutputToUserMessage(item), true
	case "custom_tool_call":
		return convertCustomToolCallToAssistantMessage(item), true
	case "tool_search_call":
		return convertToolSearchCallToAssistantMessage(item), true
	default:
		if role := stringValue(item["role"]); role != "" {
			return normalizeAnthropicMessage(item), true
		}
	}

	return nil, false
}

func normalizeAnthropicMessage(item map[string]any) map[string]any {
	role := stringValue(item["role"])
	if role == "" {
		role = "user"
	}
	if role == "developer" {
		role = "system"
	}

	content := item["content"]
	switch typed := content.(type) {
	case string:
		content = []any{map[string]any{"type": "text", "text": typed}}
	case []any:
		content = normalizeMessagesContent(typed)
	}

	return map[string]any{
		"role":    role,
		"content": content,
	}
}

func normalizeMessagesContent(parts []any) []any {
	normalized := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(part["type"]) {
		case "text", "output_text", "input_text":
			normalized = append(normalized, map[string]any{
				"type": "text",
				"text": stringValue(part["text"]),
			})
		case "image", "input_image":
			source := cloneMap(sourceMap(part["source"]))
			if len(source) == 0 {
				source = buildAnthropicImageSource(part)
			}
			if len(source) > 0 {
				normalized = append(normalized, map[string]any{
					"type":   "image",
					"source": source,
				})
			}
		case "document", "input_file":
			source := cloneMap(sourceMap(part["source"]))
			if len(source) == 0 {
				source = buildAnthropicDocumentSource(part)
			}
			if len(source) > 0 {
				normalized = append(normalized, map[string]any{
					"type":   "document",
					"source": source,
				})
			}
		case "audio", "input_audio":
			source := cloneMap(sourceMap(part["source"]))
			if len(source) == 0 {
				source = buildAnthropicAudioSource(part)
			}
			if len(source) > 0 {
				normalized = append(normalized, map[string]any{
					"type":   "audio",
					"source": source,
				})
			}
		case "tool_use":
			normalized = append(normalized, map[string]any{
				"type":  "tool_use",
				"id":    stringValue(part["id"]),
				"name":  stringValue(part["name"]),
				"input": part["input"],
			})
		case "tool_result":
			normalized = append(normalized, map[string]any{
				"type":        "tool_result",
				"tool_use_id": stringValue(part["tool_use_id"]),
				"content":     part["content"],
			})
		case "thinking":
			normalized = append(normalized, map[string]any{
				"type":     "thinking",
				"thinking": stringValue(part["thinking"]),
			})
		case "redacted_thinking":
			normalized = append(normalized, map[string]any{
				"type": "redacted_thinking",
				"data": part["data"],
			})
		default:
			normalized = append(normalized, cloneMap(part))
		}
	}
	return normalized
}

func extractMessagesReasoningText(item map[string]any) string {
	var parts []string
	for _, key := range []string{"summary", "content", "details", "reasoning_details", "thinking", "text", "data", "reasoning"} {
		collectMessagesReasoning(&parts, item[key])
	}
	return strings.Join(parts, "\n")
}

func collectMessagesReasoning(parts *[]string, value any) {
	switch typed := value.(type) {
	case nil:
		return
	case string:
		reasoning, _, found := splitThinkText(typed)
		if found {
			if strings.TrimSpace(reasoning) != "" {
				*parts = append(*parts, reasoning)
			}
			return
		}
		if strings.TrimSpace(typed) != "" {
			*parts = append(*parts, typed)
		}
	case []string:
		for _, item := range typed {
			collectMessagesReasoning(parts, item)
		}
	case []any:
		for _, item := range typed {
			collectMessagesReasoning(parts, item)
		}
	case map[string]any:
		switch stringValue(typed["type"]) {
		case "thinking":
			collectMessagesReasoning(parts, typed["thinking"])
			collectMessagesReasoning(parts, typed["text"])
			collectMessagesReasoning(parts, typed["content"])
			collectMessagesReasoning(parts, typed["summary"])
			collectMessagesReasoning(parts, typed["reasoning"])
		case "redacted_thinking":
			collectMessagesReasoning(parts, typed["data"])
			collectMessagesReasoning(parts, typed["text"])
			collectMessagesReasoning(parts, typed["content"])
		case "summary_text", "reasoning_text":
			collectMessagesReasoning(parts, typed["text"])
		default:
			for _, key := range []string{"summary", "content", "details", "reasoning_details", "thinking", "text", "data", "reasoning"} {
				collectMessagesReasoning(parts, typed[key])
			}
		}
	}
}

func convertFunctionCallToAssistantMessage(item map[string]any) map[string]any {
	args := stringValue(item["arguments"])
	input := parseJSONObject(args)
	if input == nil {
		input = map[string]any{}
	}
	return map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    firstNonEmptyString(stringValue(item["call_id"]), stringValue(item["id"])),
				"name":  stringValue(item["name"]),
				"input": input,
			},
		},
	}
}

func convertFunctionCallOutputToUserMessage(item map[string]any) map[string]any {
	return map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": firstNonEmptyString(stringValue(item["call_id"]), stringValue(item["tool_use_id"])),
				"content":     item["output"],
			},
		},
	}
}

func convertCustomToolCallToAssistantMessage(item map[string]any) map[string]any {
	return map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    firstNonEmptyString(stringValue(item["call_id"]), stringValue(item["id"])),
				"name":  stringValue(item["name"]),
				"input": map[string]any{customToolInputField: stringValue(item["input"])},
			},
		},
	}
}

func convertToolSearchCallToAssistantMessage(item map[string]any) map[string]any {
	return map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    firstNonEmptyString(stringValue(item["call_id"]), stringValue(item["id"])),
				"name":  toolSearchProxyName,
				"input": item["arguments"],
			},
		},
	}
}

func convertResponsesToolsToMessages(raw any) []any {
	tools, _ := raw.([]any)
	if len(tools) == 0 {
		return nil
	}

	var result []any
	for _, tool := range tools {
		toolMap, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(toolMap["type"]) {
		case "function":
			result = append(result, map[string]any{
				"type":         "function",
				"name":         responsesToolName(toolMap),
				"description":  valueOrDefaultString(toolMap["description"], ""),
				"input_schema": valueOrDefault(toolMap["input_schema"], toolMap["parameters"]),
			})
		case "custom":
			result = append(result, map[string]any{
				"type":        "custom",
				"name":        stringValue(toolMap["name"]),
				"description": valueOrDefaultString(toolMap["description"], ""),
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						customToolInputField: map[string]any{"type": "string"},
					},
					"required": []any{customToolInputField},
				},
			})
		case "tool_search":
			result = append(result, map[string]any{
				"type":        "function",
				"name":        toolSearchProxyName,
				"description": "Search and load Codex tools, plugins, connectors, and MCP namespaces for the current task.",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
						"limit": map[string]any{"type": "integer"},
					},
					"required": []any{"query"},
				},
			})
		case "namespace":
			children, _ := toolMap["tools"].([]any)
			if children == nil {
				children, _ = toolMap["children"].([]any)
			}
			for _, child := range children {
				childMap, _ := child.(map[string]any)
				if stringValue(childMap["type"]) != "function" {
					continue
				}
				chatName := flattenNamespaceName(stringValue(toolMap["name"]), responsesToolName(childMap))
				result = append(result, map[string]any{
					"type":         "function",
					"name":         chatName,
					"description":  valueOrDefaultString(childMap["description"], ""),
					"input_schema": valueOrDefault(childMap["input_schema"], childMap["parameters"]),
				})
			}
		}
	}
	return result
}

func sourceMap(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}

func buildAnthropicImageSource(part map[string]any) map[string]any {
	if source := sourceMap(part["source"]); source != nil {
		if typ := stringValue(source["type"]); typ == "base64" || typ == "url" || typ == "file" {
			return cloneMap(source)
		}
	}

	if fileID := stringValue(part["file_id"]); fileID != "" {
		return map[string]any{"type": "file", "file_id": fileID}
	}
	if fileID := stringValue(part["image_file_id"]); fileID != "" {
		return map[string]any{"type": "file", "file_id": fileID}
	}
	if url := imageURLString(part); url != "" {
		return map[string]any{"type": "url", "url": url}
	}
	if data := stringValue(part["file_data"]); data != "" {
		return map[string]any{
			"type":       "base64",
			"media_type": valueOrDefaultString(part["mime_type"], valueOrDefaultString(part["media_type"], "image/png")),
			"data":       data,
		}
	}
	return nil
}

func buildAnthropicDocumentSource(part map[string]any) map[string]any {
	if source := sourceMap(part["source"]); source != nil {
		if typ := stringValue(source["type"]); typ == "base64" || typ == "url" || typ == "file" {
			return cloneMap(source)
		}
	}

	if fileID := stringValue(part["file_id"]); fileID != "" {
		return map[string]any{"type": "file", "file_id": fileID}
	}
	if url := stringValue(part["file_url"]); url != "" {
		return map[string]any{"type": "url", "url": url}
	}
	if url := stringValue(part["url"]); url != "" {
		return map[string]any{"type": "url", "url": url}
	}
	if data := stringValue(part["file_data"]); data != "" {
		return map[string]any{
			"type":       "base64",
			"media_type": valueOrDefaultString(part["mime_type"], valueOrDefaultString(part["media_type"], "application/octet-stream")),
			"data":       data,
		}
	}
	return nil
}

func buildAnthropicAudioSource(part map[string]any) map[string]any {
	if source := sourceMap(part["source"]); source != nil {
		if typ := stringValue(source["type"]); typ == "base64" || typ == "url" || typ == "file" {
			return cloneMap(source)
		}
	}

	audio := extractAudioPart(part)
	if len(audio) == 0 {
		return nil
	}

	if data := stringValue(audio["data"]); data != "" {
		mediaType := valueOrDefaultString(audio["media_type"], valueOrDefaultString(audio["mime_type"], anthropicAudioMediaType(stringValue(audio["format"]))))
		return map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		}
	}

	return nil
}

func anthropicAudioMediaType(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "mp3", "mpeg":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg", "oga":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "webm":
		return "audio/webm"
	case "":
		return "application/octet-stream"
	default:
		if strings.Contains(format, "/") {
			return format
		}
		return "audio/" + format
	}
}

func imageURLString(part map[string]any) string {
	if url := stringValue(part["url"]); url != "" {
		return url
	}
	if imageURL, ok := part["image_url"].(map[string]any); ok {
		if url := stringValue(imageURL["url"]); url != "" {
			return url
		}
	}
	return stringValue(part["image_url"])
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// Messages -> Responses
// -----------------------------------------------------------------------------

func ConvertMessagesToResponses(data map[string]any, originalReq map[string]any) map[string]any {
	payload := extractMessagesPayload(data)
	responseID := convertMessagesID(stringValue(payload["id"]))
	if responseID == "" {
		responseID = "resp_unknown"
	}

	model := stringValue(payload["model"])
	if model == "" {
		model = stringValue(originalReq["model"])
	}

	output := convertMessagesOutput(payload)
	status, incomplete := mapMessagesStopReason(stringValue(payload["stop_reason"]), output)

	response := map[string]any{
		"id":                  responseID,
		"object":              "response",
		"created_at":          intValue(payload["created_at"]),
		"status":              status,
		"error":               nil,
		"incomplete_details":  incomplete,
		"instructions":        originalReq["instructions"],
		"max_output_tokens":   originalReq["max_output_tokens"],
		"model":               model,
		"output":              finalizeOutputItems(output, status),
		"parallel_tool_calls": valueOrDefault(originalReq["parallel_tool_calls"], true),
		"temperature":         valueOrDefault(originalReq["temperature"], 1.0),
		"tool_choice":         valueOrDefault(originalReq["tool_choice"], "auto"),
		"tools":               valueOrDefault(originalReq["tools"], []any{}),
		"top_p":               valueOrDefault(originalReq["top_p"], 1.0),
		"truncation":          "disabled",
		"usage":               convertMessagesUsage(payload["usage"]),
		"user":                originalReq["user"],
		"metadata":            valueOrDefault(originalReq["metadata"], map[string]any{}),
	}
	return response
}

func extractMessagesPayload(data map[string]any) map[string]any {
	if msg, ok := data["message"].(map[string]any); ok {
		return msg
	}
	return data
}

func convertMessagesID(id string) string {
	switch {
	case strings.HasPrefix(id, "msg-"):
		return "resp-" + strings.TrimPrefix(id, "msg-")
	case strings.HasPrefix(id, "msg_"):
		return "resp-" + strings.TrimPrefix(id, "msg_")
	case strings.HasPrefix(id, "message-"):
		return "resp-" + strings.TrimPrefix(id, "message-")
	case strings.HasPrefix(id, "message_"):
		return "resp-" + strings.TrimPrefix(id, "message_")
	case id != "":
		return "resp-" + id
	default:
		return "resp_unknown"
	}
}

func convertMessagesOutput(payload map[string]any) []any {
	content, _ := payload["content"].([]any)
	if len(content) == 0 {
		if blocks, ok := payload["blocks"].([]any); ok {
			content = blocks
		}
	}

	role := valueOrDefaultString(payload["role"], "assistant")
	responseID := convertMessagesID(stringValue(payload["id"]))
	suffix := responseSuffix(responseID)

	var output []any
	var messageText strings.Builder
	var reasoningParts []string
	var toolCalls []map[string]any

	for index, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}

		switch stringValue(part["type"]) {
		case "text":
			if text := stringValue(part["text"]); text != "" {
				messageText.WriteString(text)
			}
		case "thinking":
			if text := firstNonEmptyString(stringValue(part["thinking"]), stringValue(part["text"])); text != "" {
				reasoningParts = append(reasoningParts, text)
			}
		case "redacted_thinking":
			if data := stringValue(part["data"]); data != "" {
				reasoningParts = append(reasoningParts, data)
			}
		case "tool_use":
			toolCalls = append(toolCalls, convertAnthropicToolUse(part, suffix, index))
		case "tool_result":
			output = append(output, map[string]any{
				"type":    "function_call_output",
				"call_id": firstNonEmptyString(stringValue(part["tool_use_id"]), stringValue(part["id"])),
				"output":  part["content"],
				"status":  "completed",
			})
		default:
			// Unknown Anthropic blocks are ignored on purpose.
		}
	}

	if len(reasoningParts) > 0 {
		output = append(output, map[string]any{
			"type":   "reasoning",
			"id":     "rs_" + suffix,
			"status": "completed",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": strings.Join(reasoningParts, "\n")},
			},
		})
	}

	if text := strings.TrimSpace(messageText.String()); text != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + suffix,
			"status": "completed",
			"role":   role,
			"content": []any{
				map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			},
		})
	}

	output = append(output, mapSliceToAny(toolCalls)...)

	return output
}

func convertAnthropicToolUse(part map[string]any, suffix string, index int) map[string]any {
	id := firstNonEmptyString(stringValue(part["id"]), toolCallFallbackID("fc_", suffix, index))
	input := part["input"]
	if input == nil {
		input = map[string]any{}
	}
	if _, ok := input.(map[string]any); !ok {
		input = map[string]any{"input": input}
	}
	return map[string]any{
		"type":      "function_call",
		"id":        id,
		"call_id":   id,
		"status":    "completed",
		"name":      stringValue(part["name"]),
		"arguments": mustJSONString(input),
	}
}

func convertMessagesUsage(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	if len(usage) == 0 {
		return map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
			"input_tokens_details": map[string]any{
				"cached_tokens": 0,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": 0,
			},
		}
	}

	cached := intValue(usage["cache_read_input_tokens"])
	if cached == 0 {
		cached = intValue(usage["cache_creation_input_tokens"])
	}
	reasoningTokens := intValue(usage["reasoning_tokens"])
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		if reasoningTokens == 0 {
			reasoningTokens = intValue(details["reasoning_tokens"])
		}
	}

	return map[string]any{
		"input_tokens":  intValue(usage["input_tokens"]),
		"output_tokens": intValue(usage["output_tokens"]),
		"total_tokens":  intValue(usage["total_tokens"]),
		"input_tokens_details": map[string]any{
			"cached_tokens": cached,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoningTokens,
		},
	}
}

func mapMessagesStopReason(stopReason string, output []any) (string, map[string]any) {
	switch strings.ToLower(stopReason) {
	case "max_tokens":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	case "tool_use", "end_turn", "stop_sequence":
		return "completed", nil
	case "":
		if hasSubstantiveResponseOutput(output) {
			return "completed", nil
		}
		return "failed", nil
	default:
		return "completed", nil
	}
}

// -----------------------------------------------------------------------------
// Messages streaming -> Responses streaming
// -----------------------------------------------------------------------------

type MessagesStreamingConverter struct {
	originalReq map[string]any
	buffer      string
	initialized bool
	completed   bool
	responseID  string
	model       string
	createdAt   int
	role        string
	stopReason  string
	text        strings.Builder
	reasoning   strings.Builder
	toolCalls   map[int]*messagesStreamToolCall
	usage       map[string]any
}

type messagesStreamToolCall struct {
	id        string
	name      string
	args      strings.Builder
	added     bool
	outputIdx int
}

func NewMessagesStreamingConverter(originalReq map[string]any) *MessagesStreamingConverter {
	return &MessagesStreamingConverter{
		originalReq: originalReq,
		role:        "assistant",
		toolCalls:   map[int]*messagesStreamToolCall{},
		usage:       map[string]any{},
	}
}

func (c *MessagesStreamingConverter) Feed(chunk []byte) []string {
	c.buffer += strings.ReplaceAll(strings.ReplaceAll(string(chunk), "\r\n", "\n"), "\r", "\n")
	var events []string

	for {
		index := strings.Index(c.buffer, "\n\n")
		if index < 0 {
			break
		}
		rawEvent := c.buffer[:index]
		c.buffer = c.buffer[index+2:]

		eventType, payload := parseMessagesSSEEvent(rawEvent)
		if eventType == "" && len(payload) == 0 {
			continue
		}
		events = append(events, c.processEvent(eventType, payload)...)
	}

	return events
}

func (c *MessagesStreamingConverter) Finish() []string {
	if c.completed {
		return nil
	}
	events := c.finalEvents()
	c.completed = true
	return events
}

func (c *MessagesStreamingConverter) processEvent(eventType string, payload map[string]any) []string {
	if len(payload) == 0 && eventType == "" {
		return nil
	}

	switch eventType {
	case "message_start":
		return c.handleMessageStart(payload)
	case "content_block_start":
		return c.handleContentBlockStart(payload)
	case "content_block_delta":
		return c.handleContentBlockDelta(payload)
	case "message_delta":
		return c.handleMessageDelta(payload)
	case "message_stop":
		return c.finalEvents()
	default:
		if eventType == "" {
			return c.handleMessageStart(payload)
		}
		return nil
	}
}

func (c *MessagesStreamingConverter) handleMessageStart(payload map[string]any) []string {
	if c.initialized {
		return nil
	}
	c.initialized = true

	message := payload
	if msg, ok := payload["message"].(map[string]any); ok {
		message = msg
	}

	c.responseID = convertMessagesID(stringValue(message["id"]))
	if c.responseID == "" {
		c.responseID = "resp_unknown"
	}
	c.model = valueOrDefaultString(message["model"], stringValue(c.originalReq["model"]))
	c.createdAt = intValue(message["created_at"])
	c.role = valueOrDefaultString(message["role"], "assistant")

	return []string{sseEvent("response.created", map[string]any{
		"response": buildResponseStub(c.responseID, c.createdAt, "in_progress", c.model),
	})}
}

func (c *MessagesStreamingConverter) handleContentBlockStart(payload map[string]any) []string {
	index := intValue(payload["index"])
	block, _ := payload["content_block"].(map[string]any)
	if block == nil {
		return nil
	}

	switch stringValue(block["type"]) {
	case "thinking", "redacted_thinking":
		c.collectReasoningBlock(block)
	case "tool_use":
		entry := c.ensureToolCall(index)
		entry.id = firstNonEmptyString(stringValue(block["id"]), entry.id)
		entry.name = firstNonEmptyString(stringValue(block["name"]), entry.name)
		if !entry.added {
			entry.added = true
			return []string{sseEvent("response.output_item.added", map[string]any{
				"output_index": entry.outputIdx,
				"item": map[string]any{
					"type":      "function_call",
					"id":        firstNonEmptyString(entry.id, toolCallFallbackID("fc_", responseSuffix(c.responseID), index)),
					"call_id":   firstNonEmptyString(entry.id, toolCallFallbackID("fc_", responseSuffix(c.responseID), index)),
					"status":    "in_progress",
					"name":      entry.name,
					"arguments": "{}",
				},
			})}
		}
	}
	return nil
}

func (c *MessagesStreamingConverter) handleContentBlockDelta(payload map[string]any) []string {
	index := intValue(payload["index"])
	delta, _ := payload["delta"].(map[string]any)
	if delta == nil {
		delta = payload
	}

	switch stringValue(delta["type"]) {
	case "text_delta":
		text := stringValue(delta["text"])
		if text == "" {
			return nil
		}
		c.text.WriteString(text)
		return []string{sseEvent("response.output_text.delta", map[string]any{
			"item_id":       c.messageItemID(),
			"output_index":  c.messageOutputIndexValue(),
			"content_index": 0,
			"delta":         text,
		})}
	case "thinking_delta":
		text := stringValue(delta["thinking"])
		if text == "" {
			text = stringValue(delta["text"])
		}
		if text == "" {
			return nil
		}
		c.appendReasoning(text)
		return []string{sseEvent("response.reasoning_text.delta", map[string]any{
			"item_id":       c.reasoningItemID(),
			"output_index":  c.reasoningOutputIndexValue(),
			"content_index": 0,
			"delta":         text,
		})}
	case "input_json_delta":
		entry := c.ensureToolCall(index)
		if fragment := stringValue(delta["partial_json"]); fragment != "" {
			entry.args.WriteString(fragment)
			return []string{sseEvent("response.function_call_arguments.delta", map[string]any{
				"item_id":      firstNonEmptyString(entry.id, c.toolCallItemID(index)),
				"output_index": entry.outputIdx,
				"delta":        fragment,
			})}
		}
	}

	return nil
}

func (c *MessagesStreamingConverter) handleMessageDelta(payload map[string]any) []string {
	if usage, ok := payload["usage"].(map[string]any); ok {
		c.usage = usage
	}
	if delta, ok := payload["delta"].(map[string]any); ok {
		if stopReason := stringValue(delta["stop_reason"]); stopReason != "" {
			c.stopReason = stopReason
		}
	}
	if stopReason := stringValue(payload["stop_reason"]); stopReason != "" {
		c.stopReason = stopReason
	}
	return nil
}

func (c *MessagesStreamingConverter) finalEvents() []string {
	if c.completed {
		return nil
	}
	output := c.buildOutput()
	status, incomplete := mapMessagesStopReason(c.stopReason, output)
	events := append(c.ensureInitializationEvents(), sseEvent("response.completed", map[string]any{
		"response": map[string]any{
			"id":                  c.responseID,
			"object":              "response",
			"created_at":          c.createdAt,
			"status":              status,
			"error":               nil,
			"incomplete_details":  incomplete,
			"instructions":        c.originalReq["instructions"],
			"max_output_tokens":   c.originalReq["max_output_tokens"],
			"model":               c.model,
			"output":              finalizeOutputItems(output, status),
			"parallel_tool_calls": valueOrDefault(c.originalReq["parallel_tool_calls"], true),
			"temperature":         valueOrDefault(c.originalReq["temperature"], 1.0),
			"tool_choice":         valueOrDefault(c.originalReq["tool_choice"], "auto"),
			"tools":               valueOrDefault(c.originalReq["tools"], []any{}),
			"top_p":               valueOrDefault(c.originalReq["top_p"], 1.0),
			"truncation":          "disabled",
			"usage":               convertMessagesUsage(c.usage),
			"user":                c.originalReq["user"],
			"metadata":            valueOrDefault(c.originalReq["metadata"], map[string]any{}),
		},
	}))
	c.completed = true
	return events
}

func (c *MessagesStreamingConverter) ensureInitializationEvents() []string {
	if c.initialized {
		return nil
	}
	c.initialized = true
	c.responseID = "resp_unknown"
	c.model = stringValue(c.originalReq["model"])
	return []string{sseEvent("response.created", map[string]any{
		"response": buildResponseStub(c.responseID, 0, "in_progress", c.model),
	})}
}

func (c *MessagesStreamingConverter) buildOutput() []any {
	var output []any
	if reasoning := strings.TrimSpace(c.reasoning.String()); reasoning != "" {
		output = append(output, map[string]any{
			"type":   "reasoning",
			"id":     c.reasoningItemID(),
			"status": "completed",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": reasoning},
			},
		})
	}
	if text := strings.TrimSpace(c.text.String()); text != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     c.messageItemID(),
			"status": "completed",
			"role":   c.role,
			"content": []any{
				map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			},
		})
	}

	indexes := make([]int, 0, len(c.toolCalls))
	for index := range c.toolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		entry := c.toolCalls[index]
		args := strings.TrimSpace(entry.args.String())
		if args == "" {
			args = "{}"
		}
		item := map[string]any{
			"type":      "function_call",
			"id":        firstNonEmptyString(entry.id, c.toolCallItemID(index)),
			"call_id":   firstNonEmptyString(entry.id, c.toolCallItemID(index)),
			"status":    "completed",
			"name":      entry.name,
			"arguments": args,
		}
		output = append(output, item)
	}
	return output
}

func (c *MessagesStreamingConverter) collectReasoningBlock(block map[string]any) {
	var parts []string
	switch stringValue(block["type"]) {
	case "thinking":
		collectMessagesReasoning(&parts, firstNonEmptyString(stringValue(block["thinking"]), stringValue(block["text"])))
		collectMessagesReasoning(&parts, block["content"])
		collectMessagesReasoning(&parts, block["summary"])
	case "redacted_thinking":
		collectMessagesReasoning(&parts, block["data"])
		collectMessagesReasoning(&parts, block["text"])
		collectMessagesReasoning(&parts, block["content"])
	}

	for _, part := range parts {
		c.appendReasoning(part)
	}
}

func (c *MessagesStreamingConverter) appendReasoning(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if c.reasoning.Len() > 0 {
		c.reasoning.WriteString("\n")
	}
	c.reasoning.WriteString(text)
}

func (c *MessagesStreamingConverter) ensureToolCall(index int) *messagesStreamToolCall {
	entry, ok := c.toolCalls[index]
	if !ok {
		entry = &messagesStreamToolCall{outputIdx: len(c.toolCalls)}
		c.toolCalls[index] = entry
	}
	if entry.outputIdx == 0 && len(c.toolCalls) > 1 {
		entry.outputIdx = len(c.toolCalls) - 1
	}
	return entry
}

func (c *MessagesStreamingConverter) messageItemID() string {
	return "msg_" + responseSuffix(c.responseID)
}

func (c *MessagesStreamingConverter) reasoningItemID() string {
	return "rs_" + responseSuffix(c.responseID)
}

func (c *MessagesStreamingConverter) messageOutputIndexValue() int {
	return 0
}

func (c *MessagesStreamingConverter) reasoningOutputIndexValue() int {
	return 1
}

func (c *MessagesStreamingConverter) toolCallItemID(index int) string {
	return toolCallFallbackID("fc_", responseSuffix(c.responseID), index)
}

func parseMessagesSSEEvent(rawEvent string) (string, map[string]any) {
	trimmed := strings.TrimSpace(rawEvent)
	if trimmed == "" {
		return "", nil
	}

	eventType := ""
	var dataLines []string
	for _, line := range strings.Split(rawEvent, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}

	payload := map[string]any{}
	if len(dataLines) > 0 {
		body := strings.TrimSpace(strings.Join(dataLines, "\n"))
		if body != "" {
			_ = json.Unmarshal([]byte(body), &payload)
		}
	}

	if eventType == "" {
		eventType = stringValue(payload["type"])
	}
	if eventType == "" {
		return "", payload
	}
	return eventType, payload
}

func mustJSONString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
