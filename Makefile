GO ?= go
COMPOSE ?= docker compose -f deploy/docker-compose.yml
LISTEN ?= 127.0.0.1:8420

.PHONY: help start stop restart status logs run build vet vet-blackbox vet-e2e check-layers test live swagger blackbox e2e infra-up infra-down infra-wait

help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- run it all

start: infra-up infra-wait ## Bring up derived stores, wait for health, then run the server
	@echo "==> derived stores healthy; starting memory-mcp on http://$(LISTEN)"
	@echo "==> swagger UI: http://$(LISTEN)/swagger/index.html"
	$(GO) run ./cmd/server/http

stop: infra-down ## Stop the derived stores (server is foreground; Ctrl-C it)

restart: stop start ## Stop and start again

status: ## Show container state and server health
	@$(COMPOSE) ps
	@printf '\nserver: '
	@curl -fsS http://$(LISTEN)/healthz 2>/dev/null || echo 'not running'

logs: ## Tail derived-store logs
	$(COMPOSE) logs -f

## ---------------------------------------------------------------- infra only

infra-up: ## Start OpenSearch + Neo4j containers (non-persistent by design)
	$(COMPOSE) up -d --build

infra-down: ## Stop and remove the containers, and the anonymous volumes they leak
	# -v is required, not tidiness. The compose file declares no volumes (§8:
	# the derived stores are intentionally non-persistent), but the neo4j image
	# declares VOLUME /data /logs, so every `up` creates two fresh anonymous
	# volumes that a plain `down` leaves behind. Repeated blackbox runs then
	# fill the Docker disk until container creation fails with ENOSPC.
	$(COMPOSE) down -v --remove-orphans

infra-wait: ## Block until both containers report healthy
	@echo "==> waiting for opensearch + neo4j to become healthy"
	@for i in $$(seq 1 60); do \
		os=$$(docker inspect -f '{{.State.Health.Status}}' dj-memory-opensearch 2>/dev/null || echo missing); \
		nj=$$(docker inspect -f '{{.State.Health.Status}}' dj-memory-neo4j 2>/dev/null || echo missing); \
		if [ "$$os" = healthy ] && [ "$$nj" = healthy ]; then echo "==> opensearch: healthy, neo4j: healthy"; exit 0; fi; \
		printf '\r    opensearch: %-9s neo4j: %-9s (%ss)' "$$os" "$$nj" "$$((i*3))"; \
		sleep 3; \
	done; \
	echo "\n!! containers did not become healthy — check 'make logs'"; exit 1

## ---------------------------------------------------------------- dev loop

run: ## Run the server only (assumes infra is already up)
	$(GO) run ./cmd/server/http

build: ## Compile everything
	$(GO) build ./...

vet: ## go vet
	$(GO) vet ./...

vet-blackbox: ## Type-check the build-tagged §10 suite (it is invisible to plain `go test ./...`)
	$(GO) vet -tags blackbox ./test/blackbox/...

vet-e2e: ## Type-check the build-tagged P1-P12 suite (it is invisible to plain `go test ./...`)
	$(GO) vet -tags e2e ./test/e2e/...

check-layers: ## Enforce the §4.1 layer rules (handler/service/external/app import directions)
	bash tools/check_layers.sh

test: vet-blackbox vet-e2e ## Unit tests (fakes only; no containers or AWS needed)
	$(GO) test -race ./...

live: infra-up infra-wait ## Integration tests against the real containers + real S3 bucket
	DJ_MEMORY_LIVE_TEST=1 $(GO) test -count=1 -v ./internal/external/memory/... ./internal/external/thirdparty/cold/...

swagger: ## Regenerate docs/ (swagger.json + docs.go) from swag annotations
	$(GO) run github.com/swaggo/swag/cmd/swag init -g cmd/server/http/main.go -o docs --parseInternal
	$(GO) run github.com/swaggo/swag/cmd/swag fmt -d internal/handler/app/httpserver,internal/app/httpserver,cmd/server/http

blackbox: infra-up infra-wait ## §10 acceptance scenarios against real containers + real S3
	$(GO) test -tags blackbox -count=1 -v ./test/blackbox/...

e2e: infra-up infra-wait ## P1-P12 invariant scenarios (feature-inventory §5) against real containers + real S3
	$(GO) test -tags e2e -count=1 -v ./test/e2e/...
