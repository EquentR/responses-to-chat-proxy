package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const maxUnknownV1ReplayBodyBytes int64 = 10 << 20

var (
	errAllUpstreamKeysRateLimited          = errors.New("all upstream api keys are rate limited")
	errUpstreamKeyRateLimitedNonReplayable = errors.New("upstream api key rate limited and request body cannot be replayed")
)

type Server struct {
	config        Config
	routeTable    *RouteTable
	routeResolver RouteResolver
	normalClient  *http.Client
	streamClient  *http.Client
	keySelector   *upstreamKeySelector
}

type RouteResolver interface {
	ResolveRoute(ctx context.Context, incoming http.Header, model string, entrypoint RouteProtocol) (RouteEntry, bool, error)
}

type lazyRouteResolver struct {
	server *Server
}

type routeProbeUnsupportedError struct {
	message string
}

func (e *routeProbeUnsupportedError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func NewServer(cfg Config) *Server {
	upstreamKeys := configuredUpstreamKeys(cfg)
	cfg.UpstreamAPIKeys = upstreamKeys
	cooldown := upstreamKeyCooldown(cfg)
	cfg.UpstreamKeyCooldown = cooldown

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !cfg.VerifySSL}

	return &Server{
		config:        cfg,
		routeTable:    NewRouteTable(cfg.RouteTableTTL),
		routeResolver: &lazyRouteResolver{},
		keySelector:   newUpstreamKeySelector(upstreamKeys, cooldown),
		normalClient: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
		},
		streamClient: &http.Client{
			Transport: transport.Clone(),
		},
	}
}

func Run(cfg Config) error {
	proxyServer := NewServer(cfg)
	if err := proxyServer.initializeStartupDiscovery(context.Background()); err != nil {
		log.Printf("models discovery at startup failed: %v", err)
	}

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           proxyServer,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("listening on %s", httpServer.Addr)
	return httpServer.ListenAndServe()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		s.handleModels(w, r)
	case r.URL.Path == "/v1/chat/completions":
		if r.Method != http.MethodPost {
			s.writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleChatCompletions(w, r)
	case r.URL.Path == "/v1/messages":
		if r.Method != http.MethodPost {
			s.writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleMessages(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
		s.handleResponses(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		s.forwardUnknownV1(w, r)
	default:
		writeJSON(w, http.StatusNotFound, errorPayload("Not found.", "invalid_request_error", "not_found"))
	}
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	body, err := readJSONObject(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "invalid_request_error", "invalid_json"))
		return
	}
	model := s.requestModel(body)
	if model == "" {
		writeMissingModel(w)
		return
	}

	route, ok, err := s.resolveRoute(r.Context(), r.Header, model, RouteProtocolResponses)
	if err != nil {
		writeDiscoveryError(w, err)
		return
	}
	if ok {
		switch route.Protocol {
		case RouteProtocolResponses:
			endpointURL := s.routeEndpointURL(route, RouteProtocolResponses)
			if boolValue(body["stream"]) {
				s.passthroughStream(w, r, body, endpointURL, RouteProtocolResponses)
				return
			}
			s.passthroughNormal(w, r, body, endpointURL)
			return
		case RouteProtocolChat:
			s.forwardResponsesAsChat(w, r, body, route)
			return
		case RouteProtocolMessages:
			s.forwardResponsesAsMessages(w, r, body, route)
			return
		default:
			s.writeUnsupportedProtocol(w, fmt.Sprintf("Model %q resolved to unsupported protocol %q.", model, route.Protocol))
			return
		}
	}

	// Backward-compatible fallback: Responses requests without a route keep the
	// historical Responses->Chat conversion path.
	s.forwardResponsesAsChat(w, r, body, RouteEntry{Protocol: RouteProtocolChat})
}

func (s *Server) forwardResponsesAsChat(w http.ResponseWriter, r *http.Request, body map[string]any, route RouteEntry) {
	chatRequest := ConvertRequest(body, s.config)

	// Apply cache-optimizer: inject cache_control breakpoints for prompt caching.
	if s.config.CacheOptimizer {
		InjectCacheBreakpoints(chatRequest, s.config.CacheOptimizerTTL)
	}

	if boolValue(chatRequest["stream"]) {
		s.streamResponses(w, r, body, chatRequest, s.routeEndpointURL(route, RouteProtocolChat))
		return
	}

	s.forwardConvertedWithRectifier(w, r, body, chatRequest)
}

func (s *Server) forwardResponsesAsMessages(w http.ResponseWriter, r *http.Request, body map[string]any, route RouteEntry) {
	messagesRequest := ConvertResponsesToMessages(body, s.config, route)

	if boolValue(messagesRequest["stream"]) {
		s.streamMessages(w, r, body, messagesRequest, s.routeEndpointURL(route, RouteProtocolMessages))
		return
	}

	s.forwardConvertedMessagesResponse(w, r, body, messagesRequest, s.routeEndpointURL(route, RouteProtocolMessages))
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	body, err := readJSONObject(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "invalid_request_error", "invalid_json"))
		return
	}
	model := s.requestModel(body)
	if model == "" {
		writeMissingModel(w)
		return
	}

	route, ok, err := s.resolveRoute(r.Context(), r.Header, model, RouteProtocolChat)
	if err != nil {
		writeDiscoveryError(w, err)
		return
	}
	if !ok || route.Protocol != RouteProtocolChat {
		s.writeUnsupportedProtocol(w, fmt.Sprintf("Model %q does not support chat completions on this route.", model))
		return
	}

	endpointURL := s.routeEndpointURL(route, RouteProtocolChat)
	if boolValue(body["stream"]) {
		s.passthroughStream(w, r, body, endpointURL, RouteProtocolChat)
		return
	}

	s.passthroughNormal(w, r, body, endpointURL)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	body, err := readJSONObject(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload(err.Error(), "invalid_request_error", "invalid_json"))
		return
	}
	model := s.requestModel(body)
	if model == "" {
		writeMissingModel(w)
		return
	}

	route, ok, err := s.resolveRoute(r.Context(), r.Header, model, RouteProtocolMessages)
	if err != nil {
		writeDiscoveryError(w, err)
		return
	}
	if !ok || route.Protocol != RouteProtocolMessages {
		s.writeUnsupportedProtocol(w, fmt.Sprintf("Model %q does not support messages on this route.", model))
		return
	}

	endpointURL := s.routeEndpointURL(route, RouteProtocolMessages)
	if boolValue(body["stream"]) {
		s.passthroughStream(w, r, body, endpointURL, RouteProtocolMessages)
		return
	}

	s.passthroughNormal(w, r, body, endpointURL)
}

// forwardConvertedWithRectifier forwards a converted request, retrying once if
// the upstream returns a thinking signature error that can be rectified by
// stripping thinking blocks.
func (s *Server) forwardConvertedWithRectifier(w http.ResponseWriter, r *http.Request, original, chatRequest map[string]any) {
	bodyBytes, err := json.Marshal(chatRequest)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request conversion error.", "invalid_request_error", "conversion_error"))
		return
	}

	response, err := s.doRequestWithKeyFailover(
		r.Context(), s.normalClient, http.MethodPost,
		s.config.UpstreamBaseURL+"/chat/completions",
		r.Header,
		bodyBytes,
		stickyRequestKey(original),
	)
	if err != nil {
		s.writeUpstreamRequestError(w, err)
		return
	}

	if response.StatusCode >= 400 {
		respBody, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			writeJSON(w, response.StatusCode, errorPayload("Failed to read upstream error.", "upstream_error", "read_error"))
			return
		}
		errStr := TryParseErrorBody(respBody)
		if ShouldRectifyThinkingSignature(errStr) {
			stripped := cloneMap(chatRequest)
			if StripThinkingBlocks(stripped) {
				strippedBytes, _ := json.Marshal(stripped)
				retryResp, retryErr := s.doRequestWithKeyFailover(
					r.Context(), s.normalClient, http.MethodPost,
					s.config.UpstreamBaseURL+"/chat/completions",
					r.Header,
					strippedBytes,
					stickyRequestKey(original),
				)
				if retryErr != nil {
					s.writeUpstreamRequestError(w, retryErr)
					return
				}
				defer retryResp.Body.Close()
				retryData := parseResponseJSON(retryResp)
				if retryResp.StatusCode >= 400 {
					writeJSON(w, retryResp.StatusCode, NormalizeUpstreamError(retryData))
					return
				}
				writeJSON(w, retryResp.StatusCode, ConvertResponse(retryData, original))
				return
			}
		}
		writeJSON(w, response.StatusCode, NormalizeUpstreamError(parseResponseJSONFromBytes(respBody)))
		return
	}
	defer response.Body.Close()

	responseData := parseResponseJSON(response)
	writeJSON(w, response.StatusCode, ConvertResponse(responseData, original))
}

func parseResponseJSONFromBytes(raw []byte) map[string]any {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return map[string]any{"raw_response": string(raw)}
	}
	dict, ok := value.(map[string]any)
	if ok {
		return dict
	}
	return map[string]any{"raw_response": value}
}

func (s *Server) forwardUnknownV1(w http.ResponseWriter, r *http.Request) {
	upstreamURL := s.config.UpstreamBaseURL + strings.TrimPrefix(r.URL.Path, "/v1")
	if rawQuery := r.URL.RawQuery; rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}

	if !s.hasConfiguredUpstreamKeys() {
		response, err := s.doRequest(r.Context(), s.normalClient, r.Method, upstreamURL, copyHeaders(r.Header, ""), r.Body)
		if err != nil {
			s.writeUpstreamRequestError(w, err)
			return
		}
		defer response.Body.Close()

		data := parseResponseJSON(response)
		writeJSON(w, response.StatusCode, data)
		return
	}

	body, err := readUnknownV1ReplayBody(r.Body, r.ContentLength)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Failed to read request body.", "invalid_request_error", "invalid_request_body"))
		return
	}

	response, err := s.doRequestWithReplayBodyKeyFailover(r.Context(), s.normalClient, r.Method, upstreamURL, r.Header, body)
	if err != nil {
		s.writeUpstreamRequestError(w, err)
		return
	}
	defer response.Body.Close()

	data := parseResponseJSON(response)
	writeJSON(w, response.StatusCode, data)
}

func (s *Server) forwardConvertedResponse(w http.ResponseWriter, r *http.Request, originalRequest, chatRequest map[string]any, endpointURL string) {
	body, err := json.Marshal(chatRequest)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request conversion error.", "invalid_request_error", "conversion_error"))
		return
	}

	response, err := s.doRequestWithKeyFailover(
		r.Context(),
		s.normalClient,
		http.MethodPost,
		endpointURL,
		r.Header,
		body,
		stickyRequestKey(originalRequest),
	)
	if err != nil {
		s.writeUpstreamRequestError(w, err)
		return
	}
	defer response.Body.Close()

	responseData := parseResponseJSON(response)
	if response.StatusCode >= http.StatusBadRequest {
		writeJSON(w, response.StatusCode, wrapUpstreamError(responseData, response.StatusCode))
		return
	}

	writeJSON(w, response.StatusCode, ConvertResponse(responseData, originalRequest))
}

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, originalRequest map[string]any, chatRequest map[string]any, endpointURL string) {
	streamBody := cloneMap(chatRequest)
	streamBody["stream"] = true
	body, err := json.Marshal(streamBody)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request conversion error.", "invalid_request_error", "conversion_error"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.config.StreamTimeout)
	defer cancel()

	resp, err := s.doStreamRequestWithKeyFailover(ctx, http.MethodPost, endpointURL, r.Header, body, true, stickyRequestKey(originalRequest))
	if err != nil {
		if errors.Is(err, errAllUpstreamKeysRateLimited) {
			s.writeUpstreamRequestError(w, err)
			return
		}
		s.writeStreamFailure(w, err, NewStreamingConverter())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	converter := NewStreamingConverter()

	if resp.StatusCode >= http.StatusBadRequest {
		errorData := readStreamError(resp)
		writeSSEChunks(w, flusher, sseError(extractErrorMessage(errorData), fmt.Sprintf("http_%d", resp.StatusCode)))
		for _, event := range converter.Finish() {
			writeSSEChunks(w, flusher, []byte(event))
		}
		return
	}

	buffer := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			for _, event := range converter.Feed(buffer[:n]) {
				writeSSEChunks(w, flusher, []byte(event))
			}
		}

		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			for _, event := range converter.Finish() {
				writeSSEChunks(w, flusher, []byte(event))
			}
			return
		}

		writeSSEChunks(w, flusher, sseError(readErr.Error(), "internal_error", "server_error"))
		for _, event := range converter.Finish() {
			writeSSEChunks(w, flusher, []byte(event))
		}
		return
	}
}

func (s *Server) forwardConvertedMessagesResponse(w http.ResponseWriter, r *http.Request, originalRequest, messagesRequest map[string]any, endpointURL string) {
	body, err := json.Marshal(messagesRequest)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request conversion error.", "invalid_request_error", "conversion_error"))
		return
	}

	response, err := s.doRequestWithKeyFailover(
		r.Context(),
		s.normalClient,
		http.MethodPost,
		endpointURL,
		r.Header,
		body,
		stickyRequestKey(originalRequest),
	)
	if err != nil {
		s.writeUpstreamRequestError(w, err)
		return
	}
	defer response.Body.Close()

	responseData := parseResponseJSON(response)
	if response.StatusCode >= http.StatusBadRequest {
		writeJSON(w, response.StatusCode, NormalizeUpstreamError(responseData))
		return
	}

	writeJSON(w, response.StatusCode, ConvertMessagesToResponses(responseData, originalRequest))
}

func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, originalRequest, messagesRequest map[string]any, endpointURL string) {
	streamBody := cloneMap(messagesRequest)
	streamBody["stream"] = true
	body, err := json.Marshal(streamBody)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request conversion error.", "invalid_request_error", "conversion_error"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.config.StreamTimeout)
	defer cancel()

	resp, err := s.doStreamRequestWithKeyFailover(ctx, http.MethodPost, endpointURL, r.Header, body, true, stickyRequestKey(originalRequest))
	if err != nil {
		if errors.Is(err, errAllUpstreamKeysRateLimited) {
			s.writeUpstreamRequestError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if errors.Is(err, errAllUpstreamKeysRateLimited) {
			writeSSEChunks(w, flusher, sseError("All upstream API keys are rate limited. Please retry later.", "upstream_keys_rate_limited"))
		} else {
			writeSSEChunks(w, flusher, sseError(fmt.Sprintf("Upstream request failed: %v", err), "upstream_request_failed"))
		}
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	converter := NewMessagesStreamingConverter(originalRequest)

	if resp.StatusCode >= http.StatusBadRequest {
		errorData := readStreamError(resp)
		writeSSEChunks(w, flusher, sseError(extractErrorMessage(errorData), fmt.Sprintf("http_%d", resp.StatusCode)))
		for _, event := range converter.Finish() {
			writeSSEChunks(w, flusher, []byte(event))
		}
		return
	}

	buffer := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			for _, event := range converter.Feed(buffer[:n]) {
				writeSSEChunks(w, flusher, []byte(event))
			}
		}

		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			for _, event := range converter.Finish() {
				writeSSEChunks(w, flusher, []byte(event))
			}
			return
		}

		writeSSEChunks(w, flusher, sseError(fmt.Sprintf("Upstream request failed: %v", readErr), "upstream_request_failed"))
		for _, event := range converter.Finish() {
			writeSSEChunks(w, flusher, []byte(event))
		}
		return
	}
}

func (s *Server) passthroughNormal(w http.ResponseWriter, r *http.Request, body any, endpointURL string) {
	payload, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request body must be valid JSON.", "invalid_request_error", "invalid_json"))
		return
	}

	response, err := s.doRequestWithKeyFailover(
		r.Context(),
		s.normalClient,
		http.MethodPost,
		endpointURL,
		r.Header,
		payload,
		stickyRequestKeyFromBody(body),
	)
	if err != nil {
		s.writeUpstreamRequestError(w, err)
		return
	}
	defer response.Body.Close()

	writeJSON(w, response.StatusCode, parseResponseJSON(response))
}

func (s *Server) passthroughStream(w http.ResponseWriter, r *http.Request, body map[string]any, endpointURL string, protocol RouteProtocol) {
	payload, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request body must be valid JSON.", "invalid_request_error", "invalid_json"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.config.StreamTimeout)
	defer cancel()

	resp, err := s.doStreamRequestWithKeyFailover(ctx, http.MethodPost, endpointURL, r.Header, payload, protocol != RouteProtocolChat, stickyRequestKey(body))
	if err != nil {
		if errors.Is(err, errAllUpstreamKeysRateLimited) {
			s.writeUpstreamRequestError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if errors.Is(err, errAllUpstreamKeysRateLimited) {
			writeSSEChunks(w, nil, sseError("All upstream API keys are rate limited. Please retry later.", "upstream_keys_rate_limited"))
		} else {
			writeSSEChunks(w, nil, sseError(fmt.Sprintf("Upstream request failed: %v", err), "upstream_request_failed"))
		}
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if resp.StatusCode >= http.StatusBadRequest {
		errorData := readStreamError(resp)
		writeSSEChunks(w, flusher, sseError(extractErrorMessage(errorData), fmt.Sprintf("http_%d", resp.StatusCode)))
		writeSSEChunks(w, flusher, []byte("data: [DONE]\n\n"))
		return
	}
	if protocol != RouteProtocolChat {
		s.passthroughRawStream(w, flusher, resp)
		return
	}

	normalizer := newChatCompletionStreamNormalizer()
	buffer := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			for _, event := range normalizer.Feed(buffer[:n]) {
				writeSSEChunks(w, flusher, event)
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			for _, event := range normalizer.Finish() {
				writeSSEChunks(w, flusher, event)
			}
			return
		}
		writeSSEChunks(w, flusher, sseError(fmt.Sprintf("Upstream request failed: %v", readErr), "upstream_request_failed"))
		return
	}
}

func (s *Server) passthroughRawStream(w http.ResponseWriter, flusher http.Flusher, resp *http.Response) {
	buffer := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			writeSSEChunks(w, flusher, buffer[:n])
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			return
		}

		writeSSEChunks(w, flusher, sseError(fmt.Sprintf("Upstream request failed: %v", readErr), "upstream_request_failed"))
		return
	}
}

func (s *Server) requestModel(body map[string]any) string {
	if s.config.ModelOverride != "" {
		return s.config.ModelOverride
	}
	return stringValue(body["model"])
}

func (s *Server) resolveRoute(ctx context.Context, incoming http.Header, model string, entrypoint RouteProtocol) (RouteEntry, bool, error) {
	if s.routeResolver != nil {
		if lazyResolver, ok := s.routeResolver.(*lazyRouteResolver); ok && lazyResolver.server == nil {
			lazyResolver.server = s
		}
		return s.routeResolver.ResolveRoute(ctx, incoming, model, entrypoint)
	}
	identity := RouteIdentityKey(s.config.UpstreamBaseURL, s.config.UpstreamAPIKey)
	if route, ok := s.routeTable.Resolve(identity, model); ok {
		return route, true, nil
	}
	return RouteEntry{}, false, nil
}

func (r *lazyRouteResolver) ResolveRoute(ctx context.Context, incoming http.Header, model string, entrypoint RouteProtocol) (RouteEntry, bool, error) {
	if r == nil || r.server == nil {
		return RouteEntry{}, false, nil
	}
	return r.server.resolveRouteLazily(ctx, incoming, model, entrypoint)
}

func (s *Server) resolveRouteLazily(ctx context.Context, incoming http.Header, model string, entrypoint RouteProtocol) (RouteEntry, bool, error) {
	identity := RouteIdentityKey(s.config.UpstreamBaseURL, s.config.UpstreamAPIKey)
	if route, ok := s.routeTable.Resolve(identity, model); ok {
		return route, true, nil
	}
	if s.config.RouteDetection == RouteDetectionOff {
		return RouteEntry{}, false, nil
	}
	if s.config.RouteDetection == RouteDetectionStartup {
		return RouteEntry{}, false, nil
	}

	selection, err := discoverModelSelection(ctx, s.normalClient, s.config, incoming, s.keySelector)
	if err == nil {
		ApplyDiscoveredModels(s.routeTable, identity, selection.Results)
		if route, ok := s.routeTable.Resolve(identity, model); ok {
			return route, true, nil
		}
		return RouteEntry{}, false, nil
	}

	var noEvidenceErr *modelDiscoveryNoEvidenceError
	if errors.As(err, &noEvidenceErr) {
		if !s.config.RouteProbeGeneration {
			return RouteEntry{}, false, nil
		}
		return s.probeRoute(ctx, incoming, model, entrypoint)
	}

	return RouteEntry{}, false, err
}

func (s *Server) probeRoute(ctx context.Context, incoming http.Header, model string, entrypoint RouteProtocol) (RouteEntry, bool, error) {
	for _, protocol := range s.probeProtocolOrder(model, entrypoint) {
		if err := s.probeProtocol(ctx, incoming, model, protocol); err != nil {
			var unsupported *routeProbeUnsupportedError
			if errors.As(err, &unsupported) {
				continue
			}
			return RouteEntry{}, false, err
		}
		return RouteEntry{
			ModelID:    model,
			Protocol:   protocol,
			Endpoint:   defaultEndpointForProtocol(protocol),
			Confidence: RouteConfidenceProbeSuccess,
			Reasoning:  "detected from lazy probe",
		}, true, nil
	}
	return RouteEntry{}, false, nil
}

func (s *Server) probeProtocolOrder(model string, entrypoint RouteProtocol) []RouteProtocol {
	if entrypoint != RouteProtocolResponses {
		return []RouteProtocol{entrypoint}
	}

	inferredProtocol, _, _ := inferProtocolFromModelID(model)
	candidates := []RouteProtocol{RouteProtocolResponses, inferredProtocol, RouteProtocolChat, RouteProtocolMessages}
	seen := map[RouteProtocol]struct{}{}
	order := make([]RouteProtocol, 0, len(candidates))
	for _, protocol := range candidates {
		if protocol == RouteProtocolUnknown {
			continue
		}
		if _, ok := seen[protocol]; ok {
			continue
		}
		seen[protocol] = struct{}{}
		order = append(order, protocol)
	}
	return order
}

func (s *Server) probeProtocol(ctx context.Context, incoming http.Header, model string, protocol RouteProtocol) error {
	body, err := json.Marshal(s.routeProbeBody(model, protocol))
	if err != nil {
		return err
	}

	response, err := s.doRequestWithKeyFailover(
		ctx,
		s.normalClient,
		http.MethodPost,
		joinUpstreamEndpoint(s.config.UpstreamBaseURL, defaultEndpointForProtocol(protocol)),
		incoming,
		body,
		"",
	)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return &modelDiscoveryError{
			statusCode: response.StatusCode,
			message:    sanitizedModelsDiscoveryError(response.StatusCode).Error(),
		}
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return &routeProbeUnsupportedError{message: fmt.Sprintf("route probe candidate returned HTTP %d", response.StatusCode)}
	}

	if response.StatusCode >= http.StatusInternalServerError {
		return &modelDiscoveryError{
			statusCode: response.StatusCode,
			message:    sanitizedModelsDiscoveryError(response.StatusCode).Error(),
		}
	}
	if response.StatusCode >= http.StatusBadRequest {
		return &routeProbeUnsupportedError{message: fmt.Sprintf("route probe candidate returned HTTP %d", response.StatusCode)}
	}

	return nil
}

func (s *Server) routeProbeBody(model string, protocol RouteProtocol) map[string]any {
	switch protocol {
	case RouteProtocolResponses:
		return map[string]any{
			"model":             model,
			"input":             "route probe",
			"max_output_tokens": 1,
		}
	case RouteProtocolMessages:
		return map[string]any{
			"model": model,
			"messages": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "text", "text": "route probe"},
					},
				},
			},
			"max_tokens": 1,
		}
	default:
		return map[string]any{
			"model": model,
			"messages": []any{
				map[string]any{"role": "user", "content": "route probe"},
			},
			"max_tokens": 1,
		}
	}
}

func (s *Server) routeEndpointURL(route RouteEntry, fallbackProtocol RouteProtocol) string {
	endpoint := strings.TrimSpace(route.Endpoint)
	if endpoint == "" {
		endpoint = defaultEndpointForProtocol(fallbackProtocol)
	}
	return joinUpstreamEndpoint(s.config.UpstreamBaseURL, endpoint)
}

func joinUpstreamEndpoint(baseURL, endpoint string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return baseURL
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	if hasVersionPathSuffix(baseURL) && (endpoint == "/v1" || strings.HasPrefix(endpoint, "/v1/")) {
		endpoint = strings.TrimPrefix(endpoint, "/v1")
		if endpoint == "" {
			return baseURL
		}
	}
	return baseURL + endpoint
}

func (s *Server) writeUnsupportedProtocol(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, errorPayload(message, "invalid_request_error", "unsupported_protocol"))
}

func (s *Server) writeMethodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeJSON(w, http.StatusMethodNotAllowed, errorPayload("Method not allowed.", "invalid_request_error", "method_not_allowed"))
}

func writeMissingModel(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, errorPayload(
		"Missing required parameter: model",
		"invalid_request_error",
		"missing_required_parameter",
		"model",
	))
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.canRefreshRouteSnapshot(r) {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, errorPayload("Incorrect API key provided.", "authentication_error", "invalid_api_key"))
	return false
}

func (s *Server) canRefreshRouteSnapshot(r *http.Request) bool {
	if s.config.ProxyAPIKey == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+s.config.ProxyAPIKey
}

func (s *Server) doRequest(ctx context.Context, client *http.Client, method, url string, headers http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	return client.Do(req)
}

func (s *Server) hasConfiguredUpstreamKeys() bool {
	s.keySelector.mu.Lock()
	defer s.keySelector.mu.Unlock()
	return len(s.keySelector.keys) > 0
}

func (s *Server) hasMultipleConfiguredUpstreamKeys() bool {
	s.keySelector.mu.Lock()
	defer s.keySelector.mu.Unlock()
	return len(s.keySelector.keys) > 1
}

func (s *Server) doRequestWithKeyFailover(ctx context.Context, client *http.Client, method, url string, incomingHeaders http.Header, bodyBytes []byte, stickyKey string) (*http.Response, error) {
	attempts := s.keySelector.stickyAttempts(stickyKey)
	if len(attempts) == 0 {
		return nil, errAllUpstreamKeysRateLimited
	}
	retryRateLimited := s.hasMultipleConfiguredUpstreamKeys()

	for _, attempt := range attempts {
		resp, err := s.doRequest(ctx, client, method, url, copyHeaders(incomingHeaders, attempt.apiKey), bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt.configured && retryRateLimited {
			s.keySelector.markRateLimited(attempt)
			resp.Body.Close()
			continue
		}
		return resp, nil
	}

	return nil, errAllUpstreamKeysRateLimited
}

func (s *Server) doRequestWithReplayBodyKeyFailover(ctx context.Context, client *http.Client, method, url string, incomingHeaders http.Header, body *unknownV1ReplayBody) (*http.Response, error) {
	attempts := s.keySelector.attempts()
	if len(attempts) == 0 {
		return nil, errAllUpstreamKeysRateLimited
	}
	retryRateLimited := s.hasMultipleConfiguredUpstreamKeys()

	if !body.replayable {
		attempt := attempts[0]
		resp, err := s.doReplayBodyRequest(ctx, client, method, url, incomingHeaders, body, attempt)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt.configured && retryRateLimited {
			s.keySelector.markRateLimited(attempt)
			resp.Body.Close()
			return nil, errUpstreamKeyRateLimitedNonReplayable
		}
		return resp, nil
	}

	for _, attempt := range attempts {
		resp, err := s.doReplayBodyRequest(ctx, client, method, url, incomingHeaders, body, attempt)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt.configured && retryRateLimited {
			s.keySelector.markRateLimited(attempt)
			resp.Body.Close()
			continue
		}
		return resp, nil
	}

	return nil, errAllUpstreamKeysRateLimited
}

func (s *Server) doReplayBodyRequest(ctx context.Context, client *http.Client, method, url string, incomingHeaders http.Header, body *unknownV1ReplayBody, attempt upstreamKeyAttempt) (*http.Response, error) {
	reader, err := body.reader()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header = copyHeaders(incomingHeaders, attempt.apiKey)
	if body.size >= 0 {
		req.ContentLength = body.size
	}
	return client.Do(req)
}

func (s *Server) doStreamRequestWithKeyFailover(ctx context.Context, method, url string, incomingHeaders http.Header, bodyBytes []byte, acceptEventStream bool, stickyKey string) (*http.Response, error) {
	attempts := s.keySelector.stickyAttempts(stickyKey)
	if len(attempts) == 0 {
		return nil, errAllUpstreamKeysRateLimited
	}
	retryRateLimited := s.hasMultipleConfiguredUpstreamKeys()

	for _, attempt := range attempts {
		headers := copyHeaders(incomingHeaders, attempt.apiKey)
		if acceptEventStream {
			headers.Set("Accept", "text/event-stream")
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header = headers

		resp, err := s.streamClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt.configured && retryRateLimited {
			s.keySelector.markRateLimited(attempt)
			resp.Body.Close()
			continue
		}
		return resp, nil
	}

	return nil, errAllUpstreamKeysRateLimited
}

type unknownV1ReplayBody struct {
	memory     []byte
	direct     io.Reader
	size       int64
	replayable bool
	used       bool
}

func readUnknownV1ReplayBody(body io.Reader, contentLength int64) (*unknownV1ReplayBody, error) {
	if contentLength > maxUnknownV1ReplayBodyBytes {
		return &unknownV1ReplayBody{direct: body, size: contentLength}, nil
	}

	raw, err := io.ReadAll(io.LimitReader(body, maxUnknownV1ReplayBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) <= maxUnknownV1ReplayBodyBytes {
		return &unknownV1ReplayBody{memory: raw, size: int64(len(raw)), replayable: true}, nil
	}

	return &unknownV1ReplayBody{direct: io.MultiReader(bytes.NewReader(raw), body), size: -1}, nil
}

func (b *unknownV1ReplayBody) reader() (io.Reader, error) {
	if b.replayable {
		return bytes.NewReader(b.memory), nil
	}
	if b.used {
		return nil, errors.New("unknown v1 request body cannot be replayed")
	}
	b.used = true
	return b.direct, nil
}

func (s *Server) writeUpstreamRequestError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAllUpstreamKeysRateLimited) {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("All upstream API keys are rate limited. Please retry later.", "upstream_error", "upstream_keys_rate_limited"))
		return
	}
	if errors.Is(err, errUpstreamKeyRateLimitedNonReplayable) {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("Upstream API key is rate limited and this request body cannot be retried. Please retry later.", "upstream_error", "upstream_key_rate_limited_non_replayable"))
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, http.StatusGatewayTimeout, errorPayload("Request timeout. Please try again.", "timeout_error", "request_timeout"))
		return
	}
	writeJSON(w, http.StatusBadGateway, errorPayload(fmt.Sprintf("Upstream request failed: %v", err), "upstream_error", "upstream_request_failed"))
}

func (s *Server) writeStreamFailure(w http.ResponseWriter, err error, converter *StreamingConverter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if errors.Is(err, errAllUpstreamKeysRateLimited) {
		writeSSEChunks(w, flusher, sseError("All upstream API keys are rate limited. Please retry later.", "upstream_keys_rate_limited"))
	} else if errors.Is(err, context.DeadlineExceeded) {
		writeSSEChunks(w, flusher, sseError("Request timeout.", "request_timeout", "timeout_error"))
	} else {
		writeSSEChunks(w, flusher, sseError(fmt.Sprintf("Upstream request failed: %v", err), "upstream_request_failed"))
	}
	for _, event := range converter.Finish() {
		writeSSEChunks(w, flusher, []byte(event))
	}
}

func copyHeaders(incoming http.Header, upstreamAPIKey string) http.Header {
	headers := http.Header{}
	contentType := strings.TrimSpace(incoming.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	headers.Set("Content-Type", contentType)
	for key, values := range incoming {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "content-length" || lower == "accept-encoding" || lower == "connection" || lower == "content-type" {
			continue
		}
		if upstreamAPIKey != "" && (lower == "authorization" || lower == "x-api-key" || lower == "x-goog-api-key") {
			continue
		}
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	if upstreamAPIKey != "" {
		headers.Set("Authorization", "Bearer "+upstreamAPIKey)
	}
	return headers
}

func readJSONObject(body io.Reader) (map[string]any, error) {
	value, err := readAnyJSON(body)
	if err != nil {
		return nil, err
	}
	dict, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("Request body must be a JSON object.")
	}
	return dict, nil
}

func readAnyJSON(body io.Reader) (any, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("Request body must be valid JSON.")
	}
	return value, nil
}

func parseResponseJSON(resp *http.Response) map[string]any {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return map[string]any{"raw_response": err.Error()}
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return map[string]any{"raw_response": string(raw)}
	}
	dict, ok := value.(map[string]any)
	if ok {
		return dict
	}
	return map[string]any{"raw_response": value}
}

func readStreamError(resp *http.Response) map[string]any {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return map[string]any{"raw": err.Error()}
	}
	if strings.TrimSpace(string(raw)) == "" {
		return map[string]any{"raw": "<empty response body>"}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return map[string]any{"raw": string(raw)}
	}
	if dict, ok := value.(map[string]any); ok {
		return dict
	}
	return map[string]any{"raw": value}
}

func extractErrorMessage(errorData map[string]any) string {
	if errorMap, ok := errorData["error"].(map[string]any); ok {
		if value := stringValue(errorMap["message"]); value != "" {
			return value
		}
		return fmt.Sprint(errorMap)
	}
	if value := stringValue(errorData["error"]); value != "" {
		return value
	}
	if value := stringValue(errorData["message"]); value != "" {
		return value
	}
	if value := stringValue(errorData["detail"]); value != "" {
		return value
	}
	if value := stringValue(errorData["raw"]); value != "" {
		return trimMessage(value)
	}
	return "Upstream error"
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeSSEChunks(w http.ResponseWriter, flusher http.Flusher, chunk []byte) {
	_, _ = w.Write(chunk)
	if flusher != nil {
		flusher.Flush()
	}
}

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
