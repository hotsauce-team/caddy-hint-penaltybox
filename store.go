package penaltybox

import (
	"sync"
	"time"
)

const (
	numShards  = 64 // power of two; FNV hash masked onto this
	numBuckets = 16 // sliding-window resolution: window/16 per bucket
)

// boxStore is what the handler talks to. It is deliberately small so a
// distributed implementation (e.g. backed by Caddy storage, the way
// caddy-ratelimit's distributed mode works) can replace the in-memory
// store without touching handler code or config.
type boxStore interface {
	// boxedRemaining reports whether key is actively boxed and, if so,
	// how long until the box expires.
	boxedRemaining(key string) (time.Duration, bool)
	// add records the given number of weighted units against key's
	// sliding window. It returns true when this call pushed the window
	// total over the limit and boxed the key.
	add(key string, units int) bool
	// stop halts background maintenance (the sweeper goroutine).
	stop()
}

type storeConfig struct {
	window     time.Duration
	limit      int
	penaltyTTL time.Duration
	maxKeys    int
}

type store struct {
	shards      [numShards]shard
	window      time.Duration
	bucketDur   time.Duration
	limit       uint64
	penaltyTTL  time.Duration
	maxPerShard int
	clk         clock

	done chan struct{}
	wg   sync.WaitGroup
}

type shard struct {
	mu      sync.RWMutex
	entries map[string]*entry
}

// entry is one tracked client: a 16-bucket ring of weighted units
// covering the sliding window, plus box state. ~150 bytes regardless of
// traffic volume.
type entry struct {
	buckets    [numBuckets]uint32
	head       int       // index of the bucket covering headStart..headStart+bucketDur
	headStart  time.Time // start of the head bucket
	total      uint64    // running sum of live buckets
	lastSeen   time.Time // for oldest-first eviction
	boxedUntil time.Time // zero = not boxed
}

func newStore(cfg storeConfig, clk clock) *store {
	s := &store{
		window:      cfg.window,
		bucketDur:   max(cfg.window/numBuckets, 1),
		limit:       uint64(cfg.limit),
		penaltyTTL:  cfg.penaltyTTL,
		maxPerShard: max(cfg.maxKeys/numShards, 1),
		clk:         clk,
		done:        make(chan struct{}),
	}
	for i := range s.shards {
		s.shards[i].entries = make(map[string]*entry)
	}
	return s
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
func (s *store) add(key string, units int) bool {
	now := s.clk.Now()
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e, ok := sh.entries[key]
	if !ok {
		if len(sh.entries) >= s.maxPerShard {
			s.makeRoomLocked(sh, now)
		}
		e = &entry{headStart: now}
		sh.entries[key] = e
	}
	e.lastSeen = now

	// Fixed-TTL policy (Fastly semantics): traffic while boxed neither
	// counts nor extends the penalty.
	if e.boxedUntil.After(now) {
		return false
	}

	e.advance(now, s.bucketDur)
	e.buckets[e.head] += uint32(units)
	e.total += uint64(units)

	if e.total > s.limit {
		e.boxedUntil = now.Add(s.penaltyTTL)
		// The budget restarts from zero once the box expires.
		e.buckets = [numBuckets]uint32{}
		e.total = 0
		e.head = 0
		e.headStart = now
		// metrics hook (v1.1): boxed_total would increment here.
		return true
	}
	return false
}

// advance rotates the ring so the head bucket covers now, dropping
// buckets that have slid out of the window.
func (e *entry) advance(now time.Time, bucketDur time.Duration) {
	elapsed := now.Sub(e.headStart)
	if elapsed < bucketDur {
		return
	}
	steps := int(elapsed / bucketDur)
	if steps >= numBuckets {
		e.buckets = [numBuckets]uint32{}
		e.total = 0
		e.head = 0
		e.headStart = now
		return
	}
	for i := 0; i < steps; i++ {
		e.head = (e.head + 1) % numBuckets
		e.total -= uint64(e.buckets[e.head])
		e.buckets[e.head] = 0
	}
	e.headStart = e.headStart.Add(time.Duration(steps) * bucketDur)
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
// been idle longer than the window (their counters have fully decayed).
func (s *store) sweepShardLocked(sh *shard, now time.Time) {
	for k, e := range sh.entries {
		if e.boxedUntil.After(now) {
			continue
		}
		if now.Sub(e.lastSeen) > s.window {
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
	interval := max(s.window, s.penaltyTTL) / 4
	interval = min(max(interval, time.Second), time.Minute)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(interval)
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
