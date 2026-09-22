output "instance_id" {
  value = aws_instance.wsaw.id
}

output "private_ip" {
  value = aws_instance.wsaw.private_ip
}

output "public_ip" {
  value = aws_instance.wsaw.public_ip
}

output "ssm_session_command" {
  description = "Shell access with no open inbound port."
  value       = "aws ssm start-session --region ${var.aws_region} --target ${aws_instance.wsaw.id}"
}

output "webui_port_forward_command" {
  description = "Reach wsaw's web UI/API (default 127.0.0.1:8712 on the instance) from your machine at http://localhost:8712."
  value       = "aws ssm start-session --region ${var.aws_region} --target ${aws_instance.wsaw.id} --document-name AWS-StartPortForwardingSession --parameters '{\"portNumber\":[\"8712\"],\"localPortNumber\":[\"8712\"]}'"
}

output "proxy_url" {
  description = "wsaw's web UI/API over HTTPS, once Caddy has obtained its certificate (null unless domain_name is set)."
  value       = var.domain_name != null ? "https://${var.domain_name}" : null
}
