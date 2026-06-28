package proxy

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	UpstreamBaseURL       string
	UpstreamModelsURL     string
	UpstreamAPIKey        string
	ReasoningMode         ReasoningMode // reasoning parameter mapping mode
	RouteDetection        RouteDetectionMode
	RouteTableTTLSeconds  float64
	RouteTableTTL         time.Duration
	RouteTablePersist     bool
	RouteProbeGeneration  bool
	ProxyAPIKey           string
	ModelOverride         string
	Host                  string
	Port                  int
	RequestTimeout        time.Duration
	StreamTimeout         time.Duration
	VerifySSL             bool
	LogLevel              string
	CacheOptimizer        bool // inject cache_control breakpoints
	RequestTimeoutSeconds float64
	StreamTimeoutSeconds  float64
}

func LoadConfigFromEnv(dotEnvPath string) (Config, error) {
	if err := loadDotEnv(dotEnvPath); err != nil {
		return Config{}, err
	}

	cfg := Config{
		UpstreamBaseURL:       envString("UPSTREAM_BASE_URL", "https://api.openai.com/v1"),
		UpstreamModelsURL:     envString("UPSTREAM_MODELS_URL", ""),
		UpstreamAPIKey:        envString("UPSTREAM_API_KEY", ""),
		ProxyAPIKey:           envString("PROXY_API_KEY", ""),
		ModelOverride:         envString("MODEL_OVERRIDE", ""),
		Host:                  envString("HOST", "0.0.0.0"),
		Port:                  envInt("PORT", 8000),
		RequestTimeoutSeconds: envFloat("REQUEST_TIMEOUT_SECONDS", 120),
		StreamTimeoutSeconds:  envFloat("STREAM_TIMEOUT_SECONDS", 300),
		VerifySSL:             envBool("VERIFY_SSL", true),
		LogLevel:              envString("LOG_LEVEL", "info"),
		// ReasoningMode overrides model-name inference of the reasoning parameter
		// form sent upstream. Empty falls back to model/base-URL inference. Allowed:
		// effort, effort_obj, thinking, thinking_only, enable_thinking, reasoning_split.
		ReasoningMode:        ReasoningMode(envString("REASONING_MODE", "")),
		RouteDetection:       parseRouteDetectionMode(envString("ROUTE_DETECTION", string(RouteDetectionLazy))),
		RouteTableTTLSeconds: envFloat("ROUTE_TABLE_TTL_SECONDS", defaultRouteTableTTLSeconds),
		RouteTablePersist:    envBool("ROUTE_TABLE_PERSIST", false),
		RouteProbeGeneration: envBool("ROUTE_PROBE_GENERATION", defaultRouteProbeGeneration),
	}

	cfg.UpstreamBaseURL = normalizeUpstreamBaseURL(cfg.UpstreamBaseURL)
	cfg.RequestTimeout = secondsToDuration(cfg.RequestTimeoutSeconds)
	cfg.StreamTimeout = secondsToDuration(cfg.StreamTimeoutSeconds)
	cfg.RouteTableTTLSeconds = normalizeRouteTableTTLSeconds(cfg.RouteTableTTLSeconds)
	cfg.RouteTableTTL = secondsToDuration(cfg.RouteTableTTLSeconds)

	if cfg.UpstreamBaseURL == "" {
		return Config{}, errors.New("UPSTREAM_BASE_URL must not be empty")
	}

	return cfg, nil
}

func loadDotEnv(path string) error {
	if path == "" {
		return nil
	}

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}

		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}

	return scanner.Err()
}

func envString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(key string, fallback float64) float64 {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func secondsToDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func normalizeRouteTableTTLSeconds(seconds float64) float64 {
	if seconds <= 0 {
		return defaultRouteTableTTLSeconds
	}
	return seconds
}
