# Everything runs in Docker — no host Go toolchain required.
# CI overrides COMPOSE to add docker-compose.ci.yml (bind-mounted caches).
COMPOSE ?= docker compose

.PHONY: test test-integration vet tidy lint e2e e2e-logs clean

test:
	$(COMPOSE) run --rm dev go test -race -cover ./...

test-integration:
	$(COMPOSE) run --rm dev go test -race -tags integration -v -run Integration ./...

vet:
	$(COMPOSE) run --rm dev go vet ./...

tidy:
	$(COMPOSE) run --rm dev go mod tidy

lint:
	$(COMPOSE) run --rm lint golangci-lint run

e2e:
	$(COMPOSE) up --build --exit-code-from e2e-runner e2e-runner; \
	status=$$?; \
	if [ $$status -ne 0 ]; then $(COMPOSE) logs e2e-caddy e2e-upstream; fi; \
	$(COMPOSE) down --remove-orphans; \
	exit $$status

e2e-logs:
	$(COMPOSE) logs e2e-caddy e2e-upstream

clean:
	$(COMPOSE) down -v --remove-orphans
