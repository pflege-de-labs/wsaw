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

.PHONY: check
check: fmt vet lint test

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

# soak runs the long-running stability test, which is deliberately separate
# from the regular suite (Story 6.8).
.PHONY: soak
soak:
	go test -tags soak -timeout 60m -run TestSoak ./internal/soak/

.PHONY: vulncheck
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: sbom
sbom:
	@mkdir -p $(DIST)
	go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest app \
		-json -output $(DIST)/wsaw.cdx.json -main cmd/wsaw .

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
