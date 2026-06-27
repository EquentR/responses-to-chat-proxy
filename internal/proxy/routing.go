package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

type RouteProtocol string

const (
	RouteProtocolResponses RouteProtocol = "responses"
	RouteProtocolChat      RouteProtocol = "chat"
	RouteProtocolMessages  RouteProtocol = "messages"
	RouteProtocolUnknown   RouteProtocol = "unknown"
)

type RouteDetectionMode string

const (
	RouteDetectionLazy          RouteDetectionMode = "lazy"
	RouteDetectionStartup       RouteDetectionMode = "startup"
	RouteDetectionOff           RouteDetectionMode = "off"
	defaultRouteTableTTLSeconds                    = 30 * 60
	defaultRouteProbeGeneration                    = false
)

type RouteConfidence string

const (
	RouteConfidenceExplicit       RouteConfidence = "explicit"
	RouteConfidenceModelsMetadata RouteConfidence = "models_metadata"
	RouteConfidenceProbeSuccess   RouteConfidence = "probe_success"
	RouteConfidenceHeuristic      RouteConfidence = "heuristic"
	RouteConfidenceFallback       RouteConfidence = "fallback"
)

// RouteIdentity is the canonical normalized upstream identity used by the route table.
type RouteIdentity string

type RouteEntry struct {
	ModelID    string
	Protocol   RouteProtocol
	Endpoint   string
	Confidence RouteConfidence
	Features   []string
	Reasoning  string
	DetectedAt time.Time
	ExpiresAt  time.Time
	LastError  string
}

type RouteTable struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[routeTableKey]RouteEntry
}

type routeTableKey struct {
	identity RouteIdentity
	model    string
}

func NewRouteTable(ttl time.Duration) *RouteTable {
	return newRouteTableWithClock(ttl, nil)
}

func newRouteTableWithClock(ttl time.Duration, now func() time.Time) *RouteTable {
	if now == nil {
		now = time.Now
	}
	return &RouteTable{
		ttl:     normalizeRouteTableTTL(ttl),
		now:     now,
		entries: make(map[routeTableKey]RouteEntry),
	}
}

func (t *RouteTable) Store(identity RouteIdentity, model string, entry RouteEntry) RouteEntry {
	if t == nil {
		return RouteEntry{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.now == nil {
		t.now = time.Now
	}
	if t.entries == nil {
		t.entries = make(map[routeTableKey]RouteEntry)
	}

	key := routeTableKey{
		identity: identity,
		model:    normalizeModelID(model),
	}

	now := t.now()
	entry.ModelID = key.model
	if entry.DetectedAt.IsZero() {
		entry.DetectedAt = now
	}
	if entry.ExpiresAt.IsZero() {
		entry.ExpiresAt = now.Add(normalizeRouteTableTTL(t.ttl))
	}
	entry.Features = cloneStringSlice(entry.Features)

	t.entries[key] = entry
	return cloneRouteEntry(entry)
}

func (t *RouteTable) Resolve(identity RouteIdentity, model string) (RouteEntry, bool) {
	if t == nil {
		return RouteEntry{}, false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.now == nil {
		t.now = time.Now
	}
	if t.entries == nil {
		return RouteEntry{}, false
	}

	key := routeTableKey{
		identity: identity,
		model:    normalizeModelID(model),
	}

	entry, ok := t.entries[key]
	if !ok {
		return RouteEntry{}, false
	}
	if !entry.ExpiresAt.IsZero() && !t.now().Before(entry.ExpiresAt) {
		delete(t.entries, key)
		return RouteEntry{}, false
	}

	return cloneRouteEntry(entry), true
}

func RouteIdentityKey(upstreamBaseURL, apiKey string) RouteIdentity {
	return RouteIdentity(normalizeUpstreamBaseURL(upstreamBaseURL) + "|" + routeAPIKeyFingerprint(apiKey))
}

func normalizeRouteTableTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultRouteTableTTLDuration()
	}
	return ttl
}

func defaultRouteTableTTLDuration() time.Duration {
	return time.Duration(defaultRouteTableTTLSeconds) * time.Second
}

func normalizeUpstreamBaseURL(value string) string {
	return strings.TrimRight(value, "/")
}

func normalizeModelID(value string) string {
	return strings.TrimSpace(value)
}

func routeAPIKeyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

func cloneRouteEntry(entry RouteEntry) RouteEntry {
	entry.Features = cloneStringSlice(entry.Features)
	return entry
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	clone := make([]string, len(values))
	copy(clone, values)
	return clone
}

func parseRouteDetectionMode(value string) RouteDetectionMode {
	switch RouteDetectionMode(strings.ToLower(strings.TrimSpace(value))) {
	case RouteDetectionLazy:
		return RouteDetectionLazy
	case RouteDetectionStartup:
		return RouteDetectionStartup
	case RouteDetectionOff:
		return RouteDetectionOff
	default:
		return RouteDetectionLazy
	}
}
