# wsaw on EC2 (Graviton, Podman)

Runs wsaw natively on one `t4g.nano` or `t4g.micro` instance via the systemd
unit in `templates/wsaw.service.tftpl` (the same shape as `deploy/wsaw.service`,
adjusted for a rootless-Podman host). wsaw itself is not containerized: per
`browser.runtime: podman` in its config, wsaw launches the browser
(`docker.io/chromedp/headless-shell`, pinned by digest) in a rootless Podman
container per scan and removes it on exit — this is wsaw's own documented
container story (see the repo's top-level README, "Where the browser runs"),
not something this stack builds on top of it.

## What this creates

- One EC2 instance (Amazon Linux 2023, arm64), IMDSv2-only, encrypted gp3
  root volume, no inbound security group rules by default. Two opt-in
  ingress rules exist for when SSM alone isn't enough: SSH
  (`ssh_key_name` + `ssh_ingress_cidrs`) and direct access to wsaw's web
  UI/API (`web_ingress_cidrs` + `web_ingress_port`, default 8712) — each
  restricted to the CIDRs you list, and absent entirely otherwise.
- An IAM role/instance profile with exactly two things: SSM Session Manager
  (`AmazonSSMManagedInstanceCore`), and `s3:GetObject` on the one wsaw binary
  object it downloads.
- Optionally, SecureString SSM parameters for `WSAW_API_TOKEN`,
  `WSAW_SLACK_WEBHOOK` and `WSAW_TEAMS_WORKFLOW_URL` — created only for the
  ones you give a non-empty value, and readable by the instance role only.
- Optionally (when `domain_name` is set), Caddy as a front proxy: it
  terminates TLS for that domain with a Let's Encrypt certificate it
  obtains and renews itself, and reverse-proxies to wsaw on loopback. wsaw's
  own `api.listen` stays on `127.0.0.1` either way — Caddy is what's
  reachable from outside, never wsaw directly.

## What it does NOT create

- No VPC — it uses the account's default VPC/subnet unless `vpc_id`/
  `subnet_id` are set.
- No container registry, no CI/CD — there's nothing to push, since wsaw
  itself runs as a plain binary.
- No S3 evidence bucket. The default binary (`make build`) keeps evidence on
  the root volume (`store.artifactDir`); reaching S3 instead needs the
  `cloudblob` build (`make build-cloudblob`) and is out of scope here — add
  a bucket and `s3:PutObject`/`s3:GetObject`/`s3:DeleteObject`/`s3:ListBucket`
  on the instance role if you want it.

## Before you `apply`

wsaw has no published release binaries. Build one for arm64 and upload it:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o dist/wsaw_linux_arm64 ./cmd/wsaw
aws s3 cp dist/wsaw_linux_arm64 s3://<your-bucket>/wsaw_linux_arm64
```

Point `wsaw_artifact_s3_uri` at that object. Every subsequent binary update
means uploading a new object and either bumping the S3 key/version or
tainting/replacing the instance — user data only runs once, at first boot.

Write your `wsaw.yaml` (start from the repo's `wsaw.example.yaml`) and pass
it whole as `wsaw_config_yaml`. At minimum:

```yaml
browser:
  runtime: podman
  container:
    image: "" # empty uses wsaw's own built-in default, the same digest this
              # stack's browser_image pre-pulls — override both together, or
              # neither
api:
  # Keep this on loopback. Either reach it through the
  # webui_port_forward_command output, or put Caddy in front of it by
  # setting domain_name (below) — both work with wsaw staying on loopback.
  # The one case for taking it off loopback is web_ingress_cidrs, for direct
  # unencrypted access without a domain; a security group rule alone does
  # nothing for a service that isn't listening beyond loopback.
  listen: 127.0.0.1:8712
  token: "${env:WSAW_API_TOKEN}"
notify:
  - name: slack
    url: "${env:WSAW_SLACK_WEBHOOK}"
    minSeverity: high
```

`${env:NAME}` is wsaw's own secret-reference syntax (see `wsaw.example.yaml`);
this stack resolves the three names above from SSM into
`/etc/wsaw/wsaw.env`, which the systemd unit loads.

## Front proxy and TLS

Set `domain_name` to a hostname that already resolves (as an A/AAAA record
you manage elsewhere — this stack creates no DNS) to the instance's public
IP, and Caddy is installed as a front proxy: it gets a Let's Encrypt
certificate for that name via the HTTP-01 challenge and reverse-proxies to
wsaw on `127.0.0.1:${web_ingress_port}`.

```hcl
domain_name         = "wsaw.example.com"
letsencrypt_email   = "ops@example.com"   # optional; renewal/problem notices
proxy_ingress_cidrs = ["0.0.0.0/0", "::/0"]
```

`proxy_ingress_cidrs` opens ports 80 and 443 to exactly those CIDRs — closed,
like every other ingress rule here, until you set it. The one thing to
understand before restricting it: Let's Encrypt validates the HTTP-01
challenge from its own servers, not from your network, so **port 80
generally needs to stay open to the internet** (`0.0.0.0/0` and, for IPv6,
`::/0`) or certificate issuance and renewal both fail. If you need 443
restricted to specific networks, put that restriction in front of this
stack (a CDN or WAF) rather than in `proxy_ingress_cidrs`, since narrowing
it here would also narrow port 80.

The public URL is `terraform output proxy_url`. A public IP that changes
(instance replacement, EIP not used) breaks the DNS record until you update
it — this stack doesn't manage one for you.

## Example

```hcl
module "wsaw" {
  source = "./deploy/terraform"

  aws_region           = "eu-central-1"
  instance_type        = "t4g.micro"
  wsaw_artifact_s3_uri  = "s3://my-ops-bucket/wsaw_linux_arm64"
  wsaw_config_yaml      = file("${path.root}/wsaw.yaml")
  wsaw_api_token        = var.wsaw_api_token
  slack_webhook_url     = var.slack_webhook_url
}
```

```sh
terraform init
terraform apply
terraform output ssm_session_command        # shell, no open port
terraform output webui_port_forward_command # http://localhost:8712
```

## Sizing notes

- `t4g.micro` (1 GiB RAM) is the safer default. `t4g.nano` (0.5 GiB) works
  for light, infrequent scanning; this stack adds a swap file
  (`enable_swap`, `swap_size_mb`) either way, since a single busy Chromium
  page can spike well past either instance's RAM.
- `wsaw.service`'s `MemoryMax` is capped below total RAM (300M on nano, 700M
  on micro) to leave room for the OS, the SSM agent, the swap file, and
  Caddy (capped separately at 128M) when `domain_name` is set; the browser
  container's own memory limit is `browser.container.memory` in
  `wsaw.yaml`, unrelated to either cap. Caddy's footprint is small but not
  free — on `t4g.nano` the swap file is doing real work once it's added.
- Nothing here scales past one instance — wsaw's own store is a single
  SQLite file with a lock, so two instances sharing state was never the
  design point (see `store.driver` in `wsaw.example.yaml`).

## Known rough edges

- `usermod --add-subuids`/`--add-subgids` assumes a shadow-utils recent
  enough to support those flags (true on current AL2023); older images may
  need `usermod -v`/`-w` instead.
- Podman ships via AL2023's SPAL repository, which is *not* enabled by
  default — user data installs the `spal-release` package first, in its own
  `dnf` transaction, before installing `podman` (verified against a real
  `amazonlinux:2023` container in `test/`).
- Caddy has no AL2023 package (SPAL doesn't carry it, and its own COPR repo
  targets Fedora releases AL2023 doesn't identify as), so user data pulls
  the official static binary from GitHub's releases API at boot. That's an
  unauthenticated API call subject to GitHub's low per-IP rate limit — fine
  for one instance booting once, but replacing many instances at once from
  behind one NAT IP could hit it.
- Caddy obtains its certificate on first boot, which needs `domain_name`
  already resolving to the instance's public IP *before* `terraform apply`
  — otherwise the HTTP-01 challenge fails and Caddy retries with backoff
  until DNS catches up, rather than serving HTTPS immediately.
- Rotating a secret means updating the Terraform variable, re-applying (which
  updates the SSM parameter), and rebooting or re-running the relevant part
  of user data on the instance — nothing here watches SSM for changes.
