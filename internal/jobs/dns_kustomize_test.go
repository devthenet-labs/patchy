// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// kustomizeRoot is the kustomize tree, relative to this package.
const kustomizeRoot = "../../deploy/kustomize"

// buildKustomize renders an overlay of the base with components, in order,
// from an in-memory copy of the kustomize tree, so the build never writes
// into the repository.
func buildKustomize(t *testing.T, components ...string) map[string]map[string]any {
	t.Helper()
	mem := filesys.MakeFsInMemory()
	err := filepath.WalkDir(kustomizeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(kustomizeRoot, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return mem.WriteFile(filepath.Join("/k", rel), b)
	})
	if err != nil {
		t.Fatal(err)
	}
	var overlay strings.Builder
	overlay.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [../../base]\n")
	if len(components) > 0 {
		overlay.WriteString("components:\n")
		for _, c := range components {
			fmt.Fprintf(&overlay, "  - ../../components/%s\n", c)
		}
	}
	if err := mem.WriteFile("/k/overlays/check/kustomization.yaml", []byte(overlay.String())); err != nil {
		t.Fatal(err)
	}
	m, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(mem, "/k/overlays/check")
	if err != nil {
		t.Fatalf("kustomize build with %v: %v", components, err)
	}
	out := map[string]map[string]any{}
	for _, r := range m.Resources() {
		obj, err := r.Map()
		if err != nil {
			t.Fatal(err)
		}
		out[r.GetKind()+"/"+r.GetName()] = obj
	}
	return out
}

// egressPorts lists each egress rule of a NetworkPolicy or
// CiliumNetworkPolicy as its ports, comma-joined, in rule order.
func egressPorts(t *testing.T, obj map[string]any) []string {
	t.Helper()
	if obj == nil {
		t.Fatal("policy not rendered")
	}
	spec, _ := obj["spec"].(map[string]any)
	rules, _ := spec["egress"].([]any)
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		var ports []string
		list, _ := rule["ports"].([]any)
		if tp, ok := rule["toPorts"].([]any); ok {
			for _, p := range tp {
				pm, _ := p.(map[string]any)
				inner, _ := pm["ports"].([]any)
				list = append(list, inner...)
			}
		}
		for _, p := range list {
			pm, _ := p.(map[string]any)
			ports = append(ports, fmt.Sprint(pm["port"]))
		}
		out = append(out, strings.Join(ports, ","))
	}
	return out
}

func agentDNS(objs map[string]map[string]any) any {
	data, _ := objs["ConfigMap/patchy-config"]["data"].(map[string]any)
	return data["PATCHY_AGENT_DNS"]
}

// TestAgentDNSNoneComponent renders the kustomize component
// components/agent-dns-none: on the base, and after components/cilium, it
// sets PATCHY_AGENT_DNS=none and removes exactly the DNS rule of the agent
// egress policy it patches (by list index, guarded by a test op), leaving
// every other rule in its order.
func TestAgentDNSNoneComponent(t *testing.T) {
	for _, tt := range []struct {
		name, policy string
		before       []string
		with         []string
	}{
		{name: "base", policy: "NetworkPolicy/patchy-agents-egress"},
		{name: "cilium", policy: "CiliumNetworkPolicy/patchy-agent-egress-claude", before: []string{"cilium"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			off := buildKustomize(t, tt.before...)
			on := buildKustomize(t, append(slices.Clone(tt.before), "agent-dns-none")...)
			if v := agentDNS(off); v != nil {
				t.Errorf("without the component PATCHY_AGENT_DNS = %v, want unset", v)
			}
			if v := agentDNS(on); v != "none" {
				t.Errorf("with the component PATCHY_AGENT_DNS = %v, want none", v)
			}
			was, is := egressPorts(t, off[tt.policy]), egressPorts(t, on[tt.policy])
			if len(was) == 0 || !strings.Contains(was[0], "53") {
				t.Fatalf("%s rule 0 without the component is %q, want the DNS rule (port 53)", tt.policy, was)
			}
			if want := was[1:]; !slices.Equal(is, want) {
				t.Errorf("%s egress ports with the component = %q, want %q: only the DNS rule removed",
					tt.policy, is, want)
			}
			for _, rule := range is {
				if slices.Contains(strings.Split(rule, ","), "53") {
					t.Errorf("%s still admits DNS (%q)", tt.policy, is)
				}
			}
		})
	}
}
