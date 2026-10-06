// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The in-cluster names the test fixtures dial: the artifact server
// (testSpec, testEvalSpec, treesSpec) and the egress broker (brokeredConfig).
const (
	artifactHost = "patchy-source-controller.patchy.svc.cluster.local"
	brokerHost   = "patchy-egress-broker.patchy.svc.cluster.local"
)

// fakeResolver answers from a table and records what it was asked, and
// whether each lookup carried a deadline. A host it has no answer for fails
// the way a resolver does.
type fakeResolver struct {
	answers   map[string][]string
	errs      map[string]error
	asked     []string
	deadlines []bool
}

func (f *fakeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	f.asked = append(f.asked, host)
	_, ok := ctx.Deadline()
	f.deadlines = append(f.deadlines, ok)
	if err := f.errs[host]; err != nil {
		return nil, err
	}
	addrs, ok := f.answers[host]
	if !ok {
		return nil, fmt.Errorf("lookup %s: no such host", host)
	}
	return addrs, nil
}

// clusterResolver resolves the two in-cluster names to ClusterIPs.
func clusterResolver() *fakeResolver {
	return &fakeResolver{answers: map[string][]string{
		artifactHost: {"10.100.12.7"},
		brokerHost:   {"10.100.40.2"},
	}}
}

// dnsNoneConfig is brokeredConfig without a resolver for the pod.
func dnsNoneConfig(r Resolver) Config {
	cfg := brokeredConfig()
	cfg.DNS = DNSNone
	cfg.Resolver = r
	return cfg
}

// TestGoldenDNSNoneJob pins the brokered Job without a resolver: dnsPolicy
// None, the dead loopback nameserver with its fail-fast options, and the
// artifact server and the broker in the hosts file. Clearing those three
// fields gives back the brokered Job byte for byte: nothing else moves.
func TestGoldenDNSNoneJob(t *testing.T) {
	cfg := dnsNoneConfig(clusterResolver())
	job, err := New(fake.NewClientset(), cfg, nil).build(context.Background(),
		NameFor(testSpec().Finding, testSpec().Kind, int32(testSpec().Attempt)), testSpec())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	goldenJob(t, "job_dns_none", job)

	pod := &job.Spec.Template.Spec
	pod.DNSPolicy, pod.DNSConfig, pod.HostAliases = "", nil, nil
	if got, want := marshal(t, job), marshal(t, buildJobForTest(t, brokeredConfig(), testSpec())); got != want {
		t.Errorf("DNSNone changed more than the pod's name resolution:\n--- want\n%s\n--- got\n%s", want, got)
	}
}

// TestDNSClusterLeavesTheJobAlone: the default mode, named or empty, builds
// the Job exactly as before and never consults the resolver, even one that
// would fail every lookup.
func TestDNSClusterLeavesTheJobAlone(t *testing.T) {
	for _, mode := range []DNSMode{"", DNSCluster} {
		t.Run(string(mode)+"/", func(t *testing.T) {
			r := &fakeResolver{}
			cfg := brokeredConfig()
			cfg.DNS, cfg.Resolver = mode, r
			job := createJob(t, cfg, testSpec())
			pod := job.Spec.Template.Spec
			if pod.DNSPolicy != "" || pod.DNSConfig != nil || pod.HostAliases != nil {
				t.Errorf("pod DNS = %q %v %v, want the cluster default untouched", pod.DNSPolicy, pod.DNSConfig,
					pod.HostAliases)
			}
			if len(r.asked) != 0 {
				t.Errorf("resolver asked %v, want no lookups", r.asked)
			}
			if got, want := marshal(t, job), marshal(t, createJob(t, brokeredConfig(), testSpec())); got != want {
				t.Errorf("DNS mode %q changed the Job", mode)
			}
		})
	}
}

// TestDNSNonePodShape exercises Create under DNSNone: the pod resolves
// nothing but what its hosts file says, and that file names exactly the
// hosts the Job's URLs dial, each at every address the controller's
// resolver gave for it, grouped by address, in a fixed order.
func TestDNSNonePodShape(t *testing.T) {
	tests := []struct {
		name    string
		cfg     func(r Resolver) Config
		spec    func() Spec
		answers map[string][]string
		want    []corev1.HostAlias
		asked   []string
	}{
		{
			name: "brokered: the artifact server and the broker",
			cfg:  dnsNoneConfig,
			spec: testSpec,
			answers: map[string][]string{
				artifactHost: {"10.100.12.7"},
				brokerHost:   {"10.100.40.2"},
			},
			want: []corev1.HostAlias{
				{IP: "10.100.12.7", Hostnames: []string{artifactHost}},
				{IP: "10.100.40.2", Hostnames: []string{brokerHost}},
			},
			asked: []string{brokerHost, artifactHost},
		},
		{
			name: "two names on one address share an entry; dual-stack and mapped answers normalise",
			cfg:  dnsNoneConfig,
			spec: testSpec,
			answers: map[string][]string{
				artifactHost: {"fd00:10:100::7", "10.100.0.9", "::ffff:10.100.0.9"},
				brokerHost:   {"10.100.0.9"},
			},
			want: []corev1.HostAlias{
				{IP: "10.100.0.9", Hostnames: []string{brokerHost, artifactHost}},
				{IP: "fd00:10:100::7", Hostnames: []string{artifactHost}},
			},
			asked: []string{brokerHost, artifactHost},
		},
		{
			name: "an address literal needs no entry, and a host is matched in lower case",
			cfg: func(r Resolver) Config {
				cfg := dnsNoneConfig(r)
				claude := cfg.Runners["claude"]
				claude.Env = map[string]string{"ANTHROPIC_BASE_URL": "http://10.100.40.2:8080/anthropic"}
				cfg.Runners["claude"] = claude
				return cfg
			},
			spec: func() Spec {
				spec := testSpec()
				spec.ArtifactURL = "http://Patchy-Source-Controller.patchy.svc.cluster.local:9790/a.tar.gz"
				return spec
			},
			answers: map[string][]string{artifactHost: {"10.100.12.7"}},
			want:    []corev1.HostAlias{{IP: "10.100.12.7", Hostnames: []string{artifactHost}}},
			asked:   []string{artifactHost},
		},
		{
			name: "every provider route the runner carries",
			cfg: func(r Resolver) Config {
				cfg := dnsNoneConfig(r)
				claude := cfg.Runners["claude"]
				claude.Env = map[string]string{
					"ANTHROPIC_BEDROCK_BASE_URL": "http://bedrock-broker.patchy.svc.cluster.local:8080/bedrock",
					"AWS_REGION":                 "us-east-1",
				}
				cfg.Runners["claude"] = claude
				return cfg
			},
			spec: testSpec,
			answers: map[string][]string{
				artifactHost: {"10.100.12.7"},
				"bedrock-broker.patchy.svc.cluster.local": {"10.100.41.3"},
			},
			want: []corev1.HostAlias{
				{IP: "10.100.12.7", Hostnames: []string{artifactHost}},
				{IP: "10.100.41.3", Hostnames: []string{"bedrock-broker.patchy.svc.cluster.local"}},
			},
			asked: []string{"bedrock-broker.patchy.svc.cluster.local", artifactHost},
		},
		{
			name: "a plan's trees: every artifact host it fetches from",
			cfg:  dnsNoneConfig,
			spec: func() Spec {
				spec := treesSpec()
				spec.Trees[0].ArtifactURL = "http://mirror.patchy.svc.cluster.local:9790/artifacts/feedface.tar.gz"
				return spec
			},
			answers: map[string][]string{
				artifactHost:                      {"10.100.12.7"},
				brokerHost:                        {"10.100.40.2"},
				"mirror.patchy.svc.cluster.local": {"10.100.12.8"},
			},
			want: []corev1.HostAlias{
				{IP: "10.100.12.7", Hostnames: []string{artifactHost}},
				{IP: "10.100.12.8", Hostnames: []string{"mirror.patchy.svc.cluster.local"}},
				{IP: "10.100.40.2", Hostnames: []string{brokerHost}},
			},
			asked: []string{"mirror.patchy.svc.cluster.local", brokerHost, artifactHost},
		},
		{
			name: "the fake runner: the artifact server alone",
			cfg: func(r Resolver) Config {
				cfg := dnsNoneConfig(r)
				cfg.Runners["fake"] = Runner{Image: "ghcr.io/bitwise-media-group/patchy/fake-agent:1"}
				return cfg
			},
			spec: func() Spec {
				spec := testSpec()
				spec.Harness = "fake"
				return spec
			},
			answers: map[string][]string{artifactHost: {"10.100.12.7"}},
			want:    []corev1.HostAlias{{IP: "10.100.12.7", Hostnames: []string{artifactHost}}},
			asked:   []string{artifactHost},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeResolver{answers: tt.answers}
			pod := createJob(t, tt.cfg(r), tt.spec()).Spec.Template.Spec
			if pod.DNSPolicy != corev1.DNSNone {
				t.Errorf("dnsPolicy = %q, want None", pod.DNSPolicy)
			}
			if !reflect.DeepEqual(pod.DNSConfig, dnsConfig()) || pod.DNSConfig.Nameservers[0] != "127.0.0.1" ||
				len(pod.DNSConfig.Searches) != 0 {
				t.Errorf("dnsConfig = %+v, want only the loopback nameserver and the fail-fast options", pod.DNSConfig)
			}
			if !reflect.DeepEqual(pod.HostAliases, tt.want) {
				t.Errorf("hostAliases = %+v, want %+v", pod.HostAliases, tt.want)
			}
			slices.Sort(r.asked)
			slices.Sort(tt.asked)
			if !slices.Equal(r.asked, tt.asked) {
				t.Errorf("resolver asked %v, want %v", r.asked, tt.asked)
			}
			if slices.Contains(r.deadlines, false) {
				t.Error("a lookup ran without a deadline")
			}
		})
	}
}

// TestDNSNoneRefusals: a Job whose pod could not reach an endpoint is never
// created, and neither is its Secret. A name the controller cannot resolve
// is ErrUnresolved, naming the host, and is retried like any failed create;
// a URL with no host the hosts file can hold, and a runner that dials its
// model API by name, are refused outright.
func TestDNSNoneRefusals(t *testing.T) {
	refused := errors.New("server misbehaving")
	tests := []struct {
		name       string
		cfg        func(r Resolver) Config
		spec       func() Spec
		resolver   *fakeResolver
		unresolved bool
		wantErr    string
	}{
		{
			name: "the broker does not resolve",
			cfg:  dnsNoneConfig,
			spec: testSpec,
			resolver: &fakeResolver{
				answers: map[string][]string{artifactHost: {"10.100.12.7"}},
				errs:    map[string]error{brokerHost: refused},
			},
			unresolved: true,
			wantErr:    brokerHost + ": server misbehaving",
		},
		{
			name:       "the artifact server is unknown",
			cfg:        dnsNoneConfig,
			spec:       testSpec,
			resolver:   &fakeResolver{answers: map[string][]string{brokerHost: {"10.100.40.2"}}},
			unresolved: true,
			wantErr:    artifactHost + ": no such host",
		},
		{
			name: "an answer with no usable address",
			cfg:  dnsNoneConfig,
			spec: testSpec,
			resolver: &fakeResolver{answers: map[string][]string{
				artifactHost: {"10.100.12.7"},
				brokerHost:   {"not-an-address"},
			}},
			unresolved: true,
			wantErr:    brokerHost + " resolved to no address",
		},
		{
			name: "an empty answer",
			cfg:  dnsNoneConfig,
			spec: testSpec,
			resolver: &fakeResolver{answers: map[string][]string{
				artifactHost: {},
				brokerHost:   {"10.100.40.2"},
			}},
			unresolved: true,
			wantErr:    artifactHost + " resolved to no address",
		},
		{
			name: "an artifact URL with no host",
			cfg:  dnsNoneConfig,
			spec: func() Spec {
				spec := testSpec()
				spec.ArtifactURL = "/artifacts/deadbeef.tar.gz"
				return spec
			},
			resolver: clusterResolver(),
			wantErr:  `agent endpoint "/artifacts/deadbeef.tar.gz" names no host`,
		},
		{
			name: "a host the hosts file cannot hold",
			cfg:  dnsNoneConfig,
			spec: func() Spec {
				spec := testSpec()
				spec.ArtifactURL = "http://" + artifactHost + ".:9790/artifacts/deadbeef.tar.gz"
				return spec
			},
			resolver: clusterResolver(),
			wantErr:  `host "` + artifactHost + `." cannot be pinned`,
		},
		{
			name: "a runner that dials its model API by name",
			cfg:  dnsNoneConfig,
			spec: func() Spec {
				spec := testSpec()
				spec.Harness, spec.Model = "codex", "openai/gpt-5-codex"
				return spec
			},
			resolver: clusterResolver(),
			wantErr:  `runner "codex" reaches its model API by name and cannot run with agent DNS none`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset()
			_, _, err := New(cs, tt.cfg(tt.resolver), nil).Create(context.Background(), tt.spec())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Create err = %v, want one containing %q", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrUnresolved); got != tt.unresolved {
				t.Errorf("errors.Is(err, ErrUnresolved) = %v, want %v", got, tt.unresolved)
			}
			jobsList, _ := cs.BatchV1().Jobs("patchy-agents").List(context.Background(), metav1.ListOptions{})
			secrets, _ := cs.CoreV1().Secrets("patchy-agents").List(context.Background(), metav1.ListOptions{})
			if len(jobsList.Items) != 0 || len(secrets.Items) != 0 {
				t.Errorf("a refused launch left %d Jobs and %d Secrets, want none",
					len(jobsList.Items), len(secrets.Items))
			}
		})
	}
}

// TestDNSNoneEvaluationJob: an evaluation Job is held to the same mode,
// pinning its workspace bundle's host and the broker.
func TestDNSNoneEvaluationJob(t *testing.T) {
	cs := fake.NewClientset()
	name, err := New(cs, dnsNoneConfig(clusterResolver()), nil).CreateEval(context.Background(), testEvalSpec())
	if err != nil {
		t.Fatalf("CreateEval: %v", err)
	}
	job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	pod := job.Spec.Template.Spec
	want := []corev1.HostAlias{
		{IP: "10.100.12.7", Hostnames: []string{artifactHost}},
		{IP: "10.100.40.2", Hostnames: []string{brokerHost}},
	}
	if pod.DNSPolicy != corev1.DNSNone || !reflect.DeepEqual(pod.DNSConfig, dnsConfig()) ||
		!reflect.DeepEqual(pod.HostAliases, want) {
		t.Errorf("eval pod DNS = %q %+v %+v, want None, the dead nameserver and %+v",
			pod.DNSPolicy, pod.DNSConfig, pod.HostAliases, want)
	}

	cs = fake.NewClientset()
	_, err = New(cs, dnsNoneConfig(&fakeResolver{}), nil).CreateEval(context.Background(), testEvalSpec())
	if !errors.Is(err, ErrUnresolved) {
		t.Errorf("CreateEval with nothing resolvable err = %v, want ErrUnresolved", err)
	}
	jobsList, _ := cs.BatchV1().Jobs("patchy-agents").List(context.Background(), metav1.ListOptions{})
	if len(jobsList.Items) != 0 {
		t.Errorf("a refused evaluation launch left %d Jobs", len(jobsList.Items))
	}
}

func TestParseDNSMode(t *testing.T) {
	tests := []struct {
		in      string
		want    DNSMode
		wantErr bool
	}{
		{"", DNSCluster, false},
		{"cluster", DNSCluster, false},
		{"none", DNSNone, false},
		{"None", "", true},
		{"off", "", true},
	}
	for _, tt := range tests {
		got, err := ParseDNSMode(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseDNSMode(%q) = %q, %v; want %q, error %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestNeedsResolver(t *testing.T) {
	tests := []struct {
		name string
		r    Runner
		want bool
	}{
		{"brokered claude", Runner{Brokered: true, Env: map[string]string{"ANTHROPIC_BASE_URL": "http://b"}}, false},
		{"codex with its key", Runner{Secret: "openai", SecretEnv: "OPENAI_API_KEY"}, true},
		{"the fake runner", Runner{}, false},
		{"brokered, a legacy Secret ignored", Runner{Brokered: true, Secret: "anthropic"}, false},
	}
	for _, tt := range tests {
		if got := NeedsResolver(tt.r); got != tt.want {
			t.Errorf("%s: NeedsResolver = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestHostAliasesProperty: whatever the resolver answers, in whatever order
// and with whatever repeats, the hosts file states exactly what it said:
// every host at every address it resolved to and nowhere else, each address
// once and in ascending order, each entry's names sorted and distinct. So the
// pod spec is a function of the answers, not of their order.
func TestHostAliasesProperty(t *testing.T) {
	hosts := []string{"a.patchy.svc", "b.patchy.svc", "c.patchy.svc"}
	pool := []string{"10.0.0.1", "10.0.0.2", "::ffff:10.0.0.1", "fd00::1", "fd00::2", "10.0.0.10"}
	cfg := &quick.Config{
		MaxCount: 1000,
		Rand:     rand.New(rand.NewSource(20261006)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			answers := map[string][]string{}
			for _, h := range hosts {
				n := 1 + r.Intn(4)
				for range n {
					answers[h] = append(answers[h], pool[r.Intn(len(pool))])
				}
			}
			args[0] = reflect.ValueOf(answers)
			args[1] = reflect.ValueOf(r.Int63())
		},
	}
	faithful := func(answers map[string][]string, shuffleSeed int64) bool {
		c := New(fake.NewClientset(), Config{Resolver: &fakeResolver{answers: answers}}, nil)
		got, err := c.hostAliases(context.Background(), hosts)
		if err != nil {
			return false
		}
		want := map[string]map[string]bool{} // address -> hosts
		for h, addrs := range answers {
			for _, a := range addrs {
				addr := netip.MustParseAddr(a).Unmap().String()
				if want[addr] == nil {
					want[addr] = map[string]bool{}
				}
				want[addr][h] = true
			}
		}
		if len(got) != len(want) {
			return false
		}
		for i, alias := range got {
			if i > 0 && netip.MustParseAddr(got[i-1].IP).Compare(netip.MustParseAddr(alias.IP)) >= 0 {
				return false
			}
			if !slices.IsSorted(alias.Hostnames) || len(slices.Compact(slices.Clone(alias.Hostnames))) != len(alias.Hostnames) {
				return false
			}
			if len(alias.Hostnames) != len(want[alias.IP]) {
				return false
			}
			for _, h := range alias.Hostnames {
				if !want[alias.IP][h] {
					return false
				}
			}
		}
		shuffled := map[string][]string{}
		r := rand.New(rand.NewSource(shuffleSeed))
		for h, addrs := range answers {
			s := slices.Clone(addrs)
			r.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
			shuffled[h] = s
		}
		again, err := New(fake.NewClientset(), Config{Resolver: &fakeResolver{answers: shuffled}}, nil).
			hostAliases(context.Background(), hosts)
		return err == nil && reflect.DeepEqual(got, again)
	}
	if err := quick.Check(faithful, cfg); err != nil {
		t.Error(err)
	}
}
