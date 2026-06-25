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

type Server struct {
	config       Config
	normalClient *http.Client
	streamClient *http.Client
}

func NewServer(cfg Config) *Server {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !cfg.VerifySSL}

	return &Server{
		config: cfg,
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
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           NewServer(cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("listening on %s", server.Addr)
	return server.ListenAndServe()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
		s.handleResponses(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		s.handleChatCompletions(w, r)
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
	if _, ok := body["model"]; !ok {
		writeJSON(w, http.StatusBadRequest, errorPayload(
			"Missing required parameter: model",
			"invalid_request_error",
			"missing_required_parameter",
			"model",
		))
		return
	}

	chatRequest := ConvertRequest(body, s.config)
	if boolValue(chatRequest["stream"]) {
		s.streamResponses(w, r, body, chatRequest)
		return
	}

	s.forwardConvertedResponse(w, r, body, chatRequest)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	body, err := readAnyJSON(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request body must be valid JSON.", "invalid_request_error", "invalid_json"))
		return
	}

	if dict, ok := body.(map[string]any); ok && boolValue(dict["stream"]) {
		s.passthroughStream(w, r, dict)
		return
	}

	s.passthroughNormal(w, r, body)
}

func (s *Server) forwardUnknownV1(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	upstreamURL := s.config.UpstreamBaseURL + strings.TrimPrefix(r.URL.Path, "/v1")
	if rawQuery := r.URL.RawQuery; rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}

	response, err := s.doRequest(r.Context(), s.normalClient, r.Method, upstreamURL, copyHeaders(r.Header, s.config.UpstreamAPIKey), r.Body)
	if err != nil {
		s.writeUpstreamRequestError(w, err)
		return
	}
	defer response.Body.Close()

	data := parseResponseJSON(response)
	writeJSON(w, response.StatusCode, data)
}

func (s *Server) forwardConvertedResponse(w http.ResponseWriter, r *http.Request, originalRequest, chatRequest map[string]any) {
	body, err := json.Marshal(chatRequest)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request conversion error.", "invalid_request_error", "conversion_error"))
		return
	}

	response, err := s.doRequest(
		r.Context(),
		s.normalClient,
		http.MethodPost,
		s.config.UpstreamBaseURL+"/chat/completions",
		copyHeaders(r.Header, s.config.UpstreamAPIKey),
		bytes.NewReader(body),
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

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, _ map[string]any, chatRequest map[string]any) {
	streamBody := cloneMap(chatRequest)
	streamBody["stream"] = true
	body, err := json.Marshal(streamBody)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request conversion error.", "invalid_request_error", "conversion_error"))
		return
	}

	headers := copyHeaders(r.Header, s.config.UpstreamAPIKey)
	headers.Set("Accept", "text/event-stream")

	ctx, cancel := context.WithTimeout(r.Context(), s.config.StreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.UpstreamBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("Failed to build upstream request.", "server_error", "request_build_failed"))
		return
	}
	req.Header = headers

	resp, err := s.streamClient.Do(req)
	if err != nil {
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

func (s *Server) passthroughNormal(w http.ResponseWriter, r *http.Request, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request body must be valid JSON.", "invalid_request_error", "invalid_json"))
		return
	}

	response, err := s.doRequest(
		r.Context(),
		s.normalClient,
		http.MethodPost,
		s.config.UpstreamBaseURL+"/chat/completions",
		copyHeaders(r.Header, s.config.UpstreamAPIKey),
		bytes.NewReader(payload),
	)
	if err != nil {
		s.writeUpstreamRequestError(w, err)
		return
	}
	defer response.Body.Close()

	writeJSON(w, response.StatusCode, parseResponseJSON(response))
}

func (s *Server) passthroughStream(w http.ResponseWriter, r *http.Request, body map[string]any) {
	payload, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("Request body must be valid JSON.", "invalid_request_error", "invalid_json"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.config.StreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.UpstreamBaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("Failed to build upstream request.", "server_error", "request_build_failed"))
		return
	}
	req.Header = copyHeaders(r.Header, s.config.UpstreamAPIKey)

	resp, err := s.streamClient.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		writeSSEChunks(w, nil, sseError(fmt.Sprintf("Upstream request failed: %v", err), "upstream_request_failed"))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
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

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.config.ProxyAPIKey == "" {
		return true
	}
	if r.Header.Get("Authorization") == "Bearer "+s.config.ProxyAPIKey {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, errorPayload("Incorrect API key provided.", "authentication_error", "invalid_api_key"))
	return false
}

func (s *Server) doRequest(ctx context.Context, client *http.Client, method, url string, headers http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	return client.Do(req)
}

func (s *Server) writeUpstreamRequestError(w http.ResponseWriter, err error) {
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
	if errors.Is(err, context.DeadlineExceeded) {
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
	headers.Set("Content-Type", "application/json")
	for key, values := range incoming {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "content-length" || lower == "authorization" || lower == "accept-encoding" || lower == "connection" {
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
