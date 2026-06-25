package proxy

import (
	"encoding/json"
	"testing"
)

// ===========================================================================
// InjectCacheBreakpoints tests
// ===========================================================================

func TestInjectCacheBreakpointsEmptyBody(t *testing.T) {
	body := map[string]any{
		"model":    "test",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	original := toJSON(t, body)
	_ = InjectCacheBreakpoints(body, "1h")
	if toJSON(t, body) != original {
		t.Fatal("expected no injection into body without tools/system/assistant")
	}
}

func TestInjectCacheBreakpointsToolsOnly(t *testing.T) {
	body := map[string]any{
		"model": "test",
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "t1"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "t2"}},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	injected := InjectCacheBreakpoints(body, "1h")
	if injected != 1 {
		t.Fatalf("expected 1 injection, got %d", injected)
	}
	tools := body["tools"].([]any)
	last := tools[len(tools)-1].(map[string]any)
	cc, ok := last["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Fatalf("expected cache_control on last tool, got %v", last["cache_control"])
	}
}

func TestInjectCacheBreakpointsAllThreePositions(t *testing.T) {
	body := map[string]any{
		"model": "test",
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "t1"}},
		},
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "sys prompt"},
			}},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "response"},
			}},
		},
	}
	injected := InjectCacheBreakpoints(body, "1h")
	if injected != 3 {
		t.Fatalf("expected 3 injections, got %d", injected)
	}
	// Verify tools
	tools := body["tools"].([]any)
	if tools[0].(map[string]any)["cache_control"] == nil {
		t.Fatal("expected cache_control on tool")
	}
	// Verify system
	msgs := body["messages"].([]any)
	sysContent := msgs[0].(map[string]any)["content"].([]any)
	if sysContent[len(sysContent)-1].(map[string]any)["cache_control"] == nil {
		t.Fatal("expected cache_control on system last block")
	}
	// Verify assistant
	asstContent := msgs[2].(map[string]any)["content"].([]any)
	if asstContent[0].(map[string]any)["cache_control"] == nil {
		t.Fatal("expected cache_control on assistant last block")
	}
}

func TestInjectCacheBreakpointsSkipsThinkingBlocks(t *testing.T) {
	body := map[string]any{
		"model": "test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "hmm"},
				map[string]any{"type": "text", "text": "result"},
				map[string]any{"type": "redacted_thinking", "data": "xxx"},
			}},
		},
	}
	injected := InjectCacheBreakpoints(body, "1h")
	if injected != 1 {
		t.Fatalf("expected 1 injection (on text block), got %d", injected)
	}
	msgs := body["messages"].([]any)
	content := msgs[1].(map[string]any)["content"].([]any)
	// thinking block (index 0) -- no cache_control
	if content[0].(map[string]any)["cache_control"] != nil {
		t.Fatal("thinking block should not have cache_control")
	}
	// text block (index 1) -- should have cache_control
	if content[1].(map[string]any)["cache_control"] == nil {
		t.Fatal("text block should have cache_control")
	}
	// redacted_thinking block (index 2) -- no cache_control
	if content[2].(map[string]any)["cache_control"] != nil {
		t.Fatal("redacted_thinking block should not have cache_control")
	}
}

func TestInjectCacheBreakpointsMaxFourExisting(t *testing.T) {
	// 4 existing breakpoints -- should only upgrade TTL, not inject new ones
	body := map[string]any{
		"model": "test",
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "t1"}, "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "t2"}, "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
		},
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "sys", "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "ok", "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
			}},
		},
	}
	injected := InjectCacheBreakpoints(body, "1h")
	if injected != 0 {
		t.Fatalf("expected 0 new injections, got %d", injected)
	}
	// TTLs should be upgraded to 1h
	tools := body["tools"].([]any)
	if tools[0].(map[string]any)["cache_control"].(map[string]any)["ttl"] != "1h" {
		t.Fatal("expected TTL upgrade to 1h")
	}
}

// ===========================================================================
// ShouldRectifyThinkingSignature tests
// ===========================================================================

func TestShouldRectifyInvalidSignature(t *testing.T) {
	cases := []string{
		`Invalid 'signature' in 'thinking' block`,
		`invalid signature in thinking block`,
		`Messages.1.Content.0: Invalid signature in thinking block`,
	}
	for _, msg := range cases {
		if !ShouldRectifyThinkingSignature(msg) {
			t.Fatalf("should detect thinking signature error: %s", msg)
		}
	}
}

func TestShouldRectifyThoughtSignatureNotValid(t *testing.T) {
	if !ShouldRectifyThinkingSignature("Unable to submit request because Thought signature is not valid") {
		t.Fatal("should detect 'Thought signature is not valid'")
	}
}

func TestShouldRectifyMustStartWithThinking(t *testing.T) {
	if !ShouldRectifyThinkingSignature("a final assistant message must start with a thinking block") {
		t.Fatal("should detect 'must start with a thinking block'")
	}
}

func TestShouldRectifyExpectedThinkingFoundToolUse(t *testing.T) {
	if !ShouldRectifyThinkingSignature("Expected thinking or redacted_thinking, but found tool_use") {
		t.Fatal("should detect expected thinking, found tool_use")
	}
}

func TestShouldRectifySignatureFieldRequired(t *testing.T) {
	if !ShouldRectifyThinkingSignature("xxx.signature: Field required") {
		t.Fatal("should detect 'signature: Field required'")
	}
}

func TestShouldRectifyExtraInputsNotPermitted(t *testing.T) {
	if !ShouldRectifyThinkingSignature("xxx.signature: Extra inputs are not permitted") {
		t.Fatal("should detect 'signature: Extra inputs are not permitted'")
	}
}

func TestShouldRectifyCannotBeModified(t *testing.T) {
	if !ShouldRectifyThinkingSignature("thinking or redacted_thinking blocks cannot be modified") {
		t.Fatal("should detect 'cannot be modified'")
	}
}

func TestShouldRectifyInvalidRequest(t *testing.T) {
	if !ShouldRectifyThinkingSignature("invalid request: malformed JSON") {
		t.Fatal("should detect 'invalid request'")
	}
}

func TestShouldNotRectifyUnrelatedErrors(t *testing.T) {
	cases := []string{
		"Request timeout",
		"Connection refused",
		"rate limit exceeded",
		"",
	}
	for _, msg := range cases {
		if ShouldRectifyThinkingSignature(msg) {
			t.Fatalf("should NOT detect as thinking signature error: %s", msg)
		}
	}
}

// ===========================================================================
// StripThinkingBlocks tests
// ===========================================================================

func TestStripThinkingBlocksRemovesThinkingAndRedacted(t *testing.T) {
	body := map[string]any{
		"model": "test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "hmm", "signature": "sig1"},
				map[string]any{"type": "text", "text": "hello", "signature": "sig_text"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Test", "input": map[string]any{}},
				map[string]any{"type": "redacted_thinking", "data": "xxx", "signature": "sig2"},
			}},
		},
	}
	modified := StripThinkingBlocks(body)
	if !modified {
		t.Fatal("expected body to be modified")
	}
	content := body["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 remaining blocks (text + tool_use), got %d", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatal("expected text block first")
	}
	if content[1].(map[string]any)["type"] != "tool_use" {
		t.Fatal("expected tool_use block second")
	}
	// Signature should be removed from non-thinking blocks
	if content[0].(map[string]any)["signature"] != nil {
		t.Fatal("expected signature removed from text block")
	}
}

func TestStripThinkingBlocksRemovesTopLevelThinking(t *testing.T) {
	body := map[string]any{
		"model":    "test",
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Test", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
			}},
		},
	}
	modified := StripThinkingBlocks(body)
	if !modified {
		t.Fatal("expected body to be modified")
	}
	if body["thinking"] != nil {
		t.Fatal("expected top-level thinking to be removed")
	}
}

func TestStripThinkingBlocksNoChangeWhenClean(t *testing.T) {
	body := map[string]any{
		"model": "test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	modified := StripThinkingBlocks(body)
	if modified {
		t.Fatal("expected no modification for clean body")
	}
}

// ===========================================================================
// TryParseErrorBody tests
// ===========================================================================

func TestTryParseErrorBodyStandardOpenAI(t *testing.T) {
	raw := []byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	result := TryParseErrorBody(raw)
	if result != "Invalid API key" {
		t.Fatalf("expected 'Invalid API key', got %q", result)
	}
}

func TestTryParseErrorBodyMiniMax(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":2013,"status_msg":"invalid params"}}`)
	result := TryParseErrorBody(raw)
	if result != "invalid params" {
		t.Fatalf("expected 'invalid params', got %q", result)
	}
}

func TestTryParseErrorBodyPlainText(t *testing.T) {
	raw := []byte("Upstream timeout")
	result := TryParseErrorBody(raw)
	if result != "Upstream timeout" {
		t.Fatalf("expected 'Upstream timeout', got %q", result)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
