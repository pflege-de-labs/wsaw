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

# The platforms the *cloud* variant is cross-compiled for. Every one of them by
# default, so a release ships the full set; CI narrows it to a single target on
# a pull request, because a cross-compilation break in a pure-Go SDK is a
# compile error that one non-host target catches exactly as well as four, and
# each cloud target costs about 455 extra packages and ~53 MB of artifact
# (Story 8.8, AC1). Every value here must also be in PLATFORMS.
CLOUD_PLATFORMS ?= $(PLATFORMS)

# What the binary weighed before Epic 8, per platform, so `make sizes` can
# state the number Story 8.8, AC2 actually asks for: the change from adding the
# blob drivers, not merely the difference between the two variants.
#
# Measured at PRE_EPIC_COMMIT with the same flags this Makefile uses —
# CGO_ENABLED=0 -trimpath -ldflags "-s -w" — so the only difference between the
# baseline and the default column is this branch. Almost all of it is
# gocloud.dev and its local driver, but the epic's own store code is in there
# too, which is why the column is named +epic8 and not +gocloud: it is the
# figure a reader can check against a binary, not an attribution.
#
# The baselines are fixed at that commit, so they do not need re-measuring; the
# default and cloudblob columns are measured from the artifacts on every run,
# and move as this branch grows. Change a baseline only if the pre-epic commit
# itself is corrected, and say so here — docs/dependency-review-gocloud.md
# records the method and the toolchain.
PRE_EPIC_COMMIT := ac072a9
BASELINES := \
	linux/amd64=21344416 \
	linux/arm64=20381856 \
	darwin/amd64=21829936 \
	darwin/arm64=20927106

# Two variants are built, and the difference between them is what they can
# reach: the default one links the local file bucket and nothing else, while
# the cloudblob one adds the S3, GCS and Azure drivers (Story 8.8, AC3).
#
# The tag exists because of what those SDKs weigh — `make sizes` prints the
# number for every artifact, and docs/dependency-review-gocloud.md records the
# decision and the modules it pulled in. Both are released, and their names
# say which is which.
CLOUD_TAGS := cloudblob

DIST := dist

.PHONY: all
all: check build

# CGO_ENABLED=0 here as well as in release, so a local build is the same kind
# of binary as a released one. CGo-free is the promise cross-compilation rests
# on (Tenet 14), and a developer's machine is where it would first be broken.
.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/wsaw ./cmd/wsaw

# build-cloudblob is the same binary with the cloud drivers linked in. It is a
# separate output rather than a variable on `build`, so both are on disk at
# once and `make verify-variants` has the pair it compares.
.PHONY: build-cloudblob
build-cloudblob:
	CGO_ENABLED=0 go build -tags $(CLOUD_TAGS) -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/wsaw_$(CLOUD_TAGS) ./cmd/wsaw

.PHONY: install
install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/wsaw

# release cross-compiles every supported platform, in both variants. Trimpath
# and the commit-derived date keep the output reproducible.
#
# Cross-compilation is verified for the cloud build too, because that is where
# it could realistically break — the three SDKs are the only dependencies large
# enough to smuggle in a platform assumption (Story 8.8, AC1). How many cloud
# targets is CLOUD_PLATFORMS; a full release builds all of them.
#
# SIZES is written before the checksums and is covered by them. It is the
# record of what the release weighs, so it must be as verifiable as the things
# it describes.
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
	@for platform in $(CLOUD_PLATFORMS); do \
		GOOS=$${platform%/*}; GOARCH=$${platform#*/}; \
		out=$(DIST)/wsaw_$(CLOUD_TAGS)_$${GOOS}_$${GOARCH}; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH \
			go build -tags $(CLOUD_TAGS) -trimpath -ldflags "$(LDFLAGS)" \
				-o $$out ./cmd/wsaw || exit 1; \
	done
	@$(MAKE) --no-print-directory sizes > $(DIST)/SIZES
	@cat $(DIST)/SIZES
	@echo "sizes written to $(DIST)/SIZES"
	@cd $(DIST) && shasum -a 256 wsaw_* SIZES > SHA256SUMS
	@echo "checksums written to $(DIST)/SHA256SUMS"
	@$(MAKE) --no-print-directory verify-variants

# sizes states what the release weighs, per platform and per variant, and what
# adding the blob drivers cost.
#
# A number, not an assurance (Story 8.8, AC2), and three numbers rather than
# one: the binary before this epic, the binary now, and the binary with the
# cloud SDKs. The middle delta is the one the story is about, so it is in the
# file the release publishes rather than only in a document beside it.
#
# `wc -c` rather than stat or du, because stat's flags differ between BSD and
# GNU and du rounds to blocks; the output is fixed-width and ordered so two
# releases can be diffed.
#
# The toolchain is printed with the numbers. Go binary size moves by megabytes
# between minor releases and by tens of kilobytes between patch releases, so a
# size with no toolchain beside it is not a measurement. CI pins an exact patch
# for the same reason — see GO_VERSION in .github/workflows/ci.yaml.
#
# There is no release-notes generator in this repository, so the numbers go
# where the release artifacts are listed: $(DIST)/SIZES, written by `release`,
# covered by SHA256SUMS, and repeated in the CI job summary.
.PHONY: sizes
sizes:
	@echo "# wsaw release artifact sizes, in bytes, for $(VERSION)."
	@printf '# built with %s, CGO_ENABLED=0 -trimpath -ldflags "-s -w"\n' "$$(go env GOVERSION)"
	@echo "# baseline is the same binary at $(PRE_EPIC_COMMIT), the commit before Epic 8"
	@echo "# added gocloud.dev; +epic8 is what this branch costs over it, nearly all"
	@echo "# of it the blob drivers (Story 8.8, AC2)."
	@echo "# cloudblob is the same build with the S3, GCS and Azure drivers linked"
	@echo "# in; see docs/dependency-review-gocloud.md for the decision. A dash in"
	@echo "# the cloudblob columns means that platform was not built with the tag."
	@printf '# %-12s %12s %12s %12s %6s %12s %12s %6s\n' \
		platform baseline default +epic8 pct cloudblob +cloud pct
	@for platform in $(PLATFORMS); do \
		GOOS=$${platform%/*}; GOARCH=$${platform#*/}; \
		lean=$(DIST)/wsaw_$${GOOS}_$${GOARCH}; \
		full=$(DIST)/wsaw_$(CLOUD_TAGS)_$${GOOS}_$${GOARCH}; \
		test -f $$lean || { echo "$$lean is not built; run make release first" >&2; exit 2; }; \
		l=$$(wc -c < $$lean | tr -d ' '); \
		base=""; \
		for kv in $(BASELINES); do \
			case $$kv in $$platform=*) base=$${kv#*=};; esac; \
		done; \
		if [ -n "$$base" ]; then \
			epic="+$$((l - base))"; epicpct="+$$(((l - base) * 100 / base))%"; \
		else \
			base="-"; epic="-"; epicpct="-"; \
		fi; \
		if [ -f $$full ]; then \
			f=$$(wc -c < $$full | tr -d ' '); \
			cloud="+$$((f - l))"; cloudpct="+$$(((f - l) * 100 / l))%"; \
		else \
			f="-"; cloud="-"; cloudpct="-"; \
		fi; \
		printf '  %-12s %12s %12s %12s %6s %12s %12s %6s\n' \
			"$$platform" "$$base" "$$l" "$$epic" "$$epicpct" "$$f" "$$cloud" "$$cloudpct"; \
	done

# The three provider SDKs, by module path. A module is either recorded in a
# binary's build information or it is not, so this is an exact-field test
# rather than a search for a plausible-looking string.
CLOUD_MODULES := \
	github.com/aws/aws-sdk-go-v2/service/s3 \
	cloud.google.com/go/storage \
	github.com/Azure/azure-sdk-for-go/sdk/storage/azblob

# The three driver packages that register the schemes. Their presence is the
# whole of the cloud build's claim, so these are what the cloud build is
# required to contain.
CLOUD_DRIVER_SYMBOLS := \
	gocloud.dev/blob/s3blob. \
	gocloud.dev/blob/gcsblob. \
	gocloud.dev/blob/azureblob.

# The drivers, plus the entry point of each provider's credential chain. This
# longer list is asserted *absent* from the default build only — resolving a
# credential chain is the specific thing Story 8.8, AC4 forbids there.
#
# It is deliberately not required present in the cloud build. A gocloud.dev
# release that builds its chain through a different function, or a linker that
# eliminates one of the three, would then fail every release for an upstream
# refactor that broke nothing wsaw depends on.
CLOUD_SYMBOLS := $(CLOUD_DRIVER_SYMBOLS) \
	github.com/aws/aws-sdk-go-v2/config.LoadDefaultConfig \
	cloud.google.com/go/storage.NewClient \
	azidentity.NewDefaultAzureCredential

# The control probe for the string half of the check. It must be present in
# *both* variants: if it ever goes missing, the linker has stopped keeping the
# names these checks read, and every symbol-absence assertion would be passing
# for the wrong reason. The module half has its own probe, below.
LOCAL_SYMBOL := gocloud.dev/blob/fileblob.

# The control probe for the module half. gocloud.dev is recorded in the build
# information of both variants, so an absence assertion that runs against
# build information without seeing it is not reading build information at all.
LOCAL_MODULE := gocloud.dev

# verify-variants proves from the outside what the build tag buys: the default
# binary contains no cloud SDK, and the cloudblob binary contains all three
# (Story 8.8, AC4).
#
# It reads the binaries rather than the source, because the claim is about
# what was linked, and it reads two independent things — the module list the
# toolchain records in every Go binary, and the function names the runtime
# keeps for tracebacks. `go tool nm` is not used: released binaries are linked
# with -s, which leaves it nothing to read.
#
# grep -a and LC_ALL=C because the input is a binary and the pattern must not
# be reinterpreted by a locale.
#
# The build information is read once per binary, into a variable, and a failure
# to read it is a failure of the check. Piping `go version -m` into awk would
# take the pipeline's exit status from awk, so an unreadable file — a truncated
# binary, a toolchain that cannot parse its build information — would look
# exactly like a binary that records no cloud module, and every absence
# assertion would pass without anything having been read.
#
# The file list is the one the build was supposed to produce, derived from
# PLATFORMS and CLOUD_PLATFORMS, rather than whatever happens to be in $(DIST).
# A glob verifies a stale binary from an earlier branch as though it were part
# of this release, and — worse — lets a leftover cloudblob artifact satisfy the
# "both variants present" precondition after a plain `make build`.
.PHONY: verify-variants
verify-variants:
	@lean=""; full=""; \
	if [ -f $(DIST)/wsaw ] && [ -f $(DIST)/wsaw_$(CLOUD_TAGS) ]; then \
		lean="$(DIST)/wsaw"; full="$(DIST)/wsaw_$(CLOUD_TAGS)"; \
	else \
		for platform in $(PLATFORMS); do \
			lean="$$lean $(DIST)/wsaw_$${platform%/*}_$${platform#*/}"; \
		done; \
		for platform in $(CLOUD_PLATFORMS); do \
			full="$$full $(DIST)/wsaw_$(CLOUD_TAGS)_$${platform%/*}_$${platform#*/}"; \
		done; \
	fi; \
	missing=0; \
	for f in $$lean $$full; do \
		test -f $$f || { echo "expected $$f, which is not there" >&2; missing=1; }; \
	done; \
	if [ $$missing -ne 0 ]; then \
		echo "run make release, or make build build-cloudblob" >&2; \
		exit 2; \
	fi; \
	status=0; \
	for f in $$lean; do \
		echo "checking $$f links no cloud SDK"; \
		info=$$(go version -m $$f) || { \
			echo "  FAIL: cannot read the build information of $$f"; status=1; continue; }; \
		printf '%s\n' "$$info" | \
			awk '$$1 == "dep" && $$2 == "$(LOCAL_MODULE)" { ok = 1 } END { exit !ok }' || { \
				echo "  FAIL: the build information does not record $(LOCAL_MODULE), so the module checks prove nothing"; \
				status=1; }; \
		for m in $(CLOUD_MODULES); do \
			if printf '%s\n' "$$info" | \
				awk -v m=$$m '$$1 == "dep" && $$2 == m { ok = 1 } END { exit !ok }'; then \
				echo "  FAIL: build information records the module $$m"; status=1; \
			fi; \
		done; \
		for s in $(CLOUD_SYMBOLS); do \
			if LC_ALL=C grep -a -q -F $$s $$f; then \
				echo "  FAIL: the binary contains $$s"; status=1; \
			fi; \
		done; \
		LC_ALL=C grep -a -q -F $(LOCAL_SYMBOL) $$f || { \
			echo "  FAIL: the binary does not contain $(LOCAL_SYMBOL), so the symbol checks prove nothing"; \
			status=1; }; \
	done; \
	for f in $$full; do \
		echo "checking $$f links all three cloud SDKs"; \
		info=$$(go version -m $$f) || { \
			echo "  FAIL: cannot read the build information of $$f"; status=1; continue; }; \
		for m in $(CLOUD_MODULES); do \
			printf '%s\n' "$$info" | \
				awk -v m=$$m '$$1 == "dep" && $$2 == m { ok = 1 } END { exit !ok }' || { \
					echo "  FAIL: build information does not record the module $$m"; status=1; }; \
		done; \
		for s in $(CLOUD_DRIVER_SYMBOLS) $(LOCAL_SYMBOL); do \
			LC_ALL=C grep -a -q -F $$s $$f || { \
				echo "  FAIL: the binary does not contain $$s"; status=1; }; \
		done; \
	done; \
	test $$status -eq 0 || exit 1; \
	echo "verified: the default build opens no cloud SDK and resolves no credential chain"

# Pinned, all of them, and kept in step with the env block in
# .github/workflows/ci.yaml. A floating tool can change a gate's verdict
# without anything in this repository changing — and for the scanner and the
# SBOM generator that is worse than for the others, because AC5 makes their
# output part of what a release publishes: today's SBOM and today's vulncheck
# verdict have to still mean something tomorrow.
GO_LICENSES_VERSION      ?= v2.0.1
GOVULNCHECK_VERSION      ?= v1.7.0
CYCLONEDX_GOMOD_VERSION  ?= v1.12.0

# check is the local gate, and it is meant to be the same answer CI gives.
#
# test-cloudblob is in it because the build tag's file is invisible to `go
# build`, `go vet` and the linter alike, so without it the first thing to
# compile bucket_cloud.go is CI (Story 8.8, AC3). The compile is the cheap half
# and it catches the realistic failure.
#
# licenses-default rather than licenses: go-licenses classifies every licence
# file in the module cache, and the cloud run walks the three largest
# dependency trees wsaw has. Its verdict only changes when go.mod does, and CI
# runs both variants on every push, so paying for the second walk on each local
# edit buys nothing. `make licenses` runs both, deliberately, when go.mod moved.
#
# verify-variants is not here either: it needs a release to have been built.
.PHONY: check
check: fmt vet lint licenses-default test test-cloudblob

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

# Twice, for the same reason the licence gate and govulncheck run twice: a file
# whose build constraint is off is invisible to the linter. Without the second
# run, internal/store/bucket_cloud.go — the one file the tag adds — is never
# seen by godox, gosec, staticcheck or anything else in the merge gate, so a
# leftover work marker or a gosec finding in it would pass CI unnoticed
# (Story 8.8, AC3).
#
# A second invocation rather than build-tags in .golangci.yaml, which would
# swap one blind spot for another: the tagged run cannot see a file the tag
# excludes, and today there is none, but the arrangement should not depend on
# that staying true.
.PHONY: lint
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout 5m
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) \
		run --timeout 5m --build-tags $(CLOUD_TAGS)

# test runs the fast suite. Browser-dependent tests skip themselves when no
# usable Chrome is present, so this works on a machine without one.
.PHONY: test
test:
	go test -race ./...

# test-fast skips browser tests explicitly, for a tight edit loop.
.PHONY: test-fast
test-fast:
	WSAW_SKIP_BROWSER_TESTS=1 go test ./...

# test-cloudblob covers the build the tag produces (Story 8.8, AC3). Without
# it, nothing compiles bucket_cloud.go: `go build`, `go vet` and the linter
# all ignore a file whose build constraint is off, so a released variant could
# be broken for a whole epic without a single job going red.
#
# The store is the only package the tag can change, so it is the only one
# tested twice; everything else is compiled, which is what catches the failure
# mode that matters here — a driver that stops building.
.PHONY: test-cloudblob
test-cloudblob:
	go build -tags $(CLOUD_TAGS) ./...
	go vet -tags $(CLOUD_TAGS) ./...
	go test -race -tags $(CLOUD_TAGS) ./internal/store/

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
#
# Twice, because a build tag changes which packages exist: the cloud SDKs are
# invisible to the default run, and a licence gate that never looks at the
# three largest dependency trees in the module is not a gate (Story 8.8, AC5).
# GOFLAGS is how the tag reaches the analyser — go-licenses has no flag of its
# own for it.
.PHONY: licenses
licenses: licenses-default licenses-cloudblob

.PHONY: licenses-default
licenses-default:
	go run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... \
		--allowed_licenses=MIT,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

.PHONY: licenses-cloudblob
licenses-cloudblob:
	GOFLAGS=-tags=$(CLOUD_TAGS) \
		go run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... \
			--allowed_licenses=MIT,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

# Also twice, and for the same reason: govulncheck reports what the analysed
# build can reach, so a vulnerability in an SDK that only the cloud build
# links is reported by only the cloud build's run.
.PHONY: vulncheck
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) -tags $(CLOUD_TAGS) ./...

# One SBOM per released variant, named after it. They are genuinely different
# documents — the cloud build's covers three SDKs the default one never links,
# 97 dependency modules recorded in the cloud binary against 42 in the default
# one — and a release that published only the smaller document would understate
# what it ships (Story 6.1, AC3; Story 8.8, AC5).
#
# cyclonedx-gomod takes its build constraints from the environment rather than
# from flags, so CGO_ENABLED and GOFLAGS are how the released build is
# described; each document records the tag it was generated under as
# cdx:gomod:build:tag, so the two cannot be confused once separated from their
# filenames.
#
# Both documents describe the module selection for the machine they are
# generated on, which in CI is linux/amd64. wsaw has no GOOS-conditional
# requirement in go.mod, so the selection is the same for all four released
# platforms; if one is ever added, this has to grow a GOOS/GOARCH loop the way
# `release` has one.
#
# Note for anyone running this from a linked git worktree: cyclonedx-gomod
# determines the main module's version through go-git, which cannot read a
# worktree's .git file, and fails with "failed to determine version of main
# module: git: reference not found". It is not caused by anything here — the
# same command fails identically on main — and CI, which checks out normally,
# is unaffected. Run it from a normal clone.
.PHONY: sbom
sbom:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 \
		go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_GOMOD_VERSION) app \
			-json -output $(DIST)/wsaw.cdx.json -main cmd/wsaw .
	CGO_ENABLED=0 GOFLAGS=-tags=$(CLOUD_TAGS) \
		go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_GOMOD_VERSION) app \
			-json -output $(DIST)/wsaw-$(CLOUD_TAGS).cdx.json -main cmd/wsaw .

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

# The image follows the binaries: `make docker` builds the default one, and
# `make docker BUILD_TAGS=cloudblob` the one that can reach S3, GCS and Azure.
# The tag goes into the image name, because an image is pulled by name and two
# images that differ by some 28 MB of SDK must not share one. The image also
# carries org.opencontainers.image.variant, so a pulled image can answer the
# question without its tag being trusted.
BUILD_TAGS ?=
IMAGE_NAME := wsaw$(if $(BUILD_TAGS),-$(BUILD_TAGS),)

.PHONY: docker
docker:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		--build-arg BUILD_TAGS=$(BUILD_TAGS) \
		-t $(IMAGE_NAME):$(VERSION) .

.PHONY: clean
clean:
	rm -rf $(DIST) coverage.out
