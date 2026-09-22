data "aws_vpc" "selected" {
  id      = var.vpc_id
  default = var.vpc_id == null ? true : null
}

data "aws_subnets" "selected" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.selected.id]
  }

  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

locals {
  # try() because the fallback list is empty whenever var.subnet_id is set
  # and coalesce, unlike a language with short-circuiting ternaries, still
  # evaluates it.
  subnet_id = coalesce(var.subnet_id, try(data.aws_subnets.selected.ids[0], null))

  # aws_ssm_parameter names double as env var names in the systemd
  # environment file; only parameters with a non-empty value are created.
  secrets = {
    WSAW_API_TOKEN          = var.wsaw_api_token
    WSAW_SLACK_WEBHOOK      = var.slack_webhook_url
    WSAW_TEAMS_WORKFLOW_URL = var.teams_webhook_url
  }
  enabled_secrets = { for k, v in local.secrets : k => v if v != "" }

  artifact_bucket = regex("^s3://([^/]+)/", var.wsaw_artifact_s3_uri)[0]
  artifact_key    = regex("^s3://[^/]+/(.+)$", var.wsaw_artifact_s3_uri)[0]

  # t4g.nano: 0.5 GiB total; t4g.micro: 1 GiB. wsaw.service's own cap, on top
  # of the OS and the swap file this stack adds.
  memory_max_mb = var.instance_type == "t4g.nano" ? 300 : 700

  enable_proxy = var.domain_name != null
  caddyfile_content = local.enable_proxy ? templatefile("${path.module}/templates/Caddyfile.tftpl", {
    domain_name       = var.domain_name
    letsencrypt_email = var.letsencrypt_email
    wsaw_port         = var.web_ingress_port
  }) : ""

  common_tags = merge({ Name = var.name, ManagedBy = "terraform" }, var.tags)
}

data "aws_ssm_parameter" "al2023_arm64" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
}

resource "aws_ssm_parameter" "secret" {
  for_each = local.enabled_secrets

  name  = "/${var.name}/${each.key}"
  type  = "SecureString"
  value = each.value
  tags  = local.common_tags
}

resource "aws_security_group" "instance" {
  name_prefix = "${var.name}-"
  description = "wsaw host: no inbound by default, SSM/API reached via Session Manager port-forwarding"
  vpc_id      = data.aws_vpc.selected.id

  egress {
    description = "wsaw fetches target pages and pulls the browser image"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  dynamic "ingress" {
    for_each = var.ssh_key_name != null && length(var.ssh_ingress_cidrs) > 0 ? [1] : []
    content {
      description = "SSH (only when ssh_key_name and ssh_ingress_cidrs are both set)"
      from_port   = 22
      to_port     = 22
      protocol    = "tcp"
      cidr_blocks = var.ssh_ingress_cidrs
    }
  }

  dynamic "ingress" {
    for_each = length(var.web_ingress_cidrs) > 0 ? [1] : []
    content {
      description = "wsaw web UI/API (only when web_ingress_cidrs is set)"
      from_port   = var.web_ingress_port
      to_port     = var.web_ingress_port
      protocol    = "tcp"
      cidr_blocks = var.web_ingress_cidrs
    }
  }

  dynamic "ingress" {
    for_each = length(var.proxy_ingress_cidrs) > 0 ? [80, 443] : []
    content {
      description = "Caddy front proxy, HTTP (ACME challenge + redirect) and HTTPS (only when domain_name and proxy_ingress_cidrs are both set)"
      from_port   = ingress.value
      to_port     = ingress.value
      protocol    = "tcp"
      cidr_blocks = var.proxy_ingress_cidrs
    }
  }

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true

    precondition {
      condition     = length(var.proxy_ingress_cidrs) == 0 || var.domain_name != null
      error_message = "proxy_ingress_cidrs has no effect without domain_name: Caddy, which listens on 80/443, is only installed when domain_name is set."
    }
  }
}

data "aws_iam_policy_document" "assume_ec2" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "instance" {
  name_prefix        = "${var.name}-"
  assume_role_policy = data.aws_iam_policy_document.assume_ec2.json
  tags               = local.common_tags
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "instance" {
  statement {
    sid       = "FetchWsawBinary"
    actions   = ["s3:GetObject"]
    resources = ["arn:aws:s3:::${local.artifact_bucket}/${local.artifact_key}"]
  }

  dynamic "statement" {
    for_each = length(local.enabled_secrets) > 0 ? [1] : []
    content {
      sid       = "ReadWsawSecrets"
      actions   = ["ssm:GetParameter"]
      resources = [for p in aws_ssm_parameter.secret : p.arn]
    }
  }
}

resource "aws_iam_role_policy" "instance" {
  name   = "${var.name}-instance"
  role   = aws_iam_role.instance.id
  policy = data.aws_iam_policy_document.instance.json
}

resource "aws_iam_instance_profile" "instance" {
  name_prefix = "${var.name}-"
  role        = aws_iam_role.instance.name
}

resource "aws_instance" "wsaw" {
  ami                         = data.aws_ssm_parameter.al2023_arm64.value
  instance_type               = var.instance_type
  subnet_id                   = local.subnet_id
  vpc_security_group_ids      = [aws_security_group.instance.id]
  iam_instance_profile        = aws_iam_instance_profile.instance.name
  key_name                    = var.ssh_key_name
  associate_public_ip_address = true

  # A page wsaw renders is untrusted input; forcing IMDSv2 and a hop limit of
  # 1 keeps a compromised renderer from reaching instance credentials via a
  # proxied SSRF request.
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.root_volume_size_gb
    encrypted   = true
  }

  user_data = templatefile("${path.module}/templates/user_data.sh.tftpl", {
    aws_region           = var.aws_region
    wsaw_config_yaml     = var.wsaw_config_yaml
    wsaw_artifact_s3_uri = var.wsaw_artifact_s3_uri
    browser_image        = var.browser_image
    enable_swap          = var.enable_swap
    swap_size_mb         = var.swap_size_mb
    secret_param_names   = { for k, p in aws_ssm_parameter.secret : k => p.name }
    wsaw_unit = templatefile("${path.module}/templates/wsaw.service.tftpl", {
      memory_max_mb = local.memory_max_mb
    })
    enable_proxy      = local.enable_proxy
    caddyfile_content = local.caddyfile_content
    caddy_unit        = file("${path.module}/templates/caddy.service")
  })
  user_data_replace_on_change = true

  tags = local.common_tags
}
