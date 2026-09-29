# Renders the real templates with fixed test values, outside of any AWS
# state, so the smoke test can run the result in a plain container. Mirrors
# the templatefile() calls in ../main.tf — if those change, update here too.

locals {
  wsaw_config_yaml = <<-YAML
    browser:
      runtime: podman
    api:
      listen: 127.0.0.1:8712
      token: "$${env:WSAW_API_TOKEN}"
  YAML

  wsaw_unit = templatefile("${path.module}/templates/wsaw.service.tftpl", {
    memory_max_mb = 300
  })

  caddyfile_content = templatefile("${path.module}/templates/Caddyfile.tftpl", {
    domain_name       = "wsaw-smoketest.example.com"
    letsencrypt_email = "smoketest@example.com"
    wsaw_port         = 8712
  })

  common = {
    aws_region           = "eu-central-1"
    wsaw_config_yaml     = local.wsaw_config_yaml
    wsaw_artifact_s3_uri = "s3://smoketest-bucket/wsaw_linux_arm64"
    browser_image        = "ghcr.io/pflege-de-labs/wsaw-browser@sha256:cdc2555a1bcd5d962343aefc6be766808c0ba592826aca7c3f6cb5f6a7eda32b"
    enable_swap          = true
    swap_size_mb         = 1024
    secret_param_names   = { WSAW_API_TOKEN = "/wsaw/WSAW_API_TOKEN" }
    wsaw_unit            = local.wsaw_unit
    caddy_unit           = file("${path.module}/templates/caddy.service")
  }
}

output "rendered_with_proxy" {
  value = templatefile("${path.module}/templates/user_data.sh.tftpl", merge(local.common, {
    enable_proxy      = true
    caddyfile_content = local.caddyfile_content
  }))
}

output "rendered_without_proxy" {
  value = templatefile("${path.module}/templates/user_data.sh.tftpl", merge(local.common, {
    enable_proxy      = false
    caddyfile_content = ""
  }))
}
