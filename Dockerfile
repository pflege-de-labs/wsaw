# wsaw container image.
#
# Chromium is bundled here so the image has no host dependency (Story 6.3).
# Chromium rather than Chrome, because Chromium is the one that may be
# redistributed (NFR §9).

# At least the module's go directive, and an exact patch. The official images
# set GOTOOLCHAIN=local, so a builder older than that directive does not
# download a newer toolchain — it refuses to build at all, which is how the
# 1.25 pin here broke once go.mod moved to 1.26.
#
# The patch is exact and matches GO_VERSION in .github/workflows/ci.yaml, so
# the image and the released binaries are built by the same compiler. Bump the
# two together.
#
# The build stage runs on the builder's own platform and cross-compiles for the
# target, rather than running the Go toolchain under emulation for every
# architecture the image is built for. The binary is pure Go and CGo-free
# precisely so that cross-compiling it is exact (Tenet 14), and `make release`
# builds the released binaries the same way; emulating the compiler for arm64
# would make a multi-arch build several times slower and buy nothing.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build

# Set by BuildKit from --platform. Empty in a plain `docker build`, where the
# target is the builder's own platform and go build's defaults are right.
ARG TARGETOS
ARG TARGETARCH

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

# Empty by default, and "cloudblob" for an image that can store evidence in
# S3, GCS or Azure Blob Storage. It is empty by default for the reason the tag
# exists at all: those three SDKs add about 28 MB to the binary — the measured
# figure per platform is in dist/SIZES — and an installation storing its
# evidence on a volume should not carry them (Story 8.8, AC3). Build the other
# one with
#   make docker BUILD_TAGS=cloudblob
ARG BUILD_TAGS=""

WORKDIR /src

# Dependencies first, so the module cache survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO stays off: the single static binary is the whole deployment story.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -tags "${BUILD_TAGS}" \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/wsaw ./cmd/wsaw

# By digest, and Chromium by exact package version (Story 6.3, AC1). The
# browser is what renders every result, so an image whose Chromium moved
# between two builds of the same release would produce results that are not
# comparable, and nothing in the image would say so. A tag pins neither: the
# base tag is re-pushed with every Alpine point release, and apk installs
# whatever the branch holds on the day of the build.
#
# The cost is deliberate. Alpine keeps only the newest build of a package in a
# stable branch, and Chromium's security releases replace it every week or
# two, so this build — and with it ci.yaml's container-sandbox job — stops
# resolving CHROMIUM_VERSION the day the branch moves on, failing with apk's
# "unable to select packages". That is the signal to bump it: to the version
# `apk policy chromium` reports in a fresh alpine:3.24 container, in the same
# change as whatever else that release needs. Dependabot moves both digests.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

ARG CHROMIUM_VERSION=152.0.7977.82-r0

# Chromium and the fonts a page needs to render text at all. Without fonts,
# layout differs enough that element-visibility checks — which is how consent
# banners are found — behave differently from a real browser.
RUN apk add --no-cache \
      "chromium=${CHROMIUM_VERSION}" \
      ca-certificates \
      font-noto \
      font-noto-emoji \
      ttf-freefont \
      tzdata \
    && rm -rf /var/cache/apk/*

# A dedicated unprivileged user: wsaw never needs root (NFR §4).
RUN addgroup -g 10001 -S wsaw \
    && adduser -u 10001 -S -G wsaw -h /var/lib/wsaw wsaw \
    && mkdir -p /var/lib/wsaw /etc/wsaw \
    && chown -R wsaw:wsaw /var/lib/wsaw

# Which variant this image is, recorded in the image rather than only in the
# tag someone chose to type. `make docker BUILD_TAGS=cloudblob` puts it in the
# image name, but an image built by hand, or pulled by someone else, has to be
# able to answer the question itself — the two differ by that same 28 MB of
# cloud SDK, and by which URL schemes the binary can reach (Story 8.8, AC3).
ARG BUILD_TAGS=""
LABEL org.opencontainers.image.variant="${BUILD_TAGS:-default}" \
      de.pflege.wsaw.chromium.version="${CHROMIUM_VERSION}"

COPY --from=build /out/wsaw /usr/local/bin/wsaw

USER wsaw

WORKDIR /var/lib/wsaw

ENV WSAW_CHROME_PATH=/usr/bin/chromium-browser

# The Chrome sandbox stays enabled (Story 6.3, AC3). It builds each renderer a
# user namespace of its own, which Podman's default seccomp profile allows and
# Docker's does not, so under Docker run with
#   --security-opt seccomp=deploy/chromium-seccomp.json
# — Docker's default with the four syscalls the sandbox needs, derived by
# `make seccomp-profile`. As a last resort, set browser.noSandbox in the
# configuration and accept the weaker isolation; wsaw warns loudly when that
# is done.
EXPOSE 8712

ENTRYPOINT ["/usr/local/bin/wsaw"]
CMD ["run", "--config", "/etc/wsaw/wsaw.yaml"]
