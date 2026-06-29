package proxy

import (
	"encoding/json"
	"testing"
)

func TestConvertRequestStringInput(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model":             "gpt-5.1",
		"instructions":      "Be concise.",
		"input":             "Hello",
		"max_output_tokens": 20,
		"reasoning":         map[string]any{"effort": "low"},
	}, Config{})

	if converted["model"] != "gpt-5.1" {
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

func TestConvertRequestPreservesInputFile(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model": "test-model",
		"input": []any{
			map[string]any{
				"type":      "input_file",
				"file_id":   "file-123",
				"file_data": "Zm9v",
				"filename":  "notes.txt",
			},
		},
	}, Config{})

	want := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "file",
					"file": map[string]any{
						"file_id":   "file-123",
						"file_data": "Zm9v",
						"filename":  "notes.txt",
					},
				},
			},
		},
	}

	assertJSONEqual(t, want, converted["messages"])
}

func TestConvertRequestPreservesInputAudio(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model": "test-model",
		"input": []any{
			map[string]any{
				"type": "input_audio",
				"input_audio": map[string]any{
					"data":   "QUJD",
					"format": "mp3",
				},
			},
		},
	}, Config{})

	want := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "input_audio",
					"input_audio": map[string]any{
						"data":   "QUJD",
						"format": "mp3",
					},
				},
			},
		},
	}

	assertJSONEqual(t, want, converted["messages"])
}

func TestConvertRequestHandlesTopLevelInputItems(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model": "test-model",
		"input": []any{
			map[string]any{"type": "input_text", "text": "Hello"},
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
					"format": "wav",
				},
			},
		},
	}, Config{})

	want := []any{
		map[string]any{"role": "user", "content": "Hello"},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url":    "https://example.com/image.png",
						"detail": "high",
					},
				},
			},
		},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "file",
					"file": map[string]any{
						"file_id":   "file-123",
						"file_data": "Zm9v",
						"filename":  "notes.txt",
					},
				},
			},
		},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "input_audio",
					"input_audio": map[string]any{
						"data":   "QUJD",
						"format": "wav",
					},
				},
			},
		},
	}

	assertJSONEqual(t, want, converted["messages"])
}

func TestStreamingConverterKeepsToolContext(t *testing.T) {
	chatRequest := ConvertRequest(map[string]any{
		"model": "test-model",
		"input": "Use the tools.",
		"tools": []any{
			map[string]any{
				"type": "namespace",
				"name": "crm",
				"tools": []any{
					map[string]any{
						"type": "function",
						"name": "get_customer_profile",
						"parameters": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"customer_id": map[string]any{"type": "string"},
							},
							"required": []any{"customer_id"},
						},
					},
				},
			},
			map[string]any{"type": "custom", "name": "code_exec", "description": "Run code."},
			map[string]any{"type": "tool_search"},
		},
	}, Config{})

	toolNames := map[string]string{}
	for _, rawTool := range chatRequest["tools"].([]any) {
		tool := rawTool.(map[string]any)
		fn := tool["function"].(map[string]any)
		name := fn["name"].(string)
		switch name {
		case "tool_search":
			toolNames["tool_search"] = name
		case "code_exec":
			toolNames["custom"] = name
		default:
			toolNames["namespace"] = name
		}
	}

	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{
							"index": 0,
							"id":    "call_ns",
							"type":  "function",
							"function": map[string]any{
								"name":      toolNames["namespace"],
								"arguments": `{"customer_id":"c-1"}`,
							},
						},
						map[string]any{
							"index": 1,
							"id":    "call_ct",
							"type":  "function",
							"function": map[string]any{
								"name":      toolNames["custom"],
								"arguments": `{"input":"print(1)"}`,
							},
						},
						map[string]any{
							"index": 2,
							"id":    "call_ts",
							"type":  "function",
							"function": map[string]any{
								"name":      toolNames["tool_search"],
								"arguments": `{"query":"crm tools"}`,
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
			map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"},
		},
	}) + "\n\n"

	payloads := ssePayloads(converter.Feed([]byte(chunk)))
	completed := findPayload(payloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", payloads)
	}

	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("unexpected output count: %#v", output)
	}

	nsItem := output[0].(map[string]any)
	if nsItem["type"] != "function_call" || nsItem["namespace"] != "crm" || nsItem["name"] != "get_customer_profile" {
		t.Fatalf("unexpected namespace item: %#v", nsItem)
	}

	customItem := output[1].(map[string]any)
	if customItem["type"] != "custom_tool_call" || customItem["name"] != "code_exec" || customItem["input"] != "print(1)" {
		t.Fatalf("unexpected custom item: %#v", customItem)
	}

	toolSearchItem := output[2].(map[string]any)
	if toolSearchItem["type"] != "tool_search_call" || toolSearchItem["execution"] != "client" {
		t.Fatalf("unexpected tool search item: %#v", toolSearchItem)
	}
}

func TestChatStreamEOFWithoutFinishReasonProducesIncompleteOrFailed(t *testing.T) {
	withOutput := NewStreamingConverter()
	withOutputPayloads := ssePayloads(withOutput.Feed([]byte("data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta": map[string]any{"content": "Hello"},
			},
		},
	}) + "\n\n")))
	withOutputPayloads = append(withOutputPayloads, ssePayloads(withOutput.Finish())...)
	completed := findPayload(withOutputPayloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing completion payload for substantive output: %#v", withOutputPayloads)
	}
	if response := completed["response"].(map[string]any); response["status"] != "incomplete" {
		t.Fatalf("expected incomplete status, got %#v", response["status"])
	}

	empty := NewStreamingConverter()
	emptyPayloads := ssePayloads(empty.Finish())
	completed = findPayload(emptyPayloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing completion payload for empty stream: %#v", emptyPayloads)
	}
	if response := completed["response"].(map[string]any); response["status"] != "failed" {
		t.Fatalf("expected failed status, got %#v", response["status"])
	}
}

func TestConvertResponseEOFWithoutFinishReasonProducesIncompleteOrFailed(t *testing.T) {
	withOutput := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": nil,
				"message":       map[string]any{"role": "assistant", "content": "Hello"},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hi"})
	if withOutput["status"] != "incomplete" {
		t.Fatalf("expected incomplete with substantive output, got %#v", withOutput["status"])
	}
	withOutputItem := withOutput["output"].([]any)[0].(map[string]any)
	if withOutputItem["status"] == "in_progress" {
		t.Fatalf("final response output item must not remain in_progress: %#v", withOutputItem)
	}

	empty := ConvertResponse(map[string]any{
		"id":      "chatcmpl-empty",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": nil,
				"message":       map[string]any{"role": "assistant", "content": ""},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hi"})
	if empty["status"] != "failed" {
		t.Fatalf("expected failed without substantive output, got %#v", empty["status"])
	}
	if output := empty["output"].([]any); len(output) != 0 {
		t.Fatalf("failed response without substantive output should not include empty in-progress output: %#v", output)
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

func TestConvertResponsePropagatesCacheReadTokens(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-cache-read",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "Hi"},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     20,
			"completion_tokens": 3,
			"total_tokens":      23,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": 12,
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	usage := converted["usage"].(map[string]any)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != 12 {
		t.Fatalf("expected cached_tokens=12, got %#v", usage)
	}
}

func TestConvertResponseDoesNotTreatCacheCreationAsCacheRead(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-cache-creation",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "Hi"},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":               20,
			"completion_tokens":           3,
			"total_tokens":                23,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 9,
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	usage := converted["usage"].(map[string]any)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != 0 {
		t.Fatalf("expected cached_tokens=0, got %#v", usage)
	}
}

func TestReasoningExtractionCoversObjectShapeAndThinkTags(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role": "assistant",
					"reasoning": map[string]any{
						"summary": []any{
							map[string]any{"type": "summary_text", "text": "Object summary."},
						},
						"content": []any{
							map[string]any{"type": "reasoning_text", "text": "Object thought."},
						},
					},
					"content": "<think>Tagged thought.</think>Visible answer.",
				},
			},
		},
		"usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output := converted["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected output length: %#v", output)
	}

	reasoning := output[0].(map[string]any)
	summary := reasoning["summary"].([]any)
	summaryText := summary[0].(map[string]any)["text"]
	if summaryText != "Object summary.\nObject thought.\nTagged thought." {
		t.Fatalf("unexpected reasoning summary: %v", summaryText)
	}

	message := output[1].(map[string]any)
	content := message["content"].([]any)
	part := content[0].(map[string]any)
	if part["text"] != "Visible answer." {
		t.Fatalf("unexpected visible text: %v", part["text"])
	}
}

func TestConvertResponseReasoningContentPartDoesNotPolluteVisibleMessage(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "Hidden thought."}}},
						map[string]any{"type": "text", "text": "<thinking>Tagged part.</thinking>Visible answer."},
					},
				},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output := converted["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected output length: %#v", output)
	}

	reasoning := output[0].(map[string]any)
	summary := reasoning["summary"].([]any)
	if summary[0].(map[string]any)["text"] != "Hidden thought.\nTagged part." {
		t.Fatalf("unexpected reasoning summary: %#v", summary)
	}

	message := output[1].(map[string]any)
	content := message["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "Visible answer." {
		t.Fatalf("unexpected visible content: %#v", content)
	}
}

func TestConvertResponseStripsEmbeddedThinkBlocksFromTextAndOutputTextParts(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "text", "text": "before<think>hidden</think>after"},
						map[string]any{"type": "output_text", "text": "  <thinking>ghost</thinking>visible"},
					},
				},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output := converted["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected output length: %#v", output)
	}

	reasoning := output[0].(map[string]any)
	summary := reasoning["summary"].([]any)
	if got := summary[0].(map[string]any)["text"]; got != "hidden\nghost" {
		t.Fatalf("unexpected reasoning summary: %v", got)
	}

	message := output[1].(map[string]any)
	content := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("unexpected content length: %#v", content)
	}
	if got := content[0].(map[string]any)["text"]; got != "beforeafter" {
		t.Fatalf("unexpected visible text from text part: %v", got)
	}
	if got := content[1].(map[string]any)["text"]; got != "  visible" {
		t.Fatalf("unexpected visible text from output_text part: %v", got)
	}
}

func TestConvertResponseContentArraySharesThinkStateAcrossParts(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "text", "text": "Intro <think>hidden"},
						map[string]any{"type": "output_text", "text": "</think>\nAnswer"},
					},
				},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output := converted["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected output length: %#v", output)
	}

	reasoning := output[0].(map[string]any)
	summary := reasoning["summary"].([]any)
	if got := summary[0].(map[string]any)["text"]; got != "hidden" {
		t.Fatalf("unexpected reasoning summary: %v", got)
	}

	message := output[1].(map[string]any)
	content := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("unexpected content length: %#v", content)
	}
	if got := content[0].(map[string]any)["text"]; got != "Intro " {
		t.Fatalf("unexpected visible text in first part: %v", got)
	}
	if got := content[1].(map[string]any)["text"]; got != "Answer" {
		t.Fatalf("unexpected visible text in second part: %v", got)
	}
}

func TestConvertResponseContentArrayPreservesIncompleteThinkPrefix(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "output_text", "text": "Visible <thi"},
					},
				},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output := converted["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("unexpected output length: %#v", output)
	}

	message := output[0].(map[string]any)
	content := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("unexpected content length: %#v", content)
	}
	if got := content[0].(map[string]any)["text"]; got != "Visible " {
		t.Fatalf("unexpected first visible text: %v", got)
	}
	if got := content[1].(map[string]any)["text"]; got != "<thi" {
		t.Fatalf("unexpected flushed visible text: %v", got)
	}
}

func TestConvertResponseNormalizesGenericOutputPartsWithThinkTags(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "tool_result", "output": "Visible <think>hidden</think> done"},
					},
				},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output := converted["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected output length: %#v", output)
	}

	reasoning := output[0].(map[string]any)
	summary := reasoning["summary"].([]any)
	if got := summary[0].(map[string]any)["text"]; got != "hidden" {
		t.Fatalf("unexpected reasoning summary: %v", got)
	}

	message := output[1].(map[string]any)
	content := message["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("unexpected content length: %#v", content)
	}
	part := content[0].(map[string]any)
	if part["type"] != "output_text" {
		t.Fatalf("unexpected normalized part type: %#v", part)
	}
	if got := part["text"]; got != "Visible done" {
		t.Fatalf("unexpected visible text: %v", got)
	}
	if _, ok := part["output"]; ok {
		t.Fatalf("normalized output part should not keep raw output field: %#v", part)
	}
}

func TestConvertResponseNormalizesOutputTextPartsAndTrimsThinkSeparatorWhitespace(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantReasoning string
		wantVisible   string
	}{
		{
			name:          "think-newline",
			text:          "<think>Hidden</think>\nAnswer",
			wantReasoning: "Hidden",
			wantVisible:   "Answer",
		},
		{
			name:          "thinking-spaces",
			text:          "<thinking>Hidden</thinking>  Answer",
			wantReasoning: "Hidden",
			wantVisible:   "Answer",
		},
		{
			name:        "plain-output-text",
			text:        "Plain answer",
			wantVisible: "Plain answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			converted := ConvertResponse(map[string]any{
				"id":      "chatcmpl-abc",
				"created": 123,
				"model":   "test-model",
				"choices": []any{
					map[string]any{
						"finish_reason": "stop",
						"message": map[string]any{
							"role": "assistant",
							"content": []any{
								map[string]any{
									"type":        "output_text",
									"text":        tt.text,
									"annotations": []any{},
									"marker":      "raw",
								},
							},
						},
					},
				},
			}, map[string]any{"model": "test-model", "input": "Hello"})

			output := converted["output"].([]any)
			if tt.wantReasoning == "" {
				if len(output) != 1 {
					t.Fatalf("unexpected output length: %#v", output)
				}
			} else {
				if len(output) != 2 {
					t.Fatalf("unexpected output length: %#v", output)
				}
				reasoning := output[0].(map[string]any)
				summary := reasoning["summary"].([]any)
				if got := summary[0].(map[string]any)["text"]; got != tt.wantReasoning {
					t.Fatalf("unexpected reasoning summary: %v", got)
				}
			}

			messageIndex := len(output) - 1
			message := output[messageIndex].(map[string]any)
			content := message["content"].([]any)
			if len(content) != 1 {
				t.Fatalf("unexpected content length: %#v", content)
			}
			part := content[0].(map[string]any)
			if got := part["text"]; got != tt.wantVisible {
				t.Fatalf("unexpected visible text: %v", got)
			}
			if _, ok := part["marker"]; ok {
				t.Fatalf("output_text part should not be passed through raw: %#v", part)
			}
		})
	}
}

func TestConvertResponseThinkOnlyContentStaysInvisible(t *testing.T) {
	stringCase := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "<think>Hidden only.</think>"},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output := stringCase["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected string-case output length: %#v", output)
	}
	msg := output[1].(map[string]any)
	if content := msg["content"].([]any); len(content) != 0 {
		t.Fatalf("think-only string should not leak visible content: %#v", content)
	}

	arrayCase := ConvertResponse(map[string]any{
		"id":      "chatcmpl-def",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "text", "text": "<thinking>Hidden block.</thinking>"},
					},
				},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output = arrayCase["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected array-case output length: %#v", output)
	}
	msg = output[1].(map[string]any)
	if content := msg["content"].([]any); len(content) != 0 {
		t.Fatalf("think-only content part should not emit empty output_text: %#v", content)
	}
}

func TestStreamingConverterHandlesReasoningObjectDeltas(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"role":      "assistant",
					"reasoning": map[string]any{"content": []any{map[string]any{"type": "reasoning_text", "text": "First thought. "}}},
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
					"reasoning": map[string]any{"summary": []any{map[string]any{"type": "summary_text", "text": "Second thought."}}},
					"content":   "Visible answer.",
				},
				"finish_reason": nil,
			},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
		},
	}) + "\n\n"

	payloads := ssePayloads(converter.Feed([]byte(chunk)))
	completed := findPayload(payloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", payloads)
	}

	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected output count: %#v", output)
	}

	reasoning := output[0].(map[string]any)
	summary := reasoning["summary"].([]any)
	if summary[0].(map[string]any)["text"] != "First thought. Second thought." {
		t.Fatalf("unexpected reasoning summary: %#v", summary)
	}

	message := output[1].(map[string]any)
	content := message["content"].([]any)
	if content[0].(map[string]any)["text"] != "Visible answer." {
		t.Fatalf("unexpected visible text: %#v", content)
	}
}

func TestStreamingConverterReasoningUsesSummaryPartEventsAndOutputIndex(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta":         map[string]any{"role": "assistant", "content": "Visible answer."},
				"finish_reason": nil,
			},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta":         map[string]any{"reasoning_content": "Hidden thought."},
				"finish_reason": nil,
			},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
		},
	}) + "\n\n"

	payloads := ssePayloads(converter.Feed([]byte(chunk)))
	var summaryPartAdded map[string]any
	var reasoningDelta map[string]any
	for _, payload := range payloads {
		switch payload["type"] {
		case "response.reasoning_summary_part.added":
			summaryPartAdded = payload
		case "response.reasoning_summary_text.delta":
			reasoningDelta = payload
		}
	}
	if summaryPartAdded == nil {
		t.Fatalf("missing reasoning summary part payloads=%#v", payloads)
	}
	if reasoningDelta == nil {
		t.Fatalf("missing reasoning summary delta payloads=%#v", payloads)
	}
	if summaryPartAdded["item_id"] != "rs_abc" {
		t.Fatalf("expected reasoning item_id rs_abc, got %#v", summaryPartAdded["item_id"])
	}
	if summaryPartAdded["output_index"] != float64(1) {
		t.Fatalf("expected reasoning output_index 1, got %#v", summaryPartAdded["output_index"])
	}
	if summaryPartAdded["summary_index"] != float64(0) {
		t.Fatalf("expected reasoning summary_index 0, got %#v", summaryPartAdded["summary_index"])
	}
	part := summaryPartAdded["part"].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != "" {
		t.Fatalf("unexpected reasoning summary part: %#v", part)
	}
	if reasoningDelta["item_id"] != "rs_abc" {
		t.Fatalf("expected reasoning item_id rs_abc, got %#v", reasoningDelta["item_id"])
	}
	if reasoningDelta["output_index"] != float64(1) {
		t.Fatalf("expected reasoning output_index 1, got %#v", reasoningDelta["output_index"])
	}
	if reasoningDelta["summary_index"] != float64(0) {
		t.Fatalf("expected reasoning summary_index 0, got %#v", reasoningDelta["summary_index"])
	}
	if reasoningDelta["delta"] != "Hidden thought." {
		t.Fatalf("unexpected reasoning delta: %#v", reasoningDelta["delta"])
	}
	for _, payload := range payloads {
		if payload["type"] == "response.output_text.delta" && payload["delta"] == "" {
			t.Fatalf("unexpected empty visible delta: %#v", payload)
		}
	}
}

func TestStreamingConverterAppendsDoneSentinelAfterCompletion(t *testing.T) {
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
	}) + "\n\n" + "data: [DONE]\n\n"

	events := converter.Feed([]byte(chunk))
	if len(events) == 0 {
		t.Fatal("expected SSE events")
	}
	if got := events[len(events)-1]; got != "data: [DONE]\n\n" {
		t.Fatalf("expected final SSE sentinel, got %q", got)
	}

	payloads := ssePayloads(events)
	if completed := findPayload(payloads, "response.completed"); completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", payloads)
	}
}

func TestStreamingConverterAddsMonotonicSequenceNumbers(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"role":              "assistant",
					"reasoning_content": "Hidden thought.",
					"content":           "Visible answer.",
				},
				"finish_reason": nil,
			},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
		},
	}) + "\n\n"

	payloads := ssePayloads(converter.Feed([]byte(chunk)))
	if len(payloads) == 0 {
		t.Fatal("expected SSE payloads")
	}

	prev := -1
	for _, payload := range payloads {
		raw, ok := payload["sequence_number"]
		if !ok {
			t.Fatalf("missing sequence_number on payload %#v", payload)
		}
		current := intValue(raw)
		if current <= prev {
			t.Fatalf("expected monotonic sequence numbers, got prev=%d current=%d payloads=%#v", prev, current, payloads)
		}
		prev = current
	}
}

func TestStreamingConverterEmitsReasoningSummaryPartDone(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"role":              "assistant",
					"reasoning_content": "Hidden thought.",
				},
				"finish_reason": nil,
			},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
		},
	}) + "\n\n"

	payloads := ssePayloads(converter.Feed([]byte(chunk)))
	partDone := findPayload(payloads, "response.reasoning_summary_part.done")
	if partDone == nil {
		t.Fatalf("missing response.reasoning_summary_part.done payloads=%#v", payloads)
	}
	if partDone["summary_index"] != float64(0) {
		t.Fatalf("unexpected summary_index: %#v", partDone)
	}
	part := partDone["part"].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != "Hidden thought." {
		t.Fatalf("unexpected summary part done payload: %#v", partDone)
	}
}

func TestStreamingConverterEventOrderMatchesNativeResponsesShape(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"role":              "assistant",
					"reasoning_content": "Hidden thought.",
				},
				"finish_reason": nil,
			},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
		},
	}) + "\n\n"

	payloads := ssePayloads(converter.Feed([]byte(chunk)))
	var types []string
	for _, payload := range payloads {
		types = append(types, stringValue(payload["type"]))
	}

	textDone := indexOfString(types, "response.reasoning_summary_text.done")
	partDone := indexOfString(types, "response.reasoning_summary_part.done")
	itemDone := indexOfString(types, "response.output_item.done")
	completed := indexOfString(types, "response.completed")
	if textDone < 0 || partDone < 0 || itemDone < 0 || completed < 0 {
		t.Fatalf("missing expected reasoning lifecycle events: %#v", types)
	}
	if !(textDone < partDone && partDone < itemDone && itemDone < completed) {
		t.Fatalf("unexpected reasoning lifecycle order: %#v", types)
	}
}

func TestStreamingConverterStripsSplitThinkBlocksAcrossChunks(t *testing.T) {
	converter := NewStreamingConverter()
	first := ssePayloads(converter.Feed([]byte("data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta":         map[string]any{"role": "assistant", "content": "before<think>hidden"},
				"finish_reason": nil,
			},
		},
	}) + "\n\n")))
	second := ssePayloads(converter.Feed([]byte("data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta":         map[string]any{"content": "</think>after"},
				"finish_reason": "stop",
			},
		},
	}) + "\n\n")))

	payloads := append(first, second...)
	completed := findPayload(payloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", payloads)
	}

	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected output count: %#v", output)
	}

	reasoning := output[0].(map[string]any)
	summary := reasoning["summary"].([]any)
	if got := summary[0].(map[string]any)["text"]; got != "hidden" {
		t.Fatalf("unexpected reasoning summary: %v", got)
	}

	message := output[1].(map[string]any)
	content := message["content"].([]any)
	if got := content[0].(map[string]any)["text"]; got != "beforeafter" {
		t.Fatalf("unexpected visible text: %v", got)
	}
}

func TestStreamingConverterTrimsSeparatorWhitespaceAfterThinkClose(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantVisible string
	}{
		{
			name:        "think-newline",
			content:     "<think>Hidden</think>\nAnswer",
			wantVisible: "Answer",
		},
		{
			name:        "thinking-spaces",
			content:     "<thinking>Hidden</thinking>  Answer",
			wantVisible: "Answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			converter := NewStreamingConverter()
			chunk := "data: " + mustJSON(map[string]any{
				"id":      "chatcmpl-abc",
				"created": 123,
				"model":   "test-model",
				"choices": []any{
					map[string]any{
						"delta":         map[string]any{"role": "assistant", "content": tt.content},
						"finish_reason": nil,
					},
				},
			}) + "\n\n" + "data: " + mustJSON(map[string]any{
				"id":      "chatcmpl-abc",
				"created": 123,
				"model":   "test-model",
				"choices": []any{
					map[string]any{
						"delta":         map[string]any{},
						"finish_reason": "stop",
					},
				},
			}) + "\n\n"

			payloads := ssePayloads(converter.Feed([]byte(chunk)))
			var seenDelta string
			for _, payload := range payloads {
				if payload["type"] == "response.output_text.delta" {
					seenDelta = stringValue(payload["delta"])
				}
			}
			if seenDelta != tt.wantVisible {
				t.Fatalf("unexpected visible delta: %q", seenDelta)
			}

			completed := findPayload(payloads, "response.completed")
			if completed == nil {
				t.Fatalf("missing response.completed payloads=%#v", payloads)
			}
			response := completed["response"].(map[string]any)
			output := response["output"].([]any)
			if len(output) != 2 {
				t.Fatalf("unexpected output count: %#v", output)
			}
			message := output[1].(map[string]any)
			content := message["content"].([]any)
			if got := content[0].(map[string]any)["text"]; got != tt.wantVisible {
				t.Fatalf("unexpected completed visible text: %v", got)
			}
		})
	}
}

func TestStreamingConverterOrdersFinalDoneEventsAndCompletedOutputByOutputIndex(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"delta":         map[string]any{"role": "assistant", "content": "Visible answer."},
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
							"id":    "call_abc",
							"type":  "function",
							"function": map[string]any{
								"name":      "shell",
								"arguments": `{"command":"ls"}`,
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
				"delta":         map[string]any{"reasoning_content": "Hidden thought."},
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
	var doneIndexes []int
	for _, payload := range payloads {
		typ := stringValue(payload["type"])
		if len(typ) >= 5 && typ[len(typ)-5:] == ".done" {
			doneIndexes = append(doneIndexes, intValue(payload["output_index"]))
		}
	}
	wantDoneIndexes := []int{0, 0, 0, 1, 1, 2, 2, 2}
	if len(doneIndexes) != len(wantDoneIndexes) {
		t.Fatalf("unexpected done index count: got %v want %v", doneIndexes, wantDoneIndexes)
	}
	for i, want := range wantDoneIndexes {
		if doneIndexes[i] != want {
			t.Fatalf("unexpected done event order: got %v want %v", doneIndexes, wantDoneIndexes)
		}
	}

	completed := findPayload(payloads, "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed payloads=%#v", payloads)
	}
	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("unexpected output count: %#v", output)
	}
	wantTypes := []string{"message", "function_call", "reasoning"}
	for i, want := range wantTypes {
		item := output[i].(map[string]any)
		if got := stringValue(item["type"]); got != want {
			t.Fatalf("unexpected output order at %d: got %q want %q (output=%#v)", i, got, want, output)
		}
	}
}

func TestConvertResponseRestoresToolCallIDsFromResponseSuffixAndIndex(t *testing.T) {
	converted := ConvertResponse(map[string]any{
		"id":      "chatcmpl-abc",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"index": 0,
							"type":  "function",
							"function": map[string]any{
								"name":      "shell",
								"arguments": `{"command":"ls"}`,
							},
						},
						map[string]any{
							"index": 1,
							"type":  "function",
							"function": map[string]any{
								"name":      "shell",
								"arguments": `{"command":"pwd"}`,
							},
						},
					},
				},
			},
		},
	}, map[string]any{"model": "test-model", "input": "Hello"})

	output := converted["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("unexpected output length: %#v", output)
	}

	firstTool := output[1].(map[string]any)
	secondTool := output[2].(map[string]any)
	if got := firstTool["id"]; got != "fc_abc_0" {
		t.Fatalf("unexpected first fallback id: %v", got)
	}
	if got := secondTool["id"]; got != "fc_abc_1" {
		t.Fatalf("unexpected second fallback id: %v", got)
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

func TestConvertRequestPreservesCustomToolSchema(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model": "test-model",
		"input": "Use the custom tool.",
		"tools": []any{
			map[string]any{
				"type":        "custom",
				"name":        "code_exec",
				"description": "Run code in a sandbox.",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"language": map[string]any{"type": "string"},
						"code":     map[string]any{"type": "string"},
					},
					"required": []any{"language", "code"},
				},
			},
		},
	}, Config{})

	tools := converted["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	input := props[customToolInputField].(map[string]any)
	desc := input["description"].(string)

	for _, want := range []string{"Run code in a sandbox.", "input_schema", "language", "code"} {
		if !contains(desc, want) {
			t.Fatalf("custom tool input description should preserve %q, got %q", want, desc)
		}
	}
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

func TestStreamingConverterPropagatesCacheReadTokens(t *testing.T) {
	converter := NewStreamingConverter()
	chunk := "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-cache",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{"role": "assistant", "content": "Hi"}, "finish_reason": nil},
		},
	}) + "\n\n" + "data: " + mustJSON(map[string]any{
		"id":      "chatcmpl-cache",
		"created": 123,
		"model":   "test-model",
		"choices": []any{
			map[string]any{"delta": map[string]any{}, "finish_reason": "stop"},
		},
		"usage": map[string]any{
			"prompt_tokens":     20,
			"completion_tokens": 3,
			"total_tokens":      23,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": 12,
			},
		},
	}) + "\n\n"

	completed := findPayload(ssePayloads(converter.Feed([]byte(chunk))), "response.completed")
	if completed == nil {
		t.Fatal("expected response.completed event")
	}
	response := completed["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != float64(12) {
		t.Fatalf("expected cached_tokens=12, got %#v", usage)
	}
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

func findPayload(payloads []map[string]any, typ string) map[string]any {
	for _, payload := range payloads {
		if payload["type"] == typ {
			return payload
		}
	}
	return nil
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

func indexOfString(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && (haystack == needle || contains(haystack[1:], needle) || (len(haystack) >= len(needle) && haystack[:len(needle)] == needle)))
}

func TestInferReasoningMode(t *testing.T) {
	cases := []struct {
		model, baseURL string
		want           ReasoningMode
	}{
		{"gpt-5.1", "https://api.openai.com/v1", ReasoningEffort},
		{"o3-mini", "https://api.openai.com/v1", ReasoningEffort},
		{"deepseek-v4-pro", "https://api.deepseek.com/v1", ReasoningThinking},
		{"glm-5.2", "https://open.bigmodel.cn/api/v1", ReasoningThinkingOnly},
		{"kimi-k2", "https://api.moonshot.cn/v1", ReasoningThinkingOnly},
		{"qwen-max", "https://dashscope.aliyuncs.com/api/v1", ReasoningEnableThinking},
		{"MiniMax-M2.7", "https://api.minimaxi.com/v1", ReasoningSplit},
		{"glm-5.2", "https://openrouter.ai/api/v1", ReasoningEffortObj},
		{"some-custom-model", "https://my-third-party-proxy/v1", ReasoningPassthrough},
	}
	for _, tc := range cases {
		got := inferReasoningMode(tc.model, tc.baseURL)
		if got != tc.want {
			t.Errorf("inferReasoningMode(%q, %q) = %q, want %q", tc.model, tc.baseURL, got, tc.want)
		}
	}
}

func TestConvertRequestExplicitReasoningModeOverridesInference(t *testing.T) {
	// glm-5.2 would infer to thinking_only, but a custom upstream declares it
	// accepts a top-level reasoning_effort -> REASONING_MODE=effort must win.
	converted := ConvertRequest(map[string]any{
		"model":     "glm-5.2",
		"input":     "hi",
		"reasoning": map[string]any{"effort": "high"},
	}, Config{ReasoningMode: ReasoningEffort})

	if _, ok := converted["thinking"]; ok {
		t.Fatalf("explicit effort mode should not emit thinking, got %v", converted["thinking"])
	}
	if converted["reasoning_effort"] != "high" {
		t.Fatalf("expected reasoning_effort=high, got %v", converted["reasoning_effort"])
	}
}

func TestConvertRequestGLMInferredThinkingOnly(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model":     "glm-5.2",
		"input":     "hi",
		"reasoning": map[string]any{"effort": "high"},
	}, Config{})

	if _, ok := converted["reasoning_effort"]; ok {
		t.Fatalf("thinking_only must not emit top-level reasoning_effort, got %v", converted["reasoning_effort"])
	}
	if thinking, _ := converted["thinking"].(map[string]any); thinking["type"] != "enabled" {
		t.Fatalf("expected thinking.type=enabled, got %#v", converted["thinking"])
	}
}

func TestConvertRequestOpenRouterInferredEffortObj(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model":     "glm-5.2",
		"input":     "hi",
		"reasoning": map[string]any{"effort": "xhigh"},
	}, Config{UpstreamBaseURL: "https://openrouter.ai/api/v1"})

	reasoning, ok := converted["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("openrouter should emit reasoning.effort object, got %v", converted["reasoning"])
	}
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("expected reasoning.effort=xhigh, got %v", reasoning["effort"])
	}
}

func TestConvertRequestGlmpassthroughWhenUnknown(t *testing.T) {
	converted := ConvertRequest(map[string]any{
		"model":     "some-unknown-model",
		"input":     "hi",
		"reasoning": map[string]any{"effort": "high"},
	}, Config{UpstreamBaseURL: "https://my-proxy/v1"})

	if _, ok := converted["reasoning_effort"]; ok {
		t.Fatalf("passthrough should not emit reasoning_effort, got %v", converted["reasoning_effort"])
	}
	if _, ok := converted["thinking"]; ok {
		t.Fatalf("passthrough should not emit thinking, got %v", converted["thinking"])
	}
}
