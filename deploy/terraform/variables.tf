variable "aws_region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "eu-central-1"
}

variable "name" {
  description = "Prefix for resource names and tags."
  type        = string
  default     = "wsaw"
}

variable "tags" {
  description = "Extra tags applied to every resource this stack creates."
  type        = map(string)
  default     = {}
}

variable "instance_type" {
  description = "Graviton (arm64) instance size. wsaw plus a headless Chromium scan fits t4g.micro; t4g.nano works for light, infrequent scanning with the swap file this stack adds."
  type        = string
  default     = "t4g.micro"

  validation {
    condition     = contains(["t4g.nano", "t4g.micro"], var.instance_type)
    error_message = "instance_type must be t4g.nano or t4g.micro."
  }
}

variable "vpc_id" {
  description = "VPC to deploy into. Defaults to the account's default VPC."
  type        = string
  default     = null
}

variable "subnet_id" {
  description = "Subnet to deploy into. Defaults to a default subnet in the chosen (or default) VPC."
  type        = string
  default     = null
}

variable "root_volume_size_gb" {
  description = "Root EBS volume size. Holds the OS, the wsaw binary, its sqlite index and its local evidence store (screenshots/bodies, if enabled)."
  type        = number
  default     = 20
}

variable "enable_swap" {
  description = "Add a swap file. Recommended on t4g.nano (0.5 GiB RAM) since a headless Chromium page can spike well above that; t4g.micro (1 GiB) benefits from it too under bursty scan concurrency."
  type        = bool
  default     = true
}

variable "swap_size_mb" {
  type    = number
  default = 1024
}

variable "ssh_key_name" {
  description = "EC2 key pair name for SSH. Leave null (the default) to manage the instance exclusively through SSM Session Manager, which needs no open inbound port."
  type        = string
  default     = null
}

variable "ssh_ingress_cidrs" {
  description = "CIDRs allowed to reach port 22. Only takes effect together with ssh_key_name. Leave empty to keep the instance closed to inbound traffic and use SSM instead."
  type        = list(string)
  default     = []
}

variable "web_ingress_cidrs" {
  description = "CIDRs allowed to reach wsaw's web UI/API directly. Leave empty (the default) to keep the port closed and reach it only via the webui_port_forward_command output, which needs no open ingress rule. Setting this also means changing api.listen in wsaw_config_yaml off loopback (e.g. 0.0.0.0:8712), since a security group rule alone doesn't make a loopback-bound service reachable."
  type        = list(string)
  default     = []
}

variable "web_ingress_port" {
  description = "wsaw's api.listen port, per wsaw_config_yaml. Used two ways: the web_ingress_cidrs rule opens this port directly, and (when domain_name is set) Caddy proxies to 127.0.0.1 on this port instead."
  type        = number
  default     = 8712
}

variable "domain_name" {
  description = "Public DNS name that already resolves to this instance (an A/AAAA record you manage elsewhere; this stack creates no DNS). Setting this installs Caddy as a front proxy in front of wsaw and gets it a Let's Encrypt certificate for the domain. Leave null (the default) to run without a front proxy."
  type        = string
  default     = null
}

variable "letsencrypt_email" {
  description = "Contact address Caddy registers with Let's Encrypt for expiry/problem notices. Optional; only used when domain_name is set."
  type        = string
  default     = ""
}

variable "proxy_ingress_cidrs" {
  description = "CIDRs allowed to reach Caddy on ports 80 and 443. Only takes effect together with domain_name. Let's Encrypt's HTTP-01 challenge is validated from Let's Encrypt's own servers, not from your network, so port 80 generally needs [\"0.0.0.0/0\"] (plus \"::/0\" for IPv6) to issue or renew a certificate at all; restrict 443 separately at a CDN/WAF in front of this if you need to."
  type        = list(string)
  default     = []
}

variable "wsaw_artifact_s3_uri" {
  description = "s3://bucket/key pointing at a prebuilt linux/arm64 wsaw binary (`make build` cross-compiled for arm64, or the dist/wsaw_linux_arm64 output of `make release`), uploaded ahead of time. wsaw has no published release binaries to pull instead."
  type        = string

  validation {
    condition     = can(regex("^s3://[^/]+/.+", var.wsaw_artifact_s3_uri))
    error_message = "wsaw_artifact_s3_uri must look like s3://bucket/key."
  }
}

variable "browser_image" {
  description = "Container image wsaw launches per scan, pinned by digest so capture behaviour doesn't drift under a moving tag (see wsaw.example.yaml)."
  type        = string
  default     = "docker.io/chromedp/headless-shell@sha256:2d349b544a1ea6b5b5fd7c0fe99215ff662339c57407ee2e8c0a11af93516b04"
}

variable "wsaw_config_yaml" {
  description = "Full contents of wsaw.yaml (targets, notify, detection, ...). Set browser.runtime to \"podman\" and api.listen to a loopback address; reference secrets as $${env:NAME}, which this stack resolves from SSM Parameter Store into the service's environment file."
  type        = string
}

variable "wsaw_api_token" {
  description = "Value for WSAW_API_TOKEN, stored as a SecureString SSM parameter. Leave empty to skip (only safe if api.listen stays loopback-only and nothing reads the API)."
  type        = string
  default     = ""
  sensitive   = true
}

variable "slack_webhook_url" {
  type      = string
  default   = ""
  sensitive = true
}

variable "teams_webhook_url" {
  type      = string
  default   = ""
  sensitive = true
}
