package penaltybox

import (
	"sync"
	"time"
)

const (
	numShards  = 64 // power of two; FNV hash masked onto this
	numBuckets = 16 // sliding-window resolution: window/16 per bucket
	maxLevel   = 3  // wire contract: levels are 1..3
)

// boxStore is what the handler talks to. It is deliberately small so a
// distributed implementation (e.g. backed by Caddy storage, the way
// caddy-ratelimit's distributed mode works) can replace the in-memory
// store without touching handler code or config.
type boxStore interface {
	// boxedRemaining reports whether key is actively boxed and, if so,
	// how long until the box expires (the maximum across tiers).
	boxedRemaining(key string) (time.Duration, bool)
	// add records one response of the given hint level against key's
	// budget for that level's tier. It returns true when this call
	// pushed a tier over its limit and boxed the key.
	add(key string, level int) bool
	// stop halts background maintenance (the sweeper goroutine).
	stop()
}

// tierSpec is an explicit per-level budget from config.
type tierSpec struct {
	window     time.Duration
	limit      int
	penaltyTTL time.Duration
}

type storeConfig struct {
	window     time.Duration
	limit      int
	penaltyTTL time.Duration
	maxKeys    int
	tiers      map[int]tierSpec // level (1..3) -> explicit budget
}

// tierBudget is a resolved budget the store enforces. Slot 0 is always
// the default budget (the top-level window/limit/penalty_ttl), which
// keeps the original weighted semantics: a level-N response costs N
// units. Explicit tiers count responses instead (increment 1), because
// within a single-level tier a weight is just a constant multiplier —
// "limit 5" on tier 3 literally means five level-3 responses.
type tierBudget struct {
	window     time.Duration
	limit      uint64
	penaltyTTL time.Duration
	bucketDur  time.Duration
	weighted   bool // true only for the default budget (slot 0)
}

type store struct {
	shards      [numShards]shard
	budgets     []tierBudget      // slot 0 = default; then explicit tiers
	levelSlot   [maxLevel + 1]int // hint level -> budget slot
	maxWindow   time.Duration     // longest window across budgets (sweep idle bound)
	sweepEvery  time.Duration
	maxPerShard int
	clk         clock

	done chan struct{}
	wg   sync.WaitGroup
}

type shard struct {
	mu      sync.RWMutex
	entries map[string]*entry
}

// entry is one tracked client: lazily-allocated per-tier ring counters
// plus box state. boxedUntil is the maximum across tiers — safe to
// maintain incrementally because boxes have fixed TTLs (they only ever
// expire, never shrink).
type entry struct {
	lastSeen   time.Time
	boxedUntil time.Time // zero = not boxed
	counters   []*tierCounter
}

// tierCounter is a 16-bucket ring of units covering one budget's
// sliding window.
type tierCounter struct {
	buckets   [numBuckets]uint32
	head      int       // index of the bucket covering headStart..headStart+bucketDur
	headStart time.Time // start of the head bucket
	total     uint64    // running sum of live buckets
}

func newStore(cfg storeConfig, clk clock) *store {
	s := &store{
		maxPerShard: max(cfg.maxKeys/numShards, 1),
		clk:         clk,
		done:        make(chan struct{}),
	}

	// Slot 0: the default budget, weighted for backward compatibility.
	s.budgets = []tierBudget{{
		window:     cfg.window,
		limit:      uint64(cfg.limit),
		penaltyTTL: cfg.penaltyTTL,
		bucketDur:  max(cfg.window/numBuckets, 1),
		weighted:   true,
	}}
	s.maxWindow = cfg.window
	maxTTL := cfg.penaltyTTL

	slotOf := map[int]int{}
	for level := 1; level <= maxLevel; level++ {
		spec, ok := cfg.tiers[level]
		if !ok {
			continue
		}
		s.budgets = append(s.budgets, tierBudget{
			window:     spec.window,
			limit:      uint64(spec.limit),
			penaltyTTL: spec.penaltyTTL,
			bucketDur:  max(spec.window/numBuckets, 1),
		})
		slotOf[level] = len(s.budgets) - 1
		s.maxWindow = max(s.maxWindow, spec.window)
		maxTTL = max(maxTTL, spec.penaltyTTL)
	}

	// A level uses its own tier if configured, else the nearest
	// configured tier below it (a level-3 response is at least as
	// sensitive as level 2), else the default budget.
	for level := 1; level <= maxLevel; level++ {
		slot := 0
		for l := level; l >= 1; l-- {
			if sl, ok := slotOf[l]; ok {
				slot = sl
				break
			}
		}
		s.levelSlot[level] = slot
	}

	s.sweepEvery = sweepInterval(s.maxWindow, maxTTL)
	for i := range s.shards {
		s.shards[i].entries = make(map[string]*entry)
	}
	return s
}

func sweepInterval(window, ttl time.Duration) time.Duration {
	interval := max(window, ttl) / 4
	return min(max(interval, time.Second), time.Minute)
}

// shardFor hashes key with inline FNV-1a (no []byte conversion, no
// allocation — this sits on the per-request hot path).
func (s *store) shardFor(key string) *shard {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	var h uint64 = offset64
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime64
	}
	return &s.shards[h&(numShards-1)]
}

func (s *store) boxedRemaining(key string) (time.Duration, bool) {
	sh := s.shardFor(key)
	sh.mu.RLock()
	e, ok := sh.entries[key]
	var remaining time.Duration
	if ok {
		remaining = e.boxedUntil.Sub(s.clk.Now())
	}
	sh.mu.RUnlock()
	// Expired boxes are left for the sweeper so this path never needs a
	// write lock.
	if !ok || remaining <= 0 {
		return 0, false
	}
	return remaining, true
}

// Callers must filter out levels below min_level before calling add:
// keys with only low-level traffic must never allocate an entry
// (design requirement).
func (s *store) add(key string, level int) bool {
	if level < 1 || level > maxLevel {
		return false
	}
	now := s.clk.Now()
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e, ok := sh.entries[key]
	if !ok {
		if len(sh.entries) >= s.maxPerShard {
			s.makeRoomLocked(sh, now)
		}
		e = &entry{counters: make([]*tierCounter, len(s.budgets))}
		sh.entries[key] = e
	}
	e.lastSeen = now

	// Fixed-TTL policy (Fastly semantics): traffic while boxed neither
	// counts nor extends the penalty. (While boxed, origin responses
	// only arrive for requests that were in flight before the box shut.)
	if e.boxedUntil.After(now) {
		return false
	}

	slot := s.levelSlot[level]
	budget := &s.budgets[slot]
	tc := e.counters[slot]
	if tc == nil {
		// Lazy: a client only allocates counters for tiers it triggers.
		tc = &tierCounter{headStart: now}
		e.counters[slot] = tc
	}

	tc.advance(now, budget.bucketDur)
	inc := uint32(1)
	if budget.weighted {
		inc = uint32(level)
	}
	tc.buckets[tc.head] += inc
	tc.total += uint64(inc)

	if tc.total > budget.limit {
		boxedUntil := now.Add(budget.penaltyTTL)
		if boxedUntil.After(e.boxedUntil) {
			e.boxedUntil = boxedUntil
		}
		// This tier's budget restarts from zero once the box expires;
		// other tiers keep their windows (which decay naturally).
		*tc = tierCounter{headStart: now}
		// metrics hook (v1.1): boxed_total{tier} would increment here.
		return true
	}
	return false
}

// advance rotates the ring so the head bucket covers now, dropping
// buckets that have slid out of the window.
func (tc *tierCounter) advance(now time.Time, bucketDur time.Duration) {
	elapsed := now.Sub(tc.headStart)
	if elapsed < bucketDur {
		return
	}
	steps := int(elapsed / bucketDur)
	if steps >= numBuckets {
		*tc = tierCounter{headStart: now}
		return
	}
	for i := 0; i < steps; i++ {
		tc.head = (tc.head + 1) % numBuckets
		tc.total -= uint64(tc.buckets[tc.head])
		tc.buckets[tc.head] = 0
	}
	tc.headStart = tc.headStart.Add(time.Duration(steps) * bucketDur)
}

// makeRoomLocked frees at least one slot in a full shard: drop expired
// entries first, then evict the oldest-idle unboxed entry, and as a last
// resort the oldest-idle entry outright — the key cap is a hard bound
// (an attacker rotating keys exhausts the cap into evictions, never into
// unbounded memory).
func (s *store) makeRoomLocked(sh *shard, now time.Time) {
	s.sweepShardLocked(sh, now)
	if len(sh.entries) < s.maxPerShard {
		return
	}
	var oldestKey, oldestUnboxedKey string
	var oldest, oldestUnboxed time.Time
	for k, e := range sh.entries {
		if oldestKey == "" || e.lastSeen.Before(oldest) {
			oldestKey, oldest = k, e.lastSeen
		}
		if !e.boxedUntil.After(now) && (oldestUnboxedKey == "" || e.lastSeen.Before(oldestUnboxed)) {
			oldestUnboxedKey, oldestUnboxed = k, e.lastSeen
		}
	}
	if oldestUnboxedKey != "" {
		delete(sh.entries, oldestUnboxedKey)
	} else if oldestKey != "" {
		delete(sh.entries, oldestKey)
	}
}

// sweepShardLocked removes entries that are not actively boxed and have
// been idle longer than the longest configured window (all their
// counters have fully decayed).
func (s *store) sweepShardLocked(sh *shard, now time.Time) {
	for k, e := range sh.entries {
		if e.boxedUntil.After(now) {
			continue
		}
		if now.Sub(e.lastSeen) > s.maxWindow {
			delete(sh.entries, k)
		}
	}
}

func (s *store) sweepAll() {
	now := s.clk.Now()
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		s.sweepShardLocked(sh, now)
		sh.mu.Unlock()
	}
}

// startSweeper launches the background expiry sweep. The goroutine holds
// no logic of its own (it only calls sweepAll) so tests exercise sweep
// behavior directly with a fake clock instead of waiting on ticks.
func (s *store) startSweeper() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(s.sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.sweepAll()
			case <-s.done:
				return
			}
		}
	}()
}

func (s *store) stop() {
	close(s.done)
	s.wg.Wait()
}

// size reports the total tracked-key count (test helper).
func (s *store) size() int {
	n := 0
	for i := range s.shards {
		s.shards[i].mu.RLock()
		n += len(s.shards[i].entries)
		s.shards[i].mu.RUnlock()
	}
	return n
}
