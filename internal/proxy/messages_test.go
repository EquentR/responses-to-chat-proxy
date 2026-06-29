package proxy

import (
	"strings"
	"testing"
)

func TestConvertResponsesToMessagesHandlesInstructionsAndTools(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model":             "claude-4",
		"instructions":      "Be helpful.",
		"input":             "Hello",
		"max_output_tokens": 20,
		"tools": []any{
			map[string]any{
				"type": "function",
				"name": "shell",
				"function": map[string]any{
					"description": "Run shell commands.",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"command": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}, Config{}, RouteEntry{})

	if converted["system"] != "Be helpful." {
		t.Fatalf("unexpected system: %#v", converted["system"])
	}
	if converted["max_tokens"] != 20 {
		t.Fatalf("unexpected max_tokens: %#v", converted["max_tokens"])
	}
	if converted["model"] != "claude-4" {
		t.Fatalf("unexpected model: %#v", converted["model"])
	}

	messages, _ := converted["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("expected one user message, got %#v", converted["messages"])
	}
	content, _ := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("unexpected message content: %#v", messages[0])
	}

	tools, _ := converted["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected one tool, got %#v", converted["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("unexpected tool type: %#v", tool)
	}
}

func TestConvertResponsesToMessagesHandlesImagesAndReasoning(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"input": []any{
			map[string]any{"type": "input_text", "text": "Look"},
			map[string]any{
				"type":      "input_image",
				"image_url": "https://example.com/image.png",
				"detail":    "high",
			},
			map[string]any{
				"type":      "input_file",
				"file_id":   "file-123",
				"file_data": "Zm9v",
				"filename":  "notes.txt",
			},
			map[string]any{
				"type": "input_audio",
				"input_audio": map[string]any{
					"data":   "QUJD",
					"format": "mp3",
				},
			},
		},
	}, Config{}, RouteEntry{Features: []string{"image", "audio", "file"}})

	messages, _ := converted["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected four messages, got %#v", converted["messages"])
	}
	if messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] != "image" {
		t.Fatalf("expected image block, got %#v", messages[1])
	}
	if messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] != "document" {
		t.Fatalf("expected document block, got %#v", messages[2])
	}
	if messages[3].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] != "audio" {
		t.Fatalf("expected audio block, got %#v", messages[3])
	}
}

func TestConvertResponsesToMessagesOnlyEmitsThinkingWhenExplicitlyConfigured(t *testing.T) {
	input := map[string]any{
		"model": "claude-4",
		"reasoning": map[string]any{
			"effort":        "high",
			"budget_tokens": 128,
		},
		"input": "Hello",
	}

	defaultConverted := ConvertResponsesToMessages(input, Config{}, RouteEntry{})
	if _, ok := defaultConverted["thinking"]; ok {
		t.Fatalf("unexpected thinking without explicit mode: %#v", defaultConverted["thinking"])
	}

	thinkingConverted := ConvertResponsesToMessages(input, Config{ReasoningMode: ReasoningThinking}, RouteEntry{})
	thinking, _ := thinkingConverted["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != 128 {
		t.Fatalf("expected explicit thinking mode to emit thinking, got %#v", thinkingConverted["thinking"])
	}

	disabledConverted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"reasoning": map[string]any{
			"effort": "off",
		},
		"input": "Hello",
	}, Config{ReasoningMode: ReasoningThinkingOnly}, RouteEntry{})
	disabledThinking, _ := disabledConverted["thinking"].(map[string]any)
	if disabledThinking["type"] != "disabled" {
		t.Fatalf("expected explicit thinking_only mode to emit disabled thinking, got %#v", disabledConverted["thinking"])
	}
}

func TestConvertResponsesToMessagesEmitsThinkingWhenRouteFeaturesAllowIt(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"reasoning": map[string]any{
			"effort":        "medium",
			"budget_tokens": 64,
		},
		"input": "Hello",
	}, Config{}, RouteEntry{Features: []string{"thinking"}})

	thinking, _ := converted["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != 64 {
		t.Fatalf("expected route features to enable thinking, got %#v", converted["thinking"])
	}
}

func TestConvertResponsesToMessagesDoesNotEmitThinkingWithoutConfigOrRouteSupport(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"reasoning": map[string]any{
			"effort": "high",
		},
		"input": "Hello",
	}, Config{}, RouteEntry{Features: []string{"text-only"}})

	if _, ok := converted["thinking"]; ok {
		t.Fatalf("unexpected thinking without explicit support: %#v", converted["thinking"])
	}
}

func TestConvertResponsesToMessagesDoesNotEmitThinkingFromRouteReasoningTextAlone(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"reasoning": map[string]any{
			"effort": "high",
		},
		"input": "Hello",
	}, Config{}, RouteEntry{Reasoning: "supports thinking.type gating"})

	if _, ok := converted["thinking"]; ok {
		t.Fatalf("unexpected thinking from free-form route reasoning text: %#v", converted["thinking"])
	}
}

func TestConvertResponsesToMessagesDowngradesMultimodalWhenRouteIsTextOnly(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"input": []any{
			map[string]any{
				"type":      "input_image",
				"image_url": "https://example.com/image.png",
			},
			map[string]any{
				"type":     "input_file",
				"file_id":  "file-123",
				"filename": "notes.txt",
			},
			map[string]any{
				"type": "input_audio",
				"input_audio": map[string]any{
					"format": "mp3",
					"data":   "QUJD",
				},
			},
		},
	}, Config{}, RouteEntry{Features: []string{"text-only"}})

	messages, _ := converted["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected three downgraded messages, got %#v", converted["messages"])
	}

	wantSubstrings := []string{
		"[image input]",
		"[file input]",
		"[audio input]",
	}
	for index, want := range wantSubstrings {
		content, _ := messages[index].(map[string]any)["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
			t.Fatalf("expected downgraded text block at index %d, got %#v", index, messages[index])
		}
		text := content[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, want) {
			t.Fatalf("expected downgraded text %q to contain %q", text, want)
		}
	}
}

func TestConvertResponsesToMessagesKeepsMultimodalWhenRouteFeaturesAreAmbiguous(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"input": []any{
			map[string]any{
				"type":      "input_image",
				"image_url": "https://example.com/image.png",
			},
			map[string]any{
				"type":     "input_file",
				"file_id":  "file-123",
				"filename": "notes.txt",
			},
			map[string]any{
				"type": "input_audio",
				"input_audio": map[string]any{
					"format": "mp3",
					"data":   "QUJD",
				},
			},
		},
	}, Config{}, RouteEntry{Features: []string{"text"}})

	messages, _ := converted["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected three multimodal messages, got %#v", converted["messages"])
	}

	wantTypes := []string{"image", "document", "audio"}
	for index, wantType := range wantTypes {
		content, _ := messages[index].(map[string]any)["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["type"] != wantType {
			t.Fatalf("expected %s block at index %d, got %#v", wantType, index, messages[index])
		}
	}
}

func TestConvertResponsesToMessagesDowngradesOnlyWithExplicitNegativeCapabilitySignals(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"input": []any{
			map[string]any{
				"type":      "input_image",
				"image_url": "https://example.com/image.png",
			},
			map[string]any{
				"type":     "input_file",
				"file_id":  "file-123",
				"filename": "notes.txt",
			},
			map[string]any{
				"type": "input_audio",
				"input_audio": map[string]any{
					"format": "mp3",
					"data":   "QUJD",
				},
			},
		},
	}, Config{}, RouteEntry{Features: []string{"text-only", "no-image", "no-file", "no-audio"}})

	messages, _ := converted["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected three downgraded messages, got %#v", converted["messages"])
	}
	for index, raw := range messages {
		content, _ := raw.(map[string]any)["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
			t.Fatalf("expected explicit negative capability downgrade at index %d, got %#v", index, raw)
		}
	}
}

func TestConvertResponsesToMessagesDowngradesImageWhenRouteHasNoVision(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"input": []any{
			map[string]any{
				"type":      "input_image",
				"image_url": "https://example.com/image.png",
			},
		},
	}, Config{}, RouteEntry{Features: []string{"no_vision"}})

	messages, _ := converted["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("expected one downgraded message, got %#v", converted["messages"])
	}
	content, _ := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("expected no_vision to downgrade image input, got %#v", messages[0])
	}
}

func TestConvertResponsesToMessagesNormalizesNestedAudioContent(t *testing.T) {
	converted := ConvertResponsesToMessages(map[string]any{
		"model": "claude-4",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Listen"},
					map[string]any{
						"type": "input_audio",
						"input_audio": map[string]any{
							"data":   "QUJD",
							"format": "wav",
						},
					},
				},
			},
		},
	}, Config{}, RouteEntry{Features: []string{"audio"}})

	messages, _ := converted["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("expected one message, got %#v", converted["messages"])
	}

	content, _ := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected two content blocks, got %#v", messages[0])
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("expected nested input_text to normalize to text, got %#v", content[0])
	}
	if content[1].(map[string]any)["type"] != "audio" {
		t.Fatalf("expected nested input_audio to normalize to audio, got %#v", content[1])
	}
}

func TestConvertMessagesToResponsesHandlesTextThinkingAndTools(t *testing.T) {
	converted := ConvertMessagesToResponses(map[string]any{
		"id":    "msg-123",
		"model": "claude-4",
		"role":  "assistant",
		"content": []any{
			map[string]any{"type": "thinking", "text": "pondering"},
			map[string]any{"type": "text", "text": "Hello"},
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_1",
				"name":  "shell",
				"input": map[string]any{"command": "pwd"},
			},
		},
		"usage":       map[string]any{"input_tokens": 4, "output_tokens": 2, "total_tokens": 6},
		"stop_reason": "end_turn",
	}, map[string]any{"model": "claude-4", "input": "hi"})

	if converted["status"] != "completed" {
		t.Fatalf("unexpected status: %#v", converted["status"])
	}
	output := converted["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("expected reasoning, message, and tool call, got %#v", output)
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("expected reasoning first, got %#v", output[0])
	}
	if output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("expected message second, got %#v", output[1])
	}
	if output[2].(map[string]any)["type"] != "function_call" {
		t.Fatalf("expected function call third, got %#v", output[2])
	}
}

func TestConvertMessagesToResponsesPropagatesCacheReadTokens(t *testing.T) {
	converted := ConvertMessagesToResponses(map[string]any{
		"id":    "msg-123",
		"model": "claude-4",
		"role":  "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "Hello"},
		},
		"usage": map[string]any{
			"input_tokens":            20,
			"output_tokens":           3,
			"total_tokens":            23,
			"cache_read_input_tokens": 12,
		},
		"stop_reason": "end_turn",
	}, map[string]any{"model": "claude-4", "input": "hi"})

	usage := converted["usage"].(map[string]any)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != 12 {
		t.Fatalf("expected cached_tokens=12, got %#v", usage)
	}
}

func TestConvertMessagesToResponsesDoesNotTreatCacheCreationAsCacheRead(t *testing.T) {
	converted := ConvertMessagesToResponses(map[string]any{
		"id":    "msg-123",
		"model": "claude-4",
		"role":  "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "Hello"},
		},
		"usage": map[string]any{
			"input_tokens":                20,
			"output_tokens":               3,
			"total_tokens":                23,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 20,
		},
		"stop_reason": "end_turn",
	}, map[string]any{"model": "claude-4", "input": "hi"})

	usage := converted["usage"].(map[string]any)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != 0 {
		t.Fatalf("cache creation must not count as cache read, got %#v", usage)
	}
}

func TestConvertMessagesToResponsesIgnoresUnknownBlocks(t *testing.T) {
	converted := ConvertMessagesToResponses(map[string]any{
		"id":    "msg-123",
		"model": "claude-4",
		"role":  "assistant",
		"content": []any{
			map[string]any{"type": "unknown_block", "text": "ignore me"},
			map[string]any{"type": "text", "text": "Hello"},
		},
	}, map[string]any{"model": "claude-4", "input": "hi"})

	output := converted["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected only message output, got %#v", output)
	}
	if output[0].(map[string]any)["type"] != "message" {
		t.Fatalf("expected message output, got %#v", output[0])
	}
}

func TestConvertMessagesStreamToResponsesSSE(t *testing.T) {
	converter := NewMessagesStreamingConverter(map[string]any{"model": "claude-4", "input": "hi"})
	events := []string{
		"event: message_start\ndata: " + mustJSON(map[string]any{
			"message": map[string]any{"id": "msg-123", "model": "claude-4", "role": "assistant"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "part1"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "part2"},
		}) + "\n\n",
		"event: content_block_start\ndata: " + mustJSON(map[string]any{
			"index":         1,
			"content_block": map[string]any{"type": "redacted_thinking", "data": "hidden-plan"},
		}) + "\n\n",
		"event: content_block_start\ndata: " + mustJSON(map[string]any{
			"index":         2,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "shell"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 2,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": "{\"command\":\"pwd\"}"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 3,
			"delta": map[string]any{"type": "text_delta", "text": "Hello"},
		}) + "\n\n",
		"event: message_delta\ndata: " + mustJSON(map[string]any{
			"delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"input_tokens": 7, "output_tokens": 5, "total_tokens": 12, "output_tokens_details": map[string]any{"reasoning_tokens": 3}},
		}) + "\n\n",
		"event: message_stop\ndata: {}\n\n",
	}

	var out []string
	for _, event := range events {
		out = append(out, converter.Feed([]byte(event))...)
	}
	out = append(out, converter.Finish()...)

	joined := strings.Join(out, "\n")
	assertContains(t, joined, "response.created")
	assertContains(t, joined, "response.output_text.delta")
	assertContains(t, joined, "response.function_call_arguments.delta")
	assertContains(t, joined, "response.completed")

	payloads := ssePayloads(out)
	completed := findPayload(payloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", payloads)
	}

	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("expected reasoning, message, and tool call in completed output, got %#v", output)
	}

	var reasoning map[string]any
	var message map[string]any
	var toolCall map[string]any
	for _, raw := range output {
		item := raw.(map[string]any)
		switch item["type"] {
		case "reasoning":
			reasoning = item
		case "message":
			message = item
		case "function_call":
			toolCall = item
		}
	}
	if reasoning == nil || message == nil || toolCall == nil {
		t.Fatalf("expected reasoning, message, and tool call outputs, got %#v", output)
	}
	summary := reasoning["summary"].([]any)
	if summary[0].(map[string]any)["text"] != "part1part2\nhidden-plan" {
		t.Fatalf("expected redacted thinking summary to be preserved, got %#v", reasoning)
	}
	content := message["content"].([]any)
	if content[0].(map[string]any)["text"] != "Hello" {
		t.Fatalf("expected final message text, got %#v", message)
	}
	if toolCall["call_id"] != "toolu_1" {
		t.Fatalf("expected completed function call output, got %#v", toolCall)
	}
	if toolCall["arguments"] != "{\"command\":\"pwd\"}" {
		t.Fatalf("expected function call arguments to be reconstructed, got %#v", toolCall)
	}

	usage := response["usage"].(map[string]any)
	if usage["total_tokens"] != float64(12) {
		t.Fatalf("expected usage to be preserved on completion, got %#v", usage)
	}
	outputDetails := usage["output_tokens_details"].(map[string]any)
	if outputDetails["reasoning_tokens"] != float64(3) {
		t.Fatalf("expected reasoning tokens in usage details, got %#v", usage)
	}
}

func TestMessagesStreamingConverterPropagatesCacheReadTokens(t *testing.T) {
	converter := NewMessagesStreamingConverter(map[string]any{"model": "claude-4", "input": "hi"})
	events := converter.Feed([]byte("event: message_delta\ndata: " + mustJSON(map[string]any{
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{
			"input_tokens":            20,
			"output_tokens":           3,
			"total_tokens":            23,
			"cache_read_input_tokens": 12,
		},
	}) + "\n\n"))
	events = append(events, converter.Feed([]byte("event: message_stop\ndata: {}\n\n"))...)

	completed := findPayload(ssePayloads(events), "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", events)
	}
	response := completed["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != float64(12) {
		t.Fatalf("expected cached_tokens=12, got %#v", usage)
	}
}

func TestMessagesStreamingConverterDoesNotTreatCacheCreationAsCacheRead(t *testing.T) {
	converter := NewMessagesStreamingConverter(map[string]any{"model": "claude-4", "input": "hi"})
	events := converter.Feed([]byte("event: message_delta\ndata: " + mustJSON(map[string]any{
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{
			"input_tokens":                20,
			"output_tokens":               3,
			"total_tokens":                23,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 20,
		},
	}) + "\n\n"))
	events = append(events, converter.Feed([]byte("event: message_stop\ndata: {}\n\n"))...)

	completed := findPayload(ssePayloads(events), "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", events)
	}
	response := completed["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != float64(0) {
		t.Fatalf("cache creation must not count as cache read, got %#v", usage)
	}
}

func TestMessagesStreamEOFWithoutStopReasonProducesIncompleteOrFailed(t *testing.T) {
	t.Run("substantive output becomes incomplete", func(t *testing.T) {
		converter := NewMessagesStreamingConverter(map[string]any{"model": "claude-4"})
		events := converter.Feed([]byte("event: message_start\ndata: " + mustJSON(map[string]any{
			"message": map[string]any{"id": "msg-123", "model": "claude-4", "role": "assistant"},
		}) + "\n\n"))
		events = append(events, converter.Feed([]byte("event: content_block_delta\ndata: "+mustJSON(map[string]any{
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "Hello"},
		})+"\n\n"))...)
		events = append(events, converter.Finish()...)

		completed := findPayload(ssePayloads(events), "response.completed")
		if completed == nil {
			t.Fatalf("missing response.completed payloads=%#v", events)
		}
		response := completed["response"].(map[string]any)
		if response["status"] != "incomplete" {
			t.Fatalf("expected incomplete status, got %#v", response["status"])
		}
	})

	t.Run("no output becomes failed", func(t *testing.T) {
		converter := NewMessagesStreamingConverter(map[string]any{"model": "claude-4"})
		events := converter.Finish()
		completed := findPayload(ssePayloads(events), "response.completed")
		if completed == nil {
			t.Fatalf("missing response.completed payloads=%#v", events)
		}
		response := completed["response"].(map[string]any)
		if response["status"] != "failed" {
			t.Fatalf("expected failed status, got %#v", response["status"])
		}
	})
}

func TestMessagesStreamExplicitStopReasonProducesCompleted(t *testing.T) {
	converter := NewMessagesStreamingConverter(map[string]any{"model": "claude-4"})
	events := converter.Feed([]byte("event: message_start\ndata: " + mustJSON(map[string]any{
		"message": map[string]any{"id": "msg-123", "model": "claude-4", "role": "assistant"},
	}) + "\n\n"))
	events = append(events, converter.Feed([]byte("event: content_block_delta\ndata: "+mustJSON(map[string]any{
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "Hello"},
	})+"\n\n"))...)
	events = append(events, converter.Feed([]byte("event: message_delta\ndata: "+mustJSON(map[string]any{
		"delta": map[string]any{"stop_reason": "end_turn"},
	})+"\n\n"))...)
	events = append(events, converter.Feed([]byte("event: message_stop\ndata: {}\n\n"))...)

	completed := findPayload(ssePayloads(events), "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", events)
	}
	response := completed["response"].(map[string]any)
	if response["status"] != "completed" {
		t.Fatalf("expected completed status, got %#v", response["status"])
	}
}

func TestConvertMessagesStreamToResponsesSSEOutputIndexesMatchCompletedOutput(t *testing.T) {
	converter := NewMessagesStreamingConverter(map[string]any{"model": "claude-4"})
	events := []string{
		"event: message_start\ndata: " + mustJSON(map[string]any{
			"message": map[string]any{"id": "msg-123", "model": "claude-4", "role": "assistant"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "reasoning"},
		}) + "\n\n",
		"event: content_block_start\ndata: " + mustJSON(map[string]any{
			"index":         1,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "shell"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": "{\"command\":\"pwd\"}"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 2,
			"delta": map[string]any{"type": "text_delta", "text": "Hello"},
		}) + "\n\n",
		"event: message_delta\ndata: " + mustJSON(map[string]any{
			"delta": map[string]any{"stop_reason": "end_turn"},
		}) + "\n\n",
		"event: message_stop\ndata: {}\n\n",
	}

	var out []string
	for _, event := range events {
		out = append(out, converter.Feed([]byte(event))...)
	}
	payloads := ssePayloads(out)
	completed := findPayload(payloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", payloads)
	}

	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	indexByID := map[string]int{}
	for index, raw := range output {
		item := raw.(map[string]any)
		indexByID[stringValue(item["id"])] = index
	}

	for _, payload := range payloads {
		switch payload["type"] {
		case "response.reasoning_text.delta", "response.output_text.delta", "response.output_item.added", "response.function_call_arguments.delta":
			itemID := stringValue(payload["item_id"])
			if itemID == "" {
				if item, ok := payload["item"].(map[string]any); ok {
					itemID = stringValue(item["id"])
				}
			}
			if itemID == "" {
				t.Fatalf("missing item id on payload %#v", payload)
			}
			wantIndex, ok := indexByID[itemID]
			if !ok {
				t.Fatalf("completed output missing item %q in %#v", itemID, output)
			}
			if got := intValue(payload["output_index"]); got != wantIndex {
				t.Fatalf("payload index mismatch for %q: got %d want %d payload=%#v output=%#v", itemID, got, wantIndex, payload, output)
			}
		}
	}
}

func TestMessagesStreamReasoningMessageAndToolIndexesRemainStable(t *testing.T) {
	converter := NewMessagesStreamingConverter(map[string]any{"model": "claude-4"})
	events := []string{
		"event: message_start\ndata: " + mustJSON(map[string]any{
			"message": map[string]any{"id": "msg-123", "model": "claude-4", "role": "assistant"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "part1"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "part2"},
		}) + "\n\n",
		"event: content_block_start\ndata: " + mustJSON(map[string]any{
			"index":         1,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "shell"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": "{\"command\":\"pwd\"}"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 2,
			"delta": map[string]any{"type": "text_delta", "text": "Hello"},
		}) + "\n\n",
		"event: content_block_delta\ndata: " + mustJSON(map[string]any{
			"index": 2,
			"delta": map[string]any{"type": "text_delta", "text": " again"},
		}) + "\n\n",
		"event: message_delta\ndata: " + mustJSON(map[string]any{
			"delta": map[string]any{"stop_reason": "end_turn"},
		}) + "\n\n",
		"event: message_stop\ndata: {}\n\n",
	}

	var out []string
	for _, event := range events {
		out = append(out, converter.Feed([]byte(event))...)
	}

	payloads := ssePayloads(out)
	indexes := map[string]int{}
	for _, payload := range payloads {
		switch payload["type"] {
		case "response.reasoning_text.delta", "response.output_text.delta", "response.function_call_arguments.delta":
			itemID := stringValue(payload["item_id"])
			got := intValue(payload["output_index"])
			if want, ok := indexes[itemID]; ok && want != got {
				t.Fatalf("output_index changed for %q: got %d want %d payloads=%#v", itemID, got, want, payloads)
			}
			indexes[itemID] = got
		case "response.output_item.added":
			item := payload["item"].(map[string]any)
			itemID := stringValue(item["id"])
			got := intValue(payload["output_index"])
			if want, ok := indexes[itemID]; ok && want != got {
				t.Fatalf("output_index changed for %q: got %d want %d payloads=%#v", itemID, got, want, payloads)
			}
			indexes[itemID] = got
		}
	}

	completed := findPayload(payloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", payloads)
	}
	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	for index, raw := range output {
		item := raw.(map[string]any)
		itemID := stringValue(item["id"])
		if got, ok := indexes[itemID]; !ok || got != index {
			t.Fatalf("completed output index mismatch for %q: got %d want %d output=%#v indexes=%#v", itemID, got, index, output, indexes)
		}
	}
}
