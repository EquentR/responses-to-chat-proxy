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
