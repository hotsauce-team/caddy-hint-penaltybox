# Builds the exact artifact an operator would ship: a Caddy binary with
# this module compiled in via xcaddy — plus the Souin HTTP cache module
# with Otter storage, to prove the two coexist (Souin sits between this
# module's ResponseWriter shim and the upstream, replaying stored
# responses — including the hint header — through our interceptor).
FROM caddy:2.11.4-builder AS builder
COPY . /src
RUN xcaddy build v2.11.4 \
	--with github.com/hotsauce-team/caddy-hint-penaltybox=/src \
	--with github.com/darkweak/souin/plugins/caddy@v1.7.8 \
	--with github.com/darkweak/storages/otter/caddy@v0.0.19

FROM caddy:2.11.4
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
# Fail the build outright if either module didn't make it into the binary.
# (Otter is additionally proven at startup: the global `otter` cache
# option makes config load fail if the storage module is absent.)
RUN caddy list-modules | grep -q '^http.handlers.hint_penaltybox$' \
	&& caddy list-modules | grep -q '^http.handlers.cache$'
COPY e2e/Caddyfile /etc/caddy/Caddyfile
