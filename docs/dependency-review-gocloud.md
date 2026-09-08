# The gocloud.dev dependency, reviewed

Epic 8 moves evidence into a bucket, and reaches the bucket through
`gocloud.dev/blob` rather than through three hand-written provider clients.
That is the largest dependency wsaw has ever taken, in a tool whose selling
point is detecting supply-chain changes (Tenet 18). Story 8.8 asks for the
cost to be stated rather than assumed, and for the modules it brings to be
looked at once, deliberately, instead of arriving as transitive noise
(Story 8.8, AC5).

This is that review. Every number in it was measured on 2026-09-08 against
`gocloud.dev v0.46.0`, on the commit that introduced it, with Go 1.27.1 — the
toolchain `.github/workflows/ci.yaml` pins as `GO_VERSION`, so `dist/SIZES`
from a CI run is comparable with the table below rather than merely similar to
it.

**This document is a point-in-time measurement of that commit.** The byte
counts and module versions below are what was true when it was written; they
are not maintained per commit. The live numbers are `make sizes` and the
`dist/SIZES` that every release and every CI run publishes, which carry their
own toolchain and their own pre-epic baseline. Where the two disagree, the
artifact is right and this document is stale.

Reproduce it with:

```
make release          # eight binaries, four platforms, two variants
make sizes            # the size table, also written to dist/SIZES
make verify-variants  # the default build carries no cloud SDK
make check            # the local gate, including the cloudblob compile
make licenses         # the licence gate, once per variant
make sbom             # one SBOM per variant
make vulncheck        # govulncheck, once per variant
```

and the module lists with:

```
go list -deps -f '{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}' ./cmd/wsaw
go list -tags cloudblob -deps -f '{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}' ./cmd/wsaw
```

compared against the same command on the commit before this epic.

## The decision

`gocloud.dev/blob` was preferred over three per-provider clients because the
credential chains and the retry behaviour are the expensive part of talking to
object storage, and they are the part that is wrong in subtle ways when
hand-written. What it costs is a dependency tree that is larger than the rest
of wsaw put together.

The cost is not evenly distributed, and that asymmetry is why there is a build
tag. The local file driver is cheap; the three cloud SDKs are not. So the
default build links `fileblob` and nothing else — the in-memory driver is
registered by the store's test suite, not by the shipped binary — and
`s3blob`, `gcsblob` and `azureblob` are registered in
`internal/store/bucket_cloud.go` behind `//go:build cloudblob`. Both builds
are released, named after the difference:

| artifact | reaches |
| --- | --- |
| `wsaw_<os>_<arch>` | a local directory, or any `file://` URL |
| `wsaw_cloudblob_<os>_<arch>` | the same, plus `s3://`, `gs://` and `azblob://` |

Checksums (`dist/SHA256SUMS`), sizes (`dist/SIZES`) and SBOMs
(`dist/wsaw.cdx.json`, `dist/wsaw-cloudblob.cdx.json`) cover both.

The container image splits the same way: `make docker` builds the default one
and `make docker BUILD_TAGS=cloudblob` the one that can reach the three
providers. The tag goes into the image name, and the image itself carries
`org.opencontainers.image.variant` — `default` or `cloudblob` — so an image
that has been pulled, retagged or handed on can still say which of the two it
is, rather than the answer living only in a name someone chose to type.

A binary that meets a scheme it cannot reach says so and names the build that
can, rather than failing with an unsupported-scheme error — see
`cloudBuildHint` in `internal/store/bucket.go`.

## What it costs in bytes

Measured with `make release` on this branch and on the commit before the epic
(`ac072a9`), so every column is `CGO_ENABLED=0 go build -trimpath -ldflags
"-s -w …"` — the binaries that actually ship, not a plain `go build`, which is
about half again as large (37,245,842 bytes for the default `darwin/arm64`
build against the 25,406,930 below) and is not what anybody downloads. The
baseline was built from a tagless export of that commit, so its `-X` strings
are a few dozen bytes shorter than a released build's; nothing else differs.

The columns are the columns of `dist/SIZES`, and the percentages are the
integer percentages `make sizes` prints, so the two records can be compared
line by line. The baseline column is fixed at that commit; the other two are
measured from the artifacts on every run and move as the branch grows, so
`dist/SIZES` — not this table — is the record for a given release:

| platform | baseline | default | +epic8 | cloudblob | +cloud |
| --- | ---: | ---: | ---: | ---: | ---: |
| `linux/amd64` | 21,344,416 | 25,837,728 | +4,493,312 (+21%) | 55,099,552 | +29,261,824 (+113%) |
| `linux/arm64` | 20,381,856 | 24,576,160 | +4,194,304 (+20%) | 51,511,456 | +26,935,296 (+109%) |
| `darwin/amd64` | 21,829,936 | 26,561,824 | +4,731,888 (+21%) | 56,689,456 | +30,127,632 (+113%) |
| `darwin/arm64` | 20,927,106 | 25,406,930 | +4,479,824 (+21%) | 53,474,562 | +28,067,632 (+110%) |

`+epic8` is the default binary against the pre-epic baseline: the number
Story 8.8, AC2 asks for. `+cloud` is the cloud binary against the default one.
Against the baseline the cloud build is 2.5x to 2.6x the pre-epic binary.

The shape of it: about a fifth more binary for the storage abstraction and the
local driver, and two and a half times the binary for the three cloud SDKs.
The second number is what a deployment storing evidence on a disk would have
been made to carry, and it is the whole argument for the tag.

`make sizes` prints this table from whatever `make release` produced — with the
toolchain that produced it, and with the same pre-epic baseline, which lives in
the Makefile as `BASELINES` — and `make release` writes it to `dist/SIZES` and
covers it with `SHA256SUMS`. There is no release-notes generator in this
repository, so that file — plus the CI job summary, which repeats it on every
push — is where the number is recorded (Story 8.8, AC2).

## What it costs in modules

| | count |
| --- | ---: |
| `go.mod` requirements before | 30 |
| `go.mod` requirements after | 101 |
| requirements added | 72 |
| requirements removed | 1 |
| module paths in `go.sum` before | 53 |
| module paths in `go.sum` after | 141 |
| modules linked into the default build, added | 16 |
| modules linked into the cloud build only, added | 55 |
| modules required but linked into neither | 1 |

The last two rows are read off the binaries rather than off `go.mod`, so they
can be rechecked: `go version -m dist/wsaw_linux_amd64 | awk '$1=="dep"'`
records 42 modules, the same command on `dist/wsaw_cloudblob_linux_amd64`
records 97, and the pre-epic binary records 26 — 42 − 26 = 16 and
97 − 42 = 55.

The counts reconcile as 30 + 72 − 1 = 101. The one removed is
`github.com/kr/text`, which is not a loss of anything: it is still in the
module graph — `go mod why -m github.com/kr/text` reaches it through
`gopkg.in/yaml.v3`'s test dependencies — but with the larger graph its version
is selected elsewhere, so `go.mod` no longer has to name it. It is mentioned
only so that the numbers above add up on the page rather than appearing to be
one out.

The one required and never linked is `github.com/planetscale/vtprotobuf`,
which is in the module graph because gRPC's generated code references it.
Every other module added to `go.mod` ends up in one of the two binaries.

### The 16 that are in every build

These are the price of `gocloud.dev/blob` itself — none of them comes from a
cloud driver, and the default binary carries all of them.

| module | version | licence | why |
| --- | --- | --- | --- |
| `gocloud.dev` | v0.46.0 | Apache-2.0 | the bucket abstraction; the dependency this review is about |
| `go.opentelemetry.io/otel` | v1.43.0 | Apache-2.0 | `gocloud.dev/blob` → `gocloud.dev/internal/otel` |
| `go.opentelemetry.io/otel/metric` | v1.43.0 | Apache-2.0 | as above |
| `go.opentelemetry.io/otel/trace` | v1.43.0 | Apache-2.0 | as above |
| `go.opentelemetry.io/otel/sdk` | v1.43.0 | Apache-2.0 | as above; the SDK, not only the API |
| `go.opentelemetry.io/otel/sdk/metric` | v1.43.0 | Apache-2.0 | as above |
| `go.opentelemetry.io/auto/sdk` | v1.2.1 | Apache-2.0 | pulled by `otel/trace` |
| `github.com/go-logr/logr` | v1.4.3 | Apache-2.0 | OpenTelemetry's logging interface |
| `github.com/go-logr/stdr` | v1.2.2 | Apache-2.0 | its standard-library implementation |
| `github.com/cespare/xxhash/v2` | v2.3.0 | MIT | `otel/attribute`'s set hashing |
| `google.golang.org/grpc` | v1.82.1 | Apache-2.0 | `gocloud.dev/internal/gcerr` uses `grpc/codes` and `grpc/status`; `gax-go` uses the client |
| `google.golang.org/protobuf` | v1.36.11 | BSD-3-Clause | required by gRPC |
| `google.golang.org/genproto/googleapis/rpc` | (2026-04-14) | Apache-2.0 | gRPC status details |
| `google.golang.org/api` | v0.272.0 | BSD-3-Clause | `gocloud.dev/internal/useragent` imports `api/option` |
| `github.com/googleapis/gax-go/v2` | v2.19.0 | BSD-3-Clause | `gocloud.dev/internal/retry` |
| `golang.org/x/xerrors` | (2024-09-03) | BSD-3-Clause | `gocloud.dev/internal/gcerr` |

Worth stating plainly, because it is the part that surprises: **the lean build
links gRPC, protobuf and the OpenTelemetry metrics SDK**, and wsaw calls none
of them directly. They arrive because `gocloud.dev/blob` instruments itself
with OpenTelemetry, expresses its error codes as gRPC codes, and reaches for
`google.golang.org/api/option` in its user-agent helper. That is where most of
the +4.3 MB goes, and it is not removable without giving up the library.

### The 55 that only the cloud build links

Attributed by taking each driver package's module closure
(`go list -deps gocloud.dev/blob/s3blob`, and the same for `gcsblob` and
`azureblob`) and asking which closures contain the module. A module in more
than one closure is listed once, under all of them.

**S3 (`s3blob`) — 19 modules, all Apache-2.0.** The AWS SDK v2, split the way
AWS splits it: the core, the S3 service client, and the credential providers
each installation has an opinion about.

| module | version | licence |
| --- | --- | --- |
| `github.com/aws/aws-sdk-go-v2` | v1.41.9 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream` | v1.7.11 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/config` | v1.32.20 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/credentials` | v1.19.19 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/feature/ec2/imds` | v1.18.25 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager` | v0.2.3 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/internal/configsources` | v1.4.25 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2` | v2.7.25 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/internal/v4a` | v1.4.26 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding` | v1.13.10 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/internal/checksum` | v1.9.18 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/internal/presigned-url` | v1.13.25 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/internal/s3shared` | v1.19.25 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/s3` | v1.102.2 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/signin` | v1.1.1 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/sso` | v1.30.19 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/ssooidc` | v1.36.2 | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/sts` | v1.42.3 | Apache-2.0 |
| `github.com/aws/smithy-go` | v1.26.0 | Apache-2.0 |

**Google Cloud Storage (`gcsblob`) — 20 modules.** The expensive one, and not
because of the storage client. `cloud.google.com/go/storage` links gRPC's xDS
stack (for Direct Path) and a Cloud Monitoring metric exporter, which is where
`cel.dev/expr`, the Envoy and xDS packages, SPIFFE, and
`cloud.google.com/go/monitoring` come from. A deployment writing objects to a
bucket gets a service-mesh resolver and a metrics exporter with it.

| module | version | licence |
| --- | --- | --- |
| `cel.dev/expr` | v0.25.1 | Apache-2.0 |
| `cloud.google.com/go` | v0.123.0 | Apache-2.0 |
| `cloud.google.com/go/iam` | v1.5.3 | Apache-2.0 |
| `cloud.google.com/go/monitoring` | v1.24.3 | Apache-2.0 |
| `cloud.google.com/go/storage` | v1.61.3 | Apache-2.0 |
| `github.com/GoogleCloudPlatform/opentelemetry-operations-go/detectors/gcp` | v1.32.0 | Apache-2.0 |
| `github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric` | v0.55.0 | Apache-2.0 |
| `github.com/GoogleCloudPlatform/opentelemetry-operations-go/internal/resourcemapping` | v0.55.0 | Apache-2.0 |
| `github.com/cncf/xds/go` | (2026-02-02) | Apache-2.0 |
| `github.com/envoyproxy/go-control-plane/envoy` | v1.37.0 | Apache-2.0 |
| `github.com/envoyproxy/protoc-gen-validate` | v1.3.3 | Apache-2.0 |
| `github.com/felixge/httpsnoop` | v1.0.4 | MIT |
| `github.com/go-jose/go-jose/v4` | v4.1.4 | Apache-2.0 |
| `github.com/spiffe/go-spiffe/v2` | v2.6.0 | Apache-2.0 |
| `go.opentelemetry.io/contrib/detectors/gcp` | v1.43.0 | Apache-2.0 |
| `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc` | v0.67.0 | Apache-2.0 |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.67.0 | Apache-2.0 |
| `golang.org/x/time` | v0.15.0 | BSD-3-Clause |
| `google.golang.org/genproto` | (2026-03-16) | Apache-2.0 |
| `google.golang.org/genproto/googleapis/api` | (2026-04-14) | Apache-2.0 |

**Azure Blob Storage (`azureblob`) — 8 modules.** The Azure SDK plus the
Microsoft authentication library, which is what `azidentity`'s default
credential chain is made of. `github.com/pkg/browser` is in there because MSAL
can open an interactive login; wsaw never asks it to.

| module | version | licence |
| --- | --- | --- |
| `github.com/Azure/azure-sdk-for-go/sdk/azcore` | v1.21.0 | MIT |
| `github.com/Azure/azure-sdk-for-go/sdk/azidentity` | v1.13.1 | MIT |
| `github.com/Azure/azure-sdk-for-go/sdk/internal` | v1.11.2 | MIT |
| `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob` | v1.6.4 | MIT |
| `github.com/AzureAD/microsoft-authentication-library-for-go` | v1.7.0 | MIT |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | MIT |
| `github.com/kylelemons/godebug` | v1.1.0 | Apache-2.0 |
| `github.com/pkg/browser` | (2024-01-02) | BSD-2-Clause |

**Shared between drivers — 8 modules.** Google's authentication stack is in
the Azure driver's closure as well as the GCS one, because
`gocloud.dev/internal/useragent` imports `google.golang.org/api/option`; and
`github.com/google/wire` is imported directly by all three drivers for their
URL openers.

| module | version | licence | drivers |
| --- | --- | --- | --- |
| `cloud.google.com/go/auth` | v0.18.2 | Apache-2.0 | `gs`, `azblob` |
| `cloud.google.com/go/auth/oauth2adapt` | v0.2.8 | Apache-2.0 | `gs`, `azblob` |
| `cloud.google.com/go/compute/metadata` | v0.9.0 | Apache-2.0 | `gs`, `azblob` |
| `github.com/google/s2a-go` | v0.1.9 | Apache-2.0 | `gs`, `azblob` |
| `github.com/googleapis/enterprise-certificate-proxy` | v0.3.14 | Apache-2.0 | `gs`, `azblob` |
| `golang.org/x/crypto` | v0.55.0 | BSD-3-Clause | `gs`, `azblob` |
| `golang.org/x/oauth2` | v0.36.0 | BSD-3-Clause | `gs`, `azblob` |
| `github.com/google/wire` | v0.7.0 | Apache-2.0 | `s3`, `gs`, `azblob` |

## Licences

`go-licenses check` passes for both variants against the allowed set
(MIT, Apache-2.0, BSD-2-Clause, BSD-3-Clause, ISC, MPL-2.0). Counted per
package by `go-licenses csv`:

| licence | default build | cloud build |
| --- | ---: | ---: |
| Apache-2.0 | 11 | 55 |
| BSD-3-Clause | 22 | 31 |
| MIT | 17 | 24 |
| BSD-2-Clause | 0 | 1 |
| MPL-2.0 | 1 | 1 |

Nothing this dependency added is copyleft. The single MPL-2.0 entry in both
columns is `github.com/go-sql-driver/mysql`, which predates this epic and is
covered by the reasoning in NFR §9; the one BSD-2-Clause entry is
`github.com/pkg/browser`, in the cloud build only. Every module in the tables
above is Apache-2.0, MIT, BSD-2-Clause or BSD-3-Clause, all of which are
compatible with shipping wsaw under MIT. Apache-2.0 carries a patent grant and
an attribution requirement; the SBOM published per release names every
component and the licence it is under, which is what an attribution notice
would be assembled from.

## Vulnerabilities

`govulncheck` on the commit before this epic: **no vulnerabilities found**.
On this branch it exits zero for both variants, and nothing it reports is
called by wsaw:

| id | module | found | fixed in | variant | reachable | state |
| --- | --- | --- | --- | --- | --- | --- |
| GO-2026-6061 | `google.golang.org/grpc` | v1.79.3 | v1.82.1 | both | was, per govulncheck | **fixed** — bumped to v1.82.1 |
| GO-2026-5158 | `go.opentelemetry.io/otel` | v1.43.0 | v1.44.0 | both | imported, not called | open, not a failure |
| GO-2026-6355 | `golang.org/x/crypto` | v0.55.0 | v0.56.0 | cloud only | required, not called | open, not a failure |
| GO-2026-6354 | `golang.org/x/crypto` | v0.55.0 | v0.56.0 | cloud only | required, not called | open, not a failure |
| GO-2026-5932 | `golang.org/x/crypto` | v0.55.0 | none | cloud only | required, not called | open, not a failure |

GO-2026-6061 was the one that mattered. It made `make vulncheck` exit
non-zero, which is a merge blocker (AGENTS.md §6) and correctly so: gRPC is in
the module graph only because of `gocloud.dev`, so this epic is what introduced
it. The traces govulncheck printed went through `sync.Once.Do`, which is the
usual over-approximation — wsaw opens no gRPC connection in the default build —
but the rule is not to argue with the tool, and there was nothing to argue
about: the fix was a version bump.

`go get google.golang.org/grpc@v1.82.1` is applied. Both variants build, both
pass the licence gate, and `govulncheck` exits zero on both; the OpenTelemetry
and `golang.org/x/crypto` entries remain, reported as imported or required but
not called, which govulncheck does not treat as a failure. The bump carried
four modules forward with it — `genproto/googleapis/{api,rpc}` to the
2026-04-14 snapshot, `go.opentelemetry.io/contrib/detectors/gcp` v1.42.0 →
v1.43.0 and `GoogleCloudPlatform/opentelemetry-operations-go/detectors/gcp`
v1.31.0 → v1.32.0 — all still Apache-2.0, and it added and removed no module.

`govulncheck` itself is pinned (`GOVULNCHECK_VERSION`), in the Makefile and in
CI, as is `cyclonedx-gomod`. A release publishes both their outputs, so a
floating tool would make them unreproducible a week later.

## What is checked from now on, and by what

| claim | checked by | where |
| --- | --- | --- |
| the default build cross-compiles to four platforms, CGo-free | `make release` | CI `cross-compile`, every push and pull request |
| the cloud build cross-compiles too | `make release CLOUD_PLATFORMS=…` | CI `cross-compile`: `linux/arm64` on a pull request, all four on `main`, on a tag and nightly |
| the sizes, and what the epic added, are recorded | `make sizes` → `dist/SIZES` | covered by `SHA256SUMS`, repeated in the CI job summary |
| the default build links no cloud SDK and no credential chain | `make verify-variants` | run by `make release` |
| the cloud build compiles, vets and passes the store suite | `make test-cloudblob` | `make check`; CI `test`, on Linux |
| the cloud-only source is linted, not merely compiled | `make lint` (second run, `--build-tags cloudblob`) | CI `golangci-lint` |
| every dependency of both variants is permissively licensed | `make licenses` | CI `dependency licenses` |
| both variants have an SBOM | `make sbom` | CI `SBOM` |
| neither variant has a called vulnerability | `make vulncheck` | CI `govulncheck` |

One cloud cross-compilation target per pull request rather than four: the three
SDKs are pure Go, so a cross-compilation break in them is a compile error, and
`linux/arm64` — a different OS and a different architecture from the runner —
catches it exactly as well as four targets would, at a quarter of the compile
time and of the artifact storage. `main`, tags and the nightly run build all
four. A pull request also uploads only `SHA256SUMS` and `SIZES` rather than
300 MB of binaries nobody downloads.

`make check` is the local gate, and it includes `test-cloudblob` so that the
first thing to compile `bucket_cloud.go` is not CI. It runs `licenses-default`
rather than both variants: the cloud licence walk covers the three largest
dependency trees in the module, its verdict only changes when `go.mod` does,
and CI runs both on every push. `make licenses` runs both, deliberately, when
`go.mod` has moved.

`verify-variants` reads the binaries rather than the source, because the claim
is about what was linked. It checks two independent things: the module list the
toolchain records in every Go binary, and the function names the runtime keeps
for tracebacks — including the entry point of each provider's credential chain,
which is the specific thing Story 8.8, AC4 forbids in a default build.

Each of those two halves has its own control probe, because an absence
assertion that is not reading anything passes silently. For the symbols it is
`gocloud.dev/blob/fileblob.`, which must be *present* in both variants; for the
module list it is `gocloud.dev` itself, likewise required present. The build
information is read into a variable with its exit status checked rather than
piped into `awk` — a pipeline takes its status from `awk`, so an unreadable
binary would otherwise be indistinguishable from one that links no cloud
module, and all three module assertions would pass having read nothing. The two
probes are independent; neither covers the other's half.

It checks only the files the build was supposed to produce, derived from
`PLATFORMS` and `CLOUD_PLATFORMS`, rather than whatever is in `dist/`. A glob
would verify a stale binary from an earlier branch as part of this release, and
would let a leftover `wsaw_cloudblob_*` satisfy the "both variants present"
precondition after a plain `make build`.

The cloud build is required to contain the three driver packages, not the three
credential-chain entry points. The credential-chain names are asserted *absent*
from the default build, which is where AC4 puts them; requiring them present in
the cloud build would fail a release for an upstream refactor, or for a linker
that eliminated one, without anything wsaw depends on having broken.

The SBOMs describe the module selection of the machine that generates them,
which in CI is `linux/amd64`, while a release ships four platforms. wsaw has no
GOOS-conditional requirement in `go.mod`, so that selection is the same for all
four; if one is ever added, `make sbom` has to grow the `GOOS`/`GOARCH` loop
that `make release` already has. (`make sbom` cannot run from a linked git
worktree at all — `cyclonedx-gomod` reads the main module's version through
go-git, which cannot follow a worktree's `.git` file. It fails the same way on
`main`, and CI checks out normally.)

## What would invalidate this review

- A fourth provider driver, or a driver moving out from behind the build tag.
- A major-version bump of `gocloud.dev`, which is where its own imports change.
- The default build acquiring any module from the cloud-only tables — which is
  what `make verify-variants` exists to notice.
- `google.golang.org/api/option` disappearing from `gocloud.dev/internal/useragent`,
  which would take Google's auth stack out of the Azure driver and shrink both
  builds. That would be an improvement, and it would make the tables above
  wrong.
