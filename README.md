# caddy-hint-penaltybox

A Caddy v2 module that turns an origin's **rate-limit hint header** into
edge-side throttling using the classic **penalty box** pattern: every
origin response labeled `X-Rate-Limit-Level: 2` or `3` adds weighted
units to a per-client sliding-window budget; a client that exceeds the
budget is put in a penalty box and gets `429` + `Retry-After` — before
its requests reach the origin — until the box expires. The hint header
is stripped before the response reaches the client.

If you know [Fastly's penalty boxes][fastly-concepts] or HAProxy's
[stick tables][haproxy-docs], you already understand this module — it is
the deliberate Caddy counterpart to those recipes, with matching
vocabulary. Caddy needs a compiled-in module for this because it has no
edge scripting runtime, and existing rate-limit plugins are
request-side only: they cannot see an **origin response** header.

## Concept map

| Concept            | Fastly ERL                                   | HAProxy                                       | `caddy-hint-penaltybox`             |
| ------------------ | -------------------------------------------- | --------------------------------------------- | ----------------------------------- |
| Per-client counter | `ratecounter` declaration                    | stick-table `store gpc0,gpc0_rate(60s)`       | in-memory sliding-window counter    |
| Count on response  | [`ratelimit.check_rate`][fastly-check-rate] in `vcl_fetch` | `http-response sc-inc-gpc0(0) if { ... }`     | ResponseWriter shim after `next`    |
| Weighted increment | `delta` parameter = level                    | not supported (increments by 1)               | `delta = level` (Fastly-style)      |
| Penalty box        | [`penaltybox` declaration][fastly-penaltybox] + TTL | modeled via rate threshold on the table       | boxed map with per-entry TTL        |
| Enforce on request | [`ratelimit.penaltybox_has`][fastly-pb-has] in `vcl_recv` | `http-request deny if { sc0_gpc0_rate gt N }` | box check at top of `ServeHTTP`     |
| Client key         | `client.ip` (or any entry string)            | `track-sc0 src`                               | `{client_ip}` placeholder (default) |
| Strip the header   | `unset resp.http.X-Rate-Limit-Level`         | `http-response del-header`                    | module strips by default            |
| Window constraint  | 1, 10, or 60 seconds                         | arbitrary `gpc0_rate(period)`                 | free-form; default 60s              |
| Penalty TTL        | 1m–1h, minute granularity                    | table `expire`                                | free-form; default 5m               |
| Box re-offense     | TTL fixed once boxed                         | effectively extends while rate stays high     | TTL fixed once boxed (Fastly-style) |
| Clustered state    | platform-global                              | stick-table peers protocol                    | per-instance (see Trade-offs)       |

## The wire contract

The module consumes a response header (default `X-Rate-Limit-Level`)
with the strict value set `"1"`, `"2"`, or `"3"` — the **recommended
throttle strictness** of the response, never enforcement:

- **Absent header, or any other value (garbage, `"0"`, `"4"`, padded,
  multi-valued) = level 1.** Malformed input never counts and never
  errors.
- Levels at or above `min_level` (default 2) add `level` units to the
  client's window — a level-3 login attempt costs 3 units, level-1
  traffic costs nothing and allocates nothing.

Any application can emit this header; the module is not specific to any
CMS. It pairs with an app that labels sensitive routes (logins, presign
endpoints, expensive queries) with higher levels.

## Install

```sh
xcaddy build --with github.com/hotsauce-team/caddy-hint-penaltybox
```

Or with the Caddy builder image:

```dockerfile
FROM caddy:2.11.4-builder AS builder
RUN xcaddy build --with github.com/hotsauce-team/caddy-hint-penaltybox

FROM caddy:2.11.4
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
```

## Caddyfile

```caddyfile
example.com {
	route {
		hint_penaltybox {
			header      X-Rate-Limit-Level  # default; the wire contract
			key         {client_ip}         # default
			min_level   2                   # default; ignore level-1 responses
			window      60s                 # sliding window
			limit       30                  # weighted units per window before boxing
			penalty_ttl 5m                  # box duration (fixed, never extended)
			strip       true                # default
			status      429                 # default
			max_keys    100000              # default; tracked-client cap
		}
		reverse_proxy localhost:8000
	}
}
```

Outside a `route` block the directive orders itself before
`reverse_proxy` automatically; inside `route` ordering is positional, so
place it before your proxy/file-server directive.

All options and defaults:

| Option        | Default              | Meaning                                                              |
| ------------- | -------------------- | -------------------------------------------------------------------- |
| `header`      | `X-Rate-Limit-Level` | Origin response header carrying the hint level                       |
| `key`         | `{client_ip}`        | Client identity; respects the server's `trusted_proxies` config      |
| `min_level`   | `2`                  | Lowest level that counts toward the budget (1–3)                     |
| `window`      | `60s`                | Sliding window; free-form duration (Fastly's 1s/10s/60s is the interoperability convention) |
| `limit`       | `30`                 | Weighted units per window; *exceeding* (not reaching) it boxes       |
| `penalty_ttl` | `5m`                 | Box duration; Fastly allows 1m–1h — mirror that range for doc parity |
| `strip`       | `true`               | Remove the hint header before the client sees it (all responses)     |
| `status`      | `429`                | Status for boxed clients (4xx/5xx)                                   |
| `max_keys`    | `100000`             | Hard cap on tracked clients; oldest-idle evicted beyond it           |

JSON config uses the same fields under `http.handlers.hint_penaltybox`:

```json
{
	"handler": "hint_penaltybox",
	"min_level": 2,
	"window": "60s",
	"limit": 30,
	"penalty_ttl": "5m"
}
```

Do **not** key on a raw `X-Forwarded-For` read: the default
`{client_ip}` already honors the server's
[`trusted_proxies`](https://caddyserver.com/docs/caddyfile/options#trusted-proxies)
configuration, which is where XFF trust belongs.

## Semantics and trade-offs (read this)

- **Reactive by design.** A client gets a budget's worth of full-cost
  requests before the box closes. Sustained abuse (credential stuffing,
  presign farming) is the threat this addresses — not the first hit.
- **Boxing is strict-greater.** With `limit 30`, thirty units is still
  allowed; the response that pushes past 30 gets through (its hint is
  what triggers boxing) and the *next* request is rejected.
- **`Retry-After` is honest**: the ceiling of the *remaining* box
  seconds, not the configured TTL.
- **The box TTL is fixed** (Fastly semantics). Traffic during the box
  neither counts nor extends the penalty; after expiry the budget
  restarts from zero.
- **State is per-instance and in-memory.** N Caddy instances ≈ N× the
  effective threshold. A config reload resets counters and boxes
  (fails open). Distributed state is a possible future addition — the
  counter store sits behind a small interface for exactly that reason.
- **Memory is hard-bounded.** At most `max_keys` clients are tracked;
  an attacker rotating IPs exhausts the cap into evictions, not into
  unbounded memory. Actively boxed entries are the last to be evicted.

## Development

No local Go toolchain needed — everything runs in Docker:

```sh
make test              # unit tests (race detector, coverage)
make test-integration  # caddytest harness against an in-process Caddy
make e2e               # xcaddy-built binary + stub origin + curl scenarios
make lint              # golangci-lint
```

CI runs the identical targets.

## References

- Fastly: [rate-limiting concepts][fastly-concepts],
  [`ratelimit.check_rate`][fastly-check-rate],
  [`penaltybox` declaration][fastly-penaltybox],
  [`ratelimit.penaltybox_has`][fastly-pb-has]
- HAProxy: [docs.haproxy.org][haproxy-docs] (`stick-table`,
  `http-response sc-inc-gpc0`, `sc0_gpc0_rate`)
- Caddy: [Extending Caddy](https://caddyserver.com/docs/extending-caddy),
  [mholt/caddy-ratelimit](https://github.com/mholt/caddy-ratelimit)
  (prior art; request-side only — the gap this module fills),
  [xcaddy](https://github.com/caddyserver/xcaddy)

[fastly-concepts]: https://www.fastly.com/documentation/guides/concepts/rate-limiting/
[fastly-check-rate]: https://www.fastly.com/documentation/reference/vcl/functions/rate-limiting/ratelimit-check-rate/
[fastly-penaltybox]: https://www.fastly.com/documentation/reference/vcl/declarations/penaltybox/
[fastly-pb-has]: https://www.fastly.com/documentation/reference/vcl/functions/rate-limiting/ratelimit-penaltybox-has/
[haproxy-docs]: https://docs.haproxy.org/

## License

Apache-2.0
