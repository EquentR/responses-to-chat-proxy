package proxy

import (
	"encoding/json"
	"strings"
)

// ---------------------------------------------------------------------------
// Cache Injector -- mirrors cc-switch cache_injector.rs
// ---------------------------------------------------------------------------

// InjectCacheBreakpoints injects cache_control breakpoints at strategic
// positions in a Chat Completions request body to enable prompt caching on
// providers that support it (Anthropic Bedrock, etc.).
//
// Up to 4 breakpoints are injected:
//  1. Last tool definition
//  2. Last system message content block
//  3. Last assistant message's last non-thinking content block
//  4. (inherited from existing breakpoints -- up to 4 total)
//
// The function mutates body in place and returns the number of new breakpoints injected.
func InjectCacheBreakpoints(body map[string]any, ttl string) int {
	existing := countExistingBreakpoints(body)
	if existing >= 4 {
		// Already at max capacity -- just upgrade TTLs.
		upgradeExistingTTL(body, ttl)
		return 0
	}

	budget := 4 - existing
	injected := 0

	// (a) tools last element
	if budget > 0 {
		if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
			lastIndex := len(tools) - 1
			last := tools[lastIndex]
			if lastMap, ok := last.(map[string]any); ok {
				if lastMap["cache_control"] == nil {
					lastMap["cache_control"] = makeCacheControl(ttl)
					tools[lastIndex] = lastMap
					body["tools"] = tools
					budget--
					injected++
				}
			}
		}
	}

	// (b) system message last content block
	if budget > 0 {
		for _, rawMsg := range body["messages"].([]any) {
			msg, _ := rawMsg.(map[string]any)
			if stringValue(msg["role"]) != "system" {
				continue
			}
			if contentList, ok := msg["content"].([]any); ok && len(contentList) > 0 {
				lastIndex := len(contentList) - 1
				last := contentList[lastIndex]
				if lastMap, ok := last.(map[string]any); ok {
					if lastMap["cache_control"] == nil {
						lastMap["cache_control"] = makeCacheControl(ttl)
						contentList[lastIndex] = lastMap
						msg["content"] = contentList
						budget--
						injected++
						break
					}
				}
			}
			break
		}
	}

	// (c) last assistant message's last non-thinking block
	if budget > 0 {
		messages, _ := body["messages"].([]any)
		for i := len(messages) - 1; i >= 0; i-- {
			msg, _ := messages[i].(map[string]any)
			if stringValue(msg["role"]) != "assistant" {
				continue
			}
			if contentList, ok := msg["content"].([]any); ok {
				for j := len(contentList) - 1; j >= 0; j-- {
					block, _ := contentList[j].(map[string]any)
					bt := stringValue(block["type"])
					if bt == "thinking" || bt == "redacted_thinking" {
						continue
					}
					if block["cache_control"] == nil {
						block["cache_control"] = makeCacheControl(ttl)
						contentList[j] = block
						msg["content"] = contentList
						messages[i] = msg
						body["messages"] = messages
						budget--
						injected++
					}
					break
				}
			}
			break
		}
	}

	// Upgrade any existing breakpoints' TTL
	if existing > 0 {
		upgradeExistingTTL(body, ttl)
	}

	return injected
}

// countExistingBreakpoints counts all existing cache_control entries.
func countExistingBreakpoints(body map[string]any) int {
	count := 0

	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok && tm["cache_control"] != nil {
				count++
			}
		}
	}

	messages, _ := body["messages"].([]any)
	for _, rawMsg := range messages {
		msg, _ := rawMsg.(map[string]any)
		if contentList, ok := msg["content"].([]any); ok {
			for _, block := range contentList {
				if bm, ok := block.(map[string]any); ok && bm["cache_control"] != nil {
					count++
				}
			}
		}
	}

	return count
}

// upgradeExistingTTL upgrades all existing cache_control TTLs.
func upgradeExistingTTL(body map[string]any, ttl string) {
	upgrade := func(val map[string]any) {
		if cc, ok := val["cache_control"].(map[string]any); ok {
			if ttl == "5m" {
				delete(cc, "ttl")
			} else {
				cc["ttl"] = ttl
			}
		}
	}

	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok {
				upgrade(tm)
			}
		}
	}

	messages, _ := body["messages"].([]any)
	for _, rawMsg := range messages {
		msg, _ := rawMsg.(map[string]any)
		if contentList, ok := msg["content"].([]any); ok {
			for _, block := range contentList {
				if bm, ok := block.(map[string]any); ok {
					upgrade(bm)
				}
			}
		}
	}
}

func makeCacheControl(ttl string) map[string]any {
	if ttl == "5m" {
		return map[string]any{"type": "ephemeral"}
	}
	return map[string]any{"type": "ephemeral", "ttl": ttl}
}

// ---------------------------------------------------------------------------
// Thinking Rectifier -- mirrors cc-switch thinking_rectifier.rs
// ---------------------------------------------------------------------------

// ShouldRectifyThinkingSignature checks whether an upstream error message
// indicates a thinking signature problem that can be fixed by stripping
// thinking blocks and retrying.
func ShouldRectifyThinkingSignature(errorBody string) bool {
	lower := strings.ToLower(errorBody)

	// Scenario 1: Invalid signature in thinking block
	if strings.Contains(lower, "invalid") &&
		strings.Contains(lower, "signature") &&
		strings.Contains(lower, "thinking") &&
		strings.Contains(lower, "block") {
		return true
	}

	// Scenario 1b: "Thought signature is not valid"
	if strings.Contains(lower, "thought signature") &&
		(strings.Contains(lower, "not valid") || strings.Contains(lower, "invalid")) {
		return true
	}

	// Scenario 2: "must start with a thinking block"
	if strings.Contains(lower, "must start with a thinking block") {
		return true
	}

	// Scenario 3: "Expected thinking or redacted_thinking, but found tool_use"
	if strings.Contains(lower, "expected") &&
		(strings.Contains(lower, "thinking") || strings.Contains(lower, "redacted_thinking")) &&
		strings.Contains(lower, "found") &&
		strings.Contains(lower, "tool_use") {
		return true
	}

	// Scenario 4: signature field required
	if strings.Contains(lower, "signature") && strings.Contains(lower, "field required") {
		return true
	}

	// Scenario 5: signature field not permitted
	if strings.Contains(lower, "signature") && strings.Contains(lower, "extra inputs are not permitted") {
		return true
	}

	// Scenario 6: thinking blocks cannot be modified
	if (strings.Contains(lower, "thinking") || strings.Contains(lower, "redacted_thinking")) &&
		strings.Contains(lower, "cannot be modified") {
		return true
	}

	// Scenario 7: invalid request (generic fallback for Chinese/non-English errors)
	if strings.Contains(lower, "invalid request") ||
		strings.Contains(lower, "ille") {
		return true
	}

	return false
}

// StripThinkingBlocks removes all thinking and redacted_thinking content
// blocks from messages, and removes legacy signature fields from non-thinking
// blocks.  Returns true if any blocks were removed.
func StripThinkingBlocks(body map[string]any) bool {
	messages, ok := body["messages"].([]any)
	if !ok {
		return false
	}

	modified := false

	for i, rawMsg := range messages {
		msg, _ := rawMsg.(map[string]any)
		content, _ := msg["content"].([]any)
		if len(content) == 0 {
			continue
		}

		var newContent []any
		msgModified := false

		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			bt := stringValue(block["type"])

			// Remove thinking / redacted_thinking blocks
			if bt == "thinking" || bt == "redacted_thinking" {
				msgModified = true
				continue
			}

			// Remove signature field from non-thinking blocks
			if block["signature"] != nil {
				delete(block, "signature")
				msgModified = true
			}

			newContent = append(newContent, block)
		}

		if msgModified {
			msg["content"] = newContent
			messages[i] = msg
			modified = true
		}
	}

	if modified {
		body["messages"] = messages
	}

	// Remove top-level thinking when the last assistant message no longer
	// starts with a thinking block after stripping.
	if thinking, ok := body["thinking"].(map[string]any); ok {
		if stringValue(thinking["type"]) == "enabled" {
			// Find last assistant message
			for i := len(messages) - 1; i >= 0; i-- {
				msg, _ := messages[i].(map[string]any)
				if stringValue(msg["role"]) != "assistant" {
					continue
				}
				content, _ := msg["content"].([]any)
				firstType := ""
				if len(content) > 0 {
					if first, ok := content[0].(map[string]any); ok {
						firstType = stringValue(first["type"])
					}
				}
				// If first block is not thinking, remove top-level thinking
				if firstType != "thinking" && firstType != "redacted_thinking" {
					// Check if there are tool_use blocks (tool call scenario)
					hasToolUse := false
					for _, block := range content {
						if bm, ok := block.(map[string]any); ok && stringValue(bm["type"]) == "tool_use" {
							hasToolUse = true
							break
						}
					}
					if hasToolUse {
						delete(body, "thinking")
						modified = true
					}
				}
				break
			}
		}
	}

	return modified
}

// TryParseErrorBody attempts to extract an error message from an upstream
// response body for thinking rectification detection.
func TryParseErrorBody(body []byte) string {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return string(body)
	}
	if errObj, ok := data["error"].(map[string]any); ok {
		if msg := stringValue(errObj["message"]); msg != "" {
			return msg
		}
	}
	if msg := stringValue(data["error"]); msg != "" {
		return msg
	}
	if detail := stringValue(data["detail"]); detail != "" {
		return detail
	}
	// MiniMax base_resp format
	if baseResp, ok := data["base_resp"].(map[string]any); ok {
		if msg := stringValue(baseResp["status_msg"]); msg != "" {
			return msg
		}
	}
	if msg := stringValue(data["message"]); msg != "" {
		return msg
	}
	return string(body)
}
