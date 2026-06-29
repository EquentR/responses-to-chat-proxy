package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

func stickyRequestKey(body map[string]any) string {
	if body == nil {
		return ""
	}
	if explicit := explicitStickyKey(body); explicit != "" {
		return "explicit:" + explicit
	}

	parts := map[string]any{}
	if model := strings.TrimSpace(stringValue(body["model"])); model != "" {
		parts["model"] = model
	}
	if instructions := strings.TrimSpace(stringValue(body["instructions"])); instructions != "" {
		parts["instructions"] = instructions
	}
	if system := stableValue(body["system"]); system != nil {
		parts["system"] = system
	}
	if developer := stableValue(body["developer"]); developer != nil {
		parts["developer"] = developer
	}
	if tools := stableValue(body["tools"]); tools != nil {
		parts["tools"] = tools
	}
	if prefix := stableMessagePrefix(body); len(prefix) > 0 {
		parts["message_prefix"] = prefix
	}

	if len(parts) <= 1 {
		return ""
	}

	raw, err := json.Marshal(canonicalValue(parts))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "derived:" + hex.EncodeToString(sum[:])
}

func stickyRequestKeyFromBody(body any) string {
	if typed, ok := body.(map[string]any); ok {
		return stickyRequestKey(typed)
	}
	return ""
}

func explicitStickyKey(body map[string]any) string {
	for _, key := range []string{"sticky_key", "session_id", "conversation_id", "thread_id", "previous_response_id", "user"} {
		if value := strings.TrimSpace(stringValue(body[key])); value != "" {
			return key + ":" + value
		}
	}

	metadata, _ := body["metadata"].(map[string]any)
	for _, key := range []string{"sticky_key", "session_id", "conversation_id", "thread_id"} {
		if value := strings.TrimSpace(stringValue(metadata[key])); value != "" {
			return "metadata." + key + ":" + value
		}
	}
	return ""
}

func stableMessagePrefix(body map[string]any) []any {
	if messages, ok := body["messages"].([]any); ok {
		return prefixWithoutCurrentTail(messages)
	}
	if input, ok := body["input"].([]any); ok {
		return prefixWithoutCurrentTail(input)
	}
	return nil
}

func prefixWithoutCurrentTail(items []any) []any {
	if len(items) < 2 {
		return nil
	}
	prefix := make([]any, 0, len(items)-1)
	for _, item := range items[:len(items)-1] {
		stable := stableValue(item)
		if stable != nil {
			prefix = append(prefix, stable)
		}
	}
	return prefix
}

func stableValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return typed
	case bool, float64, int, int64, json.Number:
		return typed
	case []any:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			stable := stableValue(item)
			if stable != nil {
				values = append(values, stable)
			}
		}
		if len(values) == 0 {
			return nil
		}
		return values
	case map[string]any:
		values := make(map[string]any, len(typed))
		for key, item := range typed {
			if isUnstableStickyField(key) {
				continue
			}
			stable := stableValue(item)
			if stable != nil {
				values[key] = stable
			}
		}
		if len(values) == 0 {
			return nil
		}
		return values
	default:
		text := strings.TrimSpace(stringValue(typed))
		if text == "" {
			return nil
		}
		return text
	}
}

func isUnstableStickyField(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "id", "created", "created_at", "updated_at", "timestamp":
		return true
	default:
		return false
	}
}

func canonicalValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ordered := make([]any, 0, len(keys))
		for _, key := range keys {
			ordered = append(ordered, []any{key, canonicalValue(typed[key])})
		}
		return ordered
	case []any:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			values = append(values, canonicalValue(item))
		}
		return values
	default:
		return typed
	}
}
