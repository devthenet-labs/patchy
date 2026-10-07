# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Offline, deterministic: the AWS provider is mocked, so these run with no
# credentials and no network beyond the provider download for its schema.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "111122223333"
    }
  }
  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }
  mock_data "aws_region" {
    defaults = {
      region = "eu-west-2"
    }
  }
  mock_data "aws_eks_cluster" {
    defaults = {
      arn = "arn:aws:eks:eu-west-2:111122223333:cluster/acme-prod"
      vpc_config = [{
        vpc_id                    = "vpc-0acme"
        cluster_security_group_id = "sg-0cluster"
        endpoint_private_access   = true
        endpoint_public_access    = true
        public_access_cidrs       = []
        security_group_ids        = []
        subnet_ids                = []
      }]
      kubernetes_network_config = [{
        ip_family              = "ipv4"
        service_ipv4_cidr      = "172.20.0.0/16"
        service_ipv6_cidr      = ""
        elastic_load_balancing = []
      }]
    }
  }
  mock_data "aws_subnet" {
    defaults = {
      vpc_id                  = "vpc-0acme"
      cidr_block              = "10.40.0.0/19"
      map_public_ip_on_launch = false
      tags                    = {}
    }
  }
  mock_data "aws_lb" {
    defaults = {
      dns_name = "acme-alb-123456789.eu-west-2.elb.amazonaws.com"
      zone_id  = "ZHURV8PSTC4K8"
    }
  }
  mock_resource "aws_iam_openid_connect_provider" {
    defaults = {
      arn = "arn:aws:iam::111122223333:oidc-provider/token.actions.githubusercontent.com"
    }
  }
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::111122223333:role/mocked"
    }
  }
  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::111122223333:policy/mocked"
    }
  }
  mock_resource "aws_ecr_repository" {
    defaults = {
      arn            = "arn:aws:ecr:eu-west-2:111122223333:repository/mocked"
      repository_url = "111122223333.dkr.ecr.eu-west-2.amazonaws.com/mocked"
    }
  }
}

# The two public ALB subnets, distinct, in two zones and tagged for
# internet-facing ALBs.
override_data {
  target = data.aws_subnet.preview_alb["subnet-alb-a"]
  values = {
    vpc_id            = "vpc-0acme"
    availability_zone = "eu-west-2a"
    cidr_block        = "10.40.128.0/24"
    tags              = { "kubernetes.io/role/elb" = "1" }
  }
}

override_data {
  target = data.aws_subnet.preview_alb["subnet-alb-b"]
  values = {
    vpc_id            = "vpc-0acme"
    availability_zone = "eu-west-2b"
    cidr_block        = "10.40.129.0/24"
    tags              = { "kubernetes.io/role/elb" = "1" }
  }
}

# The two private node subnets, private and in the ALB subnets' zones.
override_data {
  target = data.aws_subnet.preview_node["subnet-node-a"]
  values = {
    vpc_id                  = "vpc-0acme"
    availability_zone       = "eu-west-2a"
    cidr_block              = "10.40.0.0/19"
    map_public_ip_on_launch = false
  }
}

override_data {
  target = data.aws_subnet.preview_node["subnet-node-b"]
  values = {
    vpc_id                  = "vpc-0acme"
    availability_zone       = "eu-west-2b"
    cidr_block              = "10.40.32.0/19"
    map_public_ip_on_launch = false
  }
}

override_resource {
  target = aws_acm_certificate.preview[0]
  values = {
    arn = "arn:aws:acm:eu-west-2:111122223333:certificate/11111111-1111-1111-1111-111111111111"
    domain_validation_options = [{
      domain_name           = "*.preview.acme-apps.dev"
      resource_record_name  = "_a1.preview.acme-apps.dev."
      resource_record_type  = "CNAME"
      resource_record_value = "_b1.acm-validations.aws."
    }]
  }
}

override_resource {
  target = aws_acm_certificate.edge[0]
  values = {
    arn = "arn:aws:acm:eu-west-2:111122223333:certificate/22222222-2222-2222-2222-222222222222"
    domain_validation_options = [
      {
        domain_name           = "patchy.acme.dev"
        resource_record_name  = "_a2.patchy.acme.dev."
        resource_record_type  = "CNAME"
        resource_record_value = "_b2.acm-validations.aws."
      },
      {
        domain_name           = "status.patchy.acme.dev"
        resource_record_name  = "_a3.status.patchy.acme.dev."
        resource_record_type  = "CNAME"
        resource_record_value = "_b3.acm-validations.aws."
      },
    ]
  }
}

variables {
  cluster_name = "acme-prod"
}

run "platform_only" {
  command = apply

  assert {
    condition     = length(aws_iam_openid_connect_provider.github) == 1 && output.github_oidc_provider_arn == "arn:aws:iam::111122223333:oidc-provider/token.actions.githubusercontent.com"
    error_message = "Without github_oidc_provider_arn the module creates the GitHub OIDC provider and passes its ARN on."
  }

  assert {
    condition = jsondecode(aws_iam_role.source_controller.assume_role_policy).Statement == [{
      Sid       = "AllowEksAuthToAssumeRoleForPodIdentity"
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = {
        StringEquals = {
          "aws:RequestTag/eks-cluster-arn"            = "arn:aws:eks:eu-west-2:111122223333:cluster/acme-prod"
          "aws:RequestTag/kubernetes-namespace"       = "patchy"
          "aws:RequestTag/kubernetes-service-account" = "patchy-source-controller"
        }
      }
    }]
    error_message = "source-controller's role trusts only Pod Identity sessions tagged with this cluster, namespace and ServiceAccount."
  }

  assert {
    condition = (
      aws_iam_role.source_controller.name == "acme-prod-patchy-source-controller" &&
      aws_iam_role.source_controller.path == null &&
      aws_eks_pod_identity_association.source_controller.namespace == "patchy" &&
      aws_eks_pod_identity_association.source_controller.service_account == "patchy-source-controller"
    )
    error_message = "The source-controller role is <name_prefix>-patchy-source-controller with no IAM path (the provider's default /), associated with the configured ServiceAccount."
  }

  assert {
    condition = (
      jsondecode(aws_iam_policy.source_controller.policy).Statement[1].Resource == "arn:aws:ecr:eu-west-2:111122223333:repository/patchy/app-envs/*" &&
      toset(jsondecode(aws_iam_policy.source_controller.policy).Statement[1].Action) == toset([
        "ecr:BatchGetImage",
        "ecr:GetDownloadUrlForLayer",
        "ecr:DescribeImages",
        "ecr:BatchCheckLayerAvailability",
        "ecr:ListImages",
      ])
    )
    error_message = "source-controller may only read repositories under the agent prefix."
  }

  assert {
    condition = (
      length(aws_iam_role.preview_node) == 0 && length(aws_acm_certificate.preview) == 0 &&
      length(aws_acm_certificate.edge) == 0 && length(data.aws_lb.preview) == 0 && length(data.aws_lb.edge) == 0
    )
    error_message = "Without previews or edge, no preview or edge resource and no ALB lookup exists."
  }

  assert {
    condition = yamldecode(output.helm_values) == {
      sourceController = { serviceAccount = { name = "patchy-source-controller" } }
      agent            = { repositoryImages = { registries = ["111122223333.dkr.ecr.eu-west-2.amazonaws.com/patchy/app-envs/"] } }
    }
    error_message = "Without previews or edge, helm_values names only the source-controller ServiceAccount and the agent registry prefix."
  }

  assert {
    condition     = output.preview_node_class == null && output.github_variables == {} && output.apps == {}
    error_message = "With no previews and no apps the per-app and preview outputs are empty."
  }
}

run "existing_oidc_provider_is_reused" {
  command = apply

  variables {
    github_oidc_provider_arn = "arn:aws:iam::111122223333:oidc-provider/token.actions.githubusercontent.com"
  }

  assert {
    condition     = length(aws_iam_openid_connect_provider.github) == 0 && output.github_oidc_provider_arn == var.github_oidc_provider_arn
    error_message = "Given an existing provider ARN, the module creates none and uses that ARN."
  }
}

run "full_install" {
  command = apply

  variables {
    apps = {
      hello-web = {
        github = {
          owner            = "Acme-Org"
          name             = "Hello.Web"
          repository_id    = 123456789
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
        }
      }
      tools = {
        github = {
          owner            = "Acme-Org"
          name             = "tools"
          repository_id    = 555
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org@987654/tools@555"
        }
        preview = false
      }
    }
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a", "subnet-node-b"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id      = "Z0EDGE"
      webhook_host = "patchy.acme.dev"
      status_host  = "status.patchy.acme.dev"
      alb_name     = "acme-prod"
    }
  }

  assert {
    condition = yamldecode(output.helm_values) == {
      sourceController = { serviceAccount = { name = "patchy-source-controller" } }
      agent            = { repositoryImages = { registries = ["111122223333.dkr.ecr.eu-west-2.amazonaws.com/patchy/app-envs/"] } }
      preview = {
        imageRegistry   = "111122223333.dkr.ecr.eu-west-2.amazonaws.com"
        imagePathPrefix = "patchy/previews"
        dnsCIDR         = "172.20.0.10/32"
        albSubnetCIDRs  = ["10.40.128.0/24", "10.40.129.0/24"]
        albSubnetIDs    = ["subnet-alb-a", "subnet-alb-b"]
        inboundCIDRs    = ["203.0.113.7/32"]
        certificateARN  = "arn:aws:acm:eu-west-2:111122223333:certificate/11111111-1111-1111-1111-111111111111"
        hostSuffix      = "preview.acme-apps.dev"
        albName         = "acme-prod-preview"
      }
      previewController = { config = { apiServerCIDR = "172.20.0.1/32" } }
      webhook = {
        host = "patchy.acme.dev"
        ingress = { annotations = {
          "alb.ingress.kubernetes.io/certificate-arn" = "arn:aws:acm:eu-west-2:111122223333:certificate/22222222-2222-2222-2222-222222222222"
          "alb.ingress.kubernetes.io/listen-ports"    = "[{\"HTTP\": 80}, {\"HTTPS\": 443}]"
          "alb.ingress.kubernetes.io/ssl-redirect"    = "443"
        } }
      }
      statusServer = {
        host = "status.patchy.acme.dev"
        ingress = { annotations = {
          "alb.ingress.kubernetes.io/certificate-arn" = "arn:aws:acm:eu-west-2:111122223333:certificate/22222222-2222-2222-2222-222222222222"
          "alb.ingress.kubernetes.io/listen-ports"    = "[{\"HTTP\": 80}, {\"HTTPS\": 443}]"
          "alb.ingress.kubernetes.io/ssl-redirect"    = "443"
        } }
      }
    }
    error_message = "helm_values carries exactly the infrastructure-derived chart keys, with the DNS and API server /32s derived from the service CIDR and the preview ALB pinned to the subnets whose CIDRs the slots admit."
  }

  assert {
    condition = (
      aws_iam_role.preview_node[0].name == "acme-prod-patchy-preview-node" &&
      jsondecode(aws_iam_role.preview_node[0].assume_role_policy).Statement[0].Principal == { Service = "ec2.amazonaws.com" } &&
      aws_eks_access_entry.preview_node[0].type == "EC2" &&
      aws_eks_access_policy_association.preview_node[0].policy_arn == "arn:aws:eks::aws:cluster-access-policy/AmazonEKSAutoNodePolicy" &&
      aws_eks_access_policy_association.preview_node[0].access_scope[0].type == "cluster"
    )
    error_message = "The preview node role is an EC2 Auto Mode node identity with an EC2 access entry and the Auto node policy."
  }

  assert {
    condition = jsondecode(aws_iam_policy.preview_node[0].policy).Statement == [
      {
        Sid      = "ClusterPodIdentity"
        Effect   = "Allow"
        Action   = "eks-auth:AssumeRoleForPodIdentity"
        Resource = "arn:aws:eks:eu-west-2:111122223333:cluster/acme-prod"
      },
      {
        Sid      = "RegistryAuthentication"
        Effect   = "Allow"
        Action   = "ecr:GetAuthorizationToken"
        Resource = "*"
      },
      {
        Sid      = "PullPreviewRuntimeOnly"
        Effect   = "Allow"
        Action   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
        Resource = "arn:aws:ecr:eu-west-2:111122223333:repository/patchy/previews/*"
      },
    ]
    error_message = "Preview nodes may join this cluster's Pod Identity and pull patchy/previews/* only."
  }

  assert {
    condition = (
      aws_acm_certificate.preview[0].domain_name == "*.preview.acme-apps.dev" &&
      toset(keys(aws_route53_record.preview_validation)) == toset(["*.preview.acme-apps.dev"]) &&
      aws_route53_record.preview_validation["*.preview.acme-apps.dev"].name == "_a1.preview.acme-apps.dev." &&
      aws_route53_record.preview_validation["*.preview.acme-apps.dev"].zone_id == "Z0PREVIEW" &&
      length(aws_acm_certificate_validation.preview) == 1
    )
    error_message = "The preview wildcard certificate is validated by its own record in the preview zone."
  }

  assert {
    condition = (
      aws_acm_certificate.edge[0].domain_name == "patchy.acme.dev" &&
      aws_acm_certificate.edge[0].subject_alternative_names == toset(["status.patchy.acme.dev"]) &&
      sort(keys(aws_route53_record.edge_validation)) == tolist(["patchy.acme.dev", "status.patchy.acme.dev"]) &&
      aws_route53_record.edge_validation["status.patchy.acme.dev"].records == toset(["_b3.acm-validations.aws."])
    )
    error_message = "The edge certificate covers both hosts, each validated by its own record."
  }

  assert {
    condition     = length(data.aws_lb.preview) == 0 && length(data.aws_lb.edge) == 0 && length(aws_route53_record.preview_wildcard) == 0 && length(aws_route53_record.edge) == 0
    error_message = "Before the alias flags are set, the module looks up no ALB and creates no alias record."
  }

  assert {
    condition = output.github_variables["hello-web"] == {
      AWS_REGION               = "eu-west-2"
      ECR_REGISTRY             = "111122223333.dkr.ecr.eu-west-2.amazonaws.com"
      PUBLISH_REPOSITORY_ID    = "123456789"
      PUBLISH_OWNER_ID         = "987654"
      AGENT_IMAGE_REPOSITORY   = "patchy/app-envs/hello-web"
      AGENT_ROLE_ARN           = "arn:aws:iam::111122223333:role/mocked"
      RUNTIME_IMAGE_REPOSITORY = "patchy/previews/hello-web"
      RUNTIME_ROLE_ARN         = "arn:aws:iam::111122223333:role/mocked"
    }
    error_message = "An app with previews gets exactly the eight contract variables."
  }

  assert {
    condition     = sort(keys(output.github_variables["tools"])) == tolist(["AGENT_IMAGE_REPOSITORY", "AGENT_ROLE_ARN", "AWS_REGION", "ECR_REGISTRY", "PUBLISH_OWNER_ID", "PUBLISH_REPOSITORY_ID"])
    error_message = "An app without previews gets no RUNTIME_ variables."
  }

  assert {
    condition = output.github_variables_dotenv["tools"] == join("", [
      "AGENT_IMAGE_REPOSITORY=patchy/app-envs/tools\n",
      "AGENT_ROLE_ARN=arn:aws:iam::111122223333:role/mocked\n",
      "AWS_REGION=eu-west-2\n",
      "ECR_REGISTRY=111122223333.dkr.ecr.eu-west-2.amazonaws.com\n",
      "PUBLISH_OWNER_ID=987654\n",
      "PUBLISH_REPOSITORY_ID=555\n",
    ])
    error_message = "github_variables_dotenv is one sorted NAME=value line per variable, for gh variable set -f -."
  }

  assert {
    condition = (
      output.apps["hello-web"].github_repository == "Acme-Org/Hello.Web" &&
      output.apps["hello-web"].oidc_subject == "repo:Acme-Org@987654/Hello.Web@123456789:ref:refs/heads/main" &&
      output.apps["tools"].oidc_subject == "repo:Acme-Org@987654/tools@555:ref:refs/heads/main" &&
      output.apps["tools"].runtime_role_arn == null
    )
    error_message = "apps reports each app's repository and trusted subject, with null runtime values when previews are off."
  }

  assert {
    condition = jsonencode(output.preview_node_class) == jsonencode({
      role               = "acme-prod-patchy-preview-node"
      role_arn           = "arn:aws:iam::111122223333:role/mocked"
      subnet_ids         = ["subnet-node-a", "subnet-node-b"]
      security_group_ids = ["sg-0cluster"]
    })
    error_message = "preview_node_class names the node role, the private node subnets and the cluster security group."
  }
}

# After Helm stage 1 only the edge ALB exists. Its aliases must not need the
# preview ALB, whose lookup would fail the plan with "no matching LB" (the
# mock always finds one, so the test asserts the lookup is never made).
run "edge_aliases_before_previews_are_on" {
  command = apply

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id      = "Z0EDGE"
      webhook_host = "patchy.acme.dev"
      status_host  = "status.patchy.acme.dev"
      alb_name     = "acme-prod"
    }
    create_edge_alias_records = true
  }

  assert {
    condition     = data.aws_lb.edge[0].name == "acme-prod" && sort(keys(aws_route53_record.edge)) == tolist(["patchy.acme.dev", "status.patchy.acme.dev"])
    error_message = "The edge flag aliases both edge hosts to the edge ALB."
  }

  assert {
    condition     = length(data.aws_lb.preview) == 0 && length(aws_route53_record.preview_wildcard) == 0
    error_message = "The edge aliases never look up the preview ALB, which does not exist before Helm stage 2."
  }
}

run "preview_alias_after_previews_are_on" {
  command = apply

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_name        = "acme-previews"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id      = "Z0EDGE"
      webhook_host = "patchy.acme.dev"
      status_host  = "status.patchy.acme.dev"
      alb_name     = "acme-prod"
    }
    create_edge_alias_records   = true
    create_preview_alias_record = true
  }

  assert {
    condition = (
      data.aws_lb.preview[0].name == "acme-previews" &&
      aws_route53_record.preview_wildcard[0].name == "*.preview.acme-apps.dev" &&
      aws_route53_record.preview_wildcard[0].zone_id == "Z0PREVIEW" &&
      aws_route53_record.preview_wildcard[0].alias[0].name == "acme-alb-123456789.eu-west-2.elb.amazonaws.com"
    )
    error_message = "The preview flag aliases *.<host_suffix> to the preview ALB looked up by its configured name."
  }

  assert {
    condition     = data.aws_lb.edge[0].name == "acme-prod" && sort(keys(aws_route53_record.edge)) == tolist(["patchy.acme.dev", "status.patchy.acme.dev"])
    error_message = "The edge flag aliases both edge hosts to the edge ALB."
  }
}

run "wildcard_edge_certificate_shares_one_validation_record" {
  command = plan

  variables {
    edge = {
      zone_id             = "Z0EDGE"
      webhook_host        = "patchy.acme.dev"
      status_host         = "status.patchy.acme.dev"
      certificate_domains = ["patchy.acme.dev", "*.patchy.acme.dev"]
      alb_name            = "acme-prod"
    }
  }

  assert {
    condition     = toset(keys(aws_route53_record.edge_validation)) == toset(["patchy.acme.dev"]) && aws_acm_certificate.edge[0].subject_alternative_names == toset(["*.patchy.acme.dev"])
    error_message = "A name and its wildcard share one validation record."
  }
}

run "explicit_service_cidrs_win" {
  command = apply

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
      dns_cidr        = "10.100.0.10/32"
      api_server_cidr = "10.100.0.1/32"
    }
    wait_for_certificate_validation = false
  }

  assert {
    condition = (
      yamldecode(output.helm_values).preview.dnsCIDR == "10.100.0.10/32" &&
      yamldecode(output.helm_values).previewController.config.apiServerCIDR == "10.100.0.1/32" &&
      length(aws_acm_certificate_validation.preview) == 0 &&
      yamldecode(output.helm_values).preview.certificateARN == "arn:aws:acm:eu-west-2:111122223333:certificate/11111111-1111-1111-1111-111111111111"
    )
    error_message = "Explicit DNS and API server /32s win over the derived ones, and without the validation wait the certificate ARN still reaches helm_values."
  }
}

run "ipv6_cluster_needs_explicit_service_cidrs" {
  command = plan

  override_data {
    target = data.aws_eks_cluster.this
    values = {
      arn = "arn:aws:eks:eu-west-2:111122223333:cluster/acme-prod"
      vpc_config = [{
        vpc_id                    = "vpc-0acme"
        cluster_security_group_id = "sg-0cluster"
        endpoint_private_access   = true
        endpoint_public_access    = true
        public_access_cidrs       = []
        security_group_ids        = []
        subnet_ids                = []
      }]
      kubernetes_network_config = [{
        ip_family              = "ipv6"
        service_ipv4_cidr      = ""
        service_ipv6_cidr      = "fd00:1234::/108"
        elastic_load_balancing = []
      }]
    }
  }

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [aws_iam_role.preview_node]
}

run "public_node_subnet_is_refused" {
  command = plan

  override_data {
    target = data.aws_subnet.preview_node["subnet-node-a"]
    values = {
      vpc_id                  = "vpc-0acme"
      availability_zone       = "eu-west-2a"
      cidr_block              = "10.40.130.0/24"
      map_public_ip_on_launch = true
    }
  }

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [data.aws_subnet.preview_node]
}

run "untagged_alb_subnet_is_refused" {
  command = plan

  override_data {
    target = data.aws_subnet.preview_alb["subnet-alb-a"]
    values = {
      vpc_id            = "vpc-0acme"
      availability_zone = "eu-west-2a"
      cidr_block        = "10.40.128.0/24"
      tags              = {}
    }
  }

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [data.aws_subnet.preview_alb]
}

run "alb_subnet_wider_than_slash_20_is_refused" {
  command = plan

  override_data {
    target = data.aws_subnet.preview_alb["subnet-alb-a"]
    values = {
      vpc_id            = "vpc-0acme"
      availability_zone = "eu-west-2a"
      cidr_block        = "10.40.0.0/16"
      tags              = { "kubernetes.io/role/elb" = "1" }
    }
  }

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [data.aws_subnet.preview_alb]
}

run "subnet_outside_the_cluster_vpc_is_refused" {
  command = plan

  override_data {
    target = data.aws_subnet.preview_node["subnet-node-a"]
    values = {
      vpc_id                  = "vpc-0other"
      availability_zone       = "eu-west-2a"
      cidr_block              = "10.50.0.0/19"
      map_public_ip_on_launch = false
    }
  }

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [data.aws_subnet.preview_node]
}

run "slug_must_not_be_a_github_name" {
  command = plan

  variables {
    apps = {
      "Hello.Web" = {
        github = {
          owner            = "Acme-Org"
          name             = "Hello.Web"
          repository_id    = 123456789
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
        }
      }
    }
  }

  expect_failures = [var.apps]
}

run "publisher_role_names_fit_iam" {
  command = plan

  variables {
    app_role_name_prefix = "acme-production-cluster-eu-west-2-patchy-app-"
    apps = {
      hello-web-frontend = {
        github = {
          owner            = "Acme-Org"
          name             = "Hello.Web"
          repository_id    = 123456789
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
        }
      }
    }
  }

  expect_failures = [var.apps]
}

run "two_apps_for_one_repository_are_refused" {
  command = plan

  variables {
    apps = {
      hello-web = {
        github = {
          owner            = "Acme-Org"
          name             = "Hello.Web"
          repository_id    = 123456789
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
        }
      }
      hello = {
        github = {
          owner            = "Acme-Org"
          name             = "Hello.Web"
          repository_id    = 123456789
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
        }
      }
    }
  }

  expect_failures = [var.apps]
}

run "classic_subject_is_refused" {
  command = plan

  variables {
    apps = {
      hello-web = {
        github = {
          owner            = "Acme-Org"
          name             = "Hello.Web"
          repository_id    = 123456789
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org/Hello.Web"
        }
      }
    }
  }

  expect_failures = [var.apps]
}

run "agent_prefix_must_be_disjoint_from_previews" {
  command = plan

  variables {
    agent_path_prefix = "patchy"
  }

  expect_failures = [var.agent_path_prefix]
}

# The preview prefix is the operator's. It reaches the preview nodes' pull
# grant, each app's runtime repository and the chart (helm_values
# preview.imagePathPrefix), and stays disjoint from the agent prefix.
run "custom_preview_prefix" {
  command = apply

  variables {
    agent_path_prefix   = "acme/agents"
    preview_path_prefix = "acme/runtime"
    apps = {
      hello-web = {
        github = {
          owner            = "Acme-Org"
          name             = "Hello.Web"
          repository_id    = 123456789
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
        }
      }
    }
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a", "subnet-node-b"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  assert {
    condition = (
      yamldecode(output.helm_values).preview.imagePathPrefix == "acme/runtime" &&
      yamldecode(output.helm_values).agent.repositoryImages.registries == ["111122223333.dkr.ecr.eu-west-2.amazonaws.com/acme/agents/"]
    )
    error_message = "helm_values carries the preview prefix for the chart's preview.imagePathPrefix beside the agent registry prefix."
  }

  assert {
    condition     = jsondecode(aws_iam_policy.preview_node[0].policy).Statement[2].Resource == "arn:aws:ecr:eu-west-2:111122223333:repository/acme/runtime/*"
    error_message = "Preview nodes pull under the configured preview prefix only."
  }

  assert {
    condition = (
      output.github_variables["hello-web"].RUNTIME_IMAGE_REPOSITORY == "acme/runtime/hello-web" &&
      output.github_variables["hello-web"].AGENT_IMAGE_REPOSITORY == "acme/agents/hello-web"
    )
    error_message = "Each app's runtime repository sits under the preview prefix, its agent repository under the agent prefix."
  }
}

# Managed prefix lists are optional and additive: they reach the chart's
# preview.prefixListsIDs beside inbound_cidrs, which they never replace.
run "preview_prefix_lists" {
  command = apply

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a", "subnet-node-b"]
      inbound_cidrs   = ["203.0.113.7/32"]
      prefix_list_ids = ["pl-0123456789abcdef0", "pl-01234567"]
    }
  }

  assert {
    condition = (
      yamldecode(output.helm_values).preview.prefixListsIDs == ["pl-0123456789abcdef0", "pl-01234567"] &&
      yamldecode(output.helm_values).preview.inboundCIDRs == ["203.0.113.7/32"]
    )
    error_message = "helm_values carries the prefix lists as preview.prefixListsIDs beside inboundCIDRs."
  }
}

run "preview_prefix_list_id_must_be_one" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a", "subnet-node-b"]
      inbound_cidrs   = ["203.0.113.7/32"]
      prefix_list_ids = ["sg-0123456789abcdef0"]
    }
  }

  expect_failures = [var.previews]
}

run "preview_prefix_must_not_contain_agent_prefix" {
  command = plan

  variables {
    agent_path_prefix   = "apps/previews/agents"
    preview_path_prefix = "apps/previews"
  }

  expect_failures = [var.agent_path_prefix]
}

run "preview_prefix_must_not_sit_under_agent_prefix" {
  command = plan

  variables {
    agent_path_prefix   = "apps"
    preview_path_prefix = "apps/previews"
  }

  expect_failures = [var.agent_path_prefix]
}

run "preview_prefix_must_not_equal_agent_prefix" {
  command = plan

  variables {
    agent_path_prefix   = "apps/images"
    preview_path_prefix = "apps/images"
  }

  expect_failures = [var.agent_path_prefix]
}

run "preview_prefix_must_be_a_path" {
  command = plan

  variables {
    preview_path_prefix = "patchy/previews/"
  }

  expect_failures = [var.preview_path_prefix]
}

run "empty_preview_prefix_is_refused" {
  command = plan

  variables {
    preview_path_prefix = ""
  }

  expect_failures = [var.preview_path_prefix]
}

# A 51-character slug fits IAM's 64 only with no prefix at all
# (51 + "-runtime-push"), so this plans only if "" is used as given rather
# than replaced by the "<cluster_name>-app-" default.
run "empty_app_role_name_prefix_is_honoured" {
  command = plan

  variables {
    app_role_name_prefix = ""
    apps = {
      abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefg = {
        github = {
          owner            = "Acme-Org"
          name             = "Hello.Web"
          repository_id    = 123456789
          owner_id         = 987654
          sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
        }
      }
    }
  }

  assert {
    condition     = keys(module.app) == ["abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefg"]
    error_message = "An explicitly empty app_role_name_prefix is used as given, so a 51-character slug still fits IAM's 64."
  }
}

run "empty_name_prefix_is_refused" {
  command = plan

  variables {
    name_prefix = ""
  }

  expect_failures = [var.name_prefix]
}

run "long_cluster_name_needs_a_name_prefix" {
  command = plan

  variables {
    cluster_name = "acme-production-cluster-in-eu-west-2-blue"
  }

  expect_failures = [var.name_prefix]
}

run "default_preview_alb_name_must_fit" {
  command = plan

  variables {
    cluster_name = "acme-production-eu-west-2"
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [var.previews]
}

run "previews_are_never_open_to_the_internet" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["0.0.0.0/0"]
    }
  }

  expect_failures = [var.previews]
}

run "node_subnets_must_not_be_alb_subnets" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-alb-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [var.previews]
}

# An ALB spans at least two zones: one subnet would plan, then fail when Auto
# Mode creates the ALB at Helm stage 2.
run "one_alb_subnet_is_refused" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [var.previews]
}

# An ALB takes one subnet per zone, so two listed subnets in one zone can
# never all be where the ALB is placed.
run "alb_subnets_must_be_in_distinct_zones" {
  command = plan

  override_data {
    target = data.aws_subnet.preview_alb["subnet-alb-b"]
    values = {
      vpc_id            = "vpc-0acme"
      availability_zone = "eu-west-2a"
      cidr_block        = "10.40.129.0/24"
      tags              = { "kubernetes.io/role/elb" = "1" }
    }
  }

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [aws_iam_role.preview_node]
}

# The ALB sends no traffic to a target in a zone it has not enabled
# (Target.NotInUse), so a preview node in a zone no ALB subnet covers could
# never serve its preview.
run "node_subnet_outside_the_alb_zones_is_refused" {
  command = plan

  override_data {
    target = data.aws_subnet.preview_node["subnet-node-b"]
    values = {
      vpc_id                  = "vpc-0acme"
      availability_zone       = "eu-west-2c"
      cidr_block              = "10.40.64.0/19"
      map_public_ip_on_launch = false
    }
  }

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a", "subnet-node-b"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  expect_failures = [aws_iam_role.preview_node]
}

# Fewer node zones than ALB zones is fine: every node is still reachable.
run "node_subnets_may_cover_fewer_zones_than_the_alb" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-b"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
  }

  assert {
    condition     = length(aws_iam_role.preview_node) == 1
    error_message = "a node subnet in one of the ALB's zones was refused"
  }
}

# Previews get an ALB of their own: the preview IngressClassParams limits its
# ALB to inbound_cidrs, which must never cover the GitHub webhook.
run "preview_alb_must_not_be_the_edge_alb" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_name        = "acme-prod"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id      = "Z0EDGE"
      webhook_host = "patchy.acme.dev"
      alb_name     = "acme-prod"
    }
  }

  expect_failures = [var.previews]
}

# The default preview name, "<cluster_name>-preview", collides too, whatever
# the case of the edge name.
run "default_preview_alb_name_must_not_be_the_edge_alb" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id      = "Z0EDGE"
      webhook_host = "patchy.acme.dev"
      alb_name     = "Acme-Prod-Preview"
    }
  }

  expect_failures = [var.previews]
}

run "edge_certificate_must_cover_both_hosts" {
  command = plan

  variables {
    edge = {
      zone_id             = "Z0EDGE"
      webhook_host        = "patchy.acme.dev"
      status_host         = "status.acme.dev"
      certificate_domains = ["patchy.acme.dev", "*.patchy.acme.dev"]
      alb_name            = "acme-prod"
    }
  }

  expect_failures = [var.edge]
}

run "oidc_provider_for_another_issuer_is_refused" {
  command = plan

  variables {
    github_oidc_provider_arn = "arn:aws:iam::111122223333:oidc-provider/gitlab.com"
  }

  expect_failures = [var.github_oidc_provider_arn]
}

# Preview sign-in: the relay's host joins the edge certificate and aliases,
# helm_values carries its host and edge Ingress annotations and nothing that
# turns sign-in on, and the Dex redirect URI is an output.
run "preview_auth_on_the_edge" {
  command = apply

  override_resource {
    target = aws_acm_certificate.edge[0]
    values = {
      arn = "arn:aws:acm:eu-west-2:111122223333:certificate/22222222-2222-2222-2222-222222222222"
      domain_validation_options = [
        {
          domain_name           = "patchy.acme.dev"
          resource_record_name  = "_a2.patchy.acme.dev."
          resource_record_type  = "CNAME"
          resource_record_value = "_b2.acm-validations.aws."
        },
        {
          domain_name           = "status.patchy.acme.dev"
          resource_record_name  = "_a3.status.patchy.acme.dev."
          resource_record_type  = "CNAME"
          resource_record_value = "_b3.acm-validations.aws."
        },
        {
          domain_name           = "preview-auth.patchy.acme.dev"
          resource_record_name  = "_a4.preview-auth.patchy.acme.dev."
          resource_record_type  = "CNAME"
          resource_record_value = "_b4.acm-validations.aws."
        },
      ]
    }
  }

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id      = "Z0EDGE"
      webhook_host = "patchy.acme.dev"
      status_host  = "status.patchy.acme.dev"
      alb_name     = "acme-prod"
    }
    preview_auth = {
      host = "preview-auth.patchy.acme.dev"
    }
    create_edge_alias_records = true
  }

  assert {
    condition = yamldecode(output.helm_values).previewAuth == {
      host = "preview-auth.patchy.acme.dev"
      ingress = { annotations = {
        "alb.ingress.kubernetes.io/certificate-arn" = "arn:aws:acm:eu-west-2:111122223333:certificate/22222222-2222-2222-2222-222222222222"
        "alb.ingress.kubernetes.io/listen-ports"    = "[{\"HTTP\": 80}, {\"HTTPS\": 443}]"
        "alb.ingress.kubernetes.io/ssl-redirect"    = "443"
      } }
    }
    error_message = "helm_values sets the relay's host and edge Ingress annotations, and never enabled, the stage, Dex or the viewers."
  }

  assert {
    condition     = yamldecode(output.helm_values).preview.inboundCIDRs == ["203.0.113.7/32"]
    error_message = "Turning sign-in's edge on leaves the preview allowlist as it is."
  }

  assert {
    condition = (
      contains(aws_acm_certificate.edge[0].subject_alternative_names, "preview-auth.patchy.acme.dev") &&
      contains(keys(aws_route53_record.edge_validation), "preview-auth.patchy.acme.dev") &&
      sort(keys(aws_route53_record.edge)) == tolist(["patchy.acme.dev", "preview-auth.patchy.acme.dev", "status.patchy.acme.dev"])
    )
    error_message = "The relay's host is on the edge certificate, validated, and aliased to the edge ALB."
  }

  assert {
    condition     = output.preview_auth_dex_redirect_uri == "https://preview-auth.patchy.acme.dev/dex/callback"
    error_message = "The output is the relay's one Dex redirect URI."
  }
}

run "preview_auth_off_renders_nothing" {
  command = plan

  variables {
    edge = {
      zone_id      = "Z0EDGE"
      webhook_host = "patchy.acme.dev"
      alb_name     = "acme-prod"
    }
  }

  assert {
    condition     = !contains(keys(yamldecode(output.helm_values)), "previewAuth") && output.preview_auth_dex_redirect_uri == null
    error_message = "Without preview_auth, helm_values has no previewAuth key and there is no redirect URI."
  }
}

# Rev C: with sign-in, the allowlist may be emptied; the chart is what then
# insists on the require stage, the confirmation and a short session.
run "preview_auth_admits_an_empty_allowlist" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = []
    }
    edge = {
      zone_id             = "Z0EDGE"
      webhook_host        = "patchy.acme.dev"
      certificate_domains = ["patchy.acme.dev", "*.patchy.acme.dev"]
      alb_name            = "acme-prod"
    }
    preview_auth = {
      host = "preview-auth.patchy.acme.dev"
    }
  }

  assert {
    condition     = yamldecode(output.helm_values).preview.inboundCIDRs == []
    error_message = "An empty allowlist reaches the chart as an empty list, for its own Rev C guard."
  }
}

run "empty_allowlist_without_preview_auth_is_refused" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = []
    }
  }

  expect_failures = [aws_iam_role.preview_node]
}

run "preview_auth_host_under_the_preview_suffix_is_refused" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id             = "Z0EDGE"
      webhook_host        = "patchy.acme.dev"
      certificate_domains = ["patchy.acme.dev", "*.preview.acme-apps.dev"]
      alb_name            = "acme-prod"
    }
    preview_auth = {
      host = "auth.preview.acme-apps.dev"
    }
  }

  expect_failures = [var.preview_auth]
}

run "preview_auth_needs_previews_and_edge" {
  command = plan

  variables {
    preview_auth = {
      host = "preview-auth.patchy.acme.dev"
    }
  }

  expect_failures = [var.preview_auth]
}

run "preview_auth_host_must_be_its_own" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id      = "Z0EDGE"
      webhook_host = "patchy.acme.dev"
      status_host  = "status.patchy.acme.dev"
      alb_name     = "acme-prod"
    }
    preview_auth = {
      host = "status.patchy.acme.dev"
    }
  }

  expect_failures = [var.preview_auth]
}

run "edge_certificate_must_cover_the_relay" {
  command = plan

  variables {
    previews = {
      host_suffix     = "preview.acme-apps.dev"
      zone_id         = "Z0PREVIEW"
      alb_subnet_ids  = ["subnet-alb-a", "subnet-alb-b"]
      node_subnet_ids = ["subnet-node-a"]
      inbound_cidrs   = ["203.0.113.7/32"]
    }
    edge = {
      zone_id             = "Z0EDGE"
      webhook_host        = "patchy.acme.dev"
      certificate_domains = ["patchy.acme.dev"]
      alb_name            = "acme-prod"
    }
    preview_auth = {
      host = "preview-auth.patchy.acme.dev"
    }
  }

  expect_failures = [var.preview_auth]
}
