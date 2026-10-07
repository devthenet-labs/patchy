// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestIntentAgentDNSNone runs the shipped intent-controller as the chart
// renders agent.networkPolicy.dns: none, with the broker named by a host
// this machine's resolver answers (localhost; the artifact server is an
// address already, so it needs no entry). The API server admits the plan
// Job (dnsPolicy None is valid only beside a nameserver); its pod's only
// nameserver is its own loopback; the broker's name is pinned in its hosts
// file at every address the controller resolved it to, and nothing else is;
// the claude CLI is still pointed at the broker by that name. The plan runs
// and is posted as usual.
func TestIntentAgentDNSNone(t *testing.T) {
	e := startIntents(t, "--agent-dns", "none", "--broker-url", "http://localhost:8080")
	_, name := e.fileIntent(t)
	e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	plan := e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan"))

	pod := plan.Job.Spec.Template.Spec
	if pod.DNSPolicy != corev1.DNSNone {
		t.Errorf("plan pod dnsPolicy = %q, want None", pod.DNSPolicy)
	}
	if pod.DNSConfig == nil || !slices.Equal(pod.DNSConfig.Nameservers, []string{"127.0.0.1"}) ||
		len(pod.DNSConfig.Searches) != 0 {
		t.Errorf("plan pod dnsConfig = %+v, want the loopback nameserver alone and no search path", pod.DNSConfig)
	}

	raw, err := resolvedAddrs(context.Background(), "localhost")
	if err != nil || len(raw) == 0 {
		t.Fatalf("resolve localhost on this machine: %v %v", raw, err)
	}
	got := make([]string, 0, len(pod.HostAliases))
	for _, a := range pod.HostAliases {
		if !slices.Equal(a.Hostnames, []string{"localhost"}) {
			t.Errorf("hosts file entry %s names %v, want only the broker's host", a.IP, a.Hostnames)
		}
		got = append(got, a.IP)
	}
	slices.Sort(got)
	if !slices.Equal(got, raw) {
		t.Errorf("hosts file pins localhost at %v, want every address the controller resolves it to: %v", got, raw)
	}
	if url := plan.Env["ANTHROPIC_BASE_URL"]; url != "http://localhost:8080/anthropic" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the broker by the pinned name", url)
	}
}

// resolvedAddrs resolves host the way intent-controller's default Resolver
// does (net.DefaultResolver) and normalises the answers as the Job builder
// does: each address once, sorted here as strings.
func resolvedAddrs(ctx context.Context, host string) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range addrs {
		a, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		if ip := a.Unmap().WithZone("").String(); !slices.Contains(out, ip) {
			out = append(out, ip)
		}
	}
	slices.Sort(out)
	return out, nil
}
