# The per-scan browser image (Story 1.8): headless Chromium that keeps its own
# sandbox inside the container.
#
# wsaw runs on the host and starts one of these per browser, talking CDP to it
# over a loopback-published port. It replaces chromedp/headless-shell, which
# forces --no-sandbox and so leaves the container as the only boundary around
# a renderer drawing hostile pages (AC5). Here Chromium builds its namespace
# sandbox as it does outside a container: rootless Podman's default seccomp
# profile allows that, and under Docker the container needs
#   --security-opt seccomp=deploy/chromium-seccomp.json
# which wsaw passes through browser.container.extraArgs. Where the sandbox
# cannot be built, Chromium refuses to start rather than running without it,
# so a scan fails with a reason instead of silently losing a boundary.
#
# The contract with wsaw (internal/container) is the one chromedp's image set:
# CDP on port 9222, arguments after the image name appended to Chromium's
# command line, and `--version` printing the version as its last line.
#
# Base image and Chromium version move in lockstep with the wsaw image in the
# repository's top-level Dockerfile: the same Alpine digest and the same
# CHROMIUM_VERSION, so the browser a containerised scan uses is the one the
# wsaw image bundles. ci.yaml refuses a change that lets the two drift, and
# chromium.yaml bumps both.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

ARG CHROMIUM_VERSION=152.0.7977.82-r0

# chromium-headless-shell is the old headless mode as its own binary, the one
# chromedp's image ships too, without the desktop browser around it. socat
# carries CDP from the published port to Chromium, which binds its debugging
# port to loopback. The fonts are the wsaw image's: without them layout
# differs enough that the element-visibility checks consent handling relies on
# behave differently from a real browser.
RUN apk add --no-cache \
      "chromium-headless-shell=${CHROMIUM_VERSION}" \
      socat \
      ca-certificates \
      font-noto \
      font-noto-emoji \
      ttf-freefont \
      tzdata \
    && rm -rf /var/cache/apk/*

# Unprivileged inside the container too: a renderer that escapes its sandbox
# lands as this user, and under rootless Podman that user is itself unmapped
# on the host (NFR §4).
RUN addgroup -g 10001 -S chrome \
    && adduser -u 10001 -S -G chrome -h /home/chrome chrome

COPY --chmod=0755 entrypoint.sh /usr/local/bin/wsaw-browser

# de.pflege.wsaw.browser.sandbox is how wsaw learns that this image keeps the
# sandbox, so a result's browserSandbox reports it instead of assuming every
# container disables it (AC7). It is a promise the entrypoint keeps: it never
# passes --no-sandbox.
LABEL de.pflege.wsaw.browser.sandbox="enabled" \
      de.pflege.wsaw.chromium.version="${CHROMIUM_VERSION}"

USER chrome

WORKDIR /home/chrome

EXPOSE 9222

ENTRYPOINT ["/usr/local/bin/wsaw-browser"]
