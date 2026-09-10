# wsaw build targets.
#
# Everything here is plain Go: no code generation, no asset pipeline, no Node.
# `make build` is the whole story (Tenet 14).

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# Reproducible builds: the timestamp comes from the commit, not the clock.
DATE    ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

DIST := dist

.PHONY: all
all: check build

.PHONY: build
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/wsaw ./cmd/wsaw

.PHONY: install
install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/wsaw

# release cross-compiles every supported platform. Trimpath and the
# commit-derived date keep the output reproducible.
.PHONY: release
release: clean
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		GOOS=$${platform%/*}; GOARCH=$${platform#*/}; \
		out=$(DIST)/wsaw_$${GOOS}_$${GOARCH}; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH \
			go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/wsaw || exit 1; \
	done
	@cd $(DIST) && shasum -a 256 wsaw_* > SHA256SUMS
	@echo "checksums written to $(DIST)/SHA256SUMS"

# Keep in step with GO_LICENSES_VERSION in .github/workflows/ci.yaml.
GO_LICENSES_VERSION ?= v2.0.1

.PHONY: check
check: fmt vet lint licenses test

.PHONY: fmt
fmt:
	gofmt -l -w .
	@test -z "$$(gofmt -l .)" || (echo "unformatted files remain" && exit 1)

.PHONY: vet
vet:
	go vet ./...

# Pinned, and built with this project's Go. golangci-lint refuses to analyse a
# module whose go directive is newer than the Go it was built with, and this
# module's floor comes from chromedp, which tracks Go closely — so a prebuilt
# binary drifts out of range. Building it here keeps local and CI identical.
GOLANGCI_LINT_VERSION ?= v2.13.2

.PHONY: lint
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout 5m

# test runs the fast suite. Browser-dependent tests skip themselves when no
# usable Chrome is present, so this works on a machine without one.
.PHONY: test
test:
	go test -race ./...

# test-fast skips browser tests explicitly, for a tight edit loop.
.PHONY: test-fast
test-fast:
	WSAW_SKIP_BROWSER_TESTS=1 go test ./...

# The store's behaviour must be identical on every dialect, so the same suite
# is run against each of them (Story 4.7, AC5). These targets start a database
# in a container, run the suite against it, and take it down again — so a
# developer needs a container runtime for them but not for `make test`.
#
# Ports are deliberately not the defaults, so a local PostgreSQL or MySQL is
# never touched by a test run.
STORE_TEST_RUNTIME ?= podman
PG_IMAGE           ?= docker.io/library/postgres:17-alpine
MYSQL_IMAGE        ?= docker.io/library/mysql:8.4
PG_PORT            ?= 55432
MYSQL_PORT         ?= 53306

.PHONY: test-store-all
test-store-all: test test-store-postgres test-store-mysql

.PHONY: test-store-postgres
test-store-postgres:
	$(STORE_TEST_RUNTIME) run -d --rm --name wsaw-test-pg \
		-e POSTGRES_PASSWORD=wsaw -e POSTGRES_USER=wsaw -e POSTGRES_DB=wsaw \
		-p $(PG_PORT):5432 $(PG_IMAGE)
	@echo "waiting for postgres"
	@for i in $$(seq 1 60); do \
		$(STORE_TEST_RUNTIME) exec wsaw-test-pg pg_isready -U wsaw >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	- WSAW_TEST_STORE_DRIVER=postgres \
	  WSAW_TEST_POSTGRES_DSN="postgres://wsaw:wsaw@127.0.0.1:$(PG_PORT)/wsaw?sslmode=disable" \
	  go test -count=1 ./internal/store/
	$(STORE_TEST_RUNTIME) stop wsaw-test-pg

.PHONY: test-store-mysql
test-store-mysql:
	$(STORE_TEST_RUNTIME) run -d --rm --name wsaw-test-mysql \
		-e MYSQL_ROOT_PASSWORD=wsaw -e MYSQL_DATABASE=wsaw \
		-p $(MYSQL_PORT):3306 $(MYSQL_IMAGE)
	@echo "waiting for mysql"
	@for i in $$(seq 1 60); do \
		$(STORE_TEST_RUNTIME) exec wsaw-test-mysql mysqladmin ping -uroot -pwsaw >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	- WSAW_TEST_STORE_DRIVER=mysql \
	  WSAW_TEST_MYSQL_DSN="root:wsaw@tcp(127.0.0.1:$(MYSQL_PORT))/mysql" \
	  go test -count=1 ./internal/store/
	$(STORE_TEST_RUNTIME) stop wsaw-test-mysql

# Coverage is measured with -coverpkg, not per-package. Much of wsaw is
# deliberately tested from the outside: the scanner's integration tests drive
# a real browser against fixture servers and exercise capture, browser and
# consent through it. Without -coverpkg, Go credits a test binary only for
# statements in its own package, so that whole stack reads as untested when it
# is not.
#
# The e2e fixture server and the manual browse tool are excluded: they are
# test scaffolding, and measuring them says nothing about wsaw while moving
# the number whenever a fixture grows. tools/coverreport is already out via
# //go:build ignore.
COVER_PKGS = $(shell go list ./... | grep -v '/test/e2e/' | paste -sd, -)

.PHONY: cover
cover:
	go test -coverpkg=$(COVER_PKGS) -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# COVER_MIN is a ratchet, not an aspiration: it holds the number CI last
# measured so coverage cannot silently regress. Raise it when real coverage
# rises; never lower it to make a build pass.
#
# It sits below what a developer machine measures on purpose. A machine with
# Podman and Chrome covers container paths that skip on a runner without a
# container runtime, and that was worth about four points at the Phase 0/1
# baseline: the same tree measured 78.3% locally and 74.2% on Linux CI. A
# floor calibrated against the higher number fails the build for a
# difference in the environment rather than in the code, so this tracks CI,
# with a little room for the run-to-run variance of the browser-dependent
# tests.
#
# Phases 2 and 3 (internal/app wiring, plus cmd/wsaw's browser- and
# signal-driven paths) landed without touching the container path — every
# new test forces browser.runtime: local — so they should not change that
# four-point gap much. Local now measures 82.6%. This is raised to 77.0
# rather than to a number derived from that, on the same principle the
# Phase 0/1 floor was set on: guess conservatively and let CI's next run
# correct it, because a floor set above what CI actually measures is a
# broken build, not a gate. Raise it again once CI has measured this.
COVER_MIN ?= 77.0

# cover-gate fails the build below COVER_MIN. The percentage comes from
# `go tool cover -func`, which knows how to fold the repeated blocks a
# -coverpkg profile contains — summing the profile by hand does not.
.PHONY: cover-gate
cover-gate: cover
	@total=$$(go tool cover -func=coverage.out | tail -1 | awk '{print $$NF}' | tr -d '%'); \
	echo "coverage: $$total% (minimum $(COVER_MIN)%)"; \
	awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { exit !(t + 0 >= m + 0) }' || { \
		echo "coverage $$total% is below the $(COVER_MIN)% minimum"; \
		exit 1; \
	}

# cover-report renders the same profile as a browsable page: every package
# ranked, then per file and per function. The generator is named as a file
# rather than a package because it carries //go:build ignore — that keeps it
# out of ./... so the tool never shows up in the coverage it reports on.
COVER_REPORT ?= coverage-report.html

.PHONY: cover-report
cover-report:
	go test -coverpkg=$(COVER_PKGS) -coverprofile=coverage.out -covermode=atomic ./...
	go run tools/coverreport/main.go -profile coverage.out -out $(COVER_REPORT)

# soak runs the long-running stability test, which is deliberately separate
# from the regular suite (Story 6.8).
.PHONY: soak
soak:
	go test -tags soak -timeout 60m -run TestSoak ./internal/soak/

# wsaw is MIT; a copyleft dependency would be a licensing problem, so this is
# a gate rather than a report (NFR §9). It is part of `check` on purpose: it
# used to run only in CI, and a dependency that broke it was therefore merged
# without anyone noticing until the push.
#
# v2, because v1's classifier is from 2021 and misreads several ordinary
# BSD-3 files.
#
# MPL-2.0 is allowed on purpose, for a dependency linked unmodified — see
# NFR §9. Keep this list in step with the one in .github/workflows/ci.yaml.
.PHONY: licenses
licenses:
	go run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... \
		--allowed_licenses=MIT,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

.PHONY: vulncheck
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: sbom
sbom:
	@mkdir -p $(DIST)
	go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest app \
		-json -output $(DIST)/wsaw.cdx.json -main cmd/wsaw .

# The end-to-end fixture image: a static site with a real Klaro banner, plus
# the second origin that makes first/third-party attribution testable
# (Story 7.1). Klaro is fetched by pinned version and digest during the
# build, so the image is reproducible and a scan fetches nothing from
# outside the stack.
E2E_RUNTIME ?= podman
E2E_FIXTURE_IMAGE ?= localhost/wsaw-fixture:dev

# Ports on this machine. Not the obvious ones, so a fixture left running
# cannot collide with whatever else is listening on 8080.
E2E_SITE_PORT    ?= 8081
E2E_TRACKER_PORT ?= 8082

# The hostnames the *browser* uses. They are reserved names under .example, so
# each is its own registrable domain and the first/third-party classifier has
# something real to distinguish — which 127.0.0.1 would not give it.
E2E_SITE_HOST    ?= site.example
E2E_TRACKER_HOST ?= tracker.example
E2E_EXTRA_HOST   ?= extra-tracker.example

E2E_SITE_BASE    := http://$(E2E_SITE_HOST):$(E2E_SITE_PORT)
E2E_TRACKER_BASE := http://$(E2E_TRACKER_HOST):$(E2E_TRACKER_PORT)
E2E_EXTRA_BASE   := http://$(E2E_EXTRA_HOST):$(E2E_TRACKER_PORT)

E2E_SITE_NAME    := wsaw-fixture-site
E2E_TRACKER_NAME := wsaw-fixture-tracker

E2E_DIR := $(DIST)/e2e

.PHONY: e2e-fixture-image
e2e-fixture-image:
	$(E2E_RUNTIME) build -f test/e2e/fixture/Containerfile -t $(E2E_FIXTURE_IMAGE) .

# e2e-fixture-up runs the fixture on this machine: both origins, published on
# loopback, ready to be scanned.
#
# It waits for each role to answer its health endpoint rather than sleeping.
# A race that usually passes is worse than one that always fails.
.PHONY: e2e-fixture-up
e2e-fixture-up: e2e-fixture-image e2e-fixture-down
	$(E2E_RUNTIME) run -d --name $(E2E_TRACKER_NAME) \
		-p 127.0.0.1:$(E2E_TRACKER_PORT):8080 $(E2E_FIXTURE_IMAGE) \
		-role=third-party -listen=:8080 -self-base=$(E2E_TRACKER_BASE)
	$(E2E_RUNTIME) run -d --name $(E2E_SITE_NAME) \
		-p 127.0.0.1:$(E2E_SITE_PORT):8080 $(E2E_FIXTURE_IMAGE) \
		-role=site -listen=:8080 \
		-third-party-base=$(E2E_TRACKER_BASE) \
		-extra-third-party-base=$(E2E_EXTRA_BASE)
	@for port in $(E2E_SITE_PORT) $(E2E_TRACKER_PORT); do \
		printf 'waiting for 127.0.0.1:%s ' "$$port"; \
		for i in $$(seq 1 50); do \
			if curl -fsS "http://127.0.0.1:$$port/__fixture/healthz" >/dev/null 2>&1; then \
				echo ok; break; \
			fi; \
			if [ "$$i" = 50 ]; then \
				echo; echo "the fixture never became healthy on port $$port:"; \
				$(E2E_RUNTIME) logs $(E2E_SITE_NAME) $(E2E_TRACKER_NAME) 2>&1 | tail -20; \
				exit 1; \
			fi; \
			sleep 0.2; \
		done; \
	done
	@$(MAKE) --no-print-directory e2e-fixture-config
	@echo
	@echo "The fixture is up:"
	@echo "  site         http://127.0.0.1:$(E2E_SITE_PORT)/   (as $(E2E_SITE_BASE) to the browser)"
	@echo "  third party  http://127.0.0.1:$(E2E_TRACKER_PORT)/   (as $(E2E_TRACKER_BASE))"
	@echo
	@echo "Scan it:            make e2e-fixture-scan"
	@echo "Look at it:         make e2e-fixture-browse"
	@echo "Change it:          make e2e-fixture-variant VARIANT=changed"
	@echo "Watch what it says: make e2e-fixture-logs"
	@echo "Take it down:       make e2e-fixture-down"

.PHONY: e2e-fixture-down
e2e-fixture-down:
	@$(E2E_RUNTIME) rm -f $(E2E_SITE_NAME) $(E2E_TRACKER_NAME) >/dev/null 2>&1 || true

# e2e-fixture-config writes the configuration a hand-run scan needs.
#
# The resolver rules are the point: the fixture is published on this machine's
# loopback, so the browser has to be told that its hostnames live there — and
# it has to be a local browser, because a containerised one has its own
# network namespace and cannot reach this host's loopback at all (Story 1.8,
# AC10). In the Compose stack and the Podman pod none of this applies.
.PHONY: e2e-fixture-config
e2e-fixture-config:
	@mkdir -p $(E2E_DIR)
	@printf '%s\n' \
		'# Generated by `make e2e-fixture-config`. Not for deployment.' \
		'defaults:' \
		'  consentModes: [none, reject, accept]' \
		'  robots: ignore' \
		'  idleQuiet: 2s' \
		'  hardTimeout: 25s' \
		'' \
		'browser:' \
		'  runtime: local' \
		'  extraArgs:' \
		'    - "--host-resolver-rules=MAP $(E2E_SITE_HOST) 127.0.0.1,MAP $(E2E_TRACKER_HOST) 127.0.0.1,MAP $(E2E_EXTRA_HOST) 127.0.0.1"' \
		'' \
		'targets:' \
		'  - name: fixture' \
		'    url: $(E2E_SITE_BASE)/' \
		'' \
		'store:' \
		'  path: $(CURDIR)/$(E2E_DIR)/wsaw.db' \
		'' \
		'logging:' \
		'  level: info' \
		> $(E2E_DIR)/wsaw.yaml
	@echo "wrote $(E2E_DIR)/wsaw.yaml"

# e2e-fixture-scan scans the running fixture in all three consent modes.
#
# --fail-on info makes the exit code mean something: against an unchanged
# fixture a second scan must report nothing at all, so a non-zero exit here is
# either a real change or a determinism bug (Story 7.1, AC4).
.PHONY: e2e-fixture-scan
e2e-fixture-scan: build e2e-fixture-config
	$(DIST)/wsaw scan --config $(E2E_DIR)/wsaw.yaml --fail-on info

# e2e-fixture-variant switches the fixture between its states, so a scan
# afterwards has exactly one new third-party host and one changed script.
.PHONY: e2e-fixture-variant
e2e-fixture-variant:
	@test -n "$(VARIANT)" || { echo "usage: make e2e-fixture-variant VARIANT=base|changed"; exit 2; }
	@curl -fsS -X PUT --data '$(VARIANT)' \
		http://127.0.0.1:$(E2E_SITE_PORT)/__fixture/variant

# e2e-fixture-browse opens the fixture in a visible Chrome, so a person can
# see the page and the banner wsaw scans — useful when a consent assertion
# fails and the question is what the page actually looks like.
#
# It uses a throwaway profile, which is not a detail: with a shared profile an
# already-running Chrome would take the URL and silently ignore the resolver
# rules, so the fixture's hostnames would not resolve and the failure would
# explain nothing. The profile is also fresh each time, so the banner appears
# each time; pass KEEP_PROFILE=1 to keep a decision made in the last one.
.PHONY: e2e-fixture-browse
e2e-fixture-browse:
	go run ./test/e2e/browse \
		-url=$(E2E_SITE_BASE)/ \
		-check-url=http://127.0.0.1:$(E2E_SITE_PORT)/ \
		-profile=$(CURDIR)/$(E2E_DIR)/chrome-profile \
		-resolver-rules="MAP $(E2E_SITE_HOST) 127.0.0.1,MAP $(E2E_TRACKER_HOST) 127.0.0.1,MAP $(E2E_EXTRA_HOST) 127.0.0.1" \
		$(if $(KEEP_PROFILE),-keep-profile,) \
		$(if $(CHROME_PATH),-chrome-path=$(CHROME_PATH),)

.PHONY: e2e-fixture-logs
e2e-fixture-logs:
	$(E2E_RUNTIME) logs -f $(E2E_SITE_NAME) $(E2E_TRACKER_NAME)

# The Compose stack (Story 7.2): wsaw, a server database, and the Klaro
# fixture's two origins, all addressing each other by name on one Compose
# network. Unlike e2e-fixture-up, the browser runs inside the wsaw
# container — there is no host loopback to resolve into (Story 1.8, AC10) —
# and the database is a real one, not the file store the fixture targets
# above use.
#
# The database is picked with an override file, not a flag wsaw reads: `make
# e2e-compose-up DB=postgres` runs
#   $(E2E_RUNTIME) compose -f test/e2e/compose.yaml -f test/e2e/compose.postgres.yaml ...
# `DB=mysql` the same with the other override (AC5).
E2E_COMPOSE_PROJECT ?= wsaw-e2e
# Recursive (=), not immediate (:=): DB is set as a target-specific variable
# by e2e-postgres-test/e2e-mysql-test below, and a := binding here would
# freeze $(DB) at parse time — before any target-specific value exists —
# rather than re-reading it when a recipe actually uses $(E2E_COMPOSE).
E2E_COMPOSE = $(E2E_RUNTIME) compose -p $(E2E_COMPOSE_PROJECT) -f test/e2e/compose.yaml -f test/e2e/compose.$(DB).yaml

# The same invocation with paths relative to test/e2e/ rather than the repo
# root, for WSAW_E2E_COMPOSE_CMD below: `go test` runs with its package
# directory as the working directory, not wherever `make` was invoked from.
E2E_COMPOSE_FROM_TESTDIR = $(E2E_RUNTIME) compose -p $(E2E_COMPOSE_PROJECT) -f compose.yaml -f compose.$(DB).yaml

define e2e_compose_need_db
	@test -n "$(DB)" || { echo "usage: make $(1) DB=postgres|mysql"; exit 2; }
endef

.PHONY: e2e-compose-up
e2e-compose-up:
	$(call e2e_compose_need_db,e2e-compose-up)
	$(E2E_COMPOSE) up -d --build --wait --wait-timeout 180
	@echo
	@echo "The stack is up, wsaw included, all reporting healthy."
	@echo "Scan it:            make e2e-compose-scan DB=$(DB)"
	@echo "Watch what it says: make e2e-compose-logs DB=$(DB)"
	@echo "Take it down:       make e2e-compose-down DB=$(DB)"

# e2e-compose-scan triggers a scan through wsaw's own API rather than the
# CLI, because inside the stack that API is the only thing that can reach
# it — there is no shared file store to run `wsaw scan` against from here.
.PHONY: e2e-compose-scan
e2e-compose-scan:
	$(call e2e_compose_need_db,e2e-compose-scan)
	@for mode in none reject accept; do \
		echo "scanning fixture ($$mode)..."; \
		$(E2E_COMPOSE) exec -T wsaw \
			wget -qO- --header="Authorization: Bearer wsaw-e2e-fixture-only-token" \
			--post-data="" "http://localhost:8712/api/v1/scan/fixture/$$mode" >/dev/null || exit 1; \
	done
	@echo "done — read the results back with SQL, not through wsaw (Story 7.4, AC1):"
	@echo "  make e2e-compose-logs DB=$(DB)"

# Bringing the stack down always removes its volumes: a stale database left
# over from an earlier run must never be why a later one passes or fails
# (AC8).
.PHONY: e2e-compose-down
e2e-compose-down:
	$(call e2e_compose_need_db,e2e-compose-down)
	$(E2E_COMPOSE) down -v

.PHONY: e2e-compose-logs
e2e-compose-logs:
	$(call e2e_compose_need_db,e2e-compose-logs)
	$(E2E_COMPOSE) logs -f

# The end-to-end test itself (Story 7.4): brings the Compose stack up with
# Postgres, runs the Go suite in test/e2e against it, and always tears the
# stack down afterwards — whether the suite passed or not, and dumping every
# service's log first when it did not, because a red end-to-end test that
# says only "assertion failed" costs more time than it saves (AC8). This
# make target is "the test" as much as the Go code it runs is; the Go file
# refuses to run on its own without the environment this sets.
#
# The token, DSN and ports below are exactly what test/e2e/compose.yaml and
# compose.postgres.yaml already set up wsaw and the database with.
.PHONY: e2e-postgres-test
# A target-specific value: $(E2E_COMPOSE) below is defined in terms of $(DB),
# and this target's own recipe needs that expansion, not just the DB=postgres
# argument the nested `$(MAKE)` calls get.
e2e-postgres-test: DB := postgres
e2e-postgres-test:
	$(MAKE) --no-print-directory e2e-compose-up DB=postgres
	@WSAW_E2E_API_BASE=http://127.0.0.1:18712 \
	WSAW_E2E_API_TOKEN=wsaw-e2e-fixture-only-token \
	WSAW_E2E_SITE_BASE=http://127.0.0.1:18081 \
	WSAW_E2E_DSN="postgres://wsaw:wsaw-e2e-fixture-only@127.0.0.1:5432/wsaw?sslmode=disable" \
	WSAW_E2E_COMPOSE_CMD="$(E2E_COMPOSE_FROM_TESTDIR)" \
	go test -tags e2e -count=1 -v ./test/e2e/... -run TestPostgresStack; \
	status=$$?; \
	if [ $$status -ne 0 ]; then \
		echo; echo "=== stack logs (the suite failed) ==="; \
		$(E2E_COMPOSE) logs --no-color; \
	fi; \
	$(MAKE) --no-print-directory e2e-compose-down DB=postgres; \
	exit $$status

# The same test again, against MySQL (Story 7.5): one test body parameterized
# by dialect (TestMySQLStack, alongside TestPostgresStack in the same file),
# not two files that would drift, plus the dialect differences that have no
# Postgres equivalent at all — charset/collation, strict sql_mode, timestamp
# precision, the upsert form.
.PHONY: e2e-mysql-test
e2e-mysql-test: DB := mysql
e2e-mysql-test:
	$(MAKE) --no-print-directory e2e-compose-up DB=mysql
	@WSAW_E2E_API_BASE=http://127.0.0.1:18712 \
	WSAW_E2E_API_TOKEN=wsaw-e2e-fixture-only-token \
	WSAW_E2E_SITE_BASE=http://127.0.0.1:18081 \
	WSAW_E2E_DSN="wsaw:wsaw-e2e-fixture-only@tcp(127.0.0.1:3306)/wsaw" \
	WSAW_E2E_COMPOSE_CMD="$(E2E_COMPOSE_FROM_TESTDIR)" \
	go test -tags e2e -count=1 -v ./test/e2e/... -run TestMySQLStack; \
	status=$$?; \
	if [ $$status -ne 0 ]; then \
		echo; echo "=== stack logs (the suite failed) ==="; \
		$(E2E_COMPOSE) logs --no-color; \
	fi; \
	$(MAKE) --no-print-directory e2e-compose-down DB=mysql; \
	exit $$status

# The Podman pod (Story 7.3): the same four services as the Compose stack
# above, but as a genuine pod rather than a translation of one. A pod's
# containers share one network namespace, so they reach each other on
# localhost — which means every one of them needs its own port, unlike
# Compose where each container has its own address and site/tracker can both
# listen on :8080 without conflict. A port collision here is a real failure
# mode, so the ports below are chosen to avoid one (AC2), including against
# e2e-fixture-up's own 8081/8082 on the same machine.
#
# It runs rootless, the same as every other podman target in this file —
# there is nothing pod-specific to grant Chrome's sandbox beyond what
# e2e-fixture-up and e2e-compose-up already need (AC3).
E2E_POD_NAME         ?= wsaw-e2e-pod
E2E_POD_DIR          := $(DIST)/e2e/pod
# wsaw's own documented default, 8712, is exactly the port a wsaw already
# running on this machine (the compose stack, a manual `wsaw run`, another
# deployment) would be using — so, like the fixture's own 8081/8082, the pod
# uses an unobvious one instead of colliding with it.
E2E_POD_API_PORT     ?= 18712
E2E_POD_SITE_PORT    ?= 18081
E2E_POD_TRACKER_PORT ?= 18082
E2E_POD_TOKEN        := wsaw-e2e-fixture-only-token
E2E_POD_IMAGE        := localhost/wsaw:e2e-pod

# The database is picked the same way e2e-compose-up picks one: DB=postgres
# or DB=mysql, this time selecting a set of variables rather than an
# override file, because a pod is one set of `podman run` invocations rather
# than a document another document can be merged into.
ifeq ($(DB),mysql)
E2E_POD_DB_PORT   := 3306
E2E_POD_DB_IMAGE  := docker.io/library/mysql:8.4
E2E_POD_DB_ENV    := -e MYSQL_DATABASE=wsaw -e MYSQL_USER=wsaw \
	-e MYSQL_PASSWORD=wsaw-e2e-fixture-only -e MYSQL_ROOT_PASSWORD=wsaw-e2e-fixture-only-root
# -h127.0.0.1 rather than -h localhost: the mysql client treats "localhost"
# as "connect over the Unix socket", which the init server answers on before
# the real server is listening on TCP — exactly the connection wsaw itself
# makes. A probe that is not forced onto TCP reports ready too early.
E2E_POD_DB_READY  := mysqladmin ping -h127.0.0.1 -uwsaw -pwsaw-e2e-fixture-only --silent
E2E_POD_STORE_DSN := wsaw:wsaw-e2e-fixture-only@tcp(localhost:3306)/wsaw
else
E2E_POD_DB_PORT   := 5432
E2E_POD_DB_IMAGE  := docker.io/library/postgres:16-alpine
E2E_POD_DB_ENV    := -e POSTGRES_USER=wsaw -e POSTGRES_PASSWORD=wsaw-e2e-fixture-only -e POSTGRES_DB=wsaw
E2E_POD_DB_READY  := pg_isready -U wsaw -d wsaw
E2E_POD_STORE_DSN := postgres://wsaw:wsaw-e2e-fixture-only@localhost:5432/wsaw?sslmode=disable
endif

# e2e-pod-config writes the wsaw configuration the pod's wsaw container
# mounts. It differs from test/e2e/compose/wsaw.$(DB).yaml only in using
# localhost ports rather than service names — the two are not the same file
# with the hostnames swapped, they express a genuinely different topology
# (AC2) — so it is copied rather than generated from a template.
.PHONY: e2e-pod-config
e2e-pod-config:
	$(call e2e_compose_need_db,e2e-pod-config)
	@mkdir -p $(E2E_POD_DIR)
	@cp test/e2e/pod/wsaw.$(DB).yaml $(E2E_POD_DIR)/wsaw.yaml
	@echo "wrote $(E2E_POD_DIR)/wsaw.yaml"

.PHONY: e2e-pod-up
e2e-pod-up: e2e-fixture-image e2e-pod-down e2e-pod-config
	$(call e2e_compose_need_db,e2e-pod-up)
	$(E2E_RUNTIME) build -q -f Dockerfile -t $(E2E_POD_IMAGE) .
	$(E2E_RUNTIME) pod create --name $(E2E_POD_NAME) \
		-p 127.0.0.1:$(E2E_POD_API_PORT):$(E2E_POD_API_PORT) \
		-p 127.0.0.1:$(E2E_POD_SITE_PORT):$(E2E_POD_SITE_PORT) \
		-p 127.0.0.1:$(E2E_POD_TRACKER_PORT):$(E2E_POD_TRACKER_PORT) \
		-p 127.0.0.1:$(E2E_POD_DB_PORT):$(E2E_POD_DB_PORT)
	$(E2E_RUNTIME) run -d --pod $(E2E_POD_NAME) --name $(E2E_POD_NAME)-tracker \
		$(E2E_FIXTURE_IMAGE) -role=third-party -listen=:$(E2E_POD_TRACKER_PORT) \
		-self-base=http://localhost:$(E2E_POD_TRACKER_PORT)
	$(E2E_RUNTIME) run -d --pod $(E2E_POD_NAME) --name $(E2E_POD_NAME)-site \
		$(E2E_FIXTURE_IMAGE) -role=site -listen=:$(E2E_POD_SITE_PORT) \
		-third-party-base=http://localhost:$(E2E_POD_TRACKER_PORT)
	$(E2E_RUNTIME) run -d --pod $(E2E_POD_NAME) --name $(E2E_POD_NAME)-db \
		$(E2E_POD_DB_ENV) $(E2E_POD_DB_IMAGE)
	@printf 'waiting for the database '
	@for i in $$(seq 1 50); do \
		$(E2E_RUNTIME) exec $(E2E_POD_NAME)-db sh -c '$(E2E_POD_DB_READY)' >/dev/null 2>&1 && { echo ok; break; }; \
		if [ "$$i" = 50 ]; then \
			echo; echo "the database never became ready:"; $(E2E_RUNTIME) logs $(E2E_POD_NAME)-db; exit 1; \
		fi; \
		sleep 0.5; \
	done
	$(E2E_RUNTIME) run -d --pod $(E2E_POD_NAME) --name $(E2E_POD_NAME)-wsaw \
		--shm-size=1gb \
		-e WSAW_STORE_DSN="$(E2E_POD_STORE_DSN)" \
		-e WSAW_API_TOKEN=$(E2E_POD_TOKEN) \
		-v $(CURDIR)/$(E2E_POD_DIR)/wsaw.yaml:/etc/wsaw/wsaw.yaml:ro \
		$(E2E_POD_IMAGE)
	@for name_port in site:$(E2E_POD_SITE_PORT):/__fixture/healthz tracker:$(E2E_POD_TRACKER_PORT):/__fixture/healthz wsaw:$(E2E_POD_API_PORT):/api/v1/health; do \
		name=$${name_port%%:*}; rest=$${name_port#*:}; port=$${rest%%:*}; path=$${rest#*:}; \
		printf 'waiting for %s on 127.0.0.1:%s ' "$$name" "$$port"; \
		for i in $$(seq 1 50); do \
			if curl -fsS -H "Authorization: Bearer $(E2E_POD_TOKEN)" "http://127.0.0.1:$$port$$path" >/dev/null 2>&1; then \
				echo ok; break; \
			fi; \
			if [ "$$i" = 50 ]; then \
				echo; echo "$$name never became healthy:"; $(E2E_RUNTIME) logs $(E2E_POD_NAME)-$$name 2>&1 | tail -20; exit 1; \
			fi; \
			sleep 0.2; \
		done; \
	done
	@echo
	@echo "The pod is up, wsaw included, all reporting healthy."
	@echo "Scan it:            make e2e-pod-scan DB=$(DB)"
	@echo "Watch what it says: make e2e-pod-logs"
	@echo "Take it down:       make e2e-pod-down"

.PHONY: e2e-pod-scan
e2e-pod-scan:
	@for mode in none reject accept; do \
		echo "scanning fixture ($$mode)..."; \
		curl -fsS -X POST -H "Authorization: Bearer $(E2E_POD_TOKEN)" \
			"http://127.0.0.1:$(E2E_POD_API_PORT)/api/v1/scan/fixture/$$mode" >/dev/null || exit 1; \
	done
	@echo "done — read the results back with SQL, not through wsaw (Story 7.4, AC1)."

.PHONY: e2e-pod-down
e2e-pod-down:
	@$(E2E_RUNTIME) pod rm -f $(E2E_POD_NAME) >/dev/null 2>&1 || true

.PHONY: e2e-pod-logs
e2e-pod-logs:
	$(E2E_RUNTIME) logs -f $(E2E_POD_NAME)-tracker $(E2E_POD_NAME)-site $(E2E_POD_NAME)-db $(E2E_POD_NAME)-wsaw

.PHONY: docker
docker:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t wsaw:$(VERSION) .

.PHONY: clean
clean:
	rm -rf $(DIST) coverage.out
