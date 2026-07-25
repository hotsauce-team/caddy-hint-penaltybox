package penaltybox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// serveReq runs one request through h.ServeHTTP with a Caddy replacer in
// context (as caddyhttp does for real requests). clientKey is exposed as
// the {test.client} placeholder.
func serveReq(t *testing.T, h *Handler, clientKey string, next caddyhttp.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	repl := caddy.NewReplacer()
	repl.Set("test.client", clientKey)
	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl))
	rec := httptest.NewRecorder()
	if err := h.ServeHTTP(rec, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return rec
}

func levelNext(level string) caddyhttp.Handler {
	return caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		if level != "" {
			w.Header().Set("X-Rate-Limit-Level", level)
		}
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("ok"))
		return err
	})
}

func TestServeHTTPBoxesAndRejects(t *testing.T) {
	clk := newFakeClock()
	h, _ := newTestHandler(clk, storeConfig{limit: 5, penaltyTTL: 10 * time.Second})
	h.Key = "{test.client}"
	h.Status = http.StatusTooManyRequests

	// Two level-3 responses (6 units) cross limit 5.
	for i := 0; i < 2; i++ {
		rec := serveReq(t, h, "client-a", levelNext("3"))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d should pass, got %d", i, rec.Code)
		}
		if rec.Header().Get("X-Rate-Limit-Level") != "" {
			t.Fatal("hint header must be stripped from counted responses")
		}
	}

	// Now boxed: enforced before the next handler runs.
	nextCalled := false
	rec := serveReq(t, h, "client-a", caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		nextCalled = true
		return nil
	}))
	if nextCalled {
		t.Fatal("boxed request must not reach the next handler")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 10 {
		t.Fatalf("Retry-After must be an integer in (0, ttl], got %q", rec.Header().Get("Retry-After"))
	}

	// Mid-box probe gets a smaller, honest Retry-After.
	clk.Advance(7 * time.Second)
	rec = serveReq(t, h, "client-a", levelNext(""))
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("expected honest remaining Retry-After 3, got %q", got)
	}

	// After expiry, traffic flows again.
	clk.Advance(4 * time.Second)
	rec = serveReq(t, h, "client-a", levelNext("1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after box expiry, got %d", rec.Code)
	}
}

func TestServeHTTPClientIsolation(t *testing.T) {
	clk := newFakeClock()
	h, _ := newTestHandler(clk, storeConfig{limit: 5, penaltyTTL: time.Minute})
	h.Key = "{test.client}"
	h.Status = http.StatusTooManyRequests

	serveReq(t, h, "client-a", levelNext("3"))
	serveReq(t, h, "client-a", levelNext("3"))
	if rec := serveReq(t, h, "client-a", levelNext("1")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client-a should be boxed, got %d", rec.Code)
	}
	if rec := serveReq(t, h, "client-b", levelNext("3")); rec.Code != http.StatusOK {
		t.Fatalf("boxing client-a must not affect client-b, got %d", rec.Code)
	}
}

func TestServeHTTPLevel1NeverAllocates(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	h.Key = "{test.client}"

	for i := 0; i < 50; i++ {
		serveReq(t, h, "client-a", levelNext("1"))
		serveReq(t, h, "client-b", levelNext(""))
	}
	if got := st.size(); got != 0 {
		t.Fatalf("level-1-only traffic must never allocate counters, got %d", got)
	}
}

func TestServeHTTPEmptyKeyFailsOpen(t *testing.T) {
	h, _ := newTestHandler(newFakeClock(), storeConfig{})
	h.Key = "{test.missing}" // resolves to empty

	rec := serveReq(t, h, "unused", levelNext("3"))
	if rec.Code != http.StatusOK {
		t.Fatalf("unresolvable key must fail open, got %d", rec.Code)
	}
}

func TestValidate(t *testing.T) {
	valid := Handler{
		Header:     "X-Rate-Limit-Level",
		MinLevel:   2,
		Window:     caddy.Duration(time.Minute),
		Limit:      30,
		PenaltyTTL: caddy.Duration(5 * time.Minute),
		Status:     429,
		MaxKeys:    1000,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*Handler)
	}{
		{"empty header", func(h *Handler) { h.Header = "" }},
		{"negative window", func(h *Handler) { h.Window = caddy.Duration(-time.Second) }},
		{"negative ttl", func(h *Handler) { h.PenaltyTTL = caddy.Duration(-time.Second) }},
		{"zero limit", func(h *Handler) { h.Limit = -1 }},
		{"min_level 0", func(h *Handler) { h.MinLevel = 0 }},
		{"min_level 4", func(h *Handler) { h.MinLevel = 4 }},
		{"status 200", func(h *Handler) { h.Status = 200 }},
		{"status 600", func(h *Handler) { h.Status = 600 }},
		{"negative max_keys", func(h *Handler) { h.MaxKeys = -5 }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			bad := valid
			m.mutate(&bad)
			if err := bad.Validate(); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

// BenchmarkUnboxedRequestPhase measures the hot path: an unboxed key's
// request-phase check. Informational per the design doc ("benchmark, not
// a hard gate") — expect 0 allocs/op.
func BenchmarkUnboxedRequestPhase(b *testing.B) {
	s := testStore(realClock{}, storeConfig{})
	s.add("203.0.113.7", 2) // existing but unboxed entry
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.boxedRemaining("203.0.113.7")
	}
}
