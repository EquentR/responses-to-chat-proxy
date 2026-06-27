package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestModelsDiscoveryParsesCommonMetadataShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want ModelDiscoveryResult
	}{
		{
			name: "openai-style data list",
			body: `{
				"object": "list",
				"data": [
					{
						"id": "gpt-4.1",
						"owned_by": "openai",
						"supported_endpoints": ["responses", "chat/completions"],
						"features": ["streaming", "tools"]
					}
				]
			}`,
			want: ModelDiscoveryResult{
				ModelID:    "gpt-4.1",
				Protocol:   RouteProtocolResponses,
				Endpoint:   "/v1/responses",
				Confidence: RouteConfidenceModelsMetadata,
				Features:   []string{"streaming", "tools"},
			},
		},
		{
			name: "single model object",
			body: `{
				"id": "claude-sonnet-4",
				"protocol": "messages",
				"endpoint": "/v1/messages",
				"capabilities": {
					"streaming": true,
					"vision": true
				}
			}`,
			want: ModelDiscoveryResult{
				ModelID:    "claude-sonnet-4",
				Protocol:   RouteProtocolMessages,
				Endpoint:   "/v1/messages",
				Confidence: RouteConfidenceExplicit,
				Features:   []string{"streaming", "vision"},
			},
		},
		{
			name: "direct array root",
			body: `[
				{
					"model": "mistral-small",
					"endpoints": [{"path": "chat/completions"}],
					"modalities": ["text", "image"]
				}
			]`,
			want: ModelDiscoveryResult{
				ModelID:    "mistral-small",
				Protocol:   RouteProtocolChat,
				Endpoint:   "/v1/chat/completions",
				Confidence: RouteConfidenceModelsMetadata,
				Features:   []string{"image", "text"},
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseModelDiscoveryResults([]byte(tt.body))
			if err != nil {
				t.Fatalf("ParseModelDiscoveryResults returned error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 model, got %d", len(got))
			}

			if got[0].ModelID != tt.want.ModelID {
				t.Fatalf("unexpected model id: got %q want %q", got[0].ModelID, tt.want.ModelID)
			}
			if got[0].Protocol != tt.want.Protocol {
				t.Fatalf("unexpected protocol: got %q want %q", got[0].Protocol, tt.want.Protocol)
			}
			if got[0].Endpoint != tt.want.Endpoint {
				t.Fatalf("unexpected endpoint: got %q want %q", got[0].Endpoint, tt.want.Endpoint)
			}
			if got[0].Confidence != tt.want.Confidence {
				t.Fatalf("unexpected confidence: got %q want %q", got[0].Confidence, tt.want.Confidence)
			}
			if strings.Join(got[0].Features, ",") != strings.Join(tt.want.Features, ",") {
				t.Fatalf("unexpected features: got %#v want %#v", got[0].Features, tt.want.Features)
			}
			if got[0].Raw == nil {
				t.Fatal("expected raw metadata to be preserved")
			}
		})
	}
}

func TestModelsDiscoveryFallsBackAcrossCandidates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.June, 27, 10, 0, 0, 0, time.UTC)
	routeTable := newRouteTableWithClock(time.Minute, func() time.Time { return now })

	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)

		switch r.URL.Path {
		case "/anthropic/v1/models":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
		case "/anthropic/models":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "still not here"})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{
					map[string]any{
						"id":                  "gpt-5.1",
						"supported_endpoints": []any{"responses"},
						"features":            []any{"streaming"},
					},
				},
			})
		default:
			t.Fatalf("unexpected candidate path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := Config{
		UpstreamBaseURL:   upstream.URL + "/anthropic",
		UpstreamAPIKey:    "upstream-secret",
		UpstreamModelsURL: "",
		RequestTimeout:    secondsToDuration(5),
		StreamTimeout:     secondsToDuration(5),
		VerifySSL:         true,
	}

	results, err := DiscoverModels(context.Background(), &http.Client{Timeout: 5 * time.Second}, cfg)
	if err != nil {
		t.Fatalf("DiscoverModels returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one discovered model, got %d", len(results))
	}
	if got := strings.Join(requests, " -> "); got != "/anthropic/v1/models -> /anthropic/models -> /v1/models" {
		t.Fatalf("unexpected candidate order: %s", got)
	}

	ApplyDiscoveredModels(routeTable, RouteIdentityKey(cfg.UpstreamBaseURL, cfg.UpstreamAPIKey), results)
	got, ok := routeTable.Resolve(RouteIdentityKey(cfg.UpstreamBaseURL, cfg.UpstreamAPIKey), "gpt-5.1")
	if !ok {
		t.Fatal("expected route table entry to be stored")
	}
	if got.Protocol != RouteProtocolResponses {
		t.Fatalf("unexpected stored protocol: %q", got.Protocol)
	}
	if got.Endpoint != "/v1/responses" {
		t.Fatalf("unexpected stored endpoint: %q", got.Endpoint)
	}
}

func TestModelsDiscoveryContinuesAfter501(t *testing.T) {
	t.Parallel()

	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)

		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not implemented"})
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{
					map[string]any{"id": "fallback-model", "protocol": "responses"},
				},
			})
		default:
			t.Fatalf("unexpected candidate path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := Config{
		UpstreamBaseURL:   upstream.URL,
		UpstreamAPIKey:    "upstream-secret",
		UpstreamModelsURL: "",
		RequestTimeout:    secondsToDuration(5),
		StreamTimeout:     secondsToDuration(5),
		VerifySSL:         true,
	}

	results, err := DiscoverModels(context.Background(), &http.Client{Timeout: 5 * time.Second}, cfg)
	if err != nil {
		t.Fatalf("DiscoverModels returned error: %v", err)
	}
	if len(results) != 1 || results[0].ModelID != "fallback-model" {
		t.Fatalf("unexpected discovery results: %#v", results)
	}
	if got := strings.Join(requests, " -> "); got != "/v1/models -> /models" {
		t.Fatalf("unexpected candidate order: %s", got)
	}
}

func TestModelsDiscoveryContinuesAfterEmptyResult(t *testing.T) {
	t.Parallel()

	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)

		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{
					map[string]any{
						"id":                  "fallback-empty-list-model",
						"supported_endpoints": []any{"responses"},
					},
				},
			})
		default:
			t.Fatalf("unexpected candidate path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := Config{
		UpstreamBaseURL:   upstream.URL,
		UpstreamAPIKey:    "upstream-secret",
		UpstreamModelsURL: "",
		RequestTimeout:    secondsToDuration(5),
		StreamTimeout:     secondsToDuration(5),
		VerifySSL:         true,
	}

	results, err := DiscoverModels(context.Background(), &http.Client{Timeout: 5 * time.Second}, cfg)
	if err != nil {
		t.Fatalf("DiscoverModels returned error: %v", err)
	}
	if len(results) != 1 || results[0].ModelID != "fallback-empty-list-model" {
		t.Fatalf("unexpected discovery results: %#v", results)
	}
	if got := strings.Join(requests, " -> "); got != "/v1/models -> /models" {
		t.Fatalf("unexpected candidate order: %s", got)
	}
}

func TestModelsDiscoveryInfersFallbackProtocolsWithoutMetadata(t *testing.T) {
	t.Parallel()

	body := `{
		"data": [
			{"id": "claude-4"},
			{"id": "gpt-4.1"},
			{"id": "o1-mini"},
			{"id": "local-model"}
		]
	}`

	results, err := ParseModelDiscoveryResults([]byte(body))
	if err != nil {
		t.Fatalf("ParseModelDiscoveryResults returned error: %v", err)
	}

	routeTable := newRouteTableWithClock(time.Minute, func() time.Time {
		return time.Date(2026, time.June, 27, 11, 0, 0, 0, time.UTC)
	})
	identity := RouteIdentityKey("https://upstream.example/v1", "api-key")
	ApplyDiscoveredModels(routeTable, identity, results)

	tests := []struct {
		modelID   string
		wantProto RouteProtocol
		wantEP    string
		wantConf  RouteConfidence
	}{
		{modelID: "claude-4", wantProto: RouteProtocolMessages, wantEP: "/v1/messages", wantConf: RouteConfidenceHeuristic},
		{modelID: "gpt-4.1", wantProto: RouteProtocolResponses, wantEP: "/v1/responses", wantConf: RouteConfidenceHeuristic},
		{modelID: "o1-mini", wantProto: RouteProtocolResponses, wantEP: "/v1/responses", wantConf: RouteConfidenceHeuristic},
		{modelID: "local-model", wantProto: RouteProtocolChat, wantEP: "/v1/chat/completions", wantConf: RouteConfidenceFallback},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			got, ok := routeTable.Resolve(identity, tt.modelID)
			if !ok {
				t.Fatalf("expected route for %s to be stored", tt.modelID)
			}
			if got.Protocol != tt.wantProto {
				t.Fatalf("unexpected protocol: got %q want %q", got.Protocol, tt.wantProto)
			}
			if got.Endpoint != tt.wantEP {
				t.Fatalf("unexpected endpoint: got %q want %q", got.Endpoint, tt.wantEP)
			}
			if got.Confidence != tt.wantConf {
				t.Fatalf("unexpected confidence: got %q want %q", got.Confidence, tt.wantConf)
			}
		})
	}
}

func TestModelsDiscoveryPrefersExplicitModelsURL(t *testing.T) {
	t.Parallel()

	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)

		switch r.URL.Path {
		case "/custom/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{
					map[string]any{"id": "explicit-model", "protocol": "responses"},
				},
			})
		case "/v1/models":
			t.Fatalf("fallback should not be reached after explicit models URL succeeds")
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := Config{
		UpstreamBaseURL:   upstream.URL,
		UpstreamModelsURL: upstream.URL + "/custom/models",
		UpstreamAPIKey:    "upstream-secret",
		RequestTimeout:    secondsToDuration(5),
		StreamTimeout:     secondsToDuration(5),
		VerifySSL:         true,
	}

	results, err := DiscoverModels(context.Background(), &http.Client{Timeout: 5 * time.Second}, cfg)
	if err != nil {
		t.Fatalf("DiscoverModels returned error: %v", err)
	}
	if len(results) != 1 || results[0].ModelID != "explicit-model" {
		t.Fatalf("unexpected discovery results: %#v", results)
	}
	if got := strings.Join(requests, " -> "); got != "/custom/models" {
		t.Fatalf("unexpected candidate order: %s", got)
	}
}

func TestModelsDiscoveryStopsOnAuthOrRateLimitErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, body: `{"error":{"message":"invalid api key"}}`},
		{name: "forbidden", statusCode: http.StatusForbidden, body: `{"error":{"message":"forbidden"}}`},
		{name: "rate_limited", statusCode: http.StatusTooManyRequests, body: `{"error":{"message":"too many requests"}}`},
		{name: "server_error", statusCode: http.StatusBadGateway, body: `{"error":{"message":"backend failed"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calledSecond bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					w.WriteHeader(tt.statusCode)
					_, _ = w.Write([]byte(tt.body))
				case "/models":
					calledSecond = true
					t.Fatalf("discovery should not continue after %d", tt.statusCode)
				default:
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
			}))
			defer upstream.Close()

			cfg := Config{
				UpstreamBaseURL: upstream.URL,
				UpstreamAPIKey:  "upstream-secret",
				RequestTimeout:  secondsToDuration(5),
				StreamTimeout:   secondsToDuration(5),
				VerifySSL:       true,
			}

			_, err := DiscoverModels(context.Background(), &http.Client{Timeout: 5 * time.Second}, cfg)
			if err == nil {
				t.Fatal("expected discovery to fail")
			}
			if strings.Contains(err.Error(), "invalid api key") || strings.Contains(err.Error(), "too many requests") || strings.Contains(err.Error(), "backend failed") {
				t.Fatalf("expected sanitized error, got %q", err.Error())
			}
			if calledSecond {
				t.Fatal("expected discovery to stop after terminal error")
			}
		})
	}
}

func TestModelsEndpointRefreshesRouteMetadata(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{
				map[string]any{
					"id":                  "gpt-5.1",
					"supported_endpoints": []any{"responses"},
					"features":            []any{"streaming", "tools"},
				},
			},
		})
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
		UpstreamAPIKey:  "upstream-secret",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}

	got, ok := server.routeTable.Resolve(RouteIdentityKey(server.config.UpstreamBaseURL, server.config.UpstreamAPIKey), "gpt-5.1")
	if !ok {
		t.Fatal("expected GET /v1/models to refresh route metadata")
	}
	if got.Protocol != RouteProtocolResponses {
		t.Fatalf("unexpected protocol: %q", got.Protocol)
	}
	if got.Endpoint != "/v1/responses" {
		t.Fatalf("unexpected endpoint: %q", got.Endpoint)
	}
}

func TestStartupDiscoveryIsOptional(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("startup discovery should not have contacted upstream when disabled")
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL,
		UpstreamAPIKey:  "upstream-secret",
		RouteDetection:  RouteDetectionOff,
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	if err := server.initializeStartupDiscovery(context.Background()); err != nil {
		t.Fatalf("initializeStartupDiscovery returned error: %v", err)
	}
}
