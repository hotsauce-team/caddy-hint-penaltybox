# Builds the exact artifact an operator would ship: a Caddy binary with
# this module compiled in via xcaddy.
FROM caddy:2.11.4-builder AS builder
COPY . /src
RUN xcaddy build v2.11.4 --with github.com/hotsauce-team/caddy-hint-penaltybox=/src

FROM caddy:2.11.4
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
# Fail the build outright if the module didn't make it into the binary.
RUN caddy list-modules | grep -q '^http.handlers.hint_penaltybox$'
COPY e2e/Caddyfile /etc/caddy/Caddyfile
