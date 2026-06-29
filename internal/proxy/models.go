package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const maxModelsResponseBytes = 4 << 20

var errUnrecognizedModelsCatalog = errors.New("models discovery returned an unrecognized models payload")

type ModelDiscoveryResult struct {
	ModelID            string
	Protocol           RouteProtocol
	ProtocolCandidates []RouteProtocol
	Endpoint           string
	Features           []string
	Confidence         RouteConfidence
	Reasoning          string
	Raw                map[string]any
}

func DiscoverModels(ctx context.Context, client *http.Client, cfg Config) ([]ModelDiscoveryResult, error) {
	if client == nil {
		client = http.DefaultClient
	}

	selection, err := discoverModelSelection(ctx, client, cfg, nil, newUpstreamKeySelector(configuredUpstreamKeys(cfg), upstreamKeyCooldown(cfg)))
	if err != nil {
		return nil, err
	}

	return selection.Results, nil
}

func ParseModelDiscoveryResults(raw []byte) ([]ModelDiscoveryResult, error) {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("models discovery returned invalid JSON: %w", err)
	}

	items, recognized := extractModelItems(payload)
	if !recognized {
		return nil, errUnrecognizedModelsCatalog
	}

	results := make([]ModelDiscoveryResult, 0, len(items))
	for _, item := range items {
		result, ok := normalizeModelDiscoveryItem(item)
		if !ok {
			return nil, errUnrecognizedModelsCatalog
		}
		results = append(results, result)
	}
	return results, nil
}

type modelDiscoveryPage struct {
	Body        []byte
	StatusCode  int
	ContentType string
}

type modelDiscoverySelection struct {
	Page    modelDiscoveryPage
	Results []ModelDiscoveryResult
}

type modelDiscoveryError struct {
	statusCode int
	message    string
	code       string
}

type modelDiscoveryNoEvidenceError struct {
	message string
}

func (e *modelDiscoveryError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *modelDiscoveryNoEvidenceError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func upstreamKeysRateLimitedDiscoveryError() *modelDiscoveryError {
	return &modelDiscoveryError{
		statusCode: http.StatusServiceUnavailable,
		message:    "models discovery failed: all upstream API keys are rate limited",
		code:       "upstream_keys_rate_limited",
	}
}

func fetchModelDiscoveryPage(ctx context.Context, client *http.Client, cfg Config, incoming http.Header, candidate string, selector *upstreamKeySelector) (modelDiscoveryPage, error) {
	if selector == nil {
		selector = newUpstreamKeySelector(configuredUpstreamKeys(cfg), upstreamKeyCooldown(cfg))
	}
	attempts := selector.attempts()
	if len(attempts) == 0 {
		return modelDiscoveryPage{}, upstreamKeysRateLimitedDiscoveryError()
	}
	retryRateLimited := selectorHasMultipleConfiguredKeys(selector)

	for _, attempt := range attempts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate, nil)
		if err != nil {
			return modelDiscoveryPage{}, err
		}
		req.Header = copyHeaders(incoming, attempt.apiKey)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return modelDiscoveryPage{}, fmt.Errorf("models discovery failed: %w", err)
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxModelsResponseBytes))
		contentType := resp.Header.Get("Content-Type")
		_ = resp.Body.Close()
		if readErr != nil {
			return modelDiscoveryPage{}, fmt.Errorf("models discovery failed reading upstream response: %w", readErr)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt.configured && retryRateLimited {
			selector.markRateLimited(attempt)
			continue
		}

		return modelDiscoveryPage{
			Body:        body,
			StatusCode:  resp.StatusCode,
			ContentType: contentType,
		}, nil
	}

	return modelDiscoveryPage{}, upstreamKeysRateLimitedDiscoveryError()
}

func selectorHasMultipleConfiguredKeys(selector *upstreamKeySelector) bool {
	if selector == nil {
		return false
	}
	selector.mu.Lock()
	defer selector.mu.Unlock()
	return len(selector.keys) > 1
}

func discoverModelSelection(ctx context.Context, client *http.Client, cfg Config, incoming http.Header, selector *upstreamKeySelector) (modelDiscoverySelection, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if selector == nil {
		selector = newUpstreamKeySelector(configuredUpstreamKeys(cfg), upstreamKeyCooldown(cfg))
	}

	candidates := modelDiscoveryCandidates(cfg.UpstreamBaseURL, cfg.UpstreamModelsURL)
	if len(candidates) == 0 {
		return modelDiscoverySelection{}, &modelDiscoveryNoEvidenceError{message: "models discovery has no endpoint candidates"}
	}

	var lastErr error
	var lastParseErr error
	var sawEmptyPage bool

	for _, candidate := range candidates {
		page, err := fetchModelDiscoveryPage(ctx, client, cfg, incoming, candidate, selector)
		if err != nil {
			return modelDiscoverySelection{}, err
		}

		if page.StatusCode == http.StatusNotFound || page.StatusCode == http.StatusMethodNotAllowed || page.StatusCode == http.StatusNotImplemented {
			lastErr = fmt.Errorf("models discovery candidate returned HTTP %d", page.StatusCode)
			continue
		}
		if page.StatusCode >= http.StatusBadRequest {
			return modelDiscoverySelection{}, &modelDiscoveryError{
				statusCode: page.StatusCode,
				message:    sanitizedModelsDiscoveryError(page.StatusCode).Error(),
			}
		}

		results, parseErr := ParseModelDiscoveryResults(page.Body)
		if parseErr != nil {
			if errors.Is(parseErr, errUnrecognizedModelsCatalog) {
				return modelDiscoverySelection{}, parseErr
			}
			lastParseErr = parseErr
			continue
		}
		if len(results) == 0 {
			sawEmptyPage = true
			continue
		}

		return modelDiscoverySelection{Page: page, Results: results}, nil
	}

	if lastParseErr != nil {
		return modelDiscoverySelection{}, lastParseErr
	}
	if sawEmptyPage {
		return modelDiscoverySelection{}, &modelDiscoveryNoEvidenceError{message: "models discovery returned no models from any candidate"}
	}
	if lastErr != nil {
		return modelDiscoverySelection{}, &modelDiscoveryNoEvidenceError{message: lastErr.Error()}
	}
	return modelDiscoverySelection{}, &modelDiscoveryNoEvidenceError{message: "models discovery failed"}
}

func ApplyDiscoveredModels(table *RouteTable, identity RouteIdentity, results []ModelDiscoveryResult) {
	if table == nil {
		return
	}
	entries := make([]RouteEntry, 0, len(results))
	for _, result := range results {
		if result.ModelID == "" || result.Protocol == RouteProtocolUnknown {
			continue
		}
		entries = append(entries, RouteEntry{
			ModelID:    result.ModelID,
			Protocol:   result.Protocol,
			Endpoint:   result.Endpoint,
			Confidence: result.Confidence,
			Features:   result.Features,
			Reasoning:  result.Reasoning,
		})
	}
	table.ReplaceIdentity(identity, entries)
}

func (s *Server) initializeStartupDiscovery(ctx context.Context) error {
	if s == nil || s.config.RouteDetection != RouteDetectionStartup {
		return nil
	}

	selection, err := discoverModelSelection(ctx, s.normalClient, s.config, nil, s.keySelector)
	if err != nil {
		return err
	}

	ApplyDiscoveredModels(s.routeTable, RouteIdentityKey(s.config.UpstreamBaseURL, s.config.UpstreamAPIKey), selection.Results)
	return nil
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	selection, err := discoverModelSelection(r.Context(), s.normalClient, s.config, r.Header, s.keySelector)
	if err != nil {
		writeDiscoveryError(w, err)
		return
	}

	if s.canRefreshRouteSnapshot(r) {
		ApplyDiscoveredModels(s.routeTable, RouteIdentityKey(s.config.UpstreamBaseURL, s.config.UpstreamAPIKey), selection.Results)
	}

	contentType := selection.Page.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(selection.Page.StatusCode)
	_, _ = w.Write(selection.Page.Body)
}

func modelDiscoveryCandidates(baseURL, explicitModelsURL string) []string {
	var candidates []string
	if explicit := strings.TrimSpace(explicitModelsURL); explicit != "" {
		candidates = appendUniqueString(candidates, strings.TrimRight(explicit, "/"))
	}

	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return candidates
	}

	for _, variant := range modelDiscoveryBaseVariants(base) {
		if hasVersionPathSuffix(variant) {
			candidates = appendUniqueString(candidates, variant+"/models")
			continue
		}
		candidates = appendUniqueString(candidates, variant+"/v1/models")
		candidates = appendUniqueString(candidates, variant+"/models")
	}

	return candidates
}

func extractModelItems(payload any) ([]map[string]any, bool) {
	switch typed := payload.(type) {
	case []any:
		return extractModelItemsFromSlice(typed)
	case map[string]any:
		if looksLikeErrorPayload(typed) {
			return nil, false
		}
		for _, key := range []string{"data", "models", "items", "results", "result"} {
			switch values := typed[key].(type) {
			case []any:
				return extractModelItemsFromSlice(values)
			case map[string]any:
				if looksLikeModelMetadata(values) {
					return []map[string]any{values}, true
				}
			}
		}
		if looksLikeModelMetadata(typed) {
			return []map[string]any{typed}, true
		}
	}
	return nil, false
}

func looksLikeErrorPayload(payload map[string]any) bool {
	for _, key := range []string{"error", "errors"} {
		if _, ok := payload[key]; ok {
			return true
		}
	}
	return false
}

func extractModelItemsFromSlice(values []any) ([]map[string]any, bool) {
	if len(values) == 0 {
		return nil, true
	}

	items := mapsFromSlice(values)
	if len(items) == 0 {
		return nil, false
	}
	return items, true
}

func normalizeModelDiscoveryItem(raw map[string]any) (ModelDiscoveryResult, bool) {
	modelID := firstString(raw, "id", "model", "name", "model_id")
	if modelID == "" {
		return ModelDiscoveryResult{}, false
	}

	explicitProtocol := firstString(raw, "protocol", "route_protocol", "api_protocol")
	explicitProtocolValue := normalizeRouteProtocol(explicitProtocol)
	explicitEndpointRaw := firstString(raw, "endpoint", "endpoint_url", "path")
	explicitEndpoint := normalizeModelEndpoint(explicitEndpointRaw)

	endpointSignals := collectEndpointSignals(raw)
	protocolCandidates := make([]RouteProtocol, 0, 3)
	reasonNotes := make([]string, 0, 4)
	confidence := RouteConfidenceFallback

	protocol := RouteProtocolUnknown
	if explicitProtocolValue != RouteProtocolUnknown {
		protocol = explicitProtocolValue
		protocolCandidates = appendUniqueRouteProtocol(protocolCandidates, protocol)
		reasonNotes = append(reasonNotes, "protocol declared in metadata")
		confidence = RouteConfidenceExplicit
	}

	if explicitEndpointRaw != "" {
		if inferred := inferProtocolFromSignals(explicitEndpointRaw, nil); inferred != RouteProtocolUnknown {
			protocolCandidates = appendUniqueRouteProtocol(protocolCandidates, inferred)
			if protocol == RouteProtocolUnknown {
				protocol = inferred
			}
		}
		reasonNotes = append(reasonNotes, "endpoint declared in metadata")
		confidence = RouteConfidenceExplicit
	}

	if protocol == RouteProtocolUnknown {
		if inferred := inferProtocolFromSignals("", endpointSignals); inferred != RouteProtocolUnknown {
			protocol = inferred
			protocolCandidates = appendUniqueRouteProtocol(protocolCandidates, inferred)
			reasonNotes = append(reasonNotes, "protocol inferred from models metadata")
			confidence = RouteConfidenceModelsMetadata
		}
	}

	if protocol == RouteProtocolUnknown {
		inferredProtocol, inferredConfidence, note := inferProtocolFromModelID(modelID)
		protocol = inferredProtocol
		protocolCandidates = appendUniqueRouteProtocol(protocolCandidates, inferredProtocol)
		reasonNotes = append(reasonNotes, note)
		confidence = inferredConfidence
	}

	endpoint := explicitEndpoint
	if endpoint == "" && protocol != RouteProtocolUnknown {
		endpoint = defaultEndpointForProtocol(protocol)
	}

	features := collectModelFeatures(raw)
	reasoning := firstString(raw, "reasoning", "notes", "description")
	if reasoning == "" {
		reasoning = strings.Join(reasonNotes, "; ")
	}
	if reasoning == "" && protocol != RouteProtocolUnknown {
		reasoning = "loaded from models metadata"
	}

	return ModelDiscoveryResult{
		ModelID:            modelID,
		Protocol:           protocol,
		ProtocolCandidates: protocolCandidates,
		Endpoint:           endpoint,
		Features:           features,
		Confidence:         confidence,
		Reasoning:          reasoning,
		Raw:                cloneAnyMap(raw),
	}, true
}

func collectEndpointSignals(raw map[string]any) []string {
	var signals []string
	for _, key := range []string{"supported_endpoints", "endpoints", "routes", "apis", "protocols", "capabilities"} {
		signals = append(signals, stringsFromAny(raw[key])...)
		if key == "capabilities" {
			signals = append(signals, featureStringsFromAny(raw[key])...)
		}
	}
	return dedupeStringsPreserveOrder(signals)
}

func collectModelFeatures(raw map[string]any) []string {
	var features []string
	for _, key := range []string{"features", "capabilities", "modalities", "input_modalities", "output_modalities"} {
		features = append(features, featureStringsFromAny(raw[key])...)
	}
	return dedupeSortedStrings(features)
}

func featureStringsFromAny(value any) []string {
	switch typed := value.(type) {
	case map[string]any:
		var values []string
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := typed[key]
			if boolValue(value) {
				values = append(values, normalizeFeatureName(key))
			}
		}
		return values
	default:
		return stringsFromAny(value)
	}
}

func stringsFromAny(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		if value := normalizeFeatureName(typed); value != "" {
			return []string{value}
		}
	case []any:
		var values []string
		for _, item := range typed {
			values = append(values, stringsFromAny(item)...)
		}
		return values
	case []string:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if value := normalizeFeatureName(item); value != "" {
				values = append(values, value)
			}
		}
		return values
	case map[string]any:
		for _, key := range []string{"path", "endpoint", "url", "protocol", "type", "name"} {
			if value := normalizeFeatureName(stringValue(typed[key])); value != "" {
				return []string{value}
			}
		}
	}
	return nil
}

func inferProtocolFromSignals(endpoint string, signals []string) RouteProtocol {
	allSignals := append([]string{endpoint}, signals...)
	for _, signal := range allSignals {
		normalized := strings.ToLower(strings.TrimSpace(signal))
		normalized = strings.Trim(normalized, "/")
		switch {
		case normalized == "responses" || strings.Contains(normalized, "responses"):
			return RouteProtocolResponses
		case normalized == "chat" || strings.Contains(normalized, "chat/completions") || strings.Contains(normalized, "chat-completions"):
			return RouteProtocolChat
		case normalized == "messages" || strings.Contains(normalized, "messages"):
			return RouteProtocolMessages
		}
	}
	return RouteProtocolUnknown
}

func inferProtocolFromModelID(modelID string) (RouteProtocol, RouteConfidence, string) {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.Contains(normalized, "claude"):
		return RouteProtocolMessages, RouteConfidenceHeuristic, "model id suggests anthropic messages"
	case strings.Contains(normalized, "gpt"), strings.Contains(normalized, "openai"), looksLikeOpenAIResponsesModel(normalized):
		return RouteProtocolResponses, RouteConfidenceHeuristic, "model id suggests openai responses"
	default:
		return RouteProtocolChat, RouteConfidenceFallback, "model id does not match a known provider; defaulted to chat"
	}
}

func looksLikeOpenAIResponsesModel(value string) bool {
	if len(value) < 2 || value[0] != 'o' {
		return false
	}
	next := value[1]
	return next >= '0' && next <= '9'
}

func normalizeRouteProtocol(value string) RouteProtocol {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "responses", "response":
		return RouteProtocolResponses
	case "chat", "chat_completions", "chat-completions", "chat/completions":
		return RouteProtocolChat
	case "messages", "anthropic":
		return RouteProtocolMessages
	default:
		return RouteProtocolUnknown
	}
}

func defaultEndpointForProtocol(protocol RouteProtocol) string {
	switch protocol {
	case RouteProtocolResponses:
		return "/v1/responses"
	case RouteProtocolChat:
		return "/v1/chat/completions"
	case RouteProtocolMessages:
		return "/v1/messages"
	default:
		return ""
	}
}

func normalizeModelEndpoint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		parsed, err := url.Parse(value)
		if err == nil {
			value = parsed.Path
		}
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	if strings.HasPrefix(value, "/v1/") || value == "/v1" {
		return value
	}
	if strings.HasPrefix(value, "/v") && len(value) > 2 && value[2] >= '0' && value[2] <= '9' {
		return value
	}
	return "/v1" + value
}

func sanitizedModelsDiscoveryError(statusCode int) error {
	switch statusCode {
	case http.StatusUnauthorized:
		return errors.New("models discovery failed: upstream authentication rejected the request")
	case http.StatusForbidden:
		return errors.New("models discovery failed: upstream authorization rejected the request")
	case http.StatusTooManyRequests:
		return errors.New("models discovery failed: upstream rate limited the request")
	default:
		if statusCode >= http.StatusInternalServerError {
			return fmt.Errorf("models discovery failed: upstream returned HTTP %d", statusCode)
		}
		return fmt.Errorf("models discovery failed: upstream returned HTTP %d", statusCode)
	}
}

func writeDiscoveryError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}

	statusCode := http.StatusBadGateway
	message := "Models discovery failed."
	code := ""

	var discoveryErr *modelDiscoveryError
	if errors.As(err, &discoveryErr) {
		statusCode = discoveryErr.statusCode
		message = discoveryErr.message
		code = discoveryErr.code
	} else if errors.Is(err, errAllUpstreamKeysRateLimited) {
		discoveryErr := upstreamKeysRateLimitedDiscoveryError()
		statusCode = discoveryErr.statusCode
		message = discoveryErr.message
		code = discoveryErr.code
	} else if errors.Is(err, context.DeadlineExceeded) {
		statusCode = http.StatusGatewayTimeout
		message = "Models discovery timed out."
	} else {
		message = trimMessage(err.Error())
	}
	if code == "" {
		code = fmt.Sprintf("http_%d", statusCode)
	}

	writeJSON(w, statusCode, errorPayload(message, "upstream_error", code))
}

func mapsFromSlice(values []any) []map[string]any {
	items := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if item, ok := value.(map[string]any); ok {
			items = append(items, item)
		}
	}
	return items
}

func looksLikeModelMetadata(value map[string]any) bool {
	for _, key := range []string{"id", "model", "name", "model_id"} {
		if stringValue(value[key]) != "" {
			return true
		}
	}
	return false
}

func hasVersionPathSuffix(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if len(last) < 2 || last[0] != 'v' {
		return false
	}
	for _, r := range last[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func stripTrailingVersionSegment(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return rawURL
	}
	last := parts[len(parts)-1]
	if len(last) < 2 || last[0] != 'v' {
		return rawURL
	}
	for _, r := range last[1:] {
		if r < '0' || r > '9' {
			return rawURL
		}
	}
	parts = parts[:len(parts)-1]
	clone := *parsed
	if len(parts) == 0 {
		clone.Path = ""
	} else {
		clone.Path = "/" + strings.Join(parts, "/")
	}
	clone.RawQuery = ""
	clone.Fragment = ""
	return strings.TrimRight(clone.String(), "/")
}

func modelDiscoveryBaseVariants(base string) []string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil
	}

	queue := []string{base}
	seen := map[string]struct{}{}
	var variants []string

	for len(queue) > 0 {
		current := strings.TrimRight(strings.TrimSpace(queue[0]), "/")
		queue = queue[1:]
		if current == "" {
			continue
		}
		if _, ok := seen[current]; ok {
			continue
		}
		seen[current] = struct{}{}
		variants = append(variants, current)

		if stripped := stripTrailingVersionSegment(current); stripped != current {
			queue = append(queue, stripped)
		}
		for _, stripped := range strippedCompatibilityBases(current) {
			queue = append(queue, stripped)
		}
	}

	return variants
}

func strippedCompatibilityBases(rawURL string) []string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return nil
	}

	suffixes := map[string]struct{}{
		"anthropic": {},
		"messages":  {},
		"chat":      {},
		"coding":    {},
		"claude":    {},
	}

	var bases []string
	for len(parts) > 0 {
		last := strings.ToLower(parts[len(parts)-1])
		if _, ok := suffixes[last]; !ok {
			break
		}
		parts = parts[:len(parts)-1]
		clone := *parsed
		clone.Path = "/" + strings.Join(parts, "/")
		clone.RawQuery = ""
		clone.Fragment = ""
		bases = append(bases, strings.TrimRight(clone.String(), "/"))
	}
	return bases
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringValue(values[key])); value != "" {
			return value
		}
	}
	return ""
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func normalizeFeatureName(value string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(value)), "/")
}

func dedupeSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	for _, value := range values {
		value = normalizeFeatureName(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func dedupeStringsPreserveOrder(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeFeatureName(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func appendUniqueRouteProtocol(values []RouteProtocol, value RouteProtocol) []RouteProtocol {
	if value == RouteProtocolUnknown {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func cloneAnyMap(source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
