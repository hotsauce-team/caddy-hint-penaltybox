package penaltybox

import (
	"net/http"

	"go.uber.org/zap"
)

// parseLevel applies the wire contract strictly: exactly one header
// value that is exactly "1", "2", or "3". Anything else — absent,
// garbage, out of range, padded, multi-valued — is level 1 (contract:
// absence = 1; garbage must not crash or count).
func parseLevel(vals []string) int {
	if len(vals) != 1 {
		return 1
	}
	switch vals[0] {
	case "1":
		return 1
	case "2":
		return 2
	case "3":
		return 3
	}
	return 1
}

// hintInterceptor reads (and strips) the hint header at the last moment
// it is both visible and mutable: just before response headers flush to
// the client. Bodies are never buffered — header-time interception only.
type hintInterceptor struct {
	http.ResponseWriter
	handler     *Handler
	key         string
	intercepted bool
}

func (rw *hintInterceptor) WriteHeader(status int) {
	// Interim 1xx responses (100 Continue, 102 Processing, 103 Early
	// Hints) don't carry the final header set — the hint arrives with the
	// final status — so pass them through untouched. 101 Switching
	// Protocols is NOT interim: it is the final response of an upgrade
	// (e.g. WebSocket), so it must be intercepted like any other final
	// status or the hint would leak and never count.
	switch status {
	case http.StatusContinue, http.StatusProcessing, http.StatusEarlyHints:
		rw.ResponseWriter.WriteHeader(status)
		return
	}
	rw.intercept()
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *hintInterceptor) Write(b []byte) (int, error) {
	// An implicit 200 flushes headers on the first Write; intercept first.
	rw.intercept()
	return rw.ResponseWriter.Write(b)
}

// finalize covers handlers that return without writing anything: headers
// haven't flushed, so the hint can still be read and stripped before
// Caddy's error handling (or an empty 200) takes over.
func (rw *hintInterceptor) finalize() { rw.intercept() }

func (rw *hintInterceptor) intercept() {
	if rw.intercepted {
		return
	}
	rw.intercepted = true
	h := rw.handler
	level := parseLevel(rw.Header()[h.headerCanon])
	if h.stripOn {
		delete(rw.Header(), h.headerCanon)
	}
	if level >= h.MinLevel {
		if h.store.add(rw.key, level) {
			h.logger.Debug("client boxed",
				zap.String("key", rw.key),
				zap.Int("level", level))
		}
	}
}

// Unwrap lets http.NewResponseController reach Flush/Hijack on the
// underlying writer; no legacy interface shims needed on Caddy ≥2.7.
func (rw *hintInterceptor) Unwrap() http.ResponseWriter { return rw.ResponseWriter }
