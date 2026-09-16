package main

import (
	"errors"
	"testing"
	"time"
)

// A systemd-resolved restart drops per-link config while our cache still says
// "applied"; register must re-apply after resolvedRefresh so DNS heals. A
// failed apply must not be cached, so the next poll retries it.
func TestResolvedSyncReappliesAfterRefresh(t *testing.T) {
	old := resolvectlFn
	defer func() { resolvectlFn = old }()

	var calls int
	resolvectlFn = func(args ...string) error { calls++; return nil }

	s := &resolvedSync{addr: "127.0.0.1", enabled: true, registered: map[string]string{}, lastApplied: map[string]time.Time{}}
	s.register("tsm0", "tail.ts.net", "lan")
	if calls != 2 {
		t.Fatalf("first apply: %d resolvectl calls, want 2 (domain + dns)", calls)
	}
	s.register("tsm0", "tail.ts.net", "lan") // cached within the refresh window
	if calls != 2 {
		t.Fatalf("cached re-register: %d resolvectl calls, want 2", calls)
	}

	// simulate resolved having restarted: the refresh window has elapsed
	s.mu.Lock()
	s.lastApplied["tsm0"] = time.Now().Add(-2 * resolvedRefresh)
	s.mu.Unlock()
	s.register("tsm0", "tail.ts.net", "lan")
	if calls != 4 {
		t.Fatalf("post-refresh re-apply: %d resolvectl calls, want 4", calls)
	}

	// a failing apply is not cached, so the next call retries
	failing := &resolvedSync{addr: "127.0.0.1", enabled: true, registered: map[string]string{}, lastApplied: map[string]time.Time{}}
	resolvectlFn = func(args ...string) error { calls++; return errors.New("resolved down") }
	calls = 0
	failing.register("tsm0", "tail.ts.net", "lan")
	failing.register("tsm0", "tail.ts.net", "lan")
	if calls != 2 {
		t.Fatalf("failed apply was cached: %d resolvectl calls, want 2 (retry)", calls)
	}
}
