# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Every check here is a variable validation, so a bad input fails the plan
# before anything is created (a check block would only warn).

variable "cluster_name" {
  description = "The EKS Auto Mode cluster patchy runs on. Its ARN, VPC, primary security group and service CIDR are read from it."
  type        = string

  validation {
    condition     = can(regex("^[0-9A-Za-z][A-Za-z0-9_-]{0,99}$", var.cluster_name))
    error_message = "cluster_name must be an EKS cluster name."
  }
}

variable "name_prefix" {
  description = "Prefix of the platform IAM names: `<name_prefix>-patchy-source-controller` and `<name_prefix>-patchy-preview-node`. Null means cluster_name. IAM names are account-wide, so the prefix keeps two installs in one account apart."
  type        = string
  default     = null

  validation {
    # "-patchy-source-controller" (25 characters) is the longest suffix, and
    # IAM caps a role name at 64.
    condition     = can(regex("^[A-Za-z0-9+=,.@_-]{1,39}$", var.name_prefix != null ? var.name_prefix : var.cluster_name))
    error_message = "name_prefix (or cluster_name, when name_prefix is null) must be 1-39 IAM name characters (letters, digits and +=,.@_-), so that <prefix>-patchy-source-controller fits IAM's 64."
  }
}

variable "namespace" {
  description = "The patchy Helm release namespace, where source-controller runs."
  type        = string
  default     = "patchy"

  validation {
    condition     = length(var.namespace) <= 63 && can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.namespace))
    error_message = "namespace must be a Kubernetes namespace name."
  }
}

variable "source_controller_service_account" {
  description = "The source-controller ServiceAccount the Pod Identity association binds. helm_values sets sourceController.serviceAccount.name to it, so the chart creates exactly this name whatever the release is called."
  type        = string
  default     = "patchy-source-controller"

  validation {
    condition     = length(var.source_controller_service_account) <= 63 && can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.source_controller_service_account))
    error_message = "source_controller_service_account must be a Kubernetes ServiceAccount name (a DNS label)."
  }
}

variable "github_oidc_provider_arn" {
  description = "ARN of the account's existing IAM OIDC provider for token.actions.githubusercontent.com. Null creates one. An account holds at most one provider per issuer, so pass the ARN when it already exists."
  type        = string
  default     = null

  validation {
    condition     = var.github_oidc_provider_arn == null || can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:oidc-provider/token\\.actions\\.githubusercontent\\.com$", var.github_oidc_provider_arn))
    error_message = "github_oidc_provider_arn must be the ARN of the token.actions.githubusercontent.com OIDC provider, or null."
  }
}

variable "agent_path_prefix" {
  description = "ECR path the agent toolchain images live under, with no leading or trailing slash. source-controller may read every repository under it, and helm_values admits declared images only there. It must be disjoint from preview_path_prefix."
  type        = string
  default     = "patchy/app-envs"

  validation {
    condition     = can(regex("^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$", var.agent_path_prefix))
    error_message = "agent_path_prefix must be one or more ECR path segments separated by '/', with no leading or trailing slash (for example \"patchy/app-envs\")."
  }
  validation {
    # A runtime image is built from an unreviewed same-repository PR head. It
    # must never be admissible as an agent sandbox image, and the preview
    # nodes' pull grant must never cover a toolchain image, so neither prefix
    # may equal or contain the other. Compared on segment boundaries:
    # patchy/previews-agents is a sibling of patchy/previews, not under it.
    condition     = !startswith("${var.agent_path_prefix}/", "${var.preview_path_prefix}/") && !startswith("${var.preview_path_prefix}/", "${var.agent_path_prefix}/")
    error_message = "agent_path_prefix must not equal preview_path_prefix, contain it or sit under it: agent images and preview images must have disjoint prefixes."
  }
}

variable "preview_path_prefix" {
  description = "ECR path the preview runtime images live under, with no leading or trailing slash: each app's runtime repository is `<preview_path_prefix>/<slug>`, the preview nodes may pull only under it, and helm_values sets the chart's preview.imagePathPrefix to it. It must be disjoint from agent_path_prefix."
  type        = string
  default     = "patchy/previews"

  validation {
    condition     = can(regex("^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$", var.preview_path_prefix))
    error_message = "preview_path_prefix must be one or more ECR path segments separated by '/', with no leading or trailing slash (for example \"patchy/previews\")."
  }
}

variable "app_role_name_prefix" {
  description = "Prefix of every app's publisher role and policy names: `<app_role_name_prefix><slug>-agent-push` and `<app_role_name_prefix><slug>-runtime-push`. Null means `<name_prefix>-app-`."
  type        = string
  default     = null

  validation {
    condition     = var.app_role_name_prefix == null || can(regex("^[A-Za-z0-9+=,.@_-]*$", var.app_role_name_prefix))
    error_message = "app_role_name_prefix may only contain the characters IAM allows in a role name: letters, digits and +=,.@_-"
  }
}

variable "apps" {
  description = <<-EOT
    The application repositories patchy works on, keyed by slug: the image name, lowercase letters, digits and
    inner hyphens, independent of the GitHub name (Hello.Web -> hello-web). Each app gets an agent toolchain
    repository and its publisher role; with preview (default true) also a runtime repository and its publisher.
    - github: the repository exactly as the API reports it (IAM compares case-sensitively). owner, name,
      repository_id, owner_id and default_branch come from `gh api repos/<owner>/<name>`; sub_claim_prefix,
      which is required, from `gh api repos/<owner>/<name>/actions/oidc/customization/sub --jq .sub_claim_prefix`.
      It must be GitHub's immutable form, `repo:<owner>@<owner_id>/<name>@<repository_id>`.
  EOT
  type = map(object({
    github = object({
      owner            = string
      name             = string
      repository_id    = string
      owner_id         = string
      default_branch   = optional(string, "main")
      sub_claim_prefix = string
    })
    preview = optional(bool, true)
  }))
  default = {}

  validation {
    # The preview-controller's grammar for an image leaf (one DNS-label
    # segment), which is also a valid ECR path segment.
    condition     = alltrue([for slug in keys(var.apps) : length(slug) <= 63 && can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", slug))])
    error_message = "Each apps key must be a slug: lowercase letters, digits and hyphens, starting and ending with a letter or digit (for example \"hello-web\" for a repository named Hello.Web)."
  }
  validation {
    # IAM caps role names at 64; "-runtime-push" (13) is the longer suffix.
    # The prefix as main.tf derives it: app_role_name_prefix, else
    # "<name_prefix>-app-", else "<cluster_name>-app-".
    condition = alltrue([
      for slug in keys(var.apps) : length(join("", [
        var.app_role_name_prefix != null ? var.app_role_name_prefix : "${var.name_prefix != null ? var.name_prefix : var.cluster_name}-app-",
        slug,
        "-runtime-push",
      ])) <= 64
    ])
    error_message = "An app's publisher role name, <app_role_name_prefix><slug>-runtime-push, would exceed IAM's 64 characters: shorten the slug, app_role_name_prefix or name_prefix."
  }
  validation {
    condition     = length(distinct([for app in values(var.apps) : app.github.repository_id])) == length(var.apps)
    error_message = "Two apps name the same GitHub repository ID."
  }
  validation {
    # The publisher roles trust GitHub's immutable subject only (modules/app
    # checks the same; this names the failing app at the root).
    condition = alltrue([
      for app in values(var.apps) :
      app.github.sub_claim_prefix == "repo:${app.github.owner}@${app.github.owner_id}/${app.github.name}@${app.github.repository_id}"
    ])
    error_message = "Each app's github.sub_claim_prefix must be its immutable OIDC subject prefix, repo:<owner>@<owner_id>/<name>@<repository_id>, from: gh api repos/<owner>/<name>/actions/oidc/customization/sub --jq .sub_claim_prefix. A repository on the classic repo:<owner>/<name> subject must switch to immutable subject claims first."
  }
}

variable "previews" {
  description = <<-EOT
    Preview infrastructure; null (the default) creates none. Set, the module creates the preview node role and its
    EKS access entry, and a wildcard certificate for `*.<host_suffix>` validated in zone_id.
    - host_suffix: preview hosts are `<project>-<issue>.<host_suffix>`. A separate registrable domain is recommended.
    - zone_id: the Route53 hosted zone that holds host_suffix.
    - alb_name: the preview ALB, at most 32 characters. Null means `<cluster_name>-preview`. It must differ from
      edge.alb_name: previews get an ALB of their own.
    - alb_subnet_ids: the public subnets the preview ALB is placed in: 2 to 4, one per Availability Zone, each in the
      cluster's VPC, tagged kubernetes.io/role/elb and /20 or narrower. helm_values pins the ALB to exactly these
      subnets (preview.albSubnetIDs), and their CIDRs become preview.albSubnetCIDRs, the only sources preview pods
      admit.
    - node_subnet_ids: the private subnets preview nodes run in (the NodeClass subnetSelectorTerms), each in a zone
      of one of the alb_subnet_ids: the ALB sends no traffic to a target in a zone it has not enabled. Untrusted
      preview workloads must never get public IPs, so a subnet that assigns them fails the plan. The nodes join the
      cluster and pull from ECR out of these subnets, so they need a NAT gateway or EKS, ECR and S3 endpoints.
    - inbound_cidrs: who may reach previews: 1 to 8 IPv4 /32s. With preview_auth set it may be empty (Rev C, the
      allowlist gone and sign-in alone guarding previews), which the chart then refuses unless sign-in is already
      required (previewAuth.stage=require), preview.allowPublicWithAuth is "confirmed" and
      previewAuth.sessionTimeout is at most 900: do that only after the ALB probe has run.
    - prefix_list_ids: managed prefix lists (pl-...) the preview ALB admits beside inbound_cidrs, at most 8. Optional
      and additive: inbound_cidrs stays required without preview_auth. helm_values passes them as
      preview.prefixListsIDs.
    - dns_cidr, api_server_cidr: the cluster DNS and Kubernetes API Service /32s. Null derives .10 and .1 of the
      cluster's service CIDR, which is what EKS assigns.
  EOT
  type = object({
    host_suffix     = string
    zone_id         = string
    alb_name        = optional(string)
    alb_subnet_ids  = list(string)
    node_subnet_ids = list(string)
    inbound_cidrs   = list(string)
    prefix_list_ids = optional(list(string), [])
    dns_cidr        = optional(string)
    api_server_cidr = optional(string)
  })
  default = null

  validation {
    condition     = var.previews == null ? true : can(regex("^[a-z0-9-]+(\\.[a-z0-9-]+)+$", var.previews.host_suffix))
    error_message = "previews.host_suffix must be a lowercase DNS name without a leading dot, such as preview.acme-apps.dev."
  }
  validation {
    # The chart's preview.albName grammar; ALB names are at most 32.
    condition     = var.previews == null ? true : can(regex("^[a-z][a-z0-9-]{0,31}$", var.previews.alb_name != null ? var.previews.alb_name : "${var.cluster_name}-preview"))
    error_message = "previews.alb_name (default \"<cluster_name>-preview\") must be a lowercase ALB name of at most 32 characters starting with a letter; set it explicitly when the cluster name does not fit."
  }
  validation {
    # The preview IngressClassParams uses the name as both loadBalancerName
    # and group.name and limits the ALB to inbound_cidrs, so sharing the
    # edge's ALB would either clash with the edge Ingresses or put the
    # webhook behind the preview /32s. Compared without case, so the check
    # does not depend on how AWS folds names.
    condition = var.previews == null || var.edge == null ? true : (
      lower(var.previews.alb_name != null ? var.previews.alb_name : "${var.cluster_name}-preview") != lower(var.edge.alb_name)
    )
    error_message = "previews.alb_name (default \"<cluster_name>-preview\") must differ from edge.alb_name: previews need an ALB of their own, separate from patchy's edge ALB."
  }
  validation {
    # An ALB spans at least two Availability Zones, and the chart's
    # albSubnetCIDRs takes at most four. One subnet per zone is checked
    # against the subnets themselves, in previews.tf.
    condition = var.previews == null ? true : (
      length(var.previews.alb_subnet_ids) >= 2 && length(var.previews.alb_subnet_ids) <= 4 &&
      length(distinct(var.previews.alb_subnet_ids)) == length(var.previews.alb_subnet_ids)
    )
    error_message = "previews.alb_subnet_ids must list 2 to 4 distinct subnets: an ALB needs at least two Availability Zones, and the chart's albSubnetCIDRs takes at most four."
  }
  validation {
    condition = var.previews == null ? true : (
      length(var.previews.node_subnet_ids) >= 1 &&
      length(setintersection(var.previews.node_subnet_ids, var.previews.alb_subnet_ids)) == 0
    )
    error_message = "previews.node_subnet_ids must list at least one subnet, none of them one of the public alb_subnet_ids."
  }
  validation {
    condition = var.previews == null ? true : (
      length(var.previews.inbound_cidrs) <= 8 &&
      length(distinct(var.previews.inbound_cidrs)) == length(var.previews.inbound_cidrs) &&
      alltrue([for cidr in var.previews.inbound_cidrs : can(cidrhost(cidr, 0)) && can(regex("^[0-9.]+/32$", cidr))])
    )
    error_message = "previews.inbound_cidrs must list at most 8 distinct IPv4 /32s."
  }
  validation {
    condition = var.previews == null ? true : (
      length(var.previews.prefix_list_ids) <= 8 &&
      length(distinct(var.previews.prefix_list_ids)) == length(var.previews.prefix_list_ids) &&
      alltrue([for id in var.previews.prefix_list_ids : can(regex("^pl-([0-9a-f]{8}|[0-9a-f]{17})$", id))])
    )
    error_message = "previews.prefix_list_ids must list at most 8 distinct managed prefix list IDs (pl- and 8 or 17 hex digits)."
  }
  validation {
    condition = var.previews == null ? true : alltrue([
      for cidr in compact([var.previews.dns_cidr, var.previews.api_server_cidr]) : can(cidrhost(cidr, 0)) && can(regex("^[0-9.]+/32$", cidr))
    ])
    error_message = "previews.dns_cidr and previews.api_server_cidr must be IPv4 /32s when set."
  }
}

variable "edge" {
  description = <<-EOT
    An ACM certificate, and after Helm stage 1 alias records, for patchy's own public edge on an ALB: the GitHub
    webhook and the status page. Null (the default) creates none; the edge then works with any ingress and
    certificate source, cert-manager included.
    - zone_id: the Route53 hosted zone that holds the hosts.
    - webhook_host, status_host: the two hostnames; status_host is optional.
    - certificate_domains: the names the certificate covers, which must cover both hosts. Null means the hosts.
    - alb_name: the ALB the edge Ingresses share, looked up by name only for the alias records: the chart's
      edgeIngressClass.loadBalancerName, when the chart creates the edge class.
  EOT
  type = object({
    zone_id             = string
    webhook_host        = string
    status_host         = optional(string)
    certificate_domains = optional(list(string))
    alb_name            = string
  })
  default = null

  validation {
    condition = var.edge == null ? true : alltrue([
      for host in compact([var.edge.webhook_host, var.edge.status_host]) : can(regex("^[a-z0-9-]+(\\.[a-z0-9-]+)+$", host))
    ])
    error_message = "edge.webhook_host and edge.status_host must be lowercase DNS names."
  }
  validation {
    condition = var.edge == null ? true : alltrue([
      for domain in coalesce(var.edge.certificate_domains, ["placeholder.invalid"]) : can(regex("^(\\*\\.)?[a-z0-9-]+(\\.[a-z0-9-]+)+$", domain))
    ])
    error_message = "edge.certificate_domains must be lowercase DNS names, each optionally starting with \"*.\"."
  }
  validation {
    # A host is covered by itself, or by a wildcard one label above it.
    condition = var.edge == null ? true : alltrue([
      for host in compact([var.edge.webhook_host, var.edge.status_host]) :
      contains(coalesce(var.edge.certificate_domains, [host]), host) ||
      contains(coalesce(var.edge.certificate_domains, [host]), "*.${join(".", slice(split(".", host), 1, length(split(".", host))))}")
    ])
    error_message = "edge.certificate_domains must cover edge.webhook_host and edge.status_host, each by name or by a wildcard one label above it."
  }
  validation {
    condition     = var.edge == null ? true : can(regex("^[A-Za-z0-9][A-Za-z0-9-]{0,31}$", var.edge.alb_name)) && !endswith(var.edge.alb_name, "-")
    error_message = "edge.alb_name must be an ALB name: at most 32 letters, digits and hyphens, not starting or ending with a hyphen."
  }
}

variable "preview_auth" {
  description = <<-EOT
    The preview sign-in relay's edge (the chart's previewAuth, the OpenID provider every preview host's ALB signs
    viewers in through); null (the default) creates nothing. Set, the module adds host to the edge certificate's
    names (when edge.certificate_domains is null) and to the edge alias records, and helm_values sets
    previewAuth.host and the relay Ingress's edge annotations. It never turns sign-in on: previewAuth.enabled, its
    stage, Dex and the viewers stay explicit lines in the operator's own values file. Needs previews and edge.
    - host: the relay's hostname on the edge ALB, the issuer https://<host>. It must not sit under
      previews.host_suffix (the preview wildcard would route it to the preview ALB) and must differ from the
      webhook and status hosts. Dex's static client for the relay takes exactly one redirect URI,
      https://<host>/dex/callback (output preview_auth_dex_redirect_uri).
  EOT
  type = object({
    host = string
  })
  default = null

  validation {
    condition     = var.preview_auth == null ? true : can(regex("^[a-z0-9-]+(\\.[a-z0-9-]+)+$", var.preview_auth.host))
    error_message = "preview_auth.host must be a lowercase DNS name, such as preview-auth.patchy.acme.dev."
  }
  validation {
    condition     = var.preview_auth == null ? true : (var.previews != null && var.edge != null)
    error_message = "preview_auth needs previews (the hosts it signs viewers in for) and edge (the ALB it is reached on)."
  }
  validation {
    # The edge ALB must stay reachable from the internet: the preview ALB
    # calls the relay's token endpoint from dynamic public addresses.
    condition = var.preview_auth == null || var.previews == null ? true : (
      var.preview_auth.host != var.previews.host_suffix && !endswith(var.preview_auth.host, ".${var.previews.host_suffix}")
    )
    error_message = "preview_auth.host must not be previews.host_suffix or sit under it: the preview wildcard record would send it to the preview ALB, which admits only the preview inbound CIDRs."
  }
  validation {
    condition = var.preview_auth == null || var.edge == null ? true : (
      !contains(compact([var.edge.webhook_host, var.edge.status_host]), var.preview_auth.host)
    )
    error_message = "preview_auth.host must differ from edge.webhook_host and edge.status_host."
  }
  validation {
    # A host is covered by itself, or by a wildcard one label above it.
    condition = var.preview_auth == null || var.edge == null ? true : (
      var.edge.certificate_domains == null ? true : (
        contains(var.edge.certificate_domains, var.preview_auth.host) ||
        contains(var.edge.certificate_domains, "*.${join(".", slice(split(".", var.preview_auth.host), 1, length(split(".", var.preview_auth.host))))}")
      )
    )
    error_message = "edge.certificate_domains must cover preview_auth.host, by name or by a wildcard one label above it."
  }
}

# One flag per ALB: the edge ALB exists after Helm stage 1, the preview ALB
# only after stage 2, and each lookup fails the plan while its ALB is missing.
variable "create_edge_alias_records" {
  description = "Create the Route53 alias records for edge.webhook_host, edge.status_host and preview_auth.host, pointing at the edge ALB looked up by edge.alb_name. Set it once Helm stage 1 has created the edge Ingresses and so the ALB: the lookup fails the plan while the ALB is missing. It is independent of the preview alias, so the webhook resolves before previews are turned on."
  type        = bool
  default     = false
}

variable "create_preview_alias_record" {
  description = "Create the `*.<previews.host_suffix>` Route53 alias record, pointing at the preview ALB looked up by its name. Set it once Helm stage 2 (previews on, with the placeholder Ingress) has created the preview ALB: the lookup fails the plan while the ALB is missing."
  type        = bool
  default     = false
}

variable "wait_for_certificate_validation" {
  description = "Hold apply until ACM has issued the certificates, so helm_values never carries a certificate the ALB cannot attach yet. Set false while the zones are not yet delegated; ACM then issues the certificates on its own once they are."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Tags added to every taggable resource the module creates."
  type        = map(string)
  default     = {}
}
