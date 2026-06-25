package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

func ConvertRequest(data map[string]any, cfg Config) map[string]any {
	model := stringValue(data["model"])
	if cfg.ModelOverride != "" {
		model = cfg.ModelOverride
	}

	chatData := map[string]any{
		"model": model,
	}

	var messages []any
	if instructions := stringValue(data["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch input := data["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": input})
	case []any:
		for _, item := range input {
			message, ok := convertInputItem(item)
			if ok {
				messages = append(messages, message)
			}
		}
	}

	chatData["messages"] = messages

	for _, key := range []string{
		"stream",
		"temperature",
		"top_p",
		"presence_penalty",
		"frequency_penalty",
		"stop",
		"seed",
		"tool_choice",
		"parallel_tool_calls",
		"response_format",
		"n",
		"logit_bias",
		"user",
	} {
		if value, ok := data[key]; ok {
			chatData[key] = value
		}
	}

	if value, ok := data["max_output_tokens"]; ok {
		chatData["max_tokens"] = value
	}

	if tools, ok := data["tools"].([]any); ok {
		convertedTools, hasWebSearch := convertTools(tools)
		if len(convertedTools) > 0 {
			chatData["tools"] = convertedTools
		}
		if hasWebSearch {
			chatData["webSearchEnabled"] = true
		}
	}

	if reasoning, ok := data["reasoning"].(map[string]any); ok {
		switch effort := stringValue(reasoning["effort"]); effort {
		case "low", "medium", "high":
			chatData["reasoning_effort"] = effort
		case "xhigh":
			chatData["reasoning_effort"] = "high"
		}
	}

	return chatData
}

func ConvertResponse(chatResponse, originalRequest map[string]any) map[string]any {
	chatID := stringValue(chatResponse["id"])
	responseID := convertID(chatID)
	created := intValue(chatResponse["created"])
	model := stringValue(chatResponse["model"])
	if model == "" {
		model = stringValue(originalRequest["model"])
	}

	var choice map[string]any
	if choices, ok := chatResponse["choices"].([]any); ok && len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	if choice == nil {
		choice = map[string]any{}
	}

	message, _ := choice["message"].(map[string]any)
	finishReason := stringValue(choice["finish_reason"])

	var output []any
	if len(message) > 0 {
		if item := buildOutputItem(message, responseID, finishReason); item != nil {
			output = append(output, item)
		}
	}

	status, incompleteDetails := mapFinishReason(finishReason)

	return map[string]any{
		"id":                  responseID,
		"object":              "response",
		"created_at":          created,
		"status":              status,
		"error":               nil,
		"incomplete_details":  incompleteDetails,
		"instructions":        originalRequest["instructions"],
		"max_output_tokens":   originalRequest["max_output_tokens"],
		"model":               model,
		"output":              output,
		"parallel_tool_calls": valueOrDefault(originalRequest["parallel_tool_calls"], true),
		"temperature":         valueOrDefault(originalRequest["temperature"], 1.0),
		"tool_choice":         valueOrDefault(originalRequest["tool_choice"], "auto"),
		"tools":               valueOrDefault(originalRequest["tools"], []any{}),
		"top_p":               valueOrDefault(originalRequest["top_p"], 1.0),
		"truncation":          "disabled",
		"usage":               convertUsage(chatResponse["usage"]),
		"user":                originalRequest["user"],
		"metadata":            valueOrDefault(originalRequest["metadata"], map[string]any{}),
	}
}

func convertInputItem(item any) (map[string]any, bool) {
	dict, ok := item.(map[string]any)
	if !ok {
		return nil, false
	}

	itemType := stringValue(dict["type"])
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
			convertedContent := make([]any, 0, len(contentList))
			textOnly := true
			for _, rawPart := range contentList {
				part, ok := rawPart.(map[string]any)
				if !ok {
					textOnly = false
					continue
				}

				convertedPart, ok := convertContentPart(part)
				if !ok {
					textOnly = false
					continue
				}
				convertedContent = append(convertedContent, convertedPart)
				if stringValue(convertedPart["type"]) != "text" {
					textOnly = false
				}
			}

			if textOnly {
				var builder strings.Builder
				for _, rawPart := range convertedContent {
					part, _ := rawPart.(map[string]any)
					builder.WriteString(stringValue(part["text"]))
				}
				content = builder.String()
			} else {
				content = convertedContent
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
			"tool_call_id": dict["call_id"],
			"content":      valueOrDefault(dict["output"], ""),
		}, true
	}

	return nil, false
}

func convertContentPart(part map[string]any) (map[string]any, bool) {
	switch stringValue(part["type"]) {
	case "input_text", "output_text":
		return map[string]any{"type": "text", "text": stringValue(part["text"])}, true
	case "input_image":
		imageURL := part["image_url"]
		if imageDict, ok := imageURL.(map[string]any); ok {
			imageURL = imageDict["url"]
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": stringValue(imageURL)},
		}, true
	default:
		return part, true
	}
}

func convertTools(tools []any) ([]any, bool) {
	converted := make([]any, 0, len(tools))
	hasWebSearch := false

	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}

		toolType := stringValue(tool["type"])
		if toolType == "" {
			toolType = "function"
		}
		if toolType == "web_search" {
			if boolValue(tool["external_web_access"]) {
				hasWebSearch = true
			}
			continue
		}
		if toolType == "custom" || toolType == "namespace" {
			continue
		}

		function, hasFunction := tool["function"].(map[string]any)
		name := stringValue(tool["name"])
		if hasFunction {
			name = stringValue(function["name"])
		}

		if name == "" {
			continue
		}
		if _, blocked := codexInternalTools[name]; blocked {
			continue
		}

		if hasFunction {
			converted = append(converted, tool)
			continue
		}

		converted = append(converted, map[string]any{
			"type": toolType,
			"function": map[string]any{
				"name":        name,
				"description": valueOrDefault(tool["description"], ""),
				"parameters":  valueOrDefault(tool["parameters"], map[string]any{}),
			},
		})
	}

	return converted, hasWebSearch
}

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

func buildOutputItem(message map[string]any, responseID, finishReason string) map[string]any {
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
			outputItem["content"] = append(outputItem["content"].([]any), map[string]any{
				"type":        "output_text",
				"text":        content,
				"annotations": []any{},
			})
		}
	case []any:
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			converted := convertOutputPart(part)
			if converted != nil {
				outputItem["content"] = append(outputItem["content"].([]any), converted)
			}
		}
	}

	if toolCalls, ok := message["tool_calls"].([]any); ok {
		for _, rawToolCall := range toolCalls {
			toolCall, ok := rawToolCall.(map[string]any)
			if !ok {
				continue
			}
			outputItem["content"] = append(outputItem["content"].([]any), convertToolCall(toolCall))
		}
	}

	return outputItem
}

func convertOutputPart(part map[string]any) map[string]any {
	if stringValue(part["type"]) == "text" {
		return map[string]any{
			"type":        "output_text",
			"text":        stringValue(part["text"]),
			"annotations": []any{},
		}
	}
	return part
}

func convertToolCall(toolCall map[string]any) map[string]any {
	function, _ := toolCall["function"].(map[string]any)
	return map[string]any{
		"type":      "tool_call",
		"id":        toolCall["id"],
		"call_type": valueOrDefault(toolCall["type"], "function"),
		"status":    "completed",
		"name":      stringValue(function["name"]),
		"arguments": valueOrDefault(function["arguments"], "{}"),
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

type StreamingConverter struct {
	initialized        bool
	outputItemAdded    bool
	contentPartAdded   bool
	textDone           bool
	outputItemDone     bool
	completed          bool
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
		messageRole: "assistant",
		toolCalls:   map[int]*streamToolCall{},
		usage:       map[string]any{},
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
	if reasoning := stringValue(delta["reasoning_content"]); reasoning != "" {
		c.reasoningText += reasoning
	}
	if content := stringValue(delta["content"]); content != "" {
		events = append(events, c.ensureContentPart()...)
		c.fullText += content
		events = append(events, sseEvent("response.output_text.delta", map[string]any{
			"item_id":       c.messageID,
			"output_index":  c.messageOutputIndexValue(),
			"content_index": 0,
			"delta":         content,
		}))
	}

	if rawToolCalls, ok := delta["tool_calls"].([]any); ok {
		for _, rawToolCall := range rawToolCalls {
			toolCall, ok := rawToolCall.(map[string]any)
			if !ok {
				continue
			}
			events = append(events, c.processToolCallDelta(toolCall)...)
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
				"id":     c.responseID,
				"object": "response",
				"status": "in_progress",
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
			"type":    "message",
			"id":      c.messageID,
			"status":  "in_progress",
			"role":    itemRole,
			"content": []any{},
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
		"item_id":       c.messageID,
		"output_index":  c.messageOutputIndexValue(),
		"content_index": 0,
		"part": map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{},
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
				"type":      "function_call",
				"id":        entry.ItemID,
				"call_id":   entry.ID,
				"status":    "in_progress",
				"name":      entry.Name,
				"arguments": "",
			},
		}))
	}

	if arguments := stringValue(function["arguments"]); arguments != "" {
		entry.Arguments += arguments
		events = append(events, sseEvent("response.function_call_arguments.delta", map[string]any{
			"item_id":      entry.ItemID,
			"output_index": *entry.OutputIndex,
			"delta":        arguments,
		}))
	}

	return events
}

func (c *StreamingConverter) finishOutputItems() []string {
	var events []string
	if !c.outputItemDone {
		c.outputItemDone = true
		events = append(events, c.finishTextEvents()...)
		if c.outputItemAdded {
			content := []any{}
			if c.contentPartAdded {
				content = append(content, map[string]any{
					"type":        "output_text",
					"text":        c.fullText,
					"annotations": []any{},
				})
			}
			events = append(events, sseEvent("response.output_item.done", map[string]any{
				"output_index": c.messageOutputIndexValue(),
				"item": map[string]any{
					"type":    "message",
					"id":      c.messageID,
					"status":  "completed",
					"role":    c.messageRole,
					"content": content,
				},
			}))
		}
	}
	events = append(events, c.finishToolEvents()...)
	return events
}

func (c *StreamingConverter) finishTextEvents() []string {
	if !c.contentPartAdded || c.textDone {
		return nil
	}
	c.textDone = true
	return []string{
		sseEvent("response.output_text.done", map[string]any{
			"item_id":       c.messageID,
			"output_index":  c.messageOutputIndexValue(),
			"content_index": 0,
			"text":          c.fullText,
		}),
		sseEvent("response.content_part.done", map[string]any{
			"item_id":       c.messageID,
			"output_index":  c.messageOutputIndexValue(),
			"content_index": 0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        c.fullText,
				"annotations": []any{},
			},
		}),
	}
}

func (c *StreamingConverter) finishToolEvents() []string {
	var events []string
	indexes := sortedToolIndexes(c.toolCalls)
	for _, index := range indexes {
		toolCall := c.toolCalls[index]
		if !toolCall.Added || toolCall.OutputIndex == nil {
			continue
		}
		events = append(events,
			sseEvent("response.function_call_arguments.done", map[string]any{
				"item_id":      toolCall.ItemID,
				"output_index": *toolCall.OutputIndex,
				"arguments":    toolCall.Arguments,
			}),
			sseEvent("response.output_item.done", map[string]any{
				"output_index": *toolCall.OutputIndex,
				"item": map[string]any{
					"type":      "function_call",
					"id":        toolCall.ItemID,
					"call_id":   toolCall.ID,
					"status":    "completed",
					"name":      toolCall.Name,
					"arguments": toolCall.Arguments,
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
			"id":         valueOrDefault(c.responseID, "resp_unknown"),
			"object":     "response",
			"created_at": c.created,
			"status":     status,
			"output":     c.buildOutput(),
			"usage":      c.buildUsage(),
		},
	})}
}

func (c *StreamingConverter) buildOutput() []any {
	var output []any
	if c.reasoningText != "" {
		output = append(output, map[string]any{
			"type":   "reasoning",
			"id":     "reasoning_" + c.messageID,
			"status": "completed",
			"content": []any{
				map[string]any{
					"type": "reasoning_text",
					"text": c.reasoningText,
				},
			},
		})
	}
	if c.contentPartAdded {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     c.messageID,
			"status": "completed",
			"role":   c.messageRole,
			"content": []any{
				map[string]any{
					"type":        "output_text",
					"text":        c.fullText,
					"annotations": []any{},
				},
			},
		})
	}
	for _, index := range sortedToolIndexes(c.toolCalls) {
		toolCall := c.toolCalls[index]
		if toolCall.Added {
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        toolCall.ItemID,
				"call_id":   toolCall.ID,
				"status":    "completed",
				"name":      toolCall.Name,
				"arguments": toolCall.Arguments,
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
	outputIndex := c.nextOutputIndex
	c.nextOutputIndex++
	return outputIndex
}

func (c *StreamingConverter) messageOutputIndexValue() int {
	if c.messageOutputIndex == nil {
		outputIndex := c.allocateOutputIndex()
		c.messageOutputIndex = &outputIndex
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
		"id":                  responseID,
		"object":              "response",
		"created_at":          created,
		"status":              status,
		"error":               nil,
		"incomplete_details":  nil,
		"instructions":        nil,
		"max_output_tokens":   nil,
		"model":               model,
		"output":              []any{},
		"parallel_tool_calls": true,
		"temperature":         1.0,
		"tool_choice":         "auto",
		"tools":               []any{},
		"top_p":               1.0,
		"truncation":          "disabled",
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
		},
		"user":     nil,
		"metadata": map[string]any{},
	}
}

func sseEvent(eventType string, data map[string]any) string {
	payload := map[string]any{"type": eventType}
	for key, value := range data {
		payload[key] = value
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
