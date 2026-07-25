package penaltybox

import (
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// The SECURITY.md-shaped scenario the feature exists for: a tight,
// long-window tier-3 budget (logins) and a loose, short-window tier-2
// budget (elevated admin traffic) that a single (window, limit) pair
// cannot express together.
func twoTierStore(clk clock) *store {
	return newStore(storeConfig{
		window:     time.Minute,
		limit:      30,
		penaltyTTL: 5 * time.Minute,
		maxKeys:    1000,
		tiers: map[int]tierSpec{
			3: {window: 15 * time.Minute, limit: 5, penaltyTTL: 30 * time.Minute},
			2: {window: time.Minute, limit: 30, penaltyTTL: 5 * time.Minute},
		},
	}, clk)
}

func TestTierCountsResponsesNotUnits(t *testing.T) {
	clk := newFakeClock()
	s := twoTierStore(clk)

	// Tier 3 limit 5 means five level-3 responses — not 5 weighted units.
	for i := 0; i < 5; i++ {
		if s.add("k", 3) {
			t.Fatalf("boxed after %d level-3 responses, tier limit is 5 (strictly greater)", i+1)
		}
	}
	if !s.add("k", 3) {
		t.Fatal("6th level-3 response should cross tier-3 limit 5")
	}
	remaining, boxed := s.boxedRemaining("k")
	if !boxed || remaining != 30*time.Minute {
		t.Fatalf("expected tier-3 box with 30m remaining, got %v, %v", remaining, boxed)
	}
}

func TestTierBudgetsAreIndependent(t *testing.T) {
	clk := newFakeClock()
	s := twoTierStore(clk)

	// 25 level-2 responses: well inside tier 2's limit of 30, and they
	// must not consume any of tier 3's tiny budget.
	for i := 0; i < 25; i++ {
		if s.add("k", 2) {
			t.Fatalf("level-2 response %d must not box", i+1)
		}
	}
	// Tier 3 still has its full budget of 5.
	for i := 0; i < 5; i++ {
		if s.add("k", 3) {
			t.Fatalf("level-3 response %d must not box: tier 3 budget must be untouched by level-2 traffic", i+1)
		}
	}
	if !s.add("k", 3) {
		t.Fatal("tier 3 should box on its own 6th response")
	}
}

func TestTierWindowsAreIndependent(t *testing.T) {
	clk := newFakeClock()
	s := twoTierStore(clk)

	// Fill most of tier 3's 15m budget, then wait past tier 2's 60s
	// window but well inside tier 3's.
	for i := 0; i < 5; i++ {
		s.add("k", 3)
	}
	clk.Advance(5 * time.Minute)
	// Tier 2's window has fully decayed; tier 3's has not.
	if !s.add("k", 3) {
		t.Fatal("tier 3's 15m window must still hold the earlier responses")
	}
}

func TestTierFallbackNearestBelow(t *testing.T) {
	clk := newFakeClock()
	// Only tier 2 configured: level-3 responses are at least as
	// sensitive, so they count into tier 2 (one unit per response).
	s := newStore(storeConfig{
		window: time.Minute, limit: 30, penaltyTTL: 5 * time.Minute, maxKeys: 1000,
		tiers: map[int]tierSpec{
			2: {window: time.Minute, limit: 3, penaltyTTL: time.Minute},
		},
	}, clk)

	s.add("k", 3)
	s.add("k", 2)
	s.add("k", 3)
	if s.add("k", 2) != true {
		t.Fatal("4th response (mixed levels 2/3) should cross the shared tier-2 limit 3")
	}
}

func TestTierFallbackToDefaultBudget(t *testing.T) {
	clk := newFakeClock()
	// Only tier 3 configured: level-2 responses fall to the default
	// budget, which keeps weighted semantics (2 units each).
	s := newStore(storeConfig{
		window: time.Minute, limit: 30, penaltyTTL: 5 * time.Minute, maxKeys: 1000,
		tiers: map[int]tierSpec{
			3: {window: time.Minute, limit: 100, penaltyTTL: time.Minute},
		},
	}, clk)

	for i := 0; i < 15; i++ {
		if s.add("k", 2) {
			t.Fatalf("response %d: 30 weighted units is at the default limit, not over", i+1)
		}
	}
	if !s.add("k", 2) {
		t.Fatal("16th level-2 response (32 weighted units) should cross default limit 30")
	}
}

func TestTierBoxingResetsOnlyThatTier(t *testing.T) {
	clk := newFakeClock()
	s := newStore(storeConfig{
		window: time.Minute, limit: 30, penaltyTTL: 5 * time.Minute, maxKeys: 1000,
		tiers: map[int]tierSpec{
			2: {window: 10 * time.Minute, limit: 100, penaltyTTL: time.Minute},
			3: {window: time.Minute, limit: 2, penaltyTTL: 2 * time.Second},
		},
	}, clk)

	// Load tier 2 partway, then box via tier 3.
	for i := 0; i < 50; i++ {
		s.add("k", 2)
	}
	s.add("k", 3)
	s.add("k", 3)
	if !s.add("k", 3) {
		t.Fatal("3rd level-3 should box")
	}

	// After the short tier-3 box expires, tier 2's window (10m) still
	// remembers its 50 responses.
	clk.Advance(3 * time.Second)
	if _, boxed := s.boxedRemaining("k"); boxed {
		t.Fatal("tier-3 box should have expired")
	}
	e := s.shardFor("k").entries["k"]
	tier2Slot := s.levelSlot[2]
	if got := e.counters[tier2Slot].total; got != 50 {
		t.Fatalf("tier 2's counter must survive a tier-3 box, got %d", got)
	}
	tier3Slot := s.levelSlot[3]
	if got := e.counters[tier3Slot].total; got != 0 {
		t.Fatalf("tier 3's counter must reset on boxing, got %d", got)
	}
}

func TestTierBoxTTLComesFromCrossingTier(t *testing.T) {
	clk := newFakeClock()
	s := newStore(storeConfig{
		window: time.Minute, limit: 30, penaltyTTL: 5 * time.Minute, maxKeys: 1000,
		tiers: map[int]tierSpec{
			2: {window: time.Minute, limit: 1, penaltyTTL: 10 * time.Second},
			3: {window: time.Minute, limit: 1, penaltyTTL: 10 * time.Minute},
		},
	}, clk)

	// Only one box can be active per entry (the entry-level box gates
	// further counting), so what matters is that each box carries the
	// TTL of the tier whose limit was crossed.
	s.add("k", 3)
	s.add("k", 3) // boxed for 10m
	remaining, boxed := s.boxedRemaining("k")
	if !boxed || remaining != 10*time.Minute {
		t.Fatalf("expected the tier-3 box TTL, got %v, %v", remaining, boxed)
	}

	// A different client boxed via tier 2 gets the short TTL.
	s.add("k2", 2)
	s.add("k2", 2)
	remaining, boxed = s.boxedRemaining("k2")
	if !boxed || remaining != 10*time.Second {
		t.Fatalf("expected the tier-2 box TTL, got %v, %v", remaining, boxed)
	}
}

func TestTierLazyAllocation(t *testing.T) {
	clk := newFakeClock()
	s := twoTierStore(clk)

	s.add("k", 2)
	e := s.shardFor("k").entries["k"]
	if e.counters[s.levelSlot[2]] == nil {
		t.Fatal("tier 2 counter should be allocated")
	}
	if e.counters[s.levelSlot[3]] != nil {
		t.Fatal("tier 3 counter must not be allocated for level-2-only traffic")
	}
	if e.counters[0] != nil {
		t.Fatal("default counter must not be allocated when all levels have tiers")
	}
}

func TestTierSweepUsesLongestWindow(t *testing.T) {
	clk := newFakeClock()
	s := twoTierStore(clk) // tier 3 window 15m > default 1m

	s.add("k", 3)
	clk.Advance(10 * time.Minute) // idle past default window, inside tier 3's
	s.sweepAll()
	if s.size() != 1 {
		t.Fatal("entry with live tier-3 window state must not be swept")
	}
	clk.Advance(6 * time.Minute) // now idle 16m > 15m
	s.sweepAll()
	if s.size() != 0 {
		t.Fatal("entry idle past the longest window should be swept")
	}
}

func TestCaddyfileTierBlocks(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hint_penaltybox {
		tier 3 {
			window      15m
			limit       15
			penalty_ttl 30m
		}
		tier 2 {
			window      60s
			limit       30
			penalty_ttl 5m
		}
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	t3, ok := h.Tiers[3]
	if !ok || time.Duration(t3.Window) != 15*time.Minute || t3.Limit != 15 || time.Duration(t3.PenaltyTTL) != 30*time.Minute {
		t.Fatalf("tier 3 = %+v", t3)
	}
	t2, ok := h.Tiers[2]
	if !ok || time.Duration(t2.Window) != time.Minute || t2.Limit != 30 || time.Duration(t2.PenaltyTTL) != 5*time.Minute {
		t.Fatalf("tier 2 = %+v", t2)
	}
}

func TestCaddyfileTierErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"bad level", `hint_penaltybox {
			tier three {
				limit 5
			}
		}`},
		{"missing level", `hint_penaltybox {
			tier {
				limit 5
			}
		}`},
		{"duplicate tier", `hint_penaltybox {
			tier 3 {
				limit 5
			}
			tier 3 {
				limit 6
			}
		}`},
		{"unknown tier subdirective", `hint_penaltybox {
			tier 3 {
				bogus 5
			}
		}`},
		{"bad tier duration", `hint_penaltybox {
			tier 3 {
				window banana
			}
		}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h Handler
			if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(c.input)); err == nil {
				t.Error("expected parse error")
			}
		})
	}
}

func TestTierValidation(t *testing.T) {
	base := Handler{
		Header:     "X-Rate-Limit-Level",
		MinLevel:   2,
		Window:     caddy.Duration(time.Minute),
		Limit:      30,
		PenaltyTTL: caddy.Duration(5 * time.Minute),
		Status:     429,
		MaxKeys:    1000,
	}

	valid := base
	valid.Tiers = map[int]TierConfig{
		3: {Window: caddy.Duration(15 * time.Minute), Limit: 5, PenaltyTTL: caddy.Duration(30 * time.Minute)},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid tier config rejected: %v", err)
	}

	cases := []struct {
		name  string
		tiers map[int]TierConfig
	}{
		{"level 0", map[int]TierConfig{0: {Window: caddy.Duration(time.Minute), Limit: 5, PenaltyTTL: caddy.Duration(time.Minute)}}},
		{"level 4", map[int]TierConfig{4: {Window: caddy.Duration(time.Minute), Limit: 5, PenaltyTTL: caddy.Duration(time.Minute)}}},
		{"below min_level", map[int]TierConfig{1: {Window: caddy.Duration(time.Minute), Limit: 5, PenaltyTTL: caddy.Duration(time.Minute)}}},
		{"zero window", map[int]TierConfig{3: {Limit: 5, PenaltyTTL: caddy.Duration(time.Minute)}}},
		{"negative limit", map[int]TierConfig{3: {Window: caddy.Duration(time.Minute), Limit: -1, PenaltyTTL: caddy.Duration(time.Minute)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := base
			bad.Tiers = c.tiers
			if err := bad.Validate(); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestProvisionTierInheritsTopLevelDefaults(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()

	h := Handler{Tiers: map[int]TierConfig{3: {Limit: 5}}}
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	}()

	t3 := h.Tiers[3]
	if time.Duration(t3.Window) != time.Minute || t3.Limit != 5 || time.Duration(t3.PenaltyTTL) != 5*time.Minute {
		t.Fatalf("tier should inherit top-level window/penalty_ttl defaults: %+v", t3)
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("inherited tier must validate: %v", err)
	}
}
