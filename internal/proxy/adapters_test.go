package proxy

import (
	"encoding/json"
	"testing"
)

func TestConvertRequestStringInput(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model":             "test-model",
		"instructions":      "Be concise.",
		"input":             "Hello",
		"max_output_tokens": 20,
		"reasoning":         map[string]any{"effort": "low"},
	}, Config{})

	if converted["model"] != "test-model" {
		t.Fatalf("unexpected model: %v", converted["model"])
	}

	wantMessages := []any{
		map[string]any{"role": "system", "content": "Be concise."},
		map[string]any{"role": "user", "content": "Hello"},
	}
	assertJSONEqual(t, wantMessages, converted["messages"])
	if converted["max_tokens"] != 20 {
		t.Fatalf("unexpected max_tokens: %v", converted["max_tokens"])
	}
	if converted["reasoning_effort"] != "low" {
		t.Fatalf("unexpected reasoning effort: %v", converted["reasoning_effort"])
	}
}

func TestConvertResponseNonStreaming(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "Hi"},
			},
		},
		"usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	if converted["id"] != "resp-abc" {
		t.Fatalf("unexpected response id: %v", converted["id"])
	}
	if converted["status"] != "completed" {
		t.Fatalf("unexpected status: %v", converted["status"])
	}

	output := converted["output"].([]any)
	item := output[0].(map[string]any)
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	if part["text"] != "Hi" {
		t.Fatalf("unexpected output text: %v", part["text"])
	}

	usage := converted["usage"].(map[string]any)
	if usage["input_tokens"] != 2 || usage["output_tokens"] != 3 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}

func TestConvertRequestPreservesFunctionCallHistory(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model": "test-model",
		"input": []any{
			map[string]any{"role": "user", "content": "List files."},
			map[string]any{
				"type":      "function_call",
				"id":        "fc_abc",
				"call_id":   "call_abc",
				"name":      "shell",
				"arguments": `{"command":"ls"}`,
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_abc",
				"output":  "README.md",
			},
		},
	}, Config{})

	want := []any{
		map[string]any{"role": "user", "content": "List files."},
		map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				map[string]any{
					"id":   "call_abc",
					"type": "function",
					"function": map[string]any{
						"name":      "shell",
						"arguments": `{"command":"ls"}`,
					},
				},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_abc", "content": "README.md"},
	}

	assertJSONEqual(t, want, converted["messages"])
}

func TestStreamingConverterTextDelta(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{"role": "assistant", "content": "Hi"}, "finish_reason": nil},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
		},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}) + "\n\n"

	events := stringsJoin(converter.Feed([]byte(chunk)))
	assertContains(t, events, "response.created")
	assertContains(t, events, "response.output_text.delta")
	assertContains(t, events, "response.completed")
	assertContains(t, events, "Hi")
}

func TestStreamingConverterToolOutputIndexStaysStableWhenMessageAddedLater(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"index": 0,
							"id":    "call_abc",
							"type":  "function",
							"function": map[string]any{
								"name":      "shell",
								"arguments": `{"command"`,
							},
						},
					},
				},
				"finish_reason": nil,
			},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{
							"index": 0,
							"function": map[string]any{
								"arguments": `:"ls"}`,
							},
						},
					},
					"content": "I will run it.",
				},
				"finish_reason": nil,
			},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"},
		},
	}) + "\n\n"

	payloads := ssePayloads(converter.Feed([]byte(chunk)))
	var functionEvents []map[string]any
	for _, payload := range payloads {
		switch payload["type"] {
		case "response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done":
			if payload["item_id"] == "fc_abc" {
				functionEvents = append(functionEvents, payload)
				continue
			}
			if item, ok := payload["item"].(map[string]any); ok && item["id"] == "fc_abc" {
				functionEvents = append(functionEvents, payload)
			}
		}
	}

	for _, event := range functionEvents {
		if event["output_index"] != float64(0) {
			t.Fatalf("unexpected function output index: %#v", functionEvents)
		}
	}

	foundMessageDelta := false
	for _, payload := range payloads {
		if payload["type"] == "response.output_text.delta" && payload["output_index"] == float64(1) {
			foundMessageDelta = true
		}
	}
	if !foundMessageDelta {
		t.Fatalf("expected message delta with output_index 1, payloads=%#v", payloads)
	}
}

func assertJSONEqual(t *testing.T, want, got any) {
	t.Helper()
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("unexpected JSON\nwant: %s\ngot:  %s", wantJSON, gotJSON)
	}
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func ssePayloads(events []string) []map[string]any {
	var payloads []map[string]any
	for _, event := range events {
		for _, line := range splitLines(event) {
			if len(line) > 6 && line[:6] == "data: " {
				var payload map[string]any
				_ = json.Unmarshal([]byte(line[6:]), &payload)
				payloads = append(payloads, payload)
			}
		}
	}
	return payloads
}

func splitLines(value string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(value); i++ {
		if value[i] == '\n' {
			lines = append(lines, value[start:i])
			start = i + 1
		}
	}
	if start <= len(value) {
		lines = append(lines, value[start:])
	}
	return lines
}

func stringsJoin(values []string) string {
	result := ""
	for _, value := range values {
		result += value
	}
	return result
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !contains(haystack, needle) {
		t.Fatalf("expected %q to contain %q", haystack, needle)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && (haystack == needle || contains(haystack[1:], needle) || (len(haystack) >= len(needle) && haystack[:len(needle)] == needle)))
}
