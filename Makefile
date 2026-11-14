# Everything runs in containers: only Docker (compose v2), make and a POSIX
# shell are needed on the host.
HOST_UID := $(shell id -u)
HOST_GID := $(shell id -g)
export HOST_UID HOST_GID

GOVULNCHECK_VERSION := v1.9.0
# The settings file (secrets included); ENV_FILE lets a throwaway copy use its own.
ENV_FILE ?= .env
COMPOSE_FILES := --env-file $(ENV_FILE) -f deploy/compose.yml
# The stack of `make up`; DEV_PROJECT lets a throwaway copy run beside it.
# The tools profile adds the one-shot `migrate` service (run, never up).
DEV_PROJECT ?= geoint
DEV  := docker compose -p $(DEV_PROJECT) $(COMPOSE_FILES) --profile tools
# Small PostgreSQL settings for the throwaway test stacks (make test, make
# e2e), so they run on a CI runner with little memory: the same command line
# as production (deploy/compose.yml), other values (deploy/TUNING.md).
SMALL_PG := PG_SHARED_BUFFERS=256MB PG_EFFECTIVE_CACHE_SIZE=1GB PG_MAINTENANCE_WORK_MEM=256MB \
	PG_WORK_MEM=16MB PG_MAX_WAL_SIZE=2GB PG_SHM_SIZE=512m PG_AUTOVACUUM_WORK_MEM=64MB \
	PG_AUTOVACUUM_MAX_WORKERS=3 PG_MAX_PARALLEL_WORKERS=2 PG_MAX_PARALLEL_WORKERS_PER_GATHER=2 \
	PG_MAX_PARALLEL_MAINTENANCE_WORKERS=2
TEST_PROJECT ?= geoint-test
# make test runs many packages at once, each with its own pools: more connections.
TEST := env $(SMALL_PG) PG_MAX_CONNECTIONS=200 docker compose -p $(TEST_PROJECT) $(COMPOSE_FILES) -f deploy/compose.dev.yml
# Toolbox without dependencies (no database needed), under the test project.
TOOL := $(TEST) run --rm --no-deps go
# Tool targets create the project network; drop it and its volumes so nothing is left behind.
TOOL_CLEANUP := $(TEST) down --volumes --remove-orphans >/dev/null 2>&1
ALPINE_IMAGE := alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

.PHONY: up down test test-race test-scripts lint vuln fmt tidy cache

# A missing settings file is created with random secrets (never copied
# placeholders); an existing one is never overwritten.
$(ENV_FILE):
	sh scripts/init-env.sh $(ENV_FILE)

# Shared Go module/build cache owned by the host user; survives teardown of any project.
cache:
	@docker volume inspect geoint-gocache >/dev/null 2>&1 || { \
		docker volume create geoint-gocache >/dev/null && \
		docker run --rm -u 0 -v geoint-gocache:/cache $(ALPINE_IMAGE) \
			chown $(HOST_UID):$(HOST_GID) /cache; }

# Builds, starts the infrastructure, migrates with the api and the worker
# (the loader) stopped, then starts them: new code never runs on the old
# schema. A failed migration leaves api and worker stopped. Also the update
# command (git pull && make up); idempotent. Refuses to start while the
# settings file holds a public placeholder (scripts/check-env.sh) and warns
# when the disk is short (scripts/disk-check.sh). The health probe runs in
# the web container (no curl needed on the host), each try bounded to 2 s.
MIGRATE_FAILED := echo "make: the migration failed; api and worker stay stopped. Fix the cause (log above) and run the command again." >&2; exit 1
API_PROBE := for i in $$(seq 30); do \
		$(DEV) exec -T web wget -q -T 2 -O /dev/null http://api:8080/healthz >/dev/null 2>&1 && { echo "api healthy"; exit 0; }; \
		sleep 1; \
	done; echo "api did not answer /healthz within 30s" >&2; exit 1
up: $(ENV_FILE)
	@sh scripts/check-env.sh $(ENV_FILE)
	@sh scripts/disk-check.sh || true
	$(DEV) build
	$(DEV) up -d --wait postgres nats minio keycloak web
	$(DEV) stop api worker
	$(DEV) run --rm --no-deps migrate || { $(MIGRATE_FAILED); }
	$(DEV) up -d --wait api worker
	@$(API_PROBE)

down: $(ENV_FILE)
	$(DEV) down

# Schema migrations alone (make up already migrates), in the same order:
# api and worker stopped, migrate, start them again.
.PHONY: migrate collect backup restore national-check
migrate: $(ENV_FILE)
	@sh scripts/check-env.sh $(ENV_FILE)
	$(DEV) build migrate
	$(DEV) stop api worker
	$(DEV) run --rm --no-deps migrate || { $(MIGRATE_FAILED); }
	$(DEV) up -d --wait api worker

# Queues one collection for the worker: make collect SOURCE=receita
# (receita, tse, camara, senado, cvm; ARGS="--cnpjs ... --depth N" for a
# Receita slice, ARGS="--ref YYYY-MM-DD" to pin an edition).
SOURCE ?=
ARGS ?=
collect: $(ENV_FILE)
	@test -n "$(SOURCE)" || { echo "usage: make collect SOURCE=receita|tse|camara|senado|cvm [ARGS=...]" >&2; exit 2; }
	@sh scripts/check-env.sh $(ENV_FILE)
	$(DEV) run --rm --no-deps api collect $(SOURCE) $(ARGS)

# Backup of everything that cannot be collected again (scripts/backup.sh):
# make backup DEST=/caminho/de/uma/pasta/nova. api, worker and keycloak are
# stopped while it runs and started again at the end.
DEST ?=
backup: $(ENV_FILE)
	@test -n "$(DEST)" || { echo "usage: make backup DEST=<pasta nova>" >&2; exit 2; }
	@PROJECT=$(DEV_PROJECT) ENV_FILE=$(ENV_FILE) sh scripts/backup.sh "$(DEST)"

# Restore of a backup into this project's volumes (scripts/restore.sh), then
# make up. Refuses a project whose database already holds data unless
# FORCE=1, which DELETES the project's current volumes first.
SRC ?=
FORCE ?=
restore: $(ENV_FILE)
	@test -n "$(SRC)" || { echo "usage: make restore SRC=<pasta do backup> [FORCE=1]" >&2; exit 2; }
	@sh scripts/check-env.sh $(ENV_FILE)
	@PROJECT=$(DEV_PROJECT) ENV_FILE=$(ENV_FILE) FORCE=$(FORCE) sh scripts/restore.sh "$(SRC)"
	@$(MAKE) --no-print-directory up DEV_PROJECT=$(DEV_PROJECT) ENV_FILE=$(ENV_FILE)

# Starting conditions of a national load (scripts/preflight.sh); changes nothing.
national-check: $(ENV_FILE)
	@PROJECT=$(DEV_PROJECT) ENV_FILE=$(ENV_FILE) sh scripts/preflight.sh

# Measures a loaded stack through its authenticated API (scripts/measure.sh,
# cmd/measure): make measure DEV_PROJECT=geoint-national ENV_FILE=.env.national PASS=cold
# STEP=all|samples|run|stats|criteria|cleanup|build, ONLY=m1,entity,m2,m3,m4;
# STEP=criteria takes CNPJ=, A= (a public office holder) and B= (a company)
# from the environment. Results under
# results/<project>/measure (git-ignored). A temporary analyst user is created
# in Keycloak for the run and deleted at its end.
.PHONY: measure
PASS ?= run
STEP ?= all
ONLY ?=
measure: $(ENV_FILE)
	@PROJECT=$(DEV_PROJECT) ENV_FILE=$(ENV_FILE) PASS=$(PASS) STEP=$(STEP) ONLY=$(ONLY) sh scripts/measure.sh

# Load benchmark (scripts/bench-load.sh, docs/performance/benchmark.md): the
# real collection path on a sample of a Receita edition (first load, then the
# monthly re-load), in the throwaway project geoint-bench: fresh volumes, no
# published port, torn down at the end. Results under results/bench
# (git-ignored). make bench-sample cuts the sample (once) from an edition
# already downloaded to BENCH_SRC: make bench-sample UF=GO. Options of
# bench-load are environment variables (LABEL, STEP, KEEP, BENCH_PROFILE,
# BENCH_NOCOW, ...; see the script's header).
.PHONY: bench-sample bench-load
UF ?= GO
BENCH_SRC ?= data/raw/receita/2026-10
bench-sample: cache
	@mkdir -p data/bench/receita/$(UF)
	docker run --rm -u $(HOST_UID):$(HOST_GID) -e HOME=/tmp -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 \
		-e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -v $(CURDIR):/src -v geoint-gocache:/cache -w /src \
		golang:1.27.4-alpine3.24@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf \
		go run ./cmd/loadbench sample -src $(BENCH_SRC) -out data/bench/receita/$(UF) -uf $(UF)

bench-load: cache
	@sh scripts/bench-load.sh

# Runs the suite against throwaway postgres/nats/minio (project geoint-test),
# always tears them down, and propagates the test exit status.
# Focused runs: make test PKG=./internal/claims RUN=TestDelete
PKG ?= ./...
RUN ?=
test: $(ENV_FILE) cache
	@status=0; \
	$(TEST) run --rm go go test -count=1 $(if $(RUN),-run '$(RUN)') $(PKG) || status=$$?; \
	$(TEST) down --volumes --remove-orphans >/dev/null 2>&1; \
	exit $$status

# The same with the race detector, which needs cgo and glibc: the go-race
# service of compose.dev.yml. Example: make test-race PKG=./internal/auth
test-race: $(ENV_FILE) cache
	@status=0; \
	$(TEST) run --rm go-race go test -race -count=1 $(if $(RUN),-run '$(RUN)') $(PKG) || status=$$?; \
	$(TEST) down --volumes --remove-orphans >/dev/null 2>&1; \
	exit $$status

# Tests of the host-side shell scripts (scripts/*.test.sh), run by busybox sh
# in the pinned Alpine image (the strictest POSIX shell they must work in).
# volume-init.test.sh changes owners: it runs again as root, on a temporary
# folder of the container. role-login.dtest.sh needs Docker: it runs on the
# host against a throwaway PostgreSQL of the pinned image on an internal
# network.
test-scripts:
	@docker run --rm -u $(HOST_UID):$(HOST_GID) -e HOME=/tmp -v $(CURDIR):/src:ro -w /src $(ALPINE_IMAGE) \
		sh -c 'set -e; for t in scripts/*.test.sh; do sh "$$t"; done'
	@docker run --rm -u 0 --cap-drop ALL --cap-add CHOWN --cap-add DAC_READ_SEARCH --cap-add DAC_OVERRIDE --cap-add FOWNER \
		--network none -v $(CURDIR):/src:ro -w /src $(ALPINE_IMAGE) sh scripts/volume-init.test.sh
	@sh scripts/role-login.dtest.sh

# gofmt is limited to cmd and internal so it never walks data/.
lint: $(ENV_FILE) cache
	@status=0; \
	$(TOOL) sh -c 'test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; echo "gofmt: files need formatting"; exit 1; }' \
		&& $(TOOL) go vet ./... || status=$$?; \
	$(TOOL_CLEANUP); exit $$status

vuln: $(ENV_FILE) cache
	@status=0; \
	$(TOOL) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(if $(VERBOSE),-show verbose) ./... || status=$$?; \
	$(TOOL_CLEANUP); exit $$status

fmt: $(ENV_FILE) cache
	@status=0; $(TOOL) gofmt -w cmd internal || status=$$?; $(TOOL_CLEANUP); exit $$status

tidy: $(ENV_FILE) cache
	@status=0; $(TOOL) go mod tidy || status=$$?; $(TOOL_CLEANUP); exit $$status

# Web app (web/): Node runs in a pinned container as the host user, in plain
# `docker run --rm` containers (no compose project, no fixed names), so
# concurrent runs do not collide. node_modules lives in web/ (git-ignored,
# owned by the host user) and is reinstalled with `npm ci --ignore-scripts`
# only when package-lock.json changes, under a lock taken on the web/
# directory itself (shared by every run, whatever WEB_CACHE is), so two runs
# never install at once. Tools run with
# `npm exec --no`: never fetched from the registry at run time.
NODE_IMAGE := node:24.22.1-alpine3.24@sha256:06d7adf3e0ef61a13f1d28682726366f97a15aa1888c934d3ecec86c33918b53
WEB_CACHE ?= geoint-nodecache
NODE := docker run --rm -u $(HOST_UID):$(HOST_GID) -e HOME=/tmp -e npm_config_cache=/cache/npm \
	-v $(CURDIR)/web:/src/web -v $(WEB_CACHE):/cache -w /src/web $(NODE_IMAGE)
WEB_DEPS := flock /src/web sh -c '[ node_modules/.ci-stamp -nt package-lock.json ] || { npm ci --ignore-scripts --no-audit --no-fund && touch node_modules/.ci-stamp; }'
# End-to-end test (deploy/compose.e2e.yml over compose.yml, project
# geoint-e2e): builds the images, starts a throwaway stack that publishes no
# host port, loads testdata/ through the source simulator
# (scripts/e2e-seed.sh) and runs Playwright (web/e2e) in its own container
# on the stack's network. The stack runs exactly as in production (non-root,
# read-only, the three database roles) with small PostgreSQL settings and
# its own settings file of random secrets (scripts/init-env.sh in a temporary
# folder: never your .env). Always tears the stack down with its volumes,
# also on failure or Ctrl-C, after printing the last log lines when
# something failed. Exit status: the test's. Browser artifacts of failed
# tests: web/e2e-results/ (git-ignored).
E2E_PROJECT ?= geoint-e2e
E2E := env $(SMALL_PG) docker compose -p $(E2E_PROJECT) --env-file "$$E2E_ENV_FILE" -f deploy/compose.yml -f deploy/compose.e2e.yml --profile tools

.PHONY: e2e
e2e: web-cache
	@status=1; \
	E2E_ENV_DIR=$$(mktemp -d); E2E_ENV_FILE=$$E2E_ENV_DIR/env; export E2E_ENV_FILE; \
	cleanup() { \
		if [ $$status -ne 0 ]; then \
			echo "e2e: failed; last log lines:" >&2; \
			$(E2E) logs --no-color --tail 100 api worker keycloak web sourcesim >&2 || true; \
		fi; \
		$(E2E) --profile e2e down --volumes --remove-orphans >/dev/null 2>&1 || true; \
		rm -f "$$E2E_ENV_FILE"; rmdir "$$E2E_ENV_DIR"; \
	}; \
	trap 'cleanup; exit $$status' EXIT; \
	trap 'status=130; exit 130' INT; \
	trap 'status=143; exit 143' TERM; \
	start=$$(date +%s); \
	sh scripts/init-env.sh "$$E2E_ENV_FILE" >/dev/null || exit 1; \
	pw=$$(sed -n 's/.*"@playwright\/test": "\([^"]*\)".*/\1/p' web/package.json); \
	grep -q "mcr.microsoft.com/playwright:v$$pw-" deploy/compose.e2e.yml || { \
		echo "e2e: @playwright/test $$pw in web/package.json does not match the Playwright image in deploy/compose.e2e.yml" >&2; exit 1; }; \
	$(NODE) sh -c "$(WEB_DEPS)" \
		&& $(E2E) build \
		&& env $(SMALL_PG) E2E_PROJECT=$(E2E_PROJECT) sh scripts/e2e-seed.sh \
		&& $(E2E) run --rm --no-deps playwright; \
	status=$$?; \
	echo "e2e: exit $$status after $$(( $$(date +%s) - start ))s" >&2

# check-web-image: image tag and container name derive from TEST_PROJECT.
WEB_IMAGE ?= $(TEST_PROJECT)-web-check:local

.PHONY: web-cache test-web lint-web audit-web build-web check-web-image

web-cache:
	@docker volume inspect $(WEB_CACHE) >/dev/null 2>&1 || { \
		docker volume create $(WEB_CACHE) >/dev/null && \
		docker run --rm -u 0 -v $(WEB_CACHE):/cache $(ALPINE_IMAGE) \
			chown $(HOST_UID):$(HOST_GID) /cache; }

test-web: web-cache
	@$(NODE) sh -c "$(WEB_DEPS) && npm exec --no -- vitest run"

lint-web: web-cache
	@$(NODE) sh -c "$(WEB_DEPS) && npm exec --no -- tsc --noEmit -p . && npm exec --no -- tsc --noEmit -p e2e"

audit-web: web-cache
	@$(NODE) npm audit --audit-level=high

build-web: web-cache
	@$(NODE) sh -c "$(WEB_DEPS) && npm exec --no -- vite build"

# Builds deploy/Dockerfile.web and checks the result: no inline script/style
# in index.html, start-up config validation, security headers, /config.json.
check-web-image:
	docker build -q -f deploy/Dockerfile.web -t $(WEB_IMAGE) web >/dev/null
	@status=0; sh web/docker/check-image.sh $(WEB_IMAGE) $(TEST_PROJECT)-web-check || status=$$?; \
		docker rmi -f $(WEB_IMAGE) >/dev/null 2>&1; exit $$status
