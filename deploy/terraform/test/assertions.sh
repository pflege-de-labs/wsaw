#!/bin/bash
# Checks what user_data.sh should have left behind. Runs after it, inside
# the same container. $1 is "proxy" or "noproxy".
set -uo pipefail

fail=0
check() {
  if eval "$2"; then
    echo "PASS: $1"
  else
    echo "FAIL: $1"
    fail=1
  fi
}

check "wsaw user exists with uid 10001" '[ "$(id -u wsaw)" = "10001" ]'
check "wsaw has a subuid range" 'grep -q "^wsaw:" /etc/subuid'
check "wsaw has a subgid range" 'grep -q "^wsaw:" /etc/subgid'
check "podman package installed" '[ -x /usr/bin/podman ] || [ -x /usr/libexec/podman ]'

check "wsaw.yaml written" 'grep -q "runtime: podman" /etc/wsaw/wsaw.yaml'
check "wsaw.yaml owned root:wsaw, mode 640" '[ "$(stat -c "%a %U:%G" /etc/wsaw/wsaw.yaml)" = "640 root:wsaw" ]'

check "wsaw.env has the resolved secret" 'grep -qx "WSAW_API_TOKEN=TEST-SECRET-VALUE" /etc/wsaw/wsaw.env'
check "wsaw.env mode 600" '[ "$(stat -c "%a" /etc/wsaw/wsaw.env)" = "600" ]'

check "wsaw binary installed and executable" '[ -x /usr/local/bin/wsaw ]'
check "wsaw.service written with a MemoryMax" 'grep -q "^MemoryMax=" /etc/systemd/system/wsaw.service'

if [ "$1" = "proxy" ]; then
  check "caddy user exists" 'id caddy >/dev/null 2>&1'
  check "caddy binary installed and runs" '/usr/local/bin/caddy version >/dev/null 2>&1'
  check "Caddyfile has the test domain" 'grep -q "wsaw-smoketest.example.com" /etc/caddy/Caddyfile'
  check "Caddyfile proxies to the right port" 'grep -q "reverse_proxy 127.0.0.1:8712" /etc/caddy/Caddyfile'
  check "caddy.service written" '[ -f /etc/systemd/system/caddy.service ]'
else
  check "caddy user not created" '! id caddy >/dev/null 2>&1'
  check "no Caddyfile" '[ ! -f /etc/caddy/Caddyfile ]'
  check "no caddy.service" '[ ! -f /etc/systemd/system/caddy.service ]'
fi

exit "$fail"
