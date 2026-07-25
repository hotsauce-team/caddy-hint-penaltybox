package penaltybox

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
)

// newTestHandler builds a Handler wired to a fake-clock store without
// going through caddy.Context provisioning.
func newTestHandler(clk clock, cfg storeConfig) (*Handler, *store) {
	st := testStore(clk, cfg)
	h := &Handler{
		Header:      "X-Rate-Limit-Level",
		MinLevel:    2,
		headerCanon: "X-Rate-Limit-Level",
		stripOn:     true,
		store:       st,
		logger:      zap.NewNop(),
	}
	return h, st
}

func interceptorFor(h *Handler, key string, w http.ResponseWriter) *hintInterceptor {
	return &hintInterceptor{ResponseWriter: w, handler: h, key: key}
}

func TestInterceptExplicitWriteHeader(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	rw.Header().Set("X-Rate-Limit-Level", "3")
	rw.WriteHeader(http.StatusOK)

	if got := rec.Header().Get("X-Rate-Limit-Level"); got != "" {
		t.Errorf("header should be stripped, got %q", got)
	}
	if st.size() != 1 {
		t.Errorf("level-3 response should have allocated a counter")
	}
}

func TestInterceptImplicitWriteHeaderOnWrite(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	rw.Header().Set("X-Rate-Limit-Level", "2")
	if _, err := rw.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}

	if got := rec.Header().Get("X-Rate-Limit-Level"); got != "" {
		t.Errorf("header should be stripped on implicit 200, got %q", got)
	}
	if st.size() != 1 {
		t.Errorf("level-2 response should have counted")
	}
	if rec.Body.String() != "body" {
		t.Errorf("body must pass through unbuffered, got %q", rec.Body.String())
	}
}

func TestInterceptNoWriteAtAll(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	// Handler set the header but returned without writing.
	rw.Header().Set("X-Rate-Limit-Level", "3")
	rw.finalize()

	if got := rec.Header().Get("X-Rate-Limit-Level"); got != "" {
		t.Errorf("header should be stripped even when nothing was written, got %q", got)
	}
	if st.size() != 1 {
		t.Errorf("finalize should still count the hint")
	}
}

func TestInterceptIdempotent(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	rw.Header().Set("X-Rate-Limit-Level", "3")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte("a"))
	_, _ = rw.Write([]byte("b"))
	rw.finalize()

	e := st.shardFor("k").entries["k"]
	if e.total != 3 {
		t.Errorf("hint must be counted exactly once, got %d units", e.total)
	}
}

func TestIntercept1xxPassthrough(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	// An informational response before the final one must not trigger
	// interception; the hint arrives with the final header set.
	rw.WriteHeader(http.StatusEarlyHints)
	if rw.intercepted {
		t.Fatal("1xx must not trigger interception")
	}
	if st.size() != 0 {
		t.Fatal("1xx must not count")
	}

	rw.Header().Set("X-Rate-Limit-Level", "3")
	rw.WriteHeader(http.StatusOK)
	if got := rec.Header().Get("X-Rate-Limit-Level"); got != "" {
		t.Errorf("final response header should be stripped, got %q", got)
	}
	if st.size() != 1 {
		t.Errorf("final response should have counted")
	}
}

func TestIntercept101IsFinal(t *testing.T) {
	// 101 Switching Protocols is the final response of an upgrade, not
	// an interim 1xx: the hint must be counted and stripped.
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	rw.Header().Set("X-Rate-Limit-Level", "3")
	rw.WriteHeader(http.StatusSwitchingProtocols)

	if !rw.intercepted {
		t.Fatal("101 must trigger interception")
	}
	if got := rec.Header().Get("X-Rate-Limit-Level"); got != "" {
		t.Errorf("hint must be stripped on 101, got %q", got)
	}
	if st.size() != 1 {
		t.Error("level-3 hint on a 101 response must count")
	}
}

func TestStripDisabled(t *testing.T) {
	h, _ := newTestHandler(newFakeClock(), storeConfig{})
	h.stripOn = false
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	rw.Header().Set("X-Rate-Limit-Level", "3")
	rw.WriteHeader(http.StatusOK)

	if got := rec.Header().Get("X-Rate-Limit-Level"); got != "3" {
		t.Errorf("with strip off the header must pass through, got %q", got)
	}
}

func TestStripRemovesAllValues(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	rw.Header().Add("X-Rate-Limit-Level", "3")
	rw.Header().Add("X-Rate-Limit-Level", "3")
	rw.WriteHeader(http.StatusOK)

	if vals := rec.Header().Values("X-Rate-Limit-Level"); len(vals) != 0 {
		t.Errorf("all header values must be stripped, got %v", vals)
	}
	// Multi-value is a contract violation → level 1 → below MinLevel 2.
	if st.size() != 0 {
		t.Errorf("multi-value header must not count")
	}
}

func TestBelowMinLevelNeverAllocates(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		rw := interceptorFor(h, "k", rec)
		rw.Header().Set("X-Rate-Limit-Level", "1")
		rw.WriteHeader(http.StatusOK)

		rec2 := httptest.NewRecorder()
		rw2 := interceptorFor(h, "k", rec2)
		rw2.WriteHeader(http.StatusOK) // absent header = level 1
	}
	if got := st.size(); got != 0 {
		t.Fatalf("level-1-only traffic must never allocate counters, got %d entries", got)
	}
}

func TestFlushReachesUnderlyingWriter(t *testing.T) {
	h, _ := newTestHandler(newFakeClock(), storeConfig{})
	rec := httptest.NewRecorder()
	rw := interceptorFor(h, "k", rec)

	// Caddy ≥2.7 flushes via http.NewResponseController, which follows
	// Unwrap chains; the shim must not break streaming.
	rc := http.NewResponseController(rw)
	if _, err := rw.Write([]byte("chunk")); err != nil {
		t.Fatal(err)
	}
	if err := rc.Flush(); err != nil {
		t.Fatalf("Flush through the shim failed: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
}

func TestBoxingHappensAtHeaderTime(t *testing.T) {
	clk := newFakeClock()
	h, st := newTestHandler(clk, storeConfig{limit: 5, penaltyTTL: time.Minute})

	// Two level-3 responses cross limit 5; boxing occurs during the
	// second response's WriteHeader — that response itself still passes.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		rw := interceptorFor(h, "k", rec)
		rw.Header().Set("X-Rate-Limit-Level", "3")
		rw.WriteHeader(http.StatusOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("response %d should pass through, got %d", i, rec.Code)
		}
	}
	if _, boxed := st.boxedRemaining("k"); !boxed {
		t.Fatal("key should be boxed after the crossing response")
	}
}
