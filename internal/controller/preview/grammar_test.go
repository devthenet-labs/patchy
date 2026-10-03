// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"math/rand"
	"os"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The three other places a preview image's grammar lives: the Project and
// Preview CRDs generated from api/v1alpha1, and the slot admission policy.
const (
	projectCRD      = "../../../deploy/kustomize/base/crds/patchy.bitwisemedia.uk_projects.yaml"
	previewCRD      = "../../../deploy/kustomize/base/crds/patchy.bitwisemedia.uk_previews.yaml"
	admissionPolicy = "../../../charts/patchy/templates/preview-admission.yaml"
)

// imageRepositoryPatterns collects every imageRepository pattern in a CRD:
// spec.preview and repositories[].preview on a Project, the components on a
// Preview.
func imageRepositoryPatterns(t *testing.T, path string) []*regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var crd any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	var out []*regexp.Regexp
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if repo, ok := v["imageRepository"].(map[string]any); ok {
				if p, ok := repo["pattern"].(string); ok {
					out = append(out, regexp.MustCompile(p))
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(crd)
	return out
}

// admissionLeaf is the pattern the slot admission policy holds what follows
// the image prefix to, the leaf and then the immutable tag: one expression
// in the template, rendered into both the Pod and the Deployment policy.
func admissionLeaf(t *testing.T) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(admissionPolicy)
	if err != nil {
		t.Fatalf("read %s: %v", admissionPolicy, err)
	}
	m := regexp.MustCompile(`substring\(\{\{ len \$prefix \}\}\)\.matches\('([^']+)'\)`).FindAllSubmatch(raw, -1)
	if len(m) != 1 {
		t.Fatalf("%s: want one leaf pattern after the image prefix, found %d", admissionPolicy, len(m))
	}
	return regexp.MustCompile(string(m[0][1]))
}

// genLabel is a random DNS label: namePattern's grammar.
func genLabel(r *rand.Rand) string {
	const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	b.WriteByte(alnum[r.Intn(len(alnum))])
	if n := r.Intn(12); n > 0 {
		for range n - 1 {
			b.WriteByte((alnum + "--")[r.Intn(len(alnum)+2)])
		}
		b.WriteByte(alnum[r.Intn(len(alnum))])
	}
	return b.String()
}

// genSegment is a random ECR path segment: lowercase alphanumeric runs
// joined by single ".", "_" or "-" separators.
func genSegment(r *rand.Rand) string {
	const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for run := 1 + r.Intn(3); run > 0; run-- {
		for n := 1 + r.Intn(4); n > 0; n-- {
			b.WriteByte(alnum[r.Intn(len(alnum))])
		}
		if run > 1 {
			b.WriteByte("._-"[r.Intn(3)])
		}
	}
	return b.String()
}

// genPrefix is a random image prefix in prefixPattern's grammar: a host,
// maybe a port, and one to three path segments.
func genPrefix(r *rand.Rand) string {
	hosts := []string{"registry.example", "123456789012.dkr.ecr.us-east-1.amazonaws.com", "localhost", "r1.example"}
	prefix := hosts[r.Intn(len(hosts))]
	if r.Intn(4) == 0 {
		prefix += ":5000"
	}
	for n := 1 + r.Intn(3); n > 0; n-- {
		prefix += "/" + genSegment(r)
	}
	return prefix + "/"
}

// TestImageGrammarAgrees holds the preview image grammar together across its
// four homes, as a seeded property: whatever prefix Validate accepts and
// whatever leaf the controller renders, the Project and Preview schemas
// store the repository and the slot admission policy admits the image the
// Pod pulls, so the controller never renders what another layer refuses.
// The VAP's leaf ([a-z0-9-]+) is deliberately the wider of the two, kept
// byte-identical across the configurable prefix; the controller is the
// strict one.
func TestImageGrammarAgrees(t *testing.T) {
	schemas := append(imageRepositoryPatterns(t, projectCRD), imageRepositoryPatterns(t, previewCRD)...)
	if len(schemas) != 3 {
		t.Fatalf("found %d imageRepository patterns in the CRDs, want 3 (spec.preview, repositories[].preview, "+
			"components)", len(schemas))
	}
	leaf := admissionLeaf(t)
	r := rand.New(rand.NewSource(20261003))
	for i := 0; i < 2000; i++ {
		prefix, name := genPrefix(r), genLabel(r)
		if !prefixPattern.MatchString(prefix) || !namePattern.MatchString(name) {
			t.Fatalf("case %d: generator produced %q + %q outside the controller grammar", i, prefix, name)
		}
		for _, schema := range schemas {
			if !schema.MatchString(prefix + name) {
				t.Fatalf("case %d: the schema %s refuses %q, which the controller renders", i, schema, prefix+name)
			}
		}
		if tagged := name + ":sha-" + testSHA; !leaf.MatchString(tagged) {
			t.Fatalf("case %d: the admission policy's leaf %s refuses %q", i, leaf, tagged)
		}
	}
}

// TestSchemaLeafIsADNSLabel is the schema's leaf regression without an API
// server: the CRDs admitted app- and -app (a [a-z0-9-]+ leaf) before the
// leaf became the controller's DNS label, so a Project could hold an image
// repository every Preview of it would then fail on.
func TestSchemaLeafIsADNSLabel(t *testing.T) {
	schemas := append(imageRepositoryPatterns(t, projectCRD), imageRepositoryPatterns(t, previewCRD)...)
	for _, repo := range []string{
		"registry.example/patchy/previews/app-", "registry.example/patchy/previews/-app",
		"registry.example/patchy/previews/-", "registry.example/acme/previews/demo-",
		"registry.example/patchy/previews/Demo", "registry.example/patchy/previews/demo_app",
		"registry.example/patchy/previews/demo.app", "registry.example/demo",
		"registry.example/patchy/previews/demo:latest", "registry.example/acme//demo",
	} {
		for _, schema := range schemas {
			if schema.MatchString(repo) {
				t.Errorf("the schema %s admits %q", schema, repo)
			}
		}
	}
	for _, repo := range []string{"registry.example/patchy/previews/app", "registry.example/acme/previews/demo",
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/team_a/runtime.images/web-app"} {
		for _, schema := range schemas {
			if !schema.MatchString(repo) {
				t.Errorf("the schema %s refuses %q", schema, repo)
			}
		}
	}
}
