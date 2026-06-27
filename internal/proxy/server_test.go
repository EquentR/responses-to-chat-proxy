package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthEndpoint(t *testing.T) {
	server := NewServer(Config{})
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
	assertContains(t, recorder.Body.String(), `"status":"ok"`)
}

func TestResponsesEndpointConvertsUpstreamResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != ("Bearer " + "upstream-secret") {
			t.Fatalf("unexpected authorization: %s", r.Header.Get("Authorization"))
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
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

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	if payload["id"] != "resp-abc" {
		t.Fatalf("unexpected response id: %v", payload["id"])
	}
}

func TestResponsesEndpointForwardsCallerAuthWhenUpstreamKeyMissing(t *testing.T) {
	tests := []struct {
		name        string
		headerName  string
		headerValue string
	}{
		{name: "authorization", headerName: "Authorization", headerValue: "Bearer " + "test"},
		{name: "x-api-key", headerName: "x-api-key", headerValue: "caller-secret"},
		{name: "x-goog-api-key", headerName: "x-goog-api-key", headerValue: "caller-secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get(tt.headerName); got != tt.headerValue {
					t.Fatalf("unexpected %s header: %q", tt.headerName, got)
				}

				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":      "chatcmpl-abc",
					"created": 123,
					"model":   "test-model",
					"choices": []any{
						map[string]any{
							"finish_reason": "stop",
							"message":       map[string]any{"role": "assistant", "content": "Hi"},
						},
					},
				})
			}))
			defer upstream.Close()

			server := NewServer(Config{
				UpstreamBaseURL: upstream.URL + "/v1",
				RequestTimeout:  secondsToDuration(5),
				StreamTimeout:   secondsToDuration(5),
				VerifySSL:       true,
			})

			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(tt.headerName, tt.headerValue)
			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestForwardUnknownV1Request(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "test-model"}}})
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
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
	assertContains(t, recorder.Body.String(), `"id":"test-model"`)
}

func TestProxyAuthorization(t *testing.T) {
	server := NewServer(Config{
		ProxyAPIKey:    "proxy-secret",
		RequestTimeout: secondsToDuration(5),
		StreamTimeout:  secondsToDuration(5),
		VerifySSL:      true,
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
	assertContains(t, recorder.Body.String(), `"code":"invalid_api_key"`)
}

func TestForwardUnknownV1SkipsProxyAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "m1"}}})
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
		ProxyAPIKey:     "proxy-secret",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 but got %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), `"id":"m1"`)
}

func TestModelsEndpointDoesNotRefreshRouteSnapshotWithoutProxyAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{
				map[string]any{
					"id":                  "public-model",
					"supported_endpoints": []any{"responses"},
				},
			},
		})
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
		ProxyAPIKey:     "proxy-secret",
		UpstreamAPIKey:  "upstream-secret",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	identity := RouteIdentityKey(server.config.UpstreamBaseURL, server.config.UpstreamAPIKey)
	server.routeTable.Store(identity, "legacy-model", RouteEntry{
		ModelID:    "legacy-model",
		Protocol:   RouteProtocolChat,
		Endpoint:   "/v1/chat/completions",
		Confidence: RouteConfidenceExplicit,
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), `"id":"public-model"`)

	if _, ok := server.routeTable.Resolve(identity, "legacy-model"); !ok {
		t.Fatal("expected route snapshot to remain unchanged for unauthorized request")
	}
	if _, ok := server.routeTable.Resolve(identity, "public-model"); ok {
		t.Fatal("expected unauthorized request to skip shared route refresh")
	}
}

func TestModelsEndpointKeepsLegacyRouteWhenDiscoveryReturnsOnlyEmptyCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fallthrough
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
		ProxyAPIKey:     "proxy-secret",
		UpstreamAPIKey:  "upstream-secret",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	identity := RouteIdentityKey(server.config.UpstreamBaseURL, server.config.UpstreamAPIKey)
	server.routeTable.Store(identity, "legacy-model", RouteEntry{
		ModelID:    "legacy-model",
		Protocol:   RouteProtocolChat,
		Endpoint:   "/v1/chat/completions",
		Confidence: RouteConfidenceExplicit,
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer proxy-secret")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code == http.StatusOK {
		t.Fatalf("expected discovery failure, got 200 body=%s", recorder.Body.String())
	}
	if _, ok := server.routeTable.Resolve(identity, "legacy-model"); !ok {
		t.Fatal("expected legacy route to remain when discovery returns no models")
	}
}

func TestModelsEndpointFailsClosedOnErrorPayloadWithEmptyData(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anthropic/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"error":{"message":"bad"},"data":[]}`))
		case "/anthropic/models":
			t.Fatal("discovery should not continue after an error payload with empty data")
		case "/v1/models":
			t.Fatal("discovery should not continue after an error payload with empty data")
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/anthropic",
		ProxyAPIKey:     "proxy-secret",
		UpstreamAPIKey:  "upstream-secret",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	identity := RouteIdentityKey(server.config.UpstreamBaseURL, server.config.UpstreamAPIKey)
	server.routeTable.Store(identity, "legacy-model", RouteEntry{
		ModelID:    "legacy-model",
		Protocol:   RouteProtocolChat,
		Endpoint:   "/v1/chat/completions",
		Confidence: RouteConfidenceExplicit,
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer proxy-secret")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, strings.ToLower(recorder.Body.String()), "unrecognized")

	if _, ok := server.routeTable.Resolve(identity, "legacy-model"); !ok {
		t.Fatal("expected route snapshot to remain unchanged after discovery failure")
	}
	if _, ok := server.routeTable.Resolve(identity, "bad"); ok {
		t.Fatal("expected failed discovery to avoid replacing the route snapshot")
	}
}

func TestModelsEndpointFailsClosedOnMixedValidAndMalformedCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"data": [
					{
						"id": "public-model",
						"supported_endpoints": ["responses"]
					},
					{
						"supported_endpoints": ["responses"],
						"features": ["streaming"]
					}
				]
			}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
		ProxyAPIKey:     "proxy-secret",
		UpstreamAPIKey:  "upstream-secret",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	identity := RouteIdentityKey(server.config.UpstreamBaseURL, server.config.UpstreamAPIKey)
	server.routeTable.Store(identity, "legacy-model", RouteEntry{
		ModelID:    "legacy-model",
		Protocol:   RouteProtocolChat,
		Endpoint:   "/v1/chat/completions",
		Confidence: RouteConfidenceExplicit,
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer proxy-secret")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected mixed catalog to fail closed, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, strings.ToLower(recorder.Body.String()), "unrecognized")

	if _, ok := server.routeTable.Resolve(identity, "legacy-model"); !ok {
		t.Fatal("expected legacy route to remain after mixed catalog failure")
	}
	if _, ok := server.routeTable.Resolve(identity, "public-model"); ok {
		t.Fatal("expected malformed catalog to avoid refreshing new routes")
	}
}

func TestChatCompletionsStreamSynthesizesFinishReasonOnEOF(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + mustJSON(map[string]any{
			"id":      "chatcmpl-abc",
			"object":  "chat.completion.chunk",
			"created": 123,
			"model":   "test-model",
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{"role": "assistant", "content": "Hi"},
					"finish_reason": nil,
				},
			},
		}) + "\n\n"))
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	assertContains(t, body, `"finish_reason":"stop"`)
	assertContains(t, body, "data: [DONE]")
}

func TestChatCompletionsStreamInsertsFinishReasonBeforeDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + mustJSON(map[string]any{
			"id":      "chatcmpl-abc",
			"object":  "chat.completion.chunk",
			"created": 123,
			"model":   "test-model",
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{"content": "Hi"},
					"finish_reason": nil,
				},
			},
		}) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	finishIndex := strings.Index(body, `"finish_reason":"stop"`)
	doneIndex := strings.Index(body, "data: [DONE]")
	if finishIndex < 0 || doneIndex < 0 || finishIndex > doneIndex {
		t.Fatalf("expected synthesized finish_reason before [DONE], body=%s", body)
	}
}
