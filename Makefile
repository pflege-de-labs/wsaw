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

.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null || (echo "golangci-lint is not installed: https://golangci-lint.run/welcome/install/" && exit 1)
	golangci-lint run

# test runs the fast suite. Browser-dependent tests skip themselves when no
# usable Chrome is present, so this works on a machine without one.
.PHONY: test
test:
	go test -race ./...

# test-fast skips browser tests explicitly, for a tight edit loop.
.PHONY: test-fast
test-fast:
	WSAW_SKIP_BROWSER_TESTS=1 go test ./...

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
