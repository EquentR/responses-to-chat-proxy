package proxy

import "testing"

func TestStickyRequestKeyUsesExplicitMetadataStickyKey(t *testing.T) {
	key := stickyRequestKey(map[string]any{
		"model": "test-model",
		"metadata": map[string]any{
			"sticky_key": "tenant-a/session-1",
		},
		"input": "hello",
	})

	if key == "" {
		t.Fatal("expected explicit metadata sticky key")
	}
	same := stickyRequestKey(map[string]any{
		"model": "test-model",
		"metadata": map[string]any{
			"sticky_key": "tenant-a/session-1",
		},
		"input": "different tail",
	})
	if key != same {
		t.Fatalf("expected explicit sticky key to ignore request tail, got %q then %q", key, same)
	}
}

func TestStickyRequestKeyUsesStablePromptPrefix(t *testing.T) {
	first := stickyRequestKey(map[string]any{
		"model":        "test-model",
		"instructions": "You are concise.",
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "lookup",
					"description": "Lookup records.",
				},
			},
		},
		"input": []any{
			map[string]any{"role": "user", "content": "Earlier question"},
			map[string]any{"role": "assistant", "content": "Earlier answer"},
			map[string]any{"role": "user", "content": "Current question A"},
		},
	})
	second := stickyRequestKey(map[string]any{
		"model":        "test-model",
		"instructions": "You are concise.",
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "lookup",
					"description": "Lookup records.",
				},
			},
		},
		"input": []any{
			map[string]any{"role": "user", "content": "Earlier question"},
			map[string]any{"role": "assistant", "content": "Earlier answer"},
			map[string]any{"role": "user", "content": "Current question B"},
		},
	})

	if first == "" || second == "" {
		t.Fatalf("expected stable prompt prefix sticky keys, got %q and %q", first, second)
	}
	if first != second {
		t.Fatalf("expected current tail to be excluded from sticky key, got %q then %q", first, second)
	}
}

func TestStickyRequestKeyDoesNotStickShortStatelessPrompt(t *testing.T) {
	key := stickyRequestKey(map[string]any{
		"model": "test-model",
		"input": "hello",
	})

	if key != "" {
		t.Fatalf("expected short stateless prompt to stay non-sticky, got %q", key)
	}
}
