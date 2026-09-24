#!/bin/sh
# Entrypoint of the per-scan browser image (deploy/browser/Dockerfile).
#
# Chromium binds its debugging port to loopback whatever
# --remote-debugging-address says, so it listens on 127.0.0.1:9223 and socat
# carries the published port 9222 to it. Arguments are appended to Chromium's
# command line. There is deliberately no --no-sandbox here (Story 1.8, AC5):
# an operator who needs one passes it through browser.container.browserArgs,
# and wsaw then records the scan as unsandboxed.
set -eu

chrome=/usr/bin/chromium-headless-shell

# wsaw reads the last line of `<image> --version`, so nothing else is printed.
if [ "${1:-}" = "--version" ]; then
	exec "$chrome" --version
fi

socat TCP4-LISTEN:9222,fork,reuseaddr TCP4:127.0.0.1:9223 &

exec "$chrome" \
	--use-gl=angle \
	--use-angle=swiftshader \
	--remote-debugging-address=127.0.0.1 \
	--remote-debugging-port=9223 \
	"$@"
