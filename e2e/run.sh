#!/bin/sh
# End-to-end scenario assertions against the xcaddy-built Caddy.
# Config under test: min_level 2, window 10s, limit 6, penalty_ttl 3s.
# Each scenario uses a distinct X-Forwarded-For client so scenarios
# don't contaminate each other's budgets.
set -u

BASE="http://e2e-caddy:8080"
CACHED_BASE="http://e2e-caddy:8090" # same module + Souin cache (Otter storage)
FAILURES=0

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

# code <client-ip> <query> -> prints HTTP status code
code() {
	curl -s -o /dev/null -w '%{http_code}' -H "X-Forwarded-For: $1" "$BASE/?$2"
}

# headers <client-ip> <query> -> prints raw response headers
headers() {
	curl -s -D - -o /dev/null -H "X-Forwarded-For: $1" "$BASE/?$2"
}

# ccode/cheaders: same, against the cached listener
ccode() {
	curl -s -o /dev/null -w '%{http_code}' -H "X-Forwarded-For: $1" "$CACHED_BASE/?$2"
}

cheaders() {
	curl -s -D - -o /dev/null -H "X-Forwarded-For: $1" "$CACHED_BASE/?$2"
}

# cache_status <client-ip> <query> -> prints the Cache-Status header value
cache_status() {
	cheaders "$1" "$2" | tr -d '\r' | awk -F': ' 'tolower($1)=="cache-status" {print $2}'
}

echo "=== waiting for e2e-caddy to become ready ==="
i=0
until curl -fs -o /dev/null "$BASE/?level=1" 2>/dev/null; do
	i=$((i + 1))
	if [ "$i" -ge 30 ]; then
		echo "FATAL: e2e-caddy did not become ready within 30s"
		exit 1
	fi
	sleep 1
done
echo "ready after ${i}s"

echo "=== scenario 1: weighted boxing (level 3 past the limit -> 429 + Retry-After) ==="
# 3+3=6 units: at the limit; the third response (9 > 6) boxes but passes.
for i in 1 2 3; do
	c=$(code 10.1.1.1 "level=3")
	[ "$c" = "200" ] || fail "request $i should pass while budget lasts, got $c"
done
c=$(code 10.1.1.1 "level=3")
if [ "$c" = "429" ]; then
	pass "boxed client rejected with 429"
else
	fail "expected 429 for boxed client, got $c"
fi

ra=$(headers 10.1.1.1 "level=1" | tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after" {print $2}')
case "$ra" in
'' ) fail "429 response missing Retry-After" ;;
*[!0-9]*) fail "Retry-After is not an integer: '$ra'" ;;
*)
	if [ "$ra" -ge 1 ] && [ "$ra" -le 3 ]; then
		pass "Retry-After is an honest remaining TTL ($ra s)"
	else
		fail "Retry-After $ra outside (0, penalty_ttl=3]"
	fi
	;;
esac

echo "=== scenario 2: client isolation (boxing 10.1.1.1 leaves 10.2.2.2 alone) ==="
c=$(code 10.2.2.2 "level=3")
if [ "$c" = "200" ]; then
	pass "distinct client unaffected by the boxed one"
else
	fail "expected 200 for distinct client, got $c"
fi

echo "=== scenario 3: hint header never reaches the client ==="
# Box a dedicated client so the 429-leak check is deterministic
# regardless of how much wall time earlier scenarios consumed.
for i in 1 2 3; do code 10.3.3.5 "level=3" >/dev/null; done
c=$(code 10.3.3.5 "level=3")
[ "$c" = "429" ] || fail "setup: expected 10.3.3.5 boxed, got $c"

for probe in "10.3.3.3 level=3 counted" "10.3.3.4 level=1 ignored" "10.3.3.5 level=3 boxed-429"; do
	set -- $probe
	if headers "$1" "$2" | grep -qi '^x-rate-limit-level'; then
		fail "hint header leaked on $3 response"
	else
		pass "hint header stripped on $3 response"
	fi
done

echo "=== scenario 4: level-1 traffic never boxes ==="
ok=true
for i in $(seq 1 20); do
	c=$(code 10.4.4.4 "level=1")
	[ "$c" = "200" ] || { ok=false; break; }
done
if $ok; then
	pass "20 level-1 requests all passed"
else
	fail "level-1 traffic was boxed (got $c)"
fi

echo "=== scenario 5: garbage levels are safe ==="
ok=true
for lvl in 0 4 banana -1 03; do
	for i in 1 2 3; do
		c=$(code 10.5.5.5 "level=$lvl")
		[ "$c" = "200" ] || { ok=false; break 2; }
	done
done
if $ok; then
	pass "garbage levels never count or crash"
else
	fail "garbage level boxed or errored (got $c)"
fi

echo "=== scenario 6b: two-tier policy (tight tier 3, loose tier 2) ==="
TIER_BASE="http://e2e-caddy:8091"
tcode() {
	curl -s -o /dev/null -w '%{http_code}' -H "X-Forwarded-For: $1" "$TIER_BASE/?$2"
}
# 20 level-2 responses: inside tier 2's budget of 50, and they must not
# consume tier 3's budget of 2.
ok=true
for i in $(seq 1 20); do
	c=$(tcode 10.6.6.6 "level=2")
	[ "$c" = "200" ] || { ok=false; break; }
done
if $ok; then
	pass "level-2 traffic flows inside its own tier budget"
else
	fail "level-2 traffic boxed prematurely (got $c)"
fi
# Tier 3 budget is untouched: 2 allowed, 3rd crosses, 4th rejected.
for i in 1 2 3; do
	c=$(tcode 10.6.6.6 "level=3")
	[ "$c" = "200" ] || fail "level-3 request $i should pass on its own budget, got $c"
done
c=$(tcode 10.6.6.6 "level=2")
if [ "$c" = "429" ]; then
	pass "tier-3 crossing boxes the client for all traffic"
else
	fail "expected 429 after tier-3 crossing, got $c"
fi
c=$(tcode 10.6.6.7 "level=3")
if [ "$c" = "200" ]; then
	pass "tier boxing is per-client"
else
	fail "expected 200 for distinct client on tiered listener, got $c"
fi

echo "=== scenario 6: box expires after penalty_ttl ==="
sleep 4
c=$(code 10.3.3.5 "level=1")
if [ "$c" = "200" ]; then
	pass "boxed client allowed again after TTL"
else
	fail "expected 200 after box expiry, got $c"
fi

echo "=== scenario 7: Souin cache (Otter storage) works behind the module ==="
cs=$(cache_status 10.7.7.1 "level=1&page=a")
case "$cs" in
*Souin*) pass "Souin answered on first request ($cs)" ;;
*) fail "expected a Souin Cache-Status header, got '$cs'" ;;
esac
cs=$(cache_status 10.7.7.1 "level=1&page=a")
case "$cs" in
*hit*) pass "second request served from cache ($cs)" ;;
*) fail "expected a cache hit on second request, got '$cs'" ;;
esac

echo "=== scenario 8: hint header stripped on cache MISS and HIT alike ==="
# Fresh URL: first response is a miss (stored), second (other client) a hit.
if cheaders 10.7.7.1 "level=2&page=b" | grep -qi '^x-rate-limit-level'; then
	fail "hint header leaked on cache-miss response"
else
	pass "hint header stripped on cache-miss response"
fi
resp=$(cheaders 10.7.7.2 "level=2&page=b")
if echo "$resp" | grep -qi '^x-rate-limit-level'; then
	fail "hint header leaked on cache-hit response"
else
	pass "hint header stripped on cache-hit response"
fi
if ! echo "$resp" | tr -d '\r' | grep -i '^cache-status' | grep -q 'hit'; then
	fail "scenario 8 second response was not a cache hit (Souin config issue?)"
fi

echo "=== scenario 9: cached hint headers still count — hammering a cached level-3 URL boxes ==="
# Request 1 is a miss (3 units); requests 2-3 are hits whose stored
# X-Rate-Limit-Level replays through the module (3 units each): 9 > 6.
for i in 1 2 3; do
	c=$(ccode 10.7.7.3 "level=3&page=c")
	[ "$c" = "200" ] || fail "cached request $i should pass while budget lasts, got $c"
done
c=$(ccode 10.7.7.3 "level=3&page=c")
if [ "$c" = "429" ]; then
	pass "client boxed by cache-served hint headers"
else
	fail "expected 429 after cached level-3 responses, got $c"
fi

echo "=== scenario 10: shared cache does not break client isolation ==="
c=$(ccode 10.7.7.4 "level=3&page=c")
if [ "$c" = "200" ]; then
	pass "distinct client still served (from cache) while another is boxed"
else
	fail "expected 200 for distinct client on cached URL, got $c"
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL E2E SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES E2E ASSERTION(S) FAILED"
exit 1
