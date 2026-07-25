package penaltybox

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testStore(clk clock, cfg storeConfig) *store {
	if cfg.window == 0 {
		cfg.window = time.Minute
	}
	if cfg.limit == 0 {
		cfg.limit = 30
	}
	if cfg.penaltyTTL == 0 {
		cfg.penaltyTTL = 5 * time.Minute
	}
	if cfg.maxKeys == 0 {
		cfg.maxKeys = 100_000
	}
	return newStore(cfg, clk)
}

func TestWeightedWindowArithmetic(t *testing.T) {
	clk := newFakeClock()
	s := testStore(clk, storeConfig{limit: 30})

	// Ten level-3 responses = exactly 30 units: at the limit, not over it.
	for i := 0; i < 10; i++ {
		if boxed := s.add("heavy", 3); boxed {
			t.Fatalf("boxed at %d units, limit is 30 (must be strictly greater)", (i+1)*3)
		}
	}
	// The 11th crosses.
	if !s.add("heavy", 3) {
		t.Fatal("33 units should cross limit 30 and box")
	}
	if _, boxed := s.boxedRemaining("heavy"); !boxed {
		t.Fatal("key should be boxed after crossing the limit")
	}

	// The same number of responses at weight 1 stays far under the limit.
	for i := 0; i < 11; i++ {
		if s.add("light", 1) {
			t.Fatal("11 weight-1 units must not box at limit 30")
		}
	}
	if _, boxed := s.boxedRemaining("light"); boxed {
		t.Fatal("light key must not be boxed")
	}
}

func TestWindowExpiry(t *testing.T) {
	clk := newFakeClock()
	s := testStore(clk, storeConfig{window: time.Minute, limit: 30})

	// 30 units now; after the window has fully passed they must not count.
	for i := 0; i < 10; i++ {
		s.add("k", 3)
	}
	clk.Advance(61 * time.Second)
	if s.add("k", 3) {
		t.Fatal("old units should have decayed out of the window")
	}
	if e := s.shardFor("k").entries["k"]; e.total != 3 {
		t.Fatalf("expected only the fresh 3 units, got total %d", e.total)
	}
}

func TestWindowPartialDecay(t *testing.T) {
	clk := newFakeClock()
	s := testStore(clk, storeConfig{window: 16 * time.Second, limit: 100})

	// One unit per bucket (1s buckets), then advance half the window:
	// roughly half the units should survive.
	for i := 0; i < 16; i++ {
		s.add("k", 1)
		clk.Advance(time.Second)
	}
	clk.Advance(8 * time.Second)
	s.add("k", 1)
	e := s.shardFor("k").entries["k"]
	// After 8s of decay past a full ring, 16-9=7 old units + 1 new ± ring
	// boundary effects; assert the important property: strictly between
	// fresh-only and everything-retained.
	if e.total <= 1 || e.total >= 17 {
		t.Fatalf("expected partial decay, got total %d", e.total)
	}
}

func TestBoxExpiryAndRemaining(t *testing.T) {
	clk := newFakeClock()
	s := testStore(clk, storeConfig{limit: 5, penaltyTTL: 10 * time.Second})

	s.add("k", 3)
	if !s.add("k", 3) {
		t.Fatal("6 units should cross limit 5")
	}
	remaining, boxed := s.boxedRemaining("k")
	if !boxed || remaining != 10*time.Second {
		t.Fatalf("expected boxed with 10s remaining, got %v, %v", remaining, boxed)
	}

	clk.Advance(4 * time.Second)
	remaining, boxed = s.boxedRemaining("k")
	if !boxed || remaining != 6*time.Second {
		t.Fatalf("expected 6s remaining, got %v, %v", remaining, boxed)
	}

	clk.Advance(7 * time.Second)
	if _, boxed = s.boxedRemaining("k"); boxed {
		t.Fatal("box should have expired")
	}
}

func TestBoxNotExtendedByTraffic(t *testing.T) {
	clk := newFakeClock()
	s := testStore(clk, storeConfig{limit: 5, penaltyTTL: 10 * time.Second})

	s.add("k", 3)
	s.add("k", 3) // boxed at t0, expires t0+10s

	clk.Advance(5 * time.Second)
	if s.add("k", 3) {
		t.Fatal("traffic during the box must not re-box")
	}
	remaining, boxed := s.boxedRemaining("k")
	if !boxed || remaining != 5*time.Second {
		t.Fatalf("box TTL must be fixed: expected 5s remaining, got %v, %v", remaining, boxed)
	}

	// After expiry the budget starts fresh — the mid-box add didn't count.
	clk.Advance(6 * time.Second)
	if s.add("k", 3) {
		t.Fatal("first add after box expiry must not box (budget restarted)")
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		remaining time.Duration
		want      int
	}{
		{2*time.Second + time.Millisecond, 3},
		{2 * time.Second, 2},
		{200 * time.Millisecond, 1},
		{0, 1},
	}
	for _, c := range cases {
		if got := retryAfterSeconds(c.remaining); got != c.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", c.remaining, got, c.want)
		}
	}
}

func TestEvictionUnderCapPressure(t *testing.T) {
	clk := newFakeClock()
	// 256 max keys = 4 per shard.
	s := testStore(clk, storeConfig{limit: 5, maxKeys: 256, penaltyTTL: time.Hour})

	// Box one key, then flood with fresh keys.
	s.add("boxed-key", 3)
	s.add("boxed-key", 3)
	if _, boxed := s.boxedRemaining("boxed-key"); !boxed {
		t.Fatal("setup: key should be boxed")
	}

	for i := 0; i < 10_000; i++ {
		clk.Advance(time.Millisecond) // distinct lastSeen for deterministic eviction order
		s.add(fmt.Sprintf("flood-%d", i), 2)
	}

	if got := s.size(); got > 256 {
		t.Fatalf("key cap is a hard bound: %d tracked keys > 256", got)
	}
	// The actively boxed entry must survive while unboxed candidates exist.
	if _, boxed := s.boxedRemaining("boxed-key"); !boxed {
		t.Fatal("actively boxed key was evicted despite unboxed candidates")
	}
}

func TestSweepRemovesIdleAndExpired(t *testing.T) {
	clk := newFakeClock()
	s := testStore(clk, storeConfig{window: time.Minute, limit: 5, penaltyTTL: 30 * time.Second})

	s.add("idle", 2)
	s.add("boxed", 3)
	s.add("boxed", 3)

	clk.Advance(10 * time.Second)
	s.add("boxed2", 3)
	s.add("boxed2", 3) // boxed until t0+40s, lastSeen t0+10s

	clk.Advance(65 * time.Second) // now t0+75s
	s.sweepAll()

	sh := s.shardFor("idle")
	sh.mu.RLock()
	_, idlePresent := sh.entries["idle"]
	sh.mu.RUnlock()
	if idlePresent {
		t.Fatal("idle entry should have been swept")
	}

	// boxed2's box expired at t0+40s and it has now been idle 65s > window.
	sh = s.shardFor("boxed2")
	sh.mu.RLock()
	_, present := sh.entries["boxed2"]
	sh.mu.RUnlock()
	if present {
		t.Fatal("expired-box idle entry should have been swept")
	}
}

func TestSweepKeepsActiveBox(t *testing.T) {
	clk := newFakeClock()
	s := testStore(clk, storeConfig{window: time.Second, limit: 5, penaltyTTL: time.Hour})

	s.add("k", 3)
	s.add("k", 3)
	clk.Advance(time.Minute) // way past the window, box still active
	s.sweepAll()
	if _, boxed := s.boxedRemaining("k"); !boxed {
		t.Fatal("sweep must never remove an actively boxed entry")
	}
}

func TestSweeperLifecycle(t *testing.T) {
	s := testStore(realClock{}, storeConfig{})
	s.startSweeper()
	s.stop() // must return promptly without deadlock or leak
}

func BenchmarkBoxedRemainingMiss(b *testing.B) {
	s := testStore(realClock{}, storeConfig{})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.boxedRemaining("198.51.100.42")
	}
}
