#!/bin/bash
# Smoke-tests user_data.sh.tftpl against a real amazonlinux:2023 container:
# real dnf installs, real user/subuid setup, real Caddy download from
# GitHub — everything user_data.sh does except what needs a real AWS
# account or a running systemd, which are stubbed (see stubs/ and README.md).
#
# Needs: Docker, and internet egress for dnf and github.com/api.github.com.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TF_IMAGE=hashicorp/terraform:1.9
TEST_IMAGE=amazonlinux:2023

workdir=$(mktemp -d)
cleanup() { rm -rf "$workdir"; }
trap cleanup EXIT

echo "==> rendering templates"
mkdir -p "$workdir/templates"
cp ../templates/*.tftpl "$workdir/templates/"
cp ../templates/caddy.service "$workdir/templates/"
cp render.tf "$workdir/"

docker run --rm -v "$workdir":/w -w /w "$TF_IMAGE" init -backend=false >/dev/null
docker run --rm -v "$workdir":/w -w /w "$TF_IMAGE" apply -auto-approve >/dev/null
docker run --rm -v "$workdir":/w -w /w "$TF_IMAGE" output -raw rendered_with_proxy >"$workdir/user_data_proxy.sh"
docker run --rm -v "$workdir":/w -w /w "$TF_IMAGE" output -raw rendered_without_proxy >"$workdir/user_data_noproxy.sh"

echo "==> bash syntax check"
bash -n "$workdir/user_data_proxy.sh"
bash -n "$workdir/user_data_noproxy.sh"

run_scenario() {
  local script="$1" label="$2" mode="$3"
  echo
  echo "==> scenario: $label"
  docker run --rm \
    -v "$script":/user-data.sh:ro \
    -v "$PWD/stubs":/stubs:ro \
    -v "$PWD/assertions.sh":/assertions.sh:ro \
    "$TEST_IMAGE" \
    bash -c 'set -euo pipefail
      export PATH=/stubs:$PATH
      # The plain container has no systemd package at all (unlike the real
      # AMI, where it is PID 1 and this directory always exists) — create
      # just enough of its layout for user_data.sh to write units into.
      mkdir -p /etc/systemd/system
      bash /user-data.sh
      echo "--- assertions ($0) ---"
      bash /assertions.sh "$0"' "$mode"
}

run_scenario "$workdir/user_data_proxy.sh" "with front proxy" proxy
run_scenario "$workdir/user_data_noproxy.sh" "without front proxy" noproxy

echo
echo "==> all scenarios passed"
