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

.PHONY: cover
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# cover-report renders the same profile as a browsable page: every package
# ranked, then per file and per function. The generator is named as a file
# rather than a package because it carries //go:build ignore — that keeps it
# out of ./... so the tool never shows up in the coverage it reports on.
COVER_REPORT ?= coverage-report.html

.PHONY: cover-report
cover-report:
	go test -coverprofile=coverage.out -covermode=atomic ./...
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
