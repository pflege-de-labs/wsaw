# wsaw container image.
#
# Chromium is bundled here so the image has no host dependency (Story 6.3).
# Chromium rather than Chrome, because Chromium is the one that may be
# redistributed (NFR §9).

FROM golang:1.27-alpine AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

WORKDIR /src

# Dependencies first, so the module cache survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO stays off: the single static binary is the whole deployment story.
RUN CGO_ENABLED=0 go build -trimpath \
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
