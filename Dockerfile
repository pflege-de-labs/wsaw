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
FROM golang:1.27.1-alpine AS build

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
RUN CGO_ENABLED=0 go build -trimpath -tags "${BUILD_TAGS}" \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/wsaw ./cmd/wsaw

FROM alpine:3.21

# Chromium and the fonts a page needs to render text at all. Without fonts,
# layout differs enough that element-visibility checks — which is how consent
# banners are found — behave differently from a real browser.
RUN apk add --no-cache \
      chromium \
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
LABEL org.opencontainers.image.variant="${BUILD_TAGS:-default}"

COPY --from=build /out/wsaw /usr/local/bin/wsaw

USER wsaw

WORKDIR /var/lib/wsaw

ENV WSAW_CHROME_PATH=/usr/bin/chromium-browser

# The Chrome sandbox stays enabled. It needs unprivileged user namespaces,
# which most runtimes allow; where they do not, run with
#   --security-opt seccomp=deploy/chromium-seccomp.json
# or, as a last resort, set browser.noSandbox in the configuration and accept
# the weaker isolation. wsaw warns loudly when that is done.
EXPOSE 8712

ENTRYPOINT ["/usr/local/bin/wsaw"]
CMD ["run", "--config", "/etc/wsaw/wsaw.yaml"]
