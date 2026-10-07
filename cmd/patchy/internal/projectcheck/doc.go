// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package projectcheck is the engine behind `patchy check project`: a
// workstation preflight that tells an operator whether a Project is ready
// for its first intent, and, when it is not, which part is missing.
//
// It is not a second controller. Every fact the in-cluster controllers
// already prove is read off the cluster rather than proven again: the
// Project's Ready condition (which intent-controller sets only after
// minting the App's scoped tokens on every repository and ensuring the
// labels), its IntentNameConflict condition, and each covering Forge's
// Ready condition. It never reads a Secret, so it never holds the App's
// private key: the cluster reads are Projects, Forges, ConfigMaps, one
// Ingress and Previews, with the caller's own kubeconfig.
//
// What the controllers cannot report before an intent runs, it checks with
// the operator's own identity, and says so in each reason:
//
//   - forge: forge.Resolve over the namespace's Forge CRs, the same pure
//     function the controllers call, for the intent repository and every
//     app repository;
//   - agent-image: each repository's declaration at its default-branch head
//     (read from GitHub with GH_TOKEN, else GITHUB_TOKEN, else anonymously;
//     an enterprise token goes only to the host GH_HOST names, since the
//     Project, not the caller, names the hosts), regular files only, as
//     source-controller's archive read; with runnerimage.Declare's
//     precedence, then imagecheck.Static under source-controller's live
//     policy, read from its ConfigMap (found by the app.kubernetes.io labels,
//     over kustomize's shared patchy-config); registry reads use
//     resolve.NewKeychain, so an ECR image is read with the caller's AWS
//     credentials. A signature source-controller requires but whose key is
//     not found here makes an otherwise passing image a SKIP;
//   - previews: that intent-controller writes Previews and preview-controller
//     is configured, then, per previewed repository, that its image
//     repository sits under preview-controller's prefix and that
//     sha-<default-branch head> is published there (a FAIL only with more
//     than one previewed repository, since only then does a preview run it);
//   - preview-dns and preview-tls: that <project>-0.<host suffix> resolves to
//     the preview load balancer (the placeholder Ingress's address) and
//     serves a certificate trusted for it. An Intent is <project>-<issue> and
//     no issue is numbered 0, so the probe host is one the wildcard record
//     and certificate cover and no Preview owns. Only NXDOMAIN fails the DNS
//     check; a lookup that times out is a SKIP. The load balancer admits
//     only the chart's inbound CIDRs, so a TLS timeout is a SKIP, not a FAIL;
//   - preview-auth, preview-auth-dex and preview-auth-host, only while the
//     chart's preview sign-in is on (the relay's ConfigMap exists, or
//     preview-controller requires sign-in): the relay answers its discovery
//     document at its issuer; Dex accepts the relay's client with its one
//     redirect URI (asked of Dex's authorization endpoint without a session,
//     following nothing, since Dex's clients are not readable); and, once
//     sign-in is required, the placeholder host and every Ready Preview of
//     the Project answer without credentials with the load balancer's
//     redirect to the relay for their own slot's client
//     (previewauth.JudgeProbe, the relay's own probe verdict). A timeout from
//     outside the inbound CIDRs is a SKIP.
//
// What it cannot prove: that source-controller's or the preview nodes' own
// registry credentials work from inside the cluster (a Repository's
// status.runnerImage and a Preview's status are the evidence for those).
//
// Every check is a Check line keyed by repository key (which also names the
// repository's preview component), or by IntentRepository, and is never
// limited to the first repository. Network and cluster reads go through
// seams (client.Reader, GitHub, the registry Keychain, Resolver and
// TLSDialer) so tests need no network at all.
package projectcheck
