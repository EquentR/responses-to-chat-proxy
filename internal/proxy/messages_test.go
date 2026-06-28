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
	}, Config{})

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
	}, Config{})

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
	}, Config{})

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
	out = append(out, converter.Finish()...)

	joined := strings.Join(out, "\n")
	assertContains(t, joined, "response.created")
	assertContains(t, joined, "response.output_text.delta")
	assertContains(t, joined, "response.completed")
}
