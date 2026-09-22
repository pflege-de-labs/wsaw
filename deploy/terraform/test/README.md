# Local smoke test

Runs `../templates/user_data.sh.tftpl` — rendered with fixed test values,
once with the front proxy enabled and once without — against a real
`amazonlinux:2023` container. No AWS account, no cost.

```sh
./run.sh
```

What's real: `dnf install`, `useradd`/`usermod` and the subuid/subgid ranges
they need for rootless Podman, the Caddy download from GitHub's releases
API and its extraction, every file this stack writes (`wsaw.yaml`,
`wsaw.env`, the two systemd units, the Caddyfile) and their ownership/mode.

What's stubbed, in `stubs/`, because it needs a real AWS account or a
running systemd that a plain container doesn't have: `aws` (fixed fake
secret value and a placeholder binary instead of a real S3/SSM call),
`systemctl`, `loginctl`, `swapon`/`mkswap`/`fallocate`, and `podman`'s own
`pull` (the package install itself is real; only the container pull, which
needs kernel features a plain container doesn't reliably have, is stubbed).
The real AWS CLI install is skipped too — `command -v aws` finds the stub
first, deliberately, since that installer is copied verbatim from AWS's own
docs and isn't what this test is for.

`assertions.sh` checks what should exist afterwards: the wsaw user and its
subuid range, file contents/ownership/mode, and — the one thing actually
exercised end to end — that the downloaded Caddy binary runs and reports a
version.

This doesn't replace a real deploy: no systemd unit is ever started, no
rootless Podman container is ever actually pulled or run, and no
certificate is ever issued. What it's for is catching the things most
likely to be wrong before they reach a real, billed instance: a package
that doesn't exist under the name assumed, a download URL that 404s, a
heredoc or escaping mistake that writes the wrong file content, wrong
ownership or permissions.
