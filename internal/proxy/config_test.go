package proxy

import (
	"testing"
	"time"
)

func TestParseUpstreamAPIKeysSplitsCommaAndNewline(t *testing.T) {
	got := parseUpstreamAPIKeys(" sk-a,sk-b\nsk-c\r\n, ")
	want := []string{"sk-a", "sk-b", "sk-c"}
	assertJSONEqual(t, want, got)
}

func TestLoadConfigFromEnvParsesUpstreamAPIKeysAndCooldown(t *testing.T) {
	t.Setenv("UPSTREAM_BASE_URL", "https://example.com/v1/")
	t.Setenv("UPSTREAM_API_KEY", "sk-a, sk-b")
	t.Setenv("UPSTREAM_KEY_COOLDOWN_SECONDS", "45")
	t.Setenv("REQUEST_TIMEOUT_SECONDS", "5")
	t.Setenv("STREAM_TIMEOUT_SECONDS", "5")

	cfg, err := LoadConfigFromEnv("")
	if err != nil {
		t.Fatalf("LoadConfigFromEnv returned error: %v", err)
	}

	if cfg.UpstreamBaseURL != "https://example.com/v1" {
		t.Fatalf("unexpected base URL: %q", cfg.UpstreamBaseURL)
	}
	if cfg.UpstreamAPIKey != "sk-a, sk-b" {
		t.Fatalf("single-value compatibility field changed: %q", cfg.UpstreamAPIKey)
	}
	assertJSONEqual(t, []string{"sk-a", "sk-b"}, cfg.UpstreamAPIKeys)
	if cfg.UpstreamKeyCooldownSeconds != 45 {
		t.Fatalf("unexpected cooldown seconds: %v", cfg.UpstreamKeyCooldownSeconds)
	}
	if cfg.UpstreamKeyCooldown != 45*time.Second {
		t.Fatalf("unexpected cooldown duration: %v", cfg.UpstreamKeyCooldown)
	}
}

func TestLoadConfigFromEnvDefaultsUpstreamKeyCooldown(t *testing.T) {
	t.Setenv("UPSTREAM_BASE_URL", "https://example.com/v1")
	t.Setenv("UPSTREAM_API_KEY", "")

	cfg, err := LoadConfigFromEnv("")
	if err != nil {
		t.Fatalf("LoadConfigFromEnv returned error: %v", err)
	}

	if cfg.UpstreamKeyCooldownSeconds != 30 {
		t.Fatalf("expected default cooldown seconds 30, got %v", cfg.UpstreamKeyCooldownSeconds)
	}
	if cfg.UpstreamKeyCooldown != 30*time.Second {
		t.Fatalf("expected default cooldown 30s, got %v", cfg.UpstreamKeyCooldown)
	}
}
