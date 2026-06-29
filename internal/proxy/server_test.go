package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushRecorder) Flush() {
	r.flushes++
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type sequentialReadCloser struct {
	chunks [][]byte
	index  int
}

func (r *sequentialReadCloser) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

func (r *sequentialReadCloser) Close() error {
	return nil
}

func newRouteAwareTestServer(upstreamBaseURL, upstreamAPIKey string) *Server {
	return NewServer(Config{
		UpstreamBaseURL: upstreamBaseURL,
		UpstreamAPIKey:  upstreamAPIKey,
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})
}

func storeTestRoute(server *Server, model string, protocol RouteProtocol, endpoint string) {
	server.routeTable.Store(RouteIdentityKey(server.config.UpstreamBaseURL, server.config.UpstreamAPIKey), model, RouteEntry{
		ModelID:    model,
		Protocol:   protocol,
		Endpoint:   endpoint,
		Confidence: RouteConfidenceExplicit,
	})
}

func newMultiKeyRouteAwareTestServer(upstreamBaseURL string, protocol RouteProtocol) *Server {
	server := NewServer(Config{
		UpstreamBaseURL:     upstreamBaseURL,
		UpstreamAPIKey:      "sk-a,sk-b",
		UpstreamAPIKeys:     []string{"sk-a", "sk-b"},
		UpstreamKeyCooldown: secondsToDuration(30),
		RequestTimeout:      secondsToDuration(5),
		StreamTimeout:       secondsToDuration(5),
		VerifySSL:           true,
	})
	server.routeResolver = nil
	storeTestRoute(server, "test-model", protocol, defaultEndpointForProtocol(protocol))
	return server
}

type recordingResolver struct {
	route RouteEntry
	ok    bool
	err   error
	calls []resolverInvocation
}

type resolverInvocation struct {
	entrypoint RouteProtocol
	model      string
}

func (r *recordingResolver) ResolveRoute(_ context.Context, _ http.Header, model string, entrypoint RouteProtocol) (RouteEntry, bool, error) {
	r.calls = append(r.calls, resolverInvocation{entrypoint: entrypoint, model: model})
	if r.err != nil {
		return RouteEntry{}, false, r.err
	}
	return r.route, r.ok, nil
}

func TestPassthroughStreamRoutesRawResponsesSSEAndFlushesChunks(t *testing.T) {
	server := newRouteAwareTestServer("https://upstream.example/v1", "upstream-secret")
	storeTestRoute(server, "responses-model", RouteProtocolResponses, "/v1/responses")
	server.streamClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll request body returned error: %v", err)
		}
		if !strings.Contains(string(body), `"stream":true`) {
			t.Fatalf("expected stream request body, got %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &sequentialReadCloser{
				chunks: [][]byte{
					[]byte("data: {\"type\":\"response.created\"}\n\n"),
					[]byte("data: [DONE]\n\n"),
				},
			},
		}, nil
	})}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"responses-model","input":"hello","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("unexpected content type: %s body=%s", got, recorder.Body.String())
	}
	if recorder.flushes != 2 {
		t.Fatalf("expected per-chunk flushes, got %d body=%s", recorder.flushes, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), "data: {\"type\":\"response.created\"}")
	assertContains(t, recorder.Body.String(), "data: [DONE]")
}

func TestPassthroughStreamRoutesMessagesSSEAndHandlesUpstream4xx(t *testing.T) {
	t.Run("raw stream passes through", func(t *testing.T) {
		server := newRouteAwareTestServer("https://upstream.example/v1", "upstream-secret")
		storeTestRoute(server, "messages-model", RouteProtocolMessages, "/v1/messages")
		server.streamClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/v1/messages" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("ReadAll request body returned error: %v", err)
			}
			if !strings.Contains(string(body), `"stream":true`) {
				t.Fatalf("expected stream request body, got %s", body)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: &sequentialReadCloser{
					chunks: [][]byte{
						[]byte("data: {\"type\":\"message.delta\"}\n\n"),
					},
				},
			}, nil
		})}

		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"messages-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		request.Header.Set("Content-Type", "application/json")
		recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

		server.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
			t.Fatalf("unexpected content type: %s body=%s", got, recorder.Body.String())
		}
		if recorder.flushes != 1 {
			t.Fatalf("expected stream flushes, got %d body=%s", recorder.flushes, recorder.Body.String())
		}
		assertContains(t, recorder.Body.String(), "data: {\"type\":\"message.delta\"}")
	})

	t.Run("upstream 4xx becomes local sse error", func(t *testing.T) {
		server := newRouteAwareTestServer("https://upstream.example/v1", "upstream-secret")
		storeTestRoute(server, "messages-model", RouteProtocolMessages, "/v1/messages")
		server.streamClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/v1/messages" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("ReadAll request body returned error: %v", err)
			}
			if !strings.Contains(string(body), `"stream":true`) {
				t.Fatalf("expected stream request body, got %s", body)
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"nope"}}`)),
			}, nil
		})}

		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"messages-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		request.Header.Set("Content-Type", "application/json")
		recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

		server.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
			t.Fatalf("unexpected content type: %s body=%s", got, recorder.Body.String())
		}
		assertContains(t, recorder.Body.String(), "event: error")
		assertContains(t, recorder.Body.String(), "\"message\":\"nope\"")
		assertContains(t, recorder.Body.String(), "\"code\":\"http_400\"")
		assertContains(t, recorder.Body.String(), "data: [DONE]")
	})
}

func TestResponsesNativeStream4xxUsesSameTerminationContract(t *testing.T) {
	server := newRouteAwareTestServer("https://upstream.example/v1", "upstream-secret")
	storeTestRoute(server, "responses-model", RouteProtocolResponses, "/v1/responses")
	server.streamClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"responses nope"}}`)),
		}, nil
	})}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"responses-model","input":"hello","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), "event: error")
	assertContains(t, recorder.Body.String(), "\"message\":\"responses nope\"")
	assertContains(t, recorder.Body.String(), "data: [DONE]")
}

func TestJoinUpstreamEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		{
			name:     "base with version endpoint with version",
			baseURL:  "https://upstream.example/v1",
			endpoint: "/v1/responses",
			want:     "https://upstream.example/v1/responses",
		},
		{
			name:     "base without version endpoint with version",
			baseURL:  "https://upstream.example",
			endpoint: "/v1/responses",
			want:     "https://upstream.example/v1/responses",
		},
		{
			name:     "base without version endpoint without version",
			baseURL:  "https://upstream.example",
			endpoint: "/messages",
			want:     "https://upstream.example/messages",
		},
		{
			name:     "endpoint without leading slash",
			baseURL:  "https://upstream.example/v1",
			endpoint: "responses",
			want:     "https://upstream.example/v1/responses",
		},
		{
			name:     "endpoint with whitespace",
			baseURL:  " https://upstream.example/v1/ ",
			endpoint: "  /v1/messages  ",
			want:     "https://upstream.example/v1/messages",
		},
		{
			name:     "absolute endpoint",
			baseURL:  "https://upstream.example/v1",
			endpoint: "https://alt.example/v1/messages",
			want:     "https://alt.example/v1/messages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinUpstreamEndpoint(tt.baseURL, tt.endpoint); got != tt.want {
				t.Fatalf("unexpected endpoint: got %q want %q", got, tt.want)
			}
		})
	}
}

func assertJSONTextEqual(t *testing.T, wantRaw, gotRaw string) {
	t.Helper()

	var want any
	if err := json.Unmarshal([]byte(wantRaw), &want); err != nil {
		t.Fatalf("json.Unmarshal want returned error: %v", err)
	}
	var got any
	if err := json.Unmarshal([]byte(gotRaw), &got); err != nil {
		t.Fatalf("json.Unmarshal got returned error: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("unexpected JSON\nwant: %s\ngot:  %s", wantRaw, gotRaw)
	}
}

func assertErrorTypeAndCode(t *testing.T, raw []byte, wantType, wantCode string) {
	t.Helper()

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("json.Unmarshal error payload returned error: %v body=%s", err, raw)
	}
	errorBody, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected OpenAI-style error body, got %#v", payload)
	}
	if errorBody["type"] != wantType || errorBody["code"] != wantCode {
		t.Fatalf("unexpected error body: %#v", errorBody)
	}
}

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

func TestCopyHeadersDoesNotDuplicateContentType(t *testing.T) {
	incoming := http.Header{}
	incoming.Add("Content-Type", "application/json")
	incoming.Add("User-Agent", "codex-test")

	headers := copyHeaders(incoming, "")

	values := headers.Values("Content-Type")
	if len(values) != 1 || values[0] != "application/json" {
		t.Fatalf("unexpected content-type values: %#v", values)
	}
	if got := headers.Get("User-Agent"); got != "codex-test" {
		t.Fatalf("unexpected user agent: %q", got)
	}
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
		RouteDetection:  RouteDetectionOff,
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

func TestResponsesPassesThroughResponsesUpstream(t *testing.T) {
	var upstreamPath string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp-upstream",
			"object": "response",
			"model":  "responses-model",
			"output": []any{},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	storeTestRoute(server, "responses-model", RouteProtocolResponses, "/v1/responses")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"responses-model","input":"hello","metadata":{"keep":true}}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamPath != "/v1/responses" {
		t.Fatalf("unexpected upstream path: %s", upstreamPath)
	}
	if upstreamBody["input"] != "hello" {
		t.Fatalf("expected responses body to pass through unchanged, got %#v", upstreamBody)
	}
	if _, ok := upstreamBody["messages"]; ok {
		t.Fatalf("expected responses passthrough not to include chat messages, got %#v", upstreamBody)
	}
}

func TestResponsesRoutesToChatWhenChatOnly(t *testing.T) {
	var upstreamPath string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-route",
			"created": 123,
			"model":   "chat-model",
			"choices": []any{
				map[string]any{
					"finish_reason": "stop",
					"message":       map[string]any{"role": "assistant", "content": "Hi"},
				},
			},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	storeTestRoute(server, "chat-model", RouteProtocolChat, "/v1/chat/completions")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"chat-model","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamPath != "/v1/chat/completions" {
		t.Fatalf("unexpected upstream path: %s", upstreamPath)
	}
	if _, ok := upstreamBody["messages"]; !ok {
		t.Fatalf("expected responses request to convert to chat messages, got %#v", upstreamBody)
	}
	if _, ok := upstreamBody["input"]; ok {
		t.Fatalf("expected converted chat body not to include responses input, got %#v", upstreamBody)
	}
}

func TestLazyRouteDetectionResolvesResponsesBeforeFallback(t *testing.T) {
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp-upstream",
			"object": "response",
			"model":  "responses-model",
			"output": []any{},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	resolver := &recordingResolver{
		route: RouteEntry{
			ModelID:    "responses-model",
			Protocol:   RouteProtocolResponses,
			Endpoint:   "/v1/responses",
			Confidence: RouteConfidenceModelsMetadata,
		},
		ok: true,
	}
	server.routeResolver = resolver

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"responses-model","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamPath != "/v1/responses" {
		t.Fatalf("expected native responses passthrough after lazy resolution, got %q", upstreamPath)
	}
	if len(resolver.calls) != 1 || resolver.calls[0].entrypoint != RouteProtocolResponses {
		t.Fatalf("expected one responses resolver call, got %#v", resolver.calls)
	}
}

func TestLazyRouteDetectionResolvesMessagesBeforeFallback(t *testing.T) {
	var upstreamPath string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg-123",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "Hi"},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	resolver := &recordingResolver{
		route: RouteEntry{
			ModelID:    "messages-model",
			Protocol:   RouteProtocolMessages,
			Endpoint:   "/v1/messages",
			Confidence: RouteConfidenceModelsMetadata,
			Features:   []string{"thinking"},
		},
		ok: true,
	}
	server.routeResolver = resolver

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"messages-model","instructions":"Brief.","input":"hello","reasoning":{"effort":"high"}}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamPath != "/v1/messages" {
		t.Fatalf("expected messages route after lazy resolution, got %q", upstreamPath)
	}
	if upstreamBody["system"] != "Brief." {
		t.Fatalf("expected responses request to convert to messages body, got %#v", upstreamBody)
	}
	if _, ok := upstreamBody["messages"]; !ok {
		t.Fatalf("expected messages request body, got %#v", upstreamBody)
	}
	if len(resolver.calls) != 1 || resolver.calls[0].entrypoint != RouteProtocolResponses {
		t.Fatalf("expected one responses resolver call, got %#v", resolver.calls)
	}
}

func TestLazyRouteDetectionDoesNotCrossConvertControlledEntrypoints(t *testing.T) {
	server := newRouteAwareTestServer("https://upstream.example/v1", "upstream-secret")
	resolver := &recordingResolver{
		route: RouteEntry{
			ModelID:    "chat-model",
			Protocol:   RouteProtocolMessages,
			Endpoint:   "/v1/messages",
			Confidence: RouteConfidenceModelsMetadata,
		},
		ok: true,
	}
	server.routeResolver = resolver

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertErrorTypeAndCode(t, recorder.Body.Bytes(), "invalid_request_error", "unsupported_protocol")
	if len(resolver.calls) != 1 || resolver.calls[0].entrypoint != RouteProtocolChat {
		t.Fatalf("expected controlled chat entrypoint to resolve chat only, got %#v", resolver.calls)
	}
}

func TestLazyRouteDetectionPreservesAuthAndRateLimitErrors(t *testing.T) {
	for name, status := range map[string]int{
		"auth":       http.StatusUnauthorized,
		"rate_limit": http.StatusTooManyRequests,
	} {
		t.Run(name, func(t *testing.T) {
			server := newRouteAwareTestServer("https://upstream.example/v1", "upstream-secret")
			server.routeResolver = &recordingResolver{
				err: &modelDiscoveryError{
					statusCode: status,
					message:    sanitizedModelsDiscoveryError(status).Error(),
				},
			}

			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"unknown-model","input":"hello"}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, request)

			if recorder.Code != status {
				t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
			}
			assertContains(t, recorder.Body.String(), "models discovery failed")
		})
	}
}

func TestDefaultLazyRouteResolverDiscoversMessagesRouteFromModels(t *testing.T) {
	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{
					map[string]any{
						"id":                  "claude-4",
						"supported_endpoints": []any{"messages"},
						"features":            []any{"thinking"},
					},
				},
			})
		case "/v1/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   "msg-123",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "Hi"},
				},
				"stop_reason": "end_turn",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-4","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := strings.Join(requests, " -> "); got != "/v1/models -> /v1/messages" {
		t.Fatalf("unexpected request order: %s", got)
	}
}

func TestDefaultLazyRouteResolverSurfacesModelsRateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "slow down"},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-4","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), "models discovery failed")
}

func TestNonStreamingResponsesToChatUsesThinkingRectifier(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}
		messages, _ := body["messages"].([]any)
		lastMessage, _ := messages[len(messages)-1].(map[string]any)
		content, _ := lastMessage["content"].([]any)

		if callCount == 1 {
			if len(content) < 2 {
				t.Fatalf("expected initial request to include thinking and visible text, got %#v", lastMessage)
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "Expected thinking or redacted_thinking, but found tool_use",
					"type":    "invalid_request_error",
					"code":    "invalid_format",
				},
			})
			return
		}

		if len(content) != 1 {
			t.Fatalf("expected rectified retry to strip thinking blocks, got %#v", lastMessage)
		}
		if content[0].(map[string]any)["type"] != "text" {
			t.Fatalf("expected rectified retry to preserve visible text block, got %#v", content[0])
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-rectified",
			"created": 123,
			"model":   "chat-model",
			"choices": []any{
				map[string]any{
					"finish_reason": "stop",
					"message":       map[string]any{"role": "assistant", "content": "Hi"},
				},
			},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	storeTestRoute(server, "chat-model", RouteProtocolChat, "/v1/chat/completions")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"chat-model",
		"input":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"Hidden plan","signature":"sig_1"},
				{"type":"text","text":"Visible answer","signature":"sig_2"}
			]}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if callCount != 2 {
		t.Fatalf("expected one retry after rectification, got %d upstream calls", callCount)
	}
	assertContains(t, recorder.Body.String(), `"id":"resp-rectified"`)
}

func TestThinkingRectifierRetriesExactlyOnce(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Expected thinking or redacted_thinking, but found tool_use",
				"type":    "invalid_request_error",
				"code":    "invalid_format",
			},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	storeTestRoute(server, "chat-model", RouteProtocolChat, "/v1/chat/completions")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"chat-model",
		"input":[{"role":"assistant","content":[{"type":"thinking","thinking":"Hidden"},{"type":"text","text":"Visible"}]}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if callCount != 2 {
		t.Fatalf("expected exactly two attempts, got %d", callCount)
	}
}

func TestThinkingRectifierKeepsToolHistoryAfterStrip(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}

		messages, _ := body["messages"].([]any)
		if len(messages) < 3 {
			t.Fatalf("expected tool history messages to survive rectification, got %#v", messages)
		}

		if callCount == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "Expected thinking or redacted_thinking, but found tool_use",
					"type":    "invalid_request_error",
					"code":    "invalid_format",
				},
			})
			return
		}

		var assistantCall map[string]any
		var toolResult map[string]any
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if toolCalls, _ := message["tool_calls"].([]any); len(toolCalls) > 0 {
				assistantCall = message
			}
			if message["role"] == "tool" {
				toolResult = message
			}
		}
		if assistantCall == nil {
			t.Fatalf("expected assistant tool call history to survive rectification, got %#v", messages)
		}
		if toolResult == nil || toolResult["role"] != "tool" {
			t.Fatalf("expected tool result history to survive rectification, got %#v", messages)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-tools",
			"created": 123,
			"model":   "chat-model",
			"choices": []any{
				map[string]any{
					"finish_reason": "stop",
					"message":       map[string]any{"role": "assistant", "content": "Done"},
				},
			},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	storeTestRoute(server, "chat-model", RouteProtocolChat, "/v1/chat/completions")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"chat-model",
		"input":[
			{"role":"user","content":"List files"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"Hidden"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"README.md"}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if callCount != 2 {
		t.Fatalf("expected one retry, got %d", callCount)
	}
}

func TestThinkingRectifierReturnsOriginalErrorWhenNotApplicable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Regular invalid request",
				"type":    "invalid_request_error",
				"code":    "invalid_request",
			},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	storeTestRoute(server, "chat-model", RouteProtocolChat, "/v1/chat/completions")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"chat-model","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), `"message":"Regular invalid request"`)
}

func TestForwardResponsesAsChatInjectsConfiguredCacheTTL(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-route",
			"created": 123,
			"model":   "chat-model",
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
		UpstreamBaseURL:   upstream.URL + "/v1",
		UpstreamAPIKey:    "upstream-secret",
		RequestTimeout:    secondsToDuration(5),
		StreamTimeout:     secondsToDuration(5),
		VerifySSL:         true,
		CacheOptimizer:    true,
		CacheOptimizerTTL: "5m",
	})
	storeTestRoute(server, "chat-model", RouteProtocolChat, "/v1/chat/completions")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"chat-model",
		"instructions":"Be helpful.",
		"input":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"Hidden."},
				{"type":"text","text":"Visible."}
			]}
		],
		"tools":[{"type":"function","name":"shell","function":{"parameters":{"type":"object"}}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}

	tools, _ := upstreamBody["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("expected converted tools, got %#v", upstreamBody["tools"])
	}
	tool := tools[len(tools)-1].(map[string]any)
	toolCC, _ := tool["cache_control"].(map[string]any)
	if toolCC["type"] != "ephemeral" {
		t.Fatalf("expected cache breakpoint on tool, got %#v", tool["cache_control"])
	}
	if _, ok := toolCC["ttl"]; ok {
		t.Fatalf("expected 5m ttl to omit explicit ttl field, got %#v", toolCC)
	}

	messages, _ := upstreamBody["messages"].([]any)
	if len(messages) < 2 {
		t.Fatalf("expected converted messages, got %#v", upstreamBody["messages"])
	}
	assistant := messages[len(messages)-1].(map[string]any)
	content, _ := assistant["content"].([]any)
	if len(content) < 2 {
		t.Fatalf("expected assistant multimodal content, got %#v", assistant)
	}
	lastVisible := content[len(content)-1].(map[string]any)
	cc, _ := lastVisible["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" {
		t.Fatalf("expected cache breakpoint on assistant visible block, got %#v", lastVisible["cache_control"])
	}
	if _, ok := cc["ttl"]; ok {
		t.Fatalf("expected 5m ttl to omit explicit ttl field on assistant block, got %#v", cc)
	}
}

func TestMessagesOnlyUpstreamCanServeResponsesRequests(t *testing.T) {
	var upstreamPath string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg-abc",
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "Hi"},
			},
			"usage": map[string]any{
				"input_tokens":  4,
				"output_tokens": 2,
				"total_tokens":  6,
			},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	storeTestRoute(server, "messages-model", RouteProtocolMessages, "/v1/messages")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"messages-model","instructions":"Be brief.","input":"hello","max_output_tokens":20}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamPath != "/v1/messages" {
		t.Fatalf("unexpected upstream path: %s", upstreamPath)
	}
	if upstreamBody["system"] != "Be brief." {
		t.Fatalf("expected instructions to map to system, got %#v", upstreamBody["system"])
	}
	if upstreamBody["max_tokens"] != float64(20) {
		t.Fatalf("expected max_output_tokens to map to max_tokens, got %#v", upstreamBody["max_tokens"])
	}
	messages, _ := upstreamBody["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("expected one user message, got %#v", upstreamBody["messages"])
	}
	userMsg, _ := messages[0].(map[string]any)
	if userMsg["role"] != "user" {
		t.Fatalf("expected user role, got %#v", userMsg)
	}
	content, _ := userMsg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("expected text content block, got %#v", userMsg["content"])
	}
	if payload := recorder.Body.String(); !strings.Contains(payload, `"id":"resp-abc"`) {
		t.Fatalf("expected converted responses payload, got %s", payload)
	}
}

func TestMessagesOnlyUpstreamUsesRouteFeaturesForThinkingAndDowngrade(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg-route",
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "Hi"},
			},
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	identity := RouteIdentityKey(server.config.UpstreamBaseURL, server.config.UpstreamAPIKey)
	server.routeTable.Store(identity, "messages-model", RouteEntry{
		ModelID:    "messages-model",
		Protocol:   RouteProtocolMessages,
		Endpoint:   "/v1/messages",
		Confidence: RouteConfidenceExplicit,
		Features:   []string{"text-only", "thinking", "no-image", "no-audio"},
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"messages-model",
		"reasoning":{"effort":"high"},
		"input":[
			{"type":"input_image","image_url":"https://example.com/image.png"},
			{"type":"input_audio","input_audio":{"format":"wav","data":"QUJD"}}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if thinking, _ := upstreamBody["thinking"].(map[string]any); thinking["type"] != "enabled" {
		t.Fatalf("expected route features to enable thinking, got %#v", upstreamBody["thinking"])
	}

	messages, _ := upstreamBody["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected two downgraded user messages, got %#v", upstreamBody["messages"])
	}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		content, _ := message["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
			t.Fatalf("expected route text-only downgrade, got %#v", message)
		}
	}
}

func TestChatCompletionsPassesThroughOnlyWhenRouteIsChat(t *testing.T) {
	t.Run("route chat passes through", func(t *testing.T) {
		var upstreamPath string
		var rawBody string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamPath = r.URL.Path
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll upstream body returned error: %v", err)
			}
			rawBody = string(bodyBytes)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-pass"})
		}))
		defer upstream.Close()

		server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
		storeTestRoute(server, "chat-model", RouteProtocolChat, "/v1/chat/completions")

		body := `{"model":"chat-model","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()

		server.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
		}
		if upstreamPath != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", upstreamPath)
		}
		assertJSONTextEqual(t, body, rawBody)
	})

	for _, protocol := range []RouteProtocol{RouteProtocolResponses, RouteProtocolMessages} {
		t.Run("route "+string(protocol)+" rejects cross protocol", func(t *testing.T) {
			upstreamCalled := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalled = true
			}))
			defer upstream.Close()

			server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
			storeTestRoute(server, "not-chat-model", protocol, defaultEndpointForProtocol(protocol))

			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"not-chat-model","messages":[]}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
			}
			assertErrorTypeAndCode(t, recorder.Body.Bytes(), "invalid_request_error", "unsupported_protocol")
			if upstreamCalled {
				t.Fatal("expected unsupported chat route not to call upstream")
			}
		})
	}
}

func TestMessagesEndpointCanPassThroughWhenUpstreamProtocolMatches(t *testing.T) {
	t.Run("route messages passes through", func(t *testing.T) {
		var upstreamPath string
		var rawBody string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamPath = r.URL.Path
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll upstream body returned error: %v", err)
			}
			rawBody = string(bodyBytes)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg-pass"})
		}))
		defer upstream.Close()

		server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
		storeTestRoute(server, "messages-model", RouteProtocolMessages, "/v1/messages")

		body := `{"model":"messages-model","messages":[{"role":"user","content":"hi"}],"max_tokens":20}`
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()

		server.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
		}
		if upstreamPath != "/v1/messages" {
			t.Fatalf("unexpected upstream path: %s", upstreamPath)
		}
		assertJSONTextEqual(t, body, rawBody)
	})

	for _, protocol := range []RouteProtocol{RouteProtocolChat, RouteProtocolResponses} {
		t.Run("route "+string(protocol)+" rejects cross protocol", func(t *testing.T) {
			upstreamCalled := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalled = true
			}))
			defer upstream.Close()

			server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
			storeTestRoute(server, "not-messages-model", protocol, defaultEndpointForProtocol(protocol))

			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"not-messages-model","messages":[]}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
			}
			assertErrorTypeAndCode(t, recorder.Body.Bytes(), "invalid_request_error", "unsupported_protocol")
			if upstreamCalled {
				t.Fatal("expected unsupported messages route not to call upstream")
			}
		})
	}
}

func TestMessagesStreamFalseUsesNormalPassthrough(t *testing.T) {
	var upstreamPath string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type: %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("Decode upstream body returned error: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "msg-pass",
			"object":  "message",
			"content": "ok",
		})
	}))
	defer upstream.Close()

	server := newRouteAwareTestServer(upstream.URL+"/v1", "upstream-secret")
	storeTestRoute(server, "messages-model", RouteProtocolMessages, "/v1/messages")

	body := `{"model":"messages-model","messages":[{"role":"user","content":"hi"}],"stream":false,"max_tokens":20}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamPath != "/v1/messages" {
		t.Fatalf("unexpected upstream path: %s", upstreamPath)
	}
	if upstreamBody["stream"] != false {
		t.Fatalf("expected upstream to receive stream=false, got %#v", upstreamBody["stream"])
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("unexpected content type: %s body=%s", got, recorder.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v body=%s", err, recorder.Body.String())
	}
	if payload["id"] != "msg-pass" {
		t.Fatalf("unexpected response payload: %#v", payload)
	}
}

func TestUnsupportedProtocolReturnsClearError(t *testing.T) {
	server := newRouteAwareTestServer("https://example.test/v1", "upstream-secret")
	storeTestRoute(server, "responses-model", RouteProtocolResponses, "/v1/responses")

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"responses-model","messages":[]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertErrorTypeAndCode(t, recorder.Body.Bytes(), "invalid_request_error", "unsupported_protocol")
	assertContains(t, recorder.Body.String(), "does not support chat")
}

func TestModelsEndpointStillSkipsProxyAuth(t *testing.T) {
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
		t.Fatal("expected unauthenticated models request to leave existing route snapshot intact")
	}
	if _, ok := server.routeTable.Resolve(identity, "public-model"); ok {
		t.Fatal("expected unauthenticated models request not to refresh route table")
	}
}

func TestControlledEntrypointsRequireProxyAuth(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			server := NewServer(Config{
				ProxyAPIKey:    "proxy-secret",
				RequestTimeout: secondsToDuration(5),
				StreamTimeout:  secondsToDuration(5),
				VerifySSL:      true,
			})

			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test-model","messages":[]}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
			}
			assertContains(t, recorder.Body.String(), `"code":"invalid_api_key"`)
		})
	}
}

func TestControlledEntrypointsRejectNonPostWithoutForwarding(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			upstreamCalled := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalled = true
				t.Fatalf("unexpected upstream call for %s %s", r.Method, r.URL.Path)
			}))
			defer upstream.Close()

			server := NewServer(Config{
				UpstreamBaseURL: upstream.URL + "/v1",
				ProxyAPIKey:     "proxy-secret",
				RequestTimeout:  secondsToDuration(5),
				StreamTimeout:   secondsToDuration(5),
				VerifySSL:       true,
			})

			request := httptest.NewRequest(http.MethodGet, path, nil)
			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
			}
			assertErrorTypeAndCode(t, recorder.Body.Bytes(), "invalid_request_error", "method_not_allowed")
			if got := recorder.Header().Get("Allow"); got != http.MethodPost {
				t.Fatalf("unexpected Allow header: %q", got)
			}
			if upstreamCalled {
				t.Fatal("expected non-POST controlled entrypoint not to call upstream")
			}
		})
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

func TestResponsesEndpointRetriesWithNextKeyOnRateLimit(t *testing.T) {
	var seenAuth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenAuth = append(seenAuth, auth)
		if auth == "Bearer sk-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(errorPayload("rate limited", "rate_limit_error", "rate_limit_exceeded"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-abc",
			"created": 123,
			"model":   "test-model",
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "Hi"}}},
		})
	}))
	defer upstream.Close()

	server := newMultiKeyRouteAwareTestServer(upstream.URL+"/v1", RouteProtocolChat)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("expected proxy to hide upstream 429 body, got %s", recorder.Body.String())
	}
	assertJSONEqual(t, []string{"Bearer sk-a", "Bearer sk-b"}, seenAuth)
}

func TestChatCompletionsEndpointRetriesWithNextKeyOnRateLimit(t *testing.T) {
	var seenAuth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenAuth = append(seenAuth, auth)
		if auth == "Bearer sk-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(errorPayload("rate limited", "rate_limit_error", "rate_limit_exceeded"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-abc",
			"created": 123,
			"model":   "test-model",
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "Hi"}}},
		})
	}))
	defer upstream.Close()

	server := newMultiKeyRouteAwareTestServer(upstream.URL+"/v1", RouteProtocolChat)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertJSONEqual(t, []string{"Bearer sk-a", "Bearer sk-b"}, seenAuth)
}

func TestMessagesEndpointRetriesWithNextKeyOnRateLimit(t *testing.T) {
	var seenAuth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenAuth = append(seenAuth, auth)
		if auth == "Bearer sk-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(errorPayload("rate limited", "rate_limit_error", "rate_limit_exceeded"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "msg_abc",
			"type":    "message",
			"role":    "assistant",
			"model":   "test-model",
			"content": []any{map[string]any{"type": "text", "text": "Hi"}},
		})
	}))
	defer upstream.Close()

	server := newMultiKeyRouteAwareTestServer(upstream.URL+"/v1", RouteProtocolMessages)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertJSONEqual(t, []string{"Bearer sk-a", "Bearer sk-b"}, seenAuth)
}

func TestResponsesStreamRetriesWithNextKeyOnRateLimitBeforeWritingSSE(t *testing.T) {
	var seenAuth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenAuth = append(seenAuth, auth)
		w.Header().Set("Content-Type", "text/event-stream")
		if auth == "Bearer sk-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("data: " + mustJSON(errorPayload("rate limited", "rate_limit_error", "rate_limit_exceeded")) + "\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: " + mustJSON(map[string]any{
			"id":      "chatcmpl-abc",
			"created": 123,
			"model":   "test-model",
			"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "content": "Hi"}, "finish_reason": nil}},
		}) + "\n\n"))
		_, _ = w.Write([]byte("data: " + mustJSON(map[string]any{
			"id":      "chatcmpl-abc",
			"created": 123,
			"model":   "test-model",
			"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
		}) + "\n\n"))
	}))
	defer upstream.Close()

	server := newMultiKeyRouteAwareTestServer(upstream.URL+"/v1", RouteProtocolChat)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), "response.completed")
	if strings.Contains(recorder.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("expected retry to hide upstream 429 body, got %s", recorder.Body.String())
	}
	assertJSONEqual(t, []string{"Bearer sk-a", "Bearer sk-b"}, seenAuth)
}

func TestResponsesEndpointReturns503WhenAllKeysAreRateLimited(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(errorPayload("rate limited", "rate_limit_error", "rate_limit_exceeded"))
	}))
	defer upstream.Close()

	server := newMultiKeyRouteAwareTestServer(upstream.URL+"/v1", RouteProtocolChat)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), `"code":"upstream_keys_rate_limited"`)
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

func TestUnknownV1RequestRetriesWithNextKeyOnRateLimit(t *testing.T) {
	var seenAuth []string
	var seenBody []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenAuth = append(seenAuth, auth)
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		seenBody = append(seenBody, string(rawBody))
		if auth == "Bearer sk-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(errorPayload("rate limited", "rate_limit_error", "rate_limit_exceeded"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL:     upstream.URL + "/v1",
		UpstreamAPIKey:      "sk-a,sk-b",
		UpstreamAPIKeys:     []string{"sk-a", "sk-b"},
		UpstreamKeyCooldown: secondsToDuration(30),
		RequestTimeout:      secondsToDuration(5),
		StreamTimeout:       secondsToDuration(5),
		VerifySSL:           true,
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), `"ok":true`)
	assertJSONEqual(t, []string{"Bearer sk-a", "Bearer sk-b"}, seenAuth)
	assertJSONEqual(t, []string{`{"input":"hello"}`, `{"input":"hello"}`}, seenBody)
}

func TestUnknownV1RequestWithConfiguredKeysDoesNotRetryUnreplayableLargeBodyAfterRateLimit(t *testing.T) {
	largeBody := strings.Repeat("x", (10<<20)+1)
	var seenAuth []string
	var seenBodySize []int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenAuth = append(seenAuth, auth)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		seenBodySize = append(seenBodySize, len(body))
		if auth == "Bearer sk-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(errorPayload("rate limited", "rate_limit_error", "rate_limit_exceeded"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL:     upstream.URL + "/v1",
		UpstreamAPIKey:      "sk-a,sk-b",
		UpstreamAPIKeys:     []string{"sk-a", "sk-b"},
		UpstreamKeyCooldown: secondsToDuration(30),
		RequestTimeout:      secondsToDuration(5),
		StreamTimeout:       secondsToDuration(5),
		VerifySSL:           true,
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(largeBody))
	request.Header.Set("Content-Type", "application/octet-stream")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), `"code":"upstream_key_rate_limited_non_replayable"`)
	if strings.Contains(recorder.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("expected proxy error instead of upstream 429 body, got %s", recorder.Body.String())
	}
	assertJSONEqual(t, []string{"Bearer sk-a"}, seenAuth)
	assertJSONEqual(t, []int{len(largeBody)}, seenBodySize)
}

func TestUnknownV1RequestWithConfiguredKeysForwardsLargeBody(t *testing.T) {
	largeBody := strings.Repeat("x", (10<<20)+1)
	var seenAuth string
	var seenBodySize int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		seenBodySize = len(body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL:     upstream.URL + "/v1",
		UpstreamAPIKey:      "sk-a,sk-b",
		UpstreamAPIKeys:     []string{"sk-a", "sk-b"},
		UpstreamKeyCooldown: secondsToDuration(30),
		RequestTimeout:      secondsToDuration(5),
		StreamTimeout:       secondsToDuration(5),
		VerifySSL:           true,
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(largeBody))
	request.Header.Set("Content-Type", "application/octet-stream")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertContains(t, recorder.Body.String(), `"ok":true`)
	if seenAuth != "Bearer sk-a" {
		t.Fatalf("unexpected auth: %s", seenAuth)
	}
	if seenBodySize != len(largeBody) {
		t.Fatalf("unexpected body size: got %d want %d", seenBodySize, len(largeBody))
	}
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
	storeTestRoute(server, "test-model", RouteProtocolChat, "/v1/chat/completions")

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
	storeTestRoute(server, "test-model", RouteProtocolChat, "/v1/chat/completions")

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

func TestChatCompletionsStreamIgnoresEventsAfterDone(t *testing.T) {
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
		_, _ = w.Write([]byte("data: {\"choices\":[],\"cost\":\"0\"}\n\n"))
	}))
	defer upstream.Close()

	server := NewServer(Config{
		UpstreamBaseURL: upstream.URL + "/v1",
		RequestTimeout:  secondsToDuration(5),
		StreamTimeout:   secondsToDuration(5),
		VerifySSL:       true,
	})
	storeTestRoute(server, "test-model", RouteProtocolChat, "/v1/chat/completions")

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	doneIndex := strings.Index(body, "data: [DONE]")
	if doneIndex < 0 {
		t.Fatalf("expected [DONE] marker in body=%s", body)
	}
	if strings.Contains(body, `"cost":"0"`) {
		t.Fatalf("expected stream to stop at [DONE], body=%s", body)
	}
}
