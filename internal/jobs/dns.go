// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// DNSMode is how an agent pod resolves names (Config.DNS).
type DNSMode string

const (
	// DNSCluster leaves the pod on the cluster resolver: the Job is exactly
	// what it was before the mode existed. The default.
	DNSCluster DNSMode = "cluster"
	// DNSNone gives the pod no working resolver, which closes the last
	// channel out of the sandbox: a command that encodes data into a query
	// name (<chunk>.attacker.example) has nowhere to send it. The pod's only
	// nameserver is its own loopback, where nothing listens, and the hosts
	// it legitimately dials (the artifact server, the egress broker) are
	// written into its hosts file, resolved by the controller as it creates
	// the Job. Only a runner that reaches no model API by name can run so
	// (NeedsResolver).
	DNSNone DNSMode = "none"
)

// ParseDNSMode reads a DNSMode, empty meaning DNSCluster.
func ParseDNSMode(s string) (DNSMode, error) {
	switch m := DNSMode(s); m {
	case "", DNSCluster:
		return DNSCluster, nil
	case DNSNone:
		return m, nil
	}
	return "", fmt.Errorf("agent DNS %q is not %s or %s", s, DNSCluster, DNSNone)
}

// Resolver looks up a host's addresses. *net.Resolver satisfies it; the
// zero Config uses net.DefaultResolver, the controller's own resolver, which
// still reaches the cluster DNS the agent pods no longer do.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// ErrUnresolved wraps the failure to resolve one of a DNSNone Job's
// endpoints. The Job is not created: its pod could not reach the endpoint,
// and the controller's own resolver failing is usually transient, so the
// launch is retried like any other failed create.
var ErrUnresolved = errors.New("jobs: an agent endpoint did not resolve")

// deadNameserver is a DNSNone pod's only nameserver: its own loopback,
// where nothing listens. Kubernetes requires at least one nameserver with
// dnsPolicy None; this one refuses every query at once, so a lookup the
// hosts file cannot answer fails in milliseconds instead of timing out.
const deadNameserver = "127.0.0.1"

// resolveTimeout bounds the lookups one Job's creation makes.
const resolveTimeout = 10 * time.Second

// NeedsResolver reports whether a runner's pod reaches its model API by
// name: a runner that injects a Secret credential and is not brokered
// (codex, copilot) talks to its vendor's API directly, and that API's
// addresses cannot be pinned at Job creation the way a ClusterIP Service's
// can. Such a runner cannot run under DNSNone.
func NeedsResolver(r Runner) bool {
	return !r.Brokered && r.Secret != ""
}

// dnsConfig is a DNSNone pod's resolver configuration. One second and one
// attempt bound a resolver that does not see the refusal (musl sends from an
// unconnected socket, so the ICMP error never reaches it); glibc, Go and the
// claude CLI fail at once.
func dnsConfig() *corev1.PodDNSConfig {
	return &corev1.PodDNSConfig{
		Nameservers: []string{deadNameserver},
		Options: []corev1.PodDNSConfigOption{
			{Name: "timeout", Value: new("1")},
			{Name: "attempts", Value: new("1")},
		},
	}
}

// endpointURLs are the URLs a Job's pod dials: the artifact URLs its
// prepare init fetches from, then the runner's broker routes. A brokered
// runner's gateway env carries one per provider route; any other runner's
// carries none.
func endpointURLs(runner Runner, artifactURLs ...string) []string {
	urls := slices.Clone(artifactURLs)
	for _, name := range provider.BaseURLEnvNames {
		if u, ok := runner.Env[name]; ok {
			urls = append(urls, u)
		}
	}
	return urls
}

// isolateDNS applies Config.DNS to a pod that dials urls. Under DNSCluster
// it changes nothing. Under DNSNone it resolves every host the URLs name
// with the controller's resolver and gives the pod dnsPolicy None, the dead
// nameserver and a hostAliases entry per address, or returns an error, and
// the Job is not created, when the runner needs a resolver, a URL names no
// host the hosts file can hold, or a host does not resolve.
func (c *Client) isolateDNS(ctx context.Context, pod *corev1.PodSpec, harnessID string, runner Runner,
	urls []string) error {
	if c.cfg.DNS != DNSNone {
		return nil
	}
	if NeedsResolver(runner) {
		return fmt.Errorf("jobs: runner %q reaches its model API by name and cannot run with agent DNS %s; "+
			"only a brokered or credential-free runner may", harnessID, DNSNone)
	}
	hosts, err := endpointHosts(urls)
	if err != nil {
		return err
	}
	aliases, err := c.hostAliases(ctx, hosts)
	if err != nil {
		return err
	}
	pod.DNSPolicy = corev1.DNSNone
	pod.DNSConfig = dnsConfig()
	pod.HostAliases = aliases
	return nil
}

// endpointHosts are the distinct host names urls dial, sorted, lowercased
// (a hosts file matches without regard to case; the API server accepts only
// lowercase). An IP literal needs no entry. A URL without a host, or with a
// host the API server would refuse in hostAliases (a trailing dot, say), is
// an error rather than a pod that cannot reach it.
func endpointHosts(urls []string) ([]string, error) {
	set := map[string]bool{}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("jobs: agent endpoint %q names no host to pin in the pod's hosts file", raw)
		}
		host := strings.ToLower(u.Hostname())
		if _, err := netip.ParseAddr(host); err == nil {
			continue
		}
		if errs := validation.IsDNS1123Subdomain(host); len(errs) > 0 {
			return nil, fmt.Errorf("jobs: agent endpoint host %q cannot be pinned in the pod's hosts file: %s",
				host, strings.Join(errs, "; "))
		}
		set[host] = true
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// hostAliases resolves each host and groups the hosts by address: one entry
// per address, addresses and hostnames sorted, so the pod spec is
// deterministic whatever order the resolver answers in. A host that fails to
// resolve, or resolves to nothing usable, fails the whole Job (ErrUnresolved).
func (c *Client) hostAliases(ctx context.Context, hosts []string) ([]corev1.HostAlias, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	byAddr := map[netip.Addr][]string{}
	for _, host := range hosts {
		raw, err := c.cfg.Resolver.LookupHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrUnresolved, host, err)
		}
		found := false
		for _, s := range raw {
			addr, err := netip.ParseAddr(s)
			if err != nil {
				continue
			}
			addr = addr.Unmap().WithZone("")
			if !slices.Contains(byAddr[addr], host) {
				byAddr[addr] = append(byAddr[addr], host)
			}
			found = true
		}
		if !found {
			return nil, fmt.Errorf("%w: %s resolved to no address", ErrUnresolved, host)
		}
	}
	addrs := slices.SortedFunc(maps.Keys(byAddr), netip.Addr.Compare)
	aliases := make([]corev1.HostAlias, 0, len(addrs))
	for _, addr := range addrs {
		names := byAddr[addr]
		slices.Sort(names)
		aliases = append(aliases, corev1.HostAlias{IP: addr.String(), Hostnames: names})
	}
	return aliases, nil
}
