// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package hostprobe is the preview sign-in relay's continuous check of the
// real load balancer (critique F5). Every Kubernetes object can conform
// while the ALB still serves old, unauthenticated rules (a failed model
// build for the whole IngressGroup keeps them), so the relay periodically
// asks every Ready Preview's host for "/" with no cookies and without
// following redirects, and judges the answer with previewauth.JudgeProbe: a
// 302 to this relay's /authorize for that slot's client and that host's
// callback, or the host is unprotected.
//
// It exports two gauges: patchy.preview_auth.unprotected_hosts (hosts that
// answered something else, an app's 200 above all) and
// patchy.preview_auth.probe.unreachable_hosts (no answer, which is expected
// while the preview ALB's IP allowlist does not admit the relay's egress
// addresses). Each unprotected host is also logged at warn.
package hostprobe
