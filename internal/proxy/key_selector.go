package proxy

import (
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

	if len(s.keys) == 0 {
		return []upstreamKeyAttempt{{index: -1, configured: false}}
	}

	now := s.now()
	attempts := make([]upstreamKeyAttempt, 0, len(s.keys))
	for offset := range s.keys {
		index := (s.next + offset) % len(s.keys)
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

	if len(attempts) > 0 {
		s.next = (attempts[0].index + 1) % len(s.keys)
	}
	return attempts
}

func (s *upstreamKeySelector) markRateLimited(attempt upstreamKeyAttempt) {
	if !attempt.configured || attempt.index < 0 || attempt.index >= len(s.keys) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cooling[attempt.index] = s.now().Add(s.cooldown)
}
