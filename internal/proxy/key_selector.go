package proxy

import (
	"hash/fnv"
	"sync"
	"time"
)

type upstreamKeyAttempt struct {
	apiKey     string
	index      int
	configured bool
}

type upstreamKeySelector struct {
	mu       sync.Mutex
	keys     []string
	cooldown time.Duration
	next     int
	cooling  map[int]time.Time
	now      func() time.Time
}

func newUpstreamKeySelector(keys []string, cooldown time.Duration) *upstreamKeySelector {
	seen := make(map[string]struct{}, len(keys))
	normalized := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, key)
	}

	return &upstreamKeySelector{
		keys:     normalized,
		cooldown: cooldown,
		cooling:  make(map[int]time.Time),
		now:      time.Now,
	}
}

func (s *upstreamKeySelector) attempts() []upstreamKeyAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.attemptsLocked(s.next, true)
}

func (s *upstreamKeySelector) stickyAttempts(stickyKey string) []upstreamKeyAttempt {
	if stickyKey == "" {
		return s.attempts()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.keys) == 0 {
		return []upstreamKeyAttempt{{index: -1, configured: false}}
	}

	start := int(hashStickyKey(stickyKey) % uint64(len(s.keys)))
	return s.attemptsLocked(start, false)
}

func (s *upstreamKeySelector) attemptsLocked(start int, advance bool) []upstreamKeyAttempt {
	if len(s.keys) == 0 {
		return []upstreamKeyAttempt{{index: -1, configured: false}}
	}

	now := s.now()
	attempts := make([]upstreamKeyAttempt, 0, len(s.keys))
	for offset := range s.keys {
		index := (start + offset) % len(s.keys)
		coolsUntil, cooling := s.cooling[index]
		if cooling {
			if now.Before(coolsUntil) {
				continue
			}
			delete(s.cooling, index)
		}
		attempts = append(attempts, upstreamKeyAttempt{
			apiKey:     s.keys[index],
			index:      index,
			configured: true,
		})
	}

	if advance && len(attempts) > 0 {
		s.next = (attempts[0].index + 1) % len(s.keys)
	}
	return attempts
}

func (s *upstreamKeySelector) markRateLimited(attempt upstreamKeyAttempt) {
	if !attempt.configured || attempt.index < 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if attempt.index >= len(s.keys) {
		return
	}
	s.cooling[attempt.index] = s.now().Add(s.cooldown)
}

func hashStickyKey(stickyKey string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(stickyKey))
	return hash.Sum64()
}
