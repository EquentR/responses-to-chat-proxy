package proxy

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRouteIdentityKeyIsStable(t *testing.T) {
	baseURL := "https://example.com/v1/"
	apiKey := "sk-test-routing-secret"

	got1 := RouteIdentityKey(baseURL, apiKey)
	got2 := RouteIdentityKey(strings.TrimRight(baseURL, "/"), apiKey)

	if got1 != got2 {
		t.Fatalf("expected stable identity key, got %q and %q", got1, got2)
	}
	if strings.Contains(string(got1), apiKey) {
		t.Fatalf("identity key must not contain the raw API key, got %q", got1)
	}
}

func TestRouteIdentityKeyUsesDedicatedType(t *testing.T) {
	identityType := reflect.TypeOf(RouteIdentityKey("https://example.com/v1/", "sk-test-routing-secret"))
	if identityType.Kind() != reflect.String {
		t.Fatalf("expected RouteIdentityKey to return a string-like type, got %v", identityType)
	}
	if identityType.Name() == "string" || identityType.PkgPath() == "" {
		t.Fatalf("expected RouteIdentityKey to return a dedicated named type, got %v", identityType)
	}

	tableType := reflect.TypeOf(&RouteTable{})
	storeMethod, ok := tableType.MethodByName("Store")
	if !ok {
		t.Fatal("RouteTable.Store method not found")
	}
	resolveMethod, ok := tableType.MethodByName("Resolve")
	if !ok {
		t.Fatal("RouteTable.Resolve method not found")
	}

	if got := storeMethod.Type.In(1); got.Name() == "string" || got.PkgPath() == "" {
		t.Fatalf("expected RouteTable.Store identity argument to use a dedicated type, got %v", got)
	}
	if got := resolveMethod.Type.In(1); got.Name() == "string" || got.PkgPath() == "" {
		t.Fatalf("expected RouteTable.Resolve identity argument to use a dedicated type, got %v", got)
	}
	if storeMethod.Type.In(1) != resolveMethod.Type.In(1) {
		t.Fatalf("expected RouteTable Store and Resolve identity types to match, got %v and %v", storeMethod.Type.In(1), resolveMethod.Type.In(1))
	}
}

func TestRouteTableHonorsTTL(t *testing.T) {
	now := time.Date(2026, time.June, 27, 10, 0, 0, 0, time.UTC)
	table := newRouteTableWithClock(time.Minute, func() time.Time {
		return now
	})

	entry := RouteEntry{
		ModelID:    "gpt-5.1",
		Protocol:   RouteProtocolResponses,
		Endpoint:   "/v1/responses",
		Confidence: RouteConfidenceProbeSuccess,
		Features:   []string{"responses", "streaming"},
		Reasoning:  "detected from startup probe",
		LastError:  "",
	}

	identity := RouteIdentityKey("https://example.com/v1/", "sk-test-routing-secret")
	table.Store(identity, "gpt-5.1", entry)

	got, ok := table.Resolve(identity, "gpt-5.1")
	if !ok {
		t.Fatal("expected route entry to resolve before TTL expiry")
	}
	if got.ModelID != "gpt-5.1" || got.Protocol != RouteProtocolResponses || got.Endpoint != "/v1/responses" {
		t.Fatalf("unexpected stored entry: %#v", got)
	}
	if got.ExpiresAt.IsZero() || !got.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("unexpected expiry: got %v, want %v", got.ExpiresAt, now.Add(time.Minute))
	}
	if got.Confidence != RouteConfidenceProbeSuccess {
		t.Fatalf("unexpected confidence: got %q, want %q", got.Confidence, RouteConfidenceProbeSuccess)
	}

	now = now.Add(time.Minute + time.Second)
	if _, ok := table.Resolve(identity, "gpt-5.1"); ok {
		t.Fatal("expected route entry to expire after TTL")
	}
}

func TestRouteTableNormalizesNonPositiveTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		t.Run(ttl.String(), func(t *testing.T) {
			now := time.Date(2026, time.June, 27, 10, 0, 0, 0, time.UTC)
			table := newRouteTableWithClock(ttl, func() time.Time {
				return now
			})

			identity := RouteIdentityKey("https://example.com/v1/", "sk-test-routing-secret")
			entry := RouteEntry{
				ModelID:    "gpt-5.1",
				Protocol:   RouteProtocolResponses,
				Endpoint:   "/v1/responses",
				Confidence: RouteConfidenceProbeSuccess,
			}

			got := table.Store(identity, "gpt-5.1", entry)
			wantExpiry := now.Add(30 * time.Minute)
			if got.ExpiresAt.IsZero() {
				t.Fatal("expected route table to assign an expiry for non-positive TTL")
			}
			if !got.ExpiresAt.Equal(wantExpiry) {
				t.Fatalf("unexpected expiry: got %v, want %v", got.ExpiresAt, wantExpiry)
			}

			if _, ok := table.Resolve(identity, "gpt-5.1"); !ok {
				t.Fatal("expected route entry to resolve before normalized TTL expiry")
			}

			now = now.Add(30*time.Minute + time.Second)
			if _, ok := table.Resolve(identity, "gpt-5.1"); ok {
				t.Fatal("expected route entry to expire after normalized TTL")
			}
		})
	}
}

func TestRouteTablePreservesCategoricalConfidence(t *testing.T) {
	now := time.Date(2026, time.June, 27, 10, 0, 0, 0, time.UTC)
	table := newRouteTableWithClock(time.Minute, func() time.Time {
		return now
	})

	entry := RouteEntry{
		ModelID:    "gpt-5.1",
		Protocol:   RouteProtocolChat,
		Endpoint:   "/v1/chat/completions",
		Confidence: RouteConfidenceModelsMetadata,
		Features:   []string{"chat"},
		Reasoning:  "loaded from models metadata",
	}

	identity := RouteIdentityKey("https://example.com/v1/", "sk-test-routing-secret")
	table.Store(identity, "gpt-5.1", entry)

	got, ok := table.Resolve(identity, "gpt-5.1")
	if !ok {
		t.Fatal("expected route entry to resolve")
	}
	if got.Confidence != RouteConfidenceModelsMetadata {
		t.Fatalf("unexpected confidence: got %q, want %q", got.Confidence, RouteConfidenceModelsMetadata)
	}
}

func TestLoadConfigRouteSettings(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		unsetEnvKeys(t,
			"UPSTREAM_BASE_URL",
			"UPSTREAM_API_KEY",
			"PROXY_API_KEY",
			"MODEL_OVERRIDE",
			"HOST",
			"PORT",
			"REQUEST_TIMEOUT_SECONDS",
			"STREAM_TIMEOUT_SECONDS",
			"VERIFY_SSL",
			"LOG_LEVEL",
			"REASONING_MODE",
			"UPSTREAM_MODELS_URL",
			"ROUTE_DETECTION",
			"ROUTE_TABLE_TTL_SECONDS",
			"ROUTE_TABLE_PERSIST",
			"ROUTE_PROBE_GENERATION",
			"CACHE_OPTIMIZER",
			"CACHE_OPTIMIZER_TTL",
		)

		cfg, err := LoadConfigFromEnv("")
		if err != nil {
			t.Fatalf("LoadConfigFromEnv returned error: %v", err)
		}

		if cfg.UpstreamBaseURL != "https://api.openai.com/v1" {
			t.Fatalf("unexpected default upstream base URL: %q", cfg.UpstreamBaseURL)
		}
		if cfg.UpstreamModelsURL != "" {
			t.Fatalf("unexpected default upstream models URL: %q", cfg.UpstreamModelsURL)
		}
		if cfg.RouteDetection != RouteDetectionLazy {
			t.Fatalf("unexpected default route detection mode: %q", cfg.RouteDetection)
		}
		if cfg.RouteTableTTLSeconds != 1800 {
			t.Fatalf("unexpected default route TTL seconds: %v", cfg.RouteTableTTLSeconds)
		}
		if cfg.RouteTableTTL != 30*time.Minute {
			t.Fatalf("unexpected default route TTL duration: %v", cfg.RouteTableTTL)
		}
		if cfg.RouteTablePersist {
			t.Fatal("expected route table persistence to default to false")
		}
		if cfg.RouteProbeGeneration {
			t.Fatal("expected route probe generation to default to false")
		}
		if cfg.CacheOptimizer {
			t.Fatal("expected cache optimizer to default to false")
		}
		if cfg.CacheOptimizerTTL != "1h" {
			t.Fatalf("unexpected default cache optimizer TTL: %q", cfg.CacheOptimizerTTL)
		}
	})

	t.Run("parses route settings and preserves existing config behavior", func(t *testing.T) {
		t.Setenv("UPSTREAM_BASE_URL", "https://example.com/v1/")
		t.Setenv("UPSTREAM_API_KEY", "sk-test")
		t.Setenv("PROXY_API_KEY", "proxy-test")
		t.Setenv("MODEL_OVERRIDE", "gpt-4o-mini")
		t.Setenv("HOST", "127.0.0.1")
		t.Setenv("PORT", "9001")
		t.Setenv("REQUEST_TIMEOUT_SECONDS", "12")
		t.Setenv("STREAM_TIMEOUT_SECONDS", "34")
		t.Setenv("VERIFY_SSL", "false")
		t.Setenv("LOG_LEVEL", "debug")
		t.Setenv("REASONING_MODE", "thinking")
		t.Setenv("UPSTREAM_MODELS_URL", "https://example.com/v1/models")
		t.Setenv("ROUTE_DETECTION", "startup")
		t.Setenv("ROUTE_TABLE_TTL_SECONDS", "90")
		t.Setenv("ROUTE_TABLE_PERSIST", "true")
		t.Setenv("ROUTE_PROBE_GENERATION", "true")
		t.Setenv("CACHE_OPTIMIZER", "true")
		t.Setenv("CACHE_OPTIMIZER_TTL", "5m")

		cfg, err := LoadConfigFromEnv("")
		if err != nil {
			t.Fatalf("LoadConfigFromEnv returned error: %v", err)
		}

		if cfg.UpstreamBaseURL != "https://example.com/v1" {
			t.Fatalf("unexpected normalized upstream base URL: %q", cfg.UpstreamBaseURL)
		}
		if cfg.UpstreamAPIKey != "sk-test" || cfg.ProxyAPIKey != "proxy-test" || cfg.ModelOverride != "gpt-4o-mini" {
			t.Fatalf("unexpected preserved config fields: %#v", cfg)
		}
		if cfg.Host != "127.0.0.1" || cfg.Port != 9001 {
			t.Fatalf("unexpected host/port: %#v", cfg)
		}
		if cfg.RequestTimeoutSeconds != 12 || cfg.StreamTimeoutSeconds != 34 {
			t.Fatalf("unexpected timeout settings: %#v", cfg)
		}
		if cfg.RequestTimeout != 12*time.Second || cfg.StreamTimeout != 34*time.Second {
			t.Fatalf("unexpected timeout durations: %#v", cfg)
		}
		if cfg.VerifySSL {
			t.Fatal("expected VERIFY_SSL=false to be parsed")
		}
		if cfg.LogLevel != "debug" {
			t.Fatalf("unexpected log level: %q", cfg.LogLevel)
		}
		if cfg.ReasoningMode != ReasoningThinking {
			t.Fatalf("unexpected reasoning mode: %q", cfg.ReasoningMode)
		}

		if cfg.UpstreamModelsURL != "https://example.com/v1/models" {
			t.Fatalf("unexpected upstream models URL: %q", cfg.UpstreamModelsURL)
		}
		if cfg.RouteDetection != RouteDetectionStartup {
			t.Fatalf("unexpected route detection mode: %q", cfg.RouteDetection)
		}
		if cfg.RouteTableTTLSeconds != 90 {
			t.Fatalf("unexpected route TTL seconds: %v", cfg.RouteTableTTLSeconds)
		}
		if cfg.RouteTableTTL != 90*time.Second {
			t.Fatalf("unexpected route TTL duration: %v", cfg.RouteTableTTL)
		}
		if !cfg.RouteTablePersist {
			t.Fatal("expected route table persistence to parse true")
		}
		if !cfg.RouteProbeGeneration {
			t.Fatal("expected route probe generation to parse true")
		}
		if !cfg.CacheOptimizer {
			t.Fatal("expected cache optimizer to parse true")
		}
		if cfg.CacheOptimizerTTL != "5m" {
			t.Fatalf("unexpected cache optimizer TTL: %q", cfg.CacheOptimizerTTL)
		}
	})
}

func TestLoadConfigNormalizesNonPositiveRouteTableTTL(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ROUTE_TABLE_TTL_SECONDS", value)

			cfg, err := LoadConfigFromEnv("")
			if err != nil {
				t.Fatalf("LoadConfigFromEnv returned error: %v", err)
			}

			if cfg.RouteTableTTLSeconds != defaultRouteTableTTLSeconds {
				t.Fatalf("unexpected route TTL seconds: got %v, want %v", cfg.RouteTableTTLSeconds, defaultRouteTableTTLSeconds)
			}
			if cfg.RouteTableTTL != 30*time.Minute {
				t.Fatalf("unexpected route TTL duration: got %v, want %v", cfg.RouteTableTTL, 30*time.Minute)
			}
		})
	}
}

func TestLoadConfigCacheOptimizerSettings(t *testing.T) {
	t.Setenv("CACHE_OPTIMIZER", "true")
	t.Setenv("CACHE_OPTIMIZER_TTL", "5m")

	cfg, err := LoadConfigFromEnv("")
	if err != nil {
		t.Fatalf("LoadConfigFromEnv returned error: %v", err)
	}
	if !cfg.CacheOptimizer {
		t.Fatal("expected cache optimizer to parse true")
	}
	if cfg.CacheOptimizerTTL != "5m" {
		t.Fatalf("unexpected cache optimizer TTL: %q", cfg.CacheOptimizerTTL)
	}
}

func unsetEnvKeys(t *testing.T, keys ...string) {
	t.Helper()

	original := make(map[string]*string, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			v := value
			original[key] = &v
		} else {
			original[key] = nil
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv(%s) returned error: %v", key, err)
		}
	}

	t.Cleanup(func() {
		for _, key := range keys {
			if value := original[key]; value != nil {
				_ = os.Setenv(key, *value)
				continue
			}
			_ = os.Unsetenv(key)
		}
	})
}
