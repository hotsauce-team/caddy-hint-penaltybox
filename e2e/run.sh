#!/bin/sh
# End-to-end scenario assertions against the xcaddy-built Caddy.
# Config under test: min_level 2, window 10s, limit 6, penalty_ttl 3s.
# Each scenario uses a distinct X-Forwarded-For client so scenarios
# don't contaminate each other's budgets.
set -u

BASE="http://e2e-caddy:8080"
FAILURES=0

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

# code <client-ip> <query> -> prints HTTP status code
code() {
	curl -s -o /dev/null -w '%{http_code}' -H "X-Forwarded-For: $1" "$BASE/?$2"
}

# headers <client-ip> <query> -> prints response headers (lowercased names by curl -w? no: raw)
headers() {
	curl -s -D - -o /dev/null -H "X-Forwarded-For: $1" "$BASE/?$2"
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

echo "=== scenario 6: box expires after penalty_ttl ==="
sleep 4
c=$(code 10.3.3.5 "level=1")
if [ "$c" = "200" ]; then
	pass "boxed client allowed again after TTL"
else
	fail "expected 200 after box expiry, got $c"
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL E2E SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES E2E ASSERTION(S) FAILED"
exit 1
