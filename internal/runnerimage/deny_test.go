// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// The preview image prefix the chart denies, beside agent allowlist entries
// that contain it: the overlap the chart refuses to render, which Deny must
// still close on its own.
const (
	ecrHost      = "123456789012.dkr.ecr.us-east-1.amazonaws.com"
	deniedPrefix = ecrHost + "/patchy/previews/"
)

func TestPolicyDeny(t *testing.T) {
	allow, err := NewPolicy([]string{ecrHost + "/patchy/", "123456789012.dkr-ecr.us-east-1.on.aws/patchy/",
		ecrHost + ":443/patchy/", "ghcr.io/org/"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := allow.Deny([]string{deniedPrefix})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in     string
		denied bool
	}{
		{ecrHost + "/patchy/app-envs/web:toolchain-v1", false},
		{ecrHost + "/patchy/previews-agents/web", false}, // a sibling, not under the denied path
		{"ghcr.io/org/app", false},
		{ecrHost + "/patchy/previews/web:sha-" + strings.Repeat("a", 40), true},
		{ecrHost + "/patchy/previews/web@sha256:" + hex64, true},
		{ecrHost + "/patchy/previews", true}, // the denied path itself
		{ecrHost + "/patchy/previews/team/web", true},
		{"123456789012.DKR.ECR.us-east-1.amazonaws.com/Patchy/Previews/web", true},
		// Other names for the same ECR repository, each allowlisted above.
		{"123456789012.dkr-ecr.us-east-1.on.aws/patchy/previews/web", true},
		{ecrHost + ":443/patchy/previews/web", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			ref, err := ParseDeclared(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if err := allow.Allow(ref); err != nil {
				t.Fatalf("the allowlist alone refuses %q: %v", c.in, err)
			}
			err = p.Allow(ref)
			if !c.denied {
				if err != nil {
					t.Errorf("Allow(%q) = %v, want allowed", c.in, err)
				}
				return
			}
			want := "image `" + ref.String() + "` is under `" + deniedPrefix + "`, a registry path agent images " +
				"may never come from (the operator reserves it for other images, such as previews built from pull " +
				"requests)"
			var rej *Rejection
			if !errors.As(err, &rej) || rej.Reason != "DeniedRegistry" || rej.Message != want {
				t.Errorf("Allow(%q) = %#v, want Rejection DeniedRegistry %q", c.in, err, want)
			}
		})
	}

	// A FIPS spelling is the denied repository, refused before the allowlist
	// is consulted; another region is a different registry, refused only for
	// not being allowlisted.
	for in, reason := range map[string]string{
		"123456789012.dkr.ecr-fips.us-east-1.amazonaws.com/patchy/previews/web": "DeniedRegistry",
		"123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/previews/web":      "",
	} {
		ref, err := ParseDeclared(in)
		if err != nil {
			t.Fatal(err)
		}
		var rej *Rejection
		if err := p.Allow(ref); !errors.As(err, &rej) || rej.Reason != reason {
			t.Errorf("Allow(%q) = %#v, want a Rejection with reason %q", in, err, reason)
		}
	}
}

func TestPolicyDenyValidation(t *testing.T) {
	allow, err := NewPolicy([]string{"ghcr.io/org/"})
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"":                 "denied registry path is empty",
		"ghcr.io":          "denied registry path `ghcr.io` must be `host/path/` with at least one path segment",
		"ghcr.io/org/*":    "denied registry path `ghcr.io/org/*` may not contain glob characters",
		"ghcr.io/org//x/":  "denied registry path `ghcr.io/org//x/` has an empty path segment",
		"ghcr.io/org/a:1/": "denied registry path `ghcr.io/org/a:1/` may not name a tag or digest",
		"ghcr.io/o rg/":    "denied registry path `ghcr.io/o rg/` contains whitespace",
	} {
		if _, err := allow.Deny([]string{in}); err == nil || err.Error() != want {
			t.Errorf("Deny(%q) error = %v, want %q", in, err, want)
		}
	}
	same, err := allow.Deny(nil)
	if err != nil || len(same.Denied()) != 0 || !reflect.DeepEqual(same.Entries(), allow.Entries()) {
		t.Errorf("Deny(nil) = %v, %v; want the policy unchanged", same, err)
	}
	p, err := allow.Deny([]string{"GHCR.io/Org/Previews"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Denied(), []string{"ghcr.io/org/previews/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Denied = %v, want %v", got, want)
	}
	p.Denied()[0] = "mutated/"
	if p.Denied()[0] != "ghcr.io/org/previews/" || len(allow.Denied()) != 0 {
		t.Error("Denied must return a copy, and Deny must leave the receiver as it was")
	}
}

// TestPolicyDenyProperty: under any allowlist entry, a denied path beneath it
// refuses every repository at or under it, and nothing else the entry
// admits, siblings that share its leading characters included. Seeded, so
// the gate is deterministic.
func TestPolicyDenyProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20261003))
	for i := 0; i < 500; i++ {
		entry, err := NormalizeEntry(hosts[r.Intn(len(hosts))] + "/" + strings.Join(genSegments(r), "/"))
		if err != nil {
			t.Fatal(err)
		}
		reserved := genSegment(r)
		allow, err := NewPolicy([]string{entry})
		if err != nil {
			t.Fatal(err)
		}
		p, err := allow.Deny([]string{entry + reserved})
		if err != nil {
			t.Fatal(err)
		}
		under := []string{entry + reserved}
		for n := 1 + r.Intn(3); n > 0; n-- {
			under = append(under, under[len(under)-1]+"/"+genSegment(r))
		}
		for _, in := range under {
			ref, err := ParseDeclared(in)
			if err != nil {
				t.Fatalf("case %d: %q: %v", i, in, err)
			}
			var rej *Rejection
			if err := p.Allow(ref); !errors.As(err, &rej) || rej.Reason != "DeniedRegistry" {
				t.Fatalf("case %d: %q must be denied under %q: %v", i, in, entry+reserved, err)
			}
		}
		for _, sibling := range []string{reserved + "x", "x" + reserved, reserved + "-" + genSegment(r)} {
			in := entry + sibling + "/" + genSegment(r)
			ref, err := ParseDeclared(in)
			if err != nil {
				t.Fatalf("case %d: %q: %v", i, in, err)
			}
			if err := p.Allow(ref); err != nil {
				t.Fatalf("case %d: %q must stay allowed beside %q: %v", i, in, entry+reserved, err)
			}
		}
	}
}

func TestRegistryKey(t *testing.T) {
	for in, want := range map[string]string{
		ecrHost + "/patchy/": ecrHost + "/patchy/",
		"123456789012.DKR.ECR.US-EAST-1.amazonaws.com/Patchy/":  ecrHost + "/patchy/",
		ecrHost + ":443/patchy/":                                ecrHost + "/patchy/",
		"123456789012.dkr-ecr.us-east-1.on.aws/patchy/":         ecrHost + "/patchy/",
		"123456789012.dkr-ecr-fips.us-east-1.on.aws/patchy/":    ecrHost + "/patchy/",
		"123456789012.dkr.ecr-fips.us-east-1.amazonaws.com/x/":  ecrHost + "/x/",
		"123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/":  "123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/",
		"localhost:5000/team/":                                  "localhost:5000/team/",
		"ghcr.io:4443/org/":                                     "ghcr.io:4443/org/",
		"evil.example/123456789012.dkr-ecr.us-east-1.on.aws/x/": "evil.example/123456789012.dkr-ecr.us-east-1.on.aws/x/",
		"1234567890123.dkr-ecr.us-east-1.on.aws/patchy/":        "1234567890123.dkr-ecr.us-east-1.on.aws/patchy/",
		ecrHost + ".evil.io/x/":                                 ecrHost + ".evil.io/x/",
	} {
		if got := registryKey(in); got != want {
			t.Errorf("registryKey(%q) = %q, want %q", in, got, want)
		}
	}
}
