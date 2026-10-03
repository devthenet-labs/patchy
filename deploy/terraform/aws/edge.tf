# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Optional: an ACM certificate and, in phase 2, alias records for patchy's own
# public edge on an ALB (the GitHub webhook and the status page). Any other
# ingress and certificate source works as well; leave var.edge null then.

locals {
  edge_enabled = var.edge != null
  edge_hosts   = local.edge_enabled ? compact([var.edge.webhook_host, var.edge.status_host]) : []
  edge_domains = local.edge_enabled ? coalesce(var.edge.certificate_domains, local.edge_hosts) : []

  # A name and its wildcard (example.com, *.example.com) share one validation
  # record, so there is one record per base name.
  edge_validation_names = toset([for domain in local.edge_domains : trimprefix(domain, "*.")])

  edge_certificate_arn = !local.edge_enabled ? null : (
    var.wait_for_certificate_validation ? aws_acm_certificate_validation.edge[0].certificate_arn : aws_acm_certificate.edge[0].arn
  )
}

resource "aws_acm_certificate" "edge" {
  count = local.edge_enabled ? 1 : 0

  domain_name               = local.edge_domains[0]
  subject_alternative_names = slice(local.edge_domains, 1, length(local.edge_domains))
  validation_method         = "DNS"

  tags = merge(var.tags, { Name = local.edge_domains[0] })

  lifecycle {
    create_before_destroy = true
  }
}

# Keyed by base name, which the configuration knows at plan time even before
# ACM has issued the validation records.
resource "aws_route53_record" "edge_validation" {
  for_each = local.edge_validation_names

  zone_id         = var.edge.zone_id
  name            = one(distinct([for dvo in aws_acm_certificate.edge[0].domain_validation_options : dvo.resource_record_name if trimprefix(dvo.domain_name, "*.") == each.key]))
  type            = one(distinct([for dvo in aws_acm_certificate.edge[0].domain_validation_options : dvo.resource_record_type if trimprefix(dvo.domain_name, "*.") == each.key]))
  ttl             = 300
  records         = [one(distinct([for dvo in aws_acm_certificate.edge[0].domain_validation_options : dvo.resource_record_value if trimprefix(dvo.domain_name, "*.") == each.key]))]
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "edge" {
  count = local.edge_enabled && var.wait_for_certificate_validation ? 1 : 0

  certificate_arn         = aws_acm_certificate.edge[0].arn
  validation_record_fqdns = [for record in aws_route53_record.edge_validation : record.fqdn]
}

# Phase 2: the edge ALB exists once Helm has created the edge Ingresses.
data "aws_lb" "edge" {
  count = local.edge_enabled && var.create_alias_records ? 1 : 0

  name = var.edge.alb_name
}

resource "aws_route53_record" "edge" {
  for_each = toset(local.edge_enabled && var.create_alias_records ? local.edge_hosts : [])

  zone_id = var.edge.zone_id
  name    = each.value
  type    = "A"

  alias {
    name                   = data.aws_lb.edge[0].dns_name
    zone_id                = data.aws_lb.edge[0].zone_id
    evaluate_target_health = false
  }
}
