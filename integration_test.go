//go:build integration

package penaltybox

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
)

// The integration suite drives a real in-process Caddy through the
// caddytest harness. The "upstream" is a subroute in the same config
// that echoes ?level=N back as the hint header — the same wire contract
// the CMS emits. Tests are sequential (fixed ports, one admin endpoint);
// run them inside the dev container where nothing else owns :2019/:9080.

// caddytest's harness polls the admin API on its default port 2999; the
// config must keep the admin endpoint there or the tester can't confirm
// the config went active.
const globalOptions = `{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	https_port 9443
	grace_period 1ns
}
`

// testConfig serves on :9080 with the module keyed by the X-Test-Client
// request header (deterministic client identity without fighting
// trusted-proxy resolution; the {client_ip} path is covered by e2e).
func testConfig(module string) string {
	return globalOptions + `
http://localhost:9080 {
	route {
		` + module + `
		header X-Rate-Limit-Level "{query.level}"
		respond "upstream ok" 200
	}
}
`
}

func get(t *testing.T, tester *caddytest.Tester, client, level string) *http.Response {
	t.Helper()
	url := "http://localhost:9080/"
	if level != "" {
		url += "?level=" + level
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Client", client)
	resp, err := tester.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func assertNoHintHeader(t *testing.T, resp *http.Response, context string) {
	t.Helper()
	if vals := resp.Header.Values("X-Rate-Limit-Level"); len(vals) != 0 {
		t.Errorf("%s: hint header must never reach the client, got %v", context, vals)
	}
}

// Scenario 1: level-3 responses past the limit box the client; the box
// expires after penalty_ttl.
func TestIntegrationBoxingAndRecovery(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(testConfig(`hint_penaltybox {
			key {header.X-Test-Client}
			window 30s
			limit 6
			penalty_ttl 2s
		}`), "caddyfile")

	// 3 + 3 = 6 units: at the limit, still allowed.
	for i := 0; i < 2; i++ {
		if resp := get(t, tester, "alice", "3"); resp.StatusCode != 200 {
			t.Fatalf("request %d: expected 200, got %d", i, resp.StatusCode)
		}
	}
	// 9 units > 6: this response still passes (reactive by design)...
	if resp := get(t, tester, "alice", "3"); resp.StatusCode != 200 {
		t.Fatalf("crossing response itself should pass, got %d", resp.StatusCode)
	}
	// ...but the next request is rejected before reaching the origin.
	resp := get(t, tester, "alice", "3")
	if resp.StatusCode != 429 {
		t.Fatalf("expected 429 for boxed client, got %d", resp.StatusCode)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || ra < 1 || ra > 2 {
		t.Fatalf("Retry-After must be an integer in (0, penalty_ttl], got %q", resp.Header.Get("Retry-After"))
	}
	assertNoHintHeader(t, resp, "429 response")

	// After the TTL the client is allowed again.
	time.Sleep(2500 * time.Millisecond)
	if resp := get(t, tester, "alice", "1"); resp.StatusCode != 200 {
		t.Fatalf("expected 200 after box expiry, got %d", resp.StatusCode)
	}
}

// Scenario 2: level-1-only traffic never boxes.
func TestIntegrationLevel1NeverBoxes(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(testConfig(`hint_penaltybox {
			key {header.X-Test-Client}
			window 30s
			limit 6
			penalty_ttl 1m
		}`), "caddyfile")

	for i := 0; i < 50; i++ {
		if resp := get(t, tester, "bob", "1"); resp.StatusCode != 200 {
			t.Fatalf("request %d: level-1 traffic must never box, got %d", i, resp.StatusCode)
		}
	}
	// Absent header (= level 1) too.
	for i := 0; i < 20; i++ {
		if resp := get(t, tester, "bob", ""); resp.StatusCode != 200 {
			t.Fatalf("request %d: absent-header traffic must never box, got %d", i, resp.StatusCode)
		}
	}
}

// Scenario 3: the hint header never reaches the client — counted,
// ignored, and boxed responses alike; and with strip false it passes.
func TestIntegrationHeaderStripped(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(testConfig(`hint_penaltybox {
			key {header.X-Test-Client}
			window 30s
			limit 6
			penalty_ttl 1m
		}`), "caddyfile")

	assertNoHintHeader(t, get(t, tester, "carol", "3"), "counted (level 3)")
	assertNoHintHeader(t, get(t, tester, "carol", "1"), "ignored (level 1)")
	assertNoHintHeader(t, get(t, tester, "carol", ""), "absent header")
	assertNoHintHeader(t, get(t, tester, "carol", "banana"), "garbage level")

	// Box carol and check the 429 too.
	get(t, tester, "carol", "3")
	get(t, tester, "carol", "3")
	resp := get(t, tester, "carol", "3")
	if resp.StatusCode != 429 {
		t.Fatalf("expected carol boxed, got %d", resp.StatusCode)
	}
	assertNoHintHeader(t, resp, "boxed 429")
}

func TestIntegrationStripDisabled(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(testConfig(`hint_penaltybox {
			key {header.X-Test-Client}
			strip false
			window 30s
			limit 100
			penalty_ttl 1m
		}`), "caddyfile")

	resp := get(t, tester, "dave", "2")
	if got := resp.Header.Get("X-Rate-Limit-Level"); got != "2" {
		t.Fatalf("with strip false the header must pass through, got %q", got)
	}
}

// Scenario 4: distinct keys are isolated — boxing one client does not
// affect another.
func TestIntegrationClientIsolation(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(testConfig(`hint_penaltybox {
			key {header.X-Test-Client}
			window 30s
			limit 6
			penalty_ttl 1m
		}`), "caddyfile")

	for i := 0; i < 3; i++ {
		get(t, tester, "eve", "3")
	}
	if resp := get(t, tester, "eve", "1"); resp.StatusCode != 429 {
		t.Fatalf("eve should be boxed, got %d", resp.StatusCode)
	}
	if resp := get(t, tester, "frank", "3"); resp.StatusCode != 200 {
		t.Fatalf("boxing eve must not affect frank, got %d", resp.StatusCode)
	}
}

// Scenario 5 (strict parsing end-to-end): garbage and out-of-range
// levels are treated as level 1 — they never count, never box, never
// crash the server.
func TestIntegrationGarbageLevelsSafe(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(testConfig(`hint_penaltybox {
			key {header.X-Test-Client}
			window 30s
			limit 6
			penalty_ttl 1m
		}`), "caddyfile")

	for i, level := range []string{"0", "4", "10", "banana", "-1", "3.0", "03"} {
		for j := 0; j < 5; j++ {
			resp := get(t, tester, "mallory", level)
			if resp.StatusCode != 200 {
				t.Fatalf("garbage level %q (iteration %d/%d) must not box, got %d",
					level, i, j, resp.StatusCode)
			}
			assertNoHintHeader(t, resp, fmt.Sprintf("garbage level %q", level))
		}
	}
}

// The module must strip the header even when the origin sets it on an
// error response handled without a body write.
func TestIntegrationMinLevelDefaultIgnoresLevel1(t *testing.T) {
	tester := caddytest.NewTester(t)
	// min_level omitted → default 2; limit tiny so any counting of
	// level-1 traffic would box immediately.
	tester.InitServer(testConfig(`hint_penaltybox {
			key {header.X-Test-Client}
			window 30s
			limit 2
			penalty_ttl 1m
		}`), "caddyfile")

	for i := 0; i < 10; i++ {
		if resp := get(t, tester, "grace", "1"); resp.StatusCode != 200 {
			t.Fatalf("default min_level must ignore level-1, got %d on request %d", resp.StatusCode, i)
		}
	}
}
