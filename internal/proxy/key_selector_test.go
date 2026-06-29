package proxy

import (
	"sync"
	"testing"
	"time"
)

func TestUpstreamKeySelectorReturnsPassthroughAttemptWhenNoKeysConfigured(t *testing.T) {
	selector := newUpstreamKeySelector(nil, 30*time.Second)

	attempts := selector.attempts()

	if len(attempts) != 1 {
		t.Fatalf("expected one passthrough attempt, got %#v", attempts)
	}
	if attempts[0].apiKey != "" || attempts[0].configured {
		t.Fatalf("expected passthrough attempt, got %#v", attempts[0])
	}
}

func TestUpstreamKeySelectorRoundRobinsAvailableKeys(t *testing.T) {
	selector := newUpstreamKeySelector([]string{"sk-a", "sk-b", "sk-c"}, 30*time.Second)

	first := selector.attempts()
	second := selector.attempts()
	third := selector.attempts()

	if first[0].apiKey != "sk-a" || second[0].apiKey != "sk-b" || third[0].apiKey != "sk-c" {
		t.Fatalf("unexpected first attempts: %q %q %q", first[0].apiKey, second[0].apiKey, third[0].apiKey)
	}
}

func TestUpstreamKeySelectorSkipsCoolingKeysAndRecoversAfterCooldown(t *testing.T) {
	now := time.Unix(100, 0)
	selector := newUpstreamKeySelector([]string{"sk-a", "sk-b"}, 10*time.Second)
	selector.now = func() time.Time { return now }

	attempts := selector.attempts()
	if attempts[0].apiKey != "sk-a" {
		t.Fatalf("expected first attempt sk-a, got %#v", attempts)
	}
	selector.markRateLimited(attempts[0])

	attempts = selector.attempts()
	if len(attempts) != 1 || attempts[0].apiKey != "sk-b" {
		t.Fatalf("expected only sk-b while sk-a cools, got %#v", attempts)
	}

	now = now.Add(11 * time.Second)
	attempts = selector.attempts()
	if len(attempts) != 2 {
		t.Fatalf("expected both keys after cooldown, got %#v", attempts)
	}
	foundA := false
	for _, attempt := range attempts {
		if attempt.apiKey == "sk-a" {
			foundA = true
		}
	}
	if !foundA {
		t.Fatalf("expected sk-a to recover after cooldown, got %#v", attempts)
	}
}

func TestUpstreamKeySelectorDedupesCoolingKeys(t *testing.T) {
	now := time.Unix(100, 0)
	selector := newUpstreamKeySelector([]string{"sk-a", "sk-a", "sk-b"}, 10*time.Second)
	selector.now = func() time.Time { return now }

	attempts := selector.attempts()
	if len(attempts) != 2 {
		t.Fatalf("expected duplicate keys to be deduped, got %#v", attempts)
	}
	selector.markRateLimited(attempts[0])

	attempts = selector.attempts()
	if len(attempts) != 1 || attempts[0].apiKey != "sk-b" {
		t.Fatalf("expected duplicate key to stay cooling, got %#v", attempts)
	}
}

func TestUpstreamKeySelectorReturnsNoAttemptsWhenAllKeysAreCooling(t *testing.T) {
	now := time.Unix(100, 0)
	selector := newUpstreamKeySelector([]string{"sk-a", "sk-b"}, 10*time.Second)
	selector.now = func() time.Time { return now }

	attempts := selector.attempts()
	for _, attempt := range attempts {
		selector.markRateLimited(attempt)
	}

	attempts = selector.attempts()
	if len(attempts) != 0 {
		t.Fatalf("expected no attempts while all keys cool, got %#v", attempts)
	}
}

func TestUpstreamKeySelectorConcurrentAccess(t *testing.T) {
	selector := newUpstreamKeySelector([]string{"sk-a", "sk-b", "sk-c"}, 10*time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempts := selector.attempts()
			if len(attempts) == 0 {
				return
			}
			selector.markRateLimited(attempts[0])
		}()
	}
	wg.Wait()
}
