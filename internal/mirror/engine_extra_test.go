// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"

	"github.com/bitwise-media-group/patchy/internal/mirror/spec"
	"github.com/bitwise-media-group/patchy/internal/mirror/verify"
)

// recordingVerifier captures every verification subject and answers with
// a canned error.
type recordingVerifier struct {
	mu       sync.Mutex
	subjects []verify.Subject
	err      error
}

func (r *recordingVerifier) verify(_ context.Context, s verify.Subject) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subjects = append(r.subjects, s)
	return r.err
}

// verifyEngine rebuilds the fixture engine with an injected verifier and
// an event recorder.
func verifyEngine(f *fixture, v *recordingVerifier, events *[]Event) *Engine {
	f.t.Helper()
	global, err := spec.LoadConfig(filepath.Join(f.root, "mirror.yaml"))
	if err != nil {
		f.t.Fatal(err)
	}
	var mu sync.Mutex
	f.eng = New(Config{
		Root:   f.root,
		Global: global,
		Now:    func() time.Time { return testNow },
		Verify: v.verify,
		OnEvent: func(ev Event) {
			if events == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			*events = append(*events, ev)
		},
	})
	return f.eng
}

func TestEngineDefaults(t *testing.T) {
	f := newFixture(t)
	global, err := spec.LoadConfig(filepath.Join(f.root, "mirror.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	eng := New(Config{Root: f.root, Global: global})
	if eng.workers != defaultWorkers || eng.now == nil || eng.onEvent == nil || eng.reg == nil ||
		eng.tools == nil || eng.verifyFn == nil || eng.newSigner == nil || eng.pushChart == nil {
		t.Errorf("defaults not applied: %+v", eng)
	}
	// The default event sink discards without panicking.
	eng.notef("x", "stage", "msg %d", 1)

	regs, err := eng.SelectRegistries(nil)
	if err != nil || len(regs) != 1 || regs[0].Name != "primary" {
		t.Errorf("SelectRegistries(nil) = %v, %v", regs, err)
	}
	wantErrContains(t, errOf(eng.SelectRegistries([]string{"nope"})), `unknown registry "nope"`)
}

func TestPublishPathDefaults(t *testing.T) {
	reg := spec.Registry{Name: "r", URL: "reg.example/x", ChartNamespace: "charts", ImageNamespace: "images",
		ArtifactNamespace: "artifacts"}

	if got, err := imageTarget(reg, "ghcr.io/org/app"); err != nil ||
		got != "reg.example/x/images/ghcr.io/org/app:latest" {
		t.Errorf("imageTarget untagged = %q, %v", got, err)
	}
	if got, err := imageTarget(reg, "ghcr.io/org/app:v1"); err != nil || got != "reg.example/x/images/ghcr.io/org/app:v1" {
		t.Errorf("imageTarget tagged = %q, %v", got, err)
	}
	if _, err := imageTarget(reg, ""); err == nil {
		t.Error("imageTarget(empty) want error")
	}

	chart := spec.Entry{Name: "demo", Chart: &spec.ChartManifest{}}
	if got := chartRepo(reg, chart); got != "charts/demo" {
		t.Errorf("chartRepo default = %q", got)
	}
	chart.Chart.Publish.ChartRepo = "custom/demo"
	if got := chartRepo(reg, chart); got != "custom/demo" {
		t.Errorf("chartRepo override = %q", got)
	}

	art := spec.Entry{Name: "b", Artifact: &spec.ArtifactManifest{Artifact: spec.ArtifactSpec{Ref: "ghcr.io/x/b"}}}
	if got := artifactRepo(reg, art); got != "artifacts/ghcr.io/x/b" {
		t.Errorf("artifactRepo default = %q", got)
	}
}

func TestLintArtifact(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name     string
		manifest string
		want     []string
	}{
		{
			name: "valid",
			manifest: `artifact:
  ref: example.test/b
  version: "1.0.0"
scan:
  enabled: "true"
`,
		},
		{
			name: "missing version, bad provider and scan mode",
			manifest: `artifact:
  ref: example.test/b
  verifyUpstream:
    provider: gpg
scan:
  enabled: sometimes
`,
			want: []string{
				"artifact.ref and artifact.version are required",
				`artifact.verifyUpstream: unknown provider "gpg"`,
				`scan.enabled must be auto, true or false (got "sometimes")`,
			},
		},
		{
			name: "keyless without identity",
			manifest: `artifact:
  ref: example.test/b
  version: "1.0.0"
  verifyUpstream:
    provider: cosign-keyless
`,
			want: []string{"cosign-keyless requires certificateIdentityRegexp"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f.write("artifacts/bundle/manifest.yaml",
				"apiVersion: mirror.patchy.bitwisemedia.uk/v1alpha1\nkind: Artifact\nname: bundle\n"+tc.manifest)
			eng := f.engine()
			entry, err := eng.Entry("bundle")
			if err != nil {
				t.Fatal(err)
			}
			issues, err := eng.Lint(entry)
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			if len(issues) != len(tc.want) {
				t.Fatalf("issues = %q, want %d", issues, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(issues[i], w) {
					t.Errorf("issue %d = %q, want %q", i, issues[i], w)
				}
			}
		})
	}
}

func TestLintAllowlistUnparseable(t *testing.T) {
	f := newFixture(t)
	f.artifactManifest("example.test/bundles/bundle", "")
	f.write("artifacts/bundle/security/allowlist.yaml", "vulnerabilities: [oops\n")
	eng := f.engine()
	entry, err := eng.Entry("bundle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Lint(entry); err == nil {
		t.Error("Lint over an unparseable allowlist: want error")
	}
	f.write("artifacts/bundle/security/allowlist.yaml", `vulnerabilities:
  - statement: no id
  - id: CVE-A
    expired_at: "2026-09-01"
  - id: CVE-B
    statement: s
  - id: CVE-C
    statement: s
    expired_at: "next tuesday"
`)
	issues, err := eng.Lint(entry)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{
		"allowlist entry 0 missing id",
		"allowlist CVE-A missing statement",
		"allowlist CVE-B missing expired_at",
		`allowlist CVE-C has unparseable expired_at "next tuesday"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("issues missing %q:\n%s", want, joined)
		}
	}
	if len(issues) != 4 {
		t.Errorf("issues = %q, want exactly 4", issues)
	}
}

func TestEngineArtifactCooldownPick(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	canonical := "example.test/bundles/bundle"
	f.pushImage(f.host+"/bundles/bundle:1.0.0", testNow.AddDate(0, -2, 0))
	f.pushImage(f.host+"/bundles/bundle:1.1.0", testNow.AddDate(0, 0, -10))
	// Fresh: inside the 3-day global cooldown.
	f.pushImage(f.host+"/bundles/bundle:1.2.0", testNow.AddDate(0, 0, -1))
	// Outside the constraint entirely.
	f.pushImage(f.host+"/bundles/bundle:2.0.0", testNow.AddDate(0, 0, -30))

	tests := []struct {
		name     string
		cooldown string
		want     string
	}{
		{"global cooldown holds the fresh tag", "", "1.1.0"},
		{"per-artifact zero cooldown takes the newest", "  cooldownDays: 0\n", "1.2.0"},
		{"long cooldown keeps the pin", "  cooldownDays: 30\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f.write("artifacts/bundle/manifest.yaml", fmt.Sprintf(`apiVersion: mirror.patchy.bitwisemedia.uk/v1alpha1
kind: Artifact
name: bundle
artifact:
  ref: %s
  version: "1.0.0"
  versionConstraint: ">=1.0.0 <2.0.0"
%s  verifyUpstream:
    provider: none
`, canonical, tc.cooldown))
			eng := f.engine()
			entry, err := eng.Entry("bundle")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := eng.CheckUpdates(ctx, []spec.Entry{entry})
			if err != nil {
				t.Fatalf("CheckUpdates: %v", err)
			}
			if tc.want == "" {
				if len(plan.Groups) != 0 {
					t.Errorf("plan = %+v, want no work", plan.Groups)
				}
				return
			}
			if len(plan.Groups) != 1 || plan.Groups[0].Target != tc.want ||
				plan.Groups[0].Members[0].Kind != spec.KindArtifact {
				t.Errorf("plan = %+v, want target %s", plan.Groups, tc.want)
			}
		})
	}
}

func TestNewerThan(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.2.0", "1.1.9", true},
		{"v2.0.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"garbage", "1.0.0", false},
		{"1.0.0", "garbage", false},
	}
	for _, tc := range tests {
		if got := newerThan(tc.a, tc.b); got != tc.want {
			t.Errorf("newerThan(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestGroupTargetRejectsBadVersion(t *testing.T) {
	_, err := groupTarget([]MemberPlan{{Name: "a", Newest: "1.0.0"}, {Name: "b", Newest: "not-semver"}})
	if err == nil || !strings.Contains(err.Error(), `member b newest "not-semver"`) {
		t.Errorf("groupTarget = %v", err)
	}
}

func TestCheckUpdatesErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	t.Run("artifact repository absent", func(t *testing.T) {
		f.artifactManifest("example.test/absent/bundle", "")
		eng := f.engine()
		entry, err := eng.Entry("bundle")
		if err != nil {
			t.Fatal(err)
		}
		wantErrContains(t, errOf(eng.CheckUpdates(ctx, []spec.Entry{entry})), "bundle:")
	})

	t.Run("unparseable pinned version", func(t *testing.T) {
		f.pushChart(f.host+"/upstream", "weird", "1.0.0", f.chartTgz("weird", "1.0.0", "1.0.0", appCanonical+":1.0.0"))
		f.write("charts/weird/manifest.yaml", fmt.Sprintf(`apiVersion: mirror.patchy.bitwisemedia.uk/v1alpha1
kind: Chart
name: weird
chart:
  repo: oci://%s/upstream
  name: weird
  version: "1.0.0"
  versionConstraint: "this is not a constraint"
`, f.host))
		eng := f.engine()
		entry, err := eng.Entry("weird")
		if err != nil {
			t.Fatal(err)
		}
		wantErrContains(t, errOf(eng.CheckUpdates(ctx, []spec.Entry{entry})), "weird")
	})
}

// wantErrContains fails the test unless err is non-nil and mentions want.
func wantErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want one containing %q", err, want)
	}
}

const (
	ociUpstream = "oci://example.test/upstream"
	keylessRule = "    provider: cosign-keyless\n    certificateIdentityRegexp: ^https://github.com/org/.*$\n"
	noneRule    = "    provider: none\n"
	allNone     = "    - match: \"*\"\n      provider: none\n"
)

// verifyRulesFixture builds a demo chart store and returns a manifest
// writer over its chart repo, chart rule and image rules. converge runs one
// upgrade first so the entry has a lock.
func verifyRulesFixture(t *testing.T, converge bool) (*fixture, func(repo, chartRule, imageRules string)) {
	t.Helper()
	f := newFixture(t)
	f.pushImage(f.host+"/apps/app:1.0.0", testNow.AddDate(0, -1, 0))
	f.pushChart(f.host+"/upstream", "demo", "1.0.0", f.chartTgz("demo", "1.0.0", "1.0.0", appCanonical+":1.0.0"))
	f.write("charts/demo/values/discovery.yaml", "{}\n")
	writeChart := func(repo, chartRule, imageRules string) {
		f.write("charts/demo/manifest.yaml", fmt.Sprintf(`apiVersion: mirror.patchy.bitwisemedia.uk/v1alpha1
kind: Chart
name: demo
chart:
  repo: %s
  name: demo
  version: "1.0.0"
  verifyUpstream:
%sdiscovery:
  valuesFiles: [values/discovery.yaml]
images:
  verifyUpstream:
%spublish:
  chartRepo: charts/demo
`, repo, chartRule, imageRules))
	}
	if converge {
		writeChart("oci://"+f.host+"/upstream", noneRule, allNone)
		eng := f.engine()
		entry, err := eng.Entry("demo")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Upgrade(context.Background(), entry, ""); err != nil {
			t.Fatalf("Upgrade: %v", err)
		}
	}
	return f, writeChart
}

// verifyDemo rebuilds the engine over v and verifies the demo entry.
func verifyDemo(t *testing.T, f *fixture, v *recordingVerifier, events *[]Event) (*VerifyReport, spec.Entry, error) {
	t.Helper()
	eng := verifyEngine(f, v, events)
	entry, err := eng.Entry("demo")
	if err != nil {
		t.Fatal(err)
	}
	report, err := eng.VerifyUpstream(context.Background(), entry)
	return report, entry, err
}

func TestEngineVerifyUpstreamNoLock(t *testing.T) {
	f, writeChart := verifyRulesFixture(t, false)
	writeChart(ociUpstream, noneRule, allNone)
	_, _, err := verifyDemo(t, f, &recordingVerifier{}, nil)
	wantErrContains(t, err, "no lock (run upgrade first)")
}

func TestEngineVerifyUpstreamRuleErrors(t *testing.T) {
	f, writeChart := verifyRulesFixture(t, true)
	keyRule := func(algo string) string {
		r := "    - match: \"*\"\n      provider: cosign-key\n      key: k.pub\n"
		if algo != "" {
			r += "      signatureDigestAlgorithm: " + algo + "\n"
		}
		return r
	}
	tests := []struct {
		name, repo, chartRule, imageRules string
		verifyErr                         error
		want                              string
	}{
		{"verifier failure fails the entry", ociUpstream, keylessRule, allNone,
			errors.New("no matching signatures"), "signature verification failed for chart demo"},
		{"https repo cannot be verified", "https://charts.example.test", keylessRule, allNone,
			nil, "only supported for oci:// repos"},
		{"incomplete chart rule", ociUpstream, "    provider: cosign-key\n", allNone,
			nil, "chart demo: cosign-key requires key"},
		{"image with no matching rule", ociUpstream, noneRule, "    - match: \"ghcr.io/*\"\n      provider: none\n",
			nil, "no verifyUpstream rule matches image " + appCanonical},
		{"image rule with unsupported digest algorithm", ociUpstream, noneRule, keyRule("md5"),
			nil, `unsupported signatureDigestAlgorithm "md5"`},
		{"image verification failure", ociUpstream, noneRule, keyRule(""),
			errors.New("bad sig"), "image signature verification failed for " + appCanonical},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeChart(tc.repo, tc.chartRule, tc.imageRules)
			_, _, err := verifyDemo(t, f, &recordingVerifier{err: tc.verifyErr}, nil)
			wantErrContains(t, err, tc.want)
		})
	}
}

func TestEngineVerifyUpstreamKeylessChart(t *testing.T) {
	f, writeChart := verifyRulesFixture(t, true)
	writeChart(ociUpstream, keylessRule, allNone)
	v := &recordingVerifier{}
	var events []Event
	report, _, err := verifyDemo(t, f, v, &events)
	if err != nil {
		t.Fatalf("VerifyUpstream: %v", err)
	}
	if len(report.Verified) != 1 || report.Verified[0] != "chart demo" {
		t.Errorf("verified = %v", report.Verified)
	}
	if len(report.Gaps) != 1 || !strings.Contains(report.Gaps[0], appCanonical+":1.0.0 (rule: *)") {
		t.Errorf("gaps = %v", report.Gaps)
	}
	if len(v.subjects) != 1 {
		t.Fatalf("subjects = %+v", v.subjects)
	}
	s := v.subjects[0]
	if s.Ref != f.host+"/upstream/demo:1.0.0" || s.IdentityRegexp == "" ||
		s.Issuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("subject = %+v", s)
	}
	warned := false
	for _, ev := range events {
		if ev.Kind == "warn" && strings.Contains(ev.Message, "provenance gap") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("gaps must be narrated as warnings: %+v", events)
	}
}

func TestEngineVerifyUpstreamImageByDigest(t *testing.T) {
	f, writeChart := verifyRulesFixture(t, true)
	writeChart(ociUpstream, noneRule,
		"    - match: \"example.test/*\"\n      provider: cosign-key\n      key: k.pub\n"+
			"      signatureDigestAlgorithm: sha512\n")
	v := &recordingVerifier{}
	report, entry, err := verifyDemo(t, f, v, nil)
	if err != nil {
		t.Fatalf("VerifyUpstream: %v", err)
	}
	lock, err := spec.LoadImagesLock(entry.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.subjects) != 1 || v.subjects[0].Ref != f.host+"/apps/app@"+lock.Images[0].Digest ||
		v.subjects[0].HashAlgorithm != "sha512" || v.subjects[0].KeyRef != "k.pub" {
		t.Errorf("subjects = %+v", v.subjects)
	}
	if len(report.Verified) != 1 || report.Verified[0] != appCanonical+":1.0.0" {
		t.Errorf("report = %+v", report)
	}
}

func TestEngineVerifyUpstreamArtifact(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	write := func(rule string) {
		f.write("artifacts/bundle/manifest.yaml", `apiVersion: mirror.patchy.bitwisemedia.uk/v1alpha1
kind: Artifact
name: bundle
artifact:
  ref: example.test/bundles/bundle
  version: "1.0.0"
  verifyUpstream:
`+rule)
	}

	t.Run("documented gap", func(t *testing.T) {
		write("    provider: none\n")
		v := &recordingVerifier{}
		eng := verifyEngine(f, v, nil)
		entry, _ := eng.Entry("bundle")
		report, err := eng.VerifyUpstream(ctx, entry)
		if err != nil || len(report.Gaps) != 1 || !strings.Contains(report.Gaps[0], "artifact example.test/bundles/bundle") ||
			len(v.subjects) != 0 {
			t.Errorf("report = %+v, err = %v, subjects = %v", report, err, v.subjects)
		}
	})
	t.Run("keyless verified at the rewritten tag", func(t *testing.T) {
		write("    provider: cosign-keyless\n    certificateIdentityRegexp: .*\n" +
			"    certificateOidcIssuer: https://issuer.test\n")
		v := &recordingVerifier{}
		eng := verifyEngine(f, v, nil)
		entry, _ := eng.Entry("bundle")
		report, err := eng.VerifyUpstream(ctx, entry)
		if err != nil || len(report.Verified) != 1 {
			t.Fatalf("report = %+v, err = %v", report, err)
		}
		if len(v.subjects) != 1 || v.subjects[0].Ref != f.host+"/bundles/bundle:1.0.0" ||
			v.subjects[0].Issuer != "https://issuer.test" {
			t.Errorf("subjects = %+v", v.subjects)
		}
	})
	t.Run("verification failure", func(t *testing.T) {
		write("    provider: cosign-keyless\n    certificateIdentityRegexp: .*\n")
		eng := verifyEngine(f, &recordingVerifier{err: errors.New("nope")}, nil)
		entry, _ := eng.Entry("bundle")
		wantErrContains(t, errOf(eng.VerifyUpstream(ctx, entry)), "artifact example.test")
	})

	t.Run("validate records verification failure without aborting", func(t *testing.T) {
		write("    provider: cosign-keyless\n    certificateIdentityRegexp: .*\n")
		f.pushImage(f.host+"/bundles/bundle:1.0.0", testNow.AddDate(0, -1, 0))
		eng := verifyEngine(f, &recordingVerifier{err: errors.New("nope")}, nil)
		entry, _ := eng.Entry("bundle")
		if _, err := eng.Upgrade(ctx, entry, ""); err != nil {
			t.Fatalf("Upgrade: %v", err)
		}
		res, err := eng.Validate(ctx, entry, []string{StageRegen, StageVerify, StageLint})
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if res.Err == "" || !res.Failed() || len(res.RegenDiffs) != 0 {
			t.Errorf("result = %+v", res)
		}
		if res.Lint != nil {
			t.Errorf("stages after a failed verify must not run: lint = %v", res.Lint)
		}
	})
}

func TestEngineValidateArtifactDrift(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushImage(f.host+"/bundles/bundle:1.0.0", testNow.AddDate(0, -1, 0))
	f.artifactManifest("example.test/bundles/bundle", "")
	eng := f.engine()
	entry, err := eng.Entry("bundle")
	if err != nil {
		t.Fatal(err)
	}

	// No lock committed yet: the fresh resolution differs from nothing.
	res, err := eng.Validate(ctx, entry, []string{StageRegen})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(res.RegenDiffs) != 1 || !strings.Contains(res.RegenDiffs[0], "lock.yaml differs") {
		t.Errorf("diffs = %v", res.RegenDiffs)
	}

	if _, err := eng.Upgrade(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	res, err = eng.Validate(ctx, entry, []string{StageRegen})
	if err != nil || len(res.RegenDiffs) != 0 {
		t.Errorf("converged artifact: diffs = %v, err = %v", res.RegenDiffs, err)
	}

	// Upstream moves the tag: the committed lock is now stale.
	f.pushImage(f.host+"/bundles/bundle:1.0.0", testNow)
	res, err = eng.Validate(ctx, entry, []string{StageRegen})
	if err != nil || len(res.RegenDiffs) != 1 {
		t.Errorf("moved tag: diffs = %v, err = %v", res.RegenDiffs, err)
	}

	// Upstream tag deleted entirely: regeneration itself fails.
	f.artifactManifest("example.test/bundles/absent", "")
	entry, err = eng.Entry("bundle")
	if err != nil {
		t.Fatal(err)
	}
	wantErrContains(t, errOf(eng.Validate(ctx, entry, []string{StageRegen})),
		"resolve digest for example.test/bundles/absent:1.0.0")
}

func TestEngineValidateChartVendorDrift(t *testing.T) {
	f, entry := scanFixture(t)
	ctx := context.Background()
	eng := f.eng

	f.write("charts/demo/vendor/demo/templates/extra.yaml", "kind: Hand-edited\n")
	f.write("charts/demo/rendered/manifests.yaml", "# tampered\n")
	res, err := eng.Validate(ctx, entry, []string{StageRegen})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	joined := strings.Join(res.RegenDiffs, "\n")
	if !strings.Contains(joined, "vendor: templates/extra.yaml: only in") ||
		!strings.Contains(joined, "rendered/manifests.yaml differs from a fresh render") {
		t.Errorf("diffs = %v", res.RegenDiffs)
	}

	// A vendor tree that no longer exists is an engine error, not a diff.
	if err := os.RemoveAll(filepath.Join(entry.Dir, "vendor")); err != nil {
		t.Fatal(err)
	}
	wantErrContains(t, errOf(eng.Validate(ctx, entry, []string{StageRegen})), "walk")
}

func TestRegenerateChartErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushImage(f.host+"/apps/app:1.0.0", testNow.AddDate(0, -1, 0))

	t.Run("missing values file", func(t *testing.T) {
		f.pushChart(f.host+"/upstream", "novals", "1.0.0", f.chartTgz("novals", "1.0.0", "1.0.0", appCanonical+":1.0.0"))
		f.chartManifest("novals", "")
		eng := f.engine()
		entry, _ := eng.Entry("novals")
		_, err := eng.Regenerate(ctx, entry, t.TempDir(), false)
		if err == nil || !strings.Contains(err.Error(), "values file values/discovery.yaml not found") {
			t.Errorf("Regenerate = %v", err)
		}
	})

	t.Run("chart pull fails", func(t *testing.T) {
		f.chartManifest("ghost", "")
		f.write("charts/ghost/values/discovery.yaml", "{}\n")
		eng := f.engine()
		entry, _ := eng.Entry("ghost")
		_, err := eng.Regenerate(ctx, entry, t.TempDir(), false)
		if err == nil || !strings.Contains(err.Error(), "ghost: pull chart ghost 1.0.0") {
			t.Errorf("Regenerate = %v", err)
		}
	})

	t.Run("archive without Chart.yaml", func(t *testing.T) {
		tgz := f.chartTgz("other", "1.0.0", "1.0.0", appCanonical+":1.0.0")
		f.pushChart(f.host+"/upstream", "mislabelled", "1.0.0", tgz)
		f.chartManifest("mislabelled", "")
		f.write("charts/mislabelled/values/discovery.yaml", "{}\n")
		eng := f.engine()
		entry, _ := eng.Entry("mislabelled")
		_, err := eng.Regenerate(ctx, entry, t.TempDir(), false)
		if err == nil || !strings.Contains(err.Error(), "extracted chart missing Chart.yaml") {
			t.Errorf("Regenerate = %v", err)
		}
	})

	t.Run("unresolvable image", func(t *testing.T) {
		f.pushChart(f.host+"/upstream", "noimg", "1.0.0",
			f.chartTgz("noimg", "1.0.0", "1.0.0", "example.test/apps/missing:9.9.9"))
		f.chartManifest("noimg", "")
		f.write("charts/noimg/values/discovery.yaml", "{}\n")
		eng := f.engine()
		entry, _ := eng.Entry("noimg")
		_, err := eng.Regenerate(ctx, entry, t.TempDir(), false)
		if err == nil || !strings.Contains(err.Error(), "resolve digest for example.test/apps/missing:9.9.9") {
			t.Errorf("Regenerate = %v", err)
		}
	})

	t.Run("broken sidecar", func(t *testing.T) {
		f.pushChart(f.host+"/upstream", "badside", "1.0.0", f.chartTgz("badside", "1.0.0", "1.0.0", appCanonical+":1.0.0"))
		f.chartManifest("badside", "")
		f.write("charts/badside/values/discovery.yaml", "{}\n")
		f.write("charts/badside/"+spec.SidecarFile, "extra: [oops\n")
		eng := f.engine()
		entry, _ := eng.Entry("badside")
		_, err := eng.Regenerate(ctx, entry, t.TempDir(), false)
		if err == nil || !strings.Contains(err.Error(), "parse") {
			t.Errorf("Regenerate = %v", err)
		}
	})
}

func TestWriteAndReadFactsErrors(t *testing.T) {
	dir := t.TempDir()
	chart := spec.Entry{Name: "c", Kind: spec.KindChart, Dir: filepath.Join(dir, "c"), Chart: &spec.ChartManifest{}}

	// Nothing committed: both come back nil, not an error.
	rendered, lock, err := ReadCommittedFacts(chart)
	if err != nil || rendered != nil || lock != nil {
		t.Errorf("ReadCommittedFacts(empty) = %q, %q, %v", rendered, lock, err)
	}

	// A file where the entry dir should be blocks every write.
	if err := os.WriteFile(chart.Dir, []byte("blocker"), 0o644); err != nil {
		t.Fatal(err)
	}
	facts := &Facts{Rendered: []byte("x"), ImagesLock: &spec.ImagesLock{}}
	if err := WriteFacts(chart, facts); err == nil {
		t.Error("WriteFacts through a file: want error")
	}
	art := spec.Entry{Name: "a", Kind: spec.KindArtifact, Dir: chart.Dir, Artifact: &spec.ArtifactManifest{}}
	wantErrContains(t, WriteFacts(art, &Facts{ArtifactLock: &spec.ArtifactLock{}}), "write ")
	if _, _, err := ReadCommittedFacts(chart); err == nil {
		t.Error("ReadCommittedFacts through a file: want error")
	}
	if _, _, err := ReadCommittedFacts(art); err == nil {
		t.Error("ReadCommittedFacts artifact through a file: want error")
	}
}

func TestSetVersionErrors(t *testing.T) {
	f := newFixture(t)
	f.artifactManifest("example.test/bundles/bundle", "")
	eng := f.engine()
	entry, err := eng.Entry("bundle")
	if err != nil {
		t.Fatal(err)
	}

	// The on-disk pin moved since the entry was loaded: the splice refuses.
	f.write("artifacts/bundle/manifest.yaml", strings.Replace(f.read("artifacts/bundle/manifest.yaml"),
		`version: "1.0.0"`, `version: "1.0.1"`, 1))
	wantErrContains(t, eng.SetVersion(entry, "1.2.0"), "bundle: splice version")

	gone := entry
	gone.Dir = filepath.Join(f.root, "artifacts", "gone")
	if err := eng.SetVersion(gone, "1.2.0"); err == nil {
		t.Error("SetVersion on a missing manifest: want error")
	}

	// Upgrade surfaces the same failure before touching anything else.
	if _, err := eng.Upgrade(context.Background(), entry, "1.2.0"); err == nil {
		t.Error("Upgrade with a stale entry: want error")
	}
}

func TestArtifactUpgradeSplicesVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushImage(f.host+"/bundles/bundle:1.0.0", testNow.AddDate(0, -1, 0))
	want := f.pushImage(f.host+"/bundles/bundle:1.1.0", testNow.AddDate(0, 0, -10))
	f.artifactManifest("example.test/bundles/bundle", "")
	var events []Event
	eng := verifyEngine(f, &recordingVerifier{}, &events)
	entry, err := eng.Entry("bundle")
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Upgrade(ctx, entry, "1.1.0")
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if res.From != "1.0.0" || res.To != "1.1.0" || res.Tracked != nil {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(f.read("artifacts/bundle/manifest.yaml"), `version: "1.1.0"`) {
		t.Error("artifact pin not spliced")
	}
	lock, err := spec.LoadArtifactLock(filepath.Join(f.root, "artifacts", "bundle", "lock.yaml"))
	if err != nil || lock.Artifact.Digest != want || lock.Artifact.Version != "1.1.0" {
		t.Errorf("lock = %+v, %v", lock, err)
	}
	pinned := false
	for _, ev := range events {
		if ev.Stage == "upgrade" && strings.Contains(ev.Message, "pinned 1.0.0 -> 1.1.0") {
			pinned = true
		}
	}
	if !pinned {
		t.Errorf("events missing the pin narration: %+v", events)
	}
}

func TestEngineDeriveDistribution(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The distribution artifact: a flattened filesystem carrying two
	// version trees of the product; the newest one wins.
	deploy := func(image string) []byte {
		return []byte("apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n" +
			"        - name: manager\n          image: " + image + "\n")
	}
	img, err := crane.Image(map[string][]byte{
		"flux/v2.5.0/source-controller.yaml": deploy("fluxcd/source-controller:v1.5.0"),
		"flux/v2.6.1/source-controller.yaml": deploy("fluxcd/source-controller:v1.6.1"),
		"flux/v2.6.1/helm-controller.yaml":   deploy("quay.io/fluxcd/helm-controller:v1.3.0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, f.host+"/dist/manifests:v9.9.9"); err != nil {
		t.Fatal(err)
	}

	f.write("charts/flux/manifest.yaml", fmt.Sprintf(`apiVersion: mirror.patchy.bitwisemedia.uk/v1alpha1
kind: Chart
name: flux
chart:
  repo: oci://%s/upstream
  name: flux
  version: "1.0.0"
images:
  distributionManifests:
    artifact: oci://example.test/dist/manifests:{appVersion}
    components: [source-controller, helm-controller]
`, f.host))
	var events []Event
	eng := verifyEngine(f, &recordingVerifier{}, &events)
	entry, err := eng.Entry("flux")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no vendored chart", func(t *testing.T) {
		wantErrContains(t, errOf(eng.DeriveDistribution(ctx, entry)), "no vendored appVersion")
	})

	f.write("charts/flux/vendor/flux/Chart.yaml", "apiVersion: v2\nname: flux\nversion: 1.0.0\nappVersion: v9.9.9\n")

	t.Run("derives the newest tree into the sidecar", func(t *testing.T) {
		extras, err := eng.DeriveDistribution(ctx, entry)
		if err != nil {
			t.Fatalf("DeriveDistribution: %v", err)
		}
		if len(extras) != 2 || extras[0].Image != "ghcr.io/fluxcd/source-controller:v1.6.1" ||
			extras[1].Image != "quay.io/fluxcd/helm-controller:v1.3.0" {
			t.Errorf("extras = %+v", extras)
		}
		sidecar, err := spec.LoadSidecar(entry.Dir)
		if err != nil || len(sidecar.Extra) != 2 || sidecar.Extra[0] != extras[0] {
			t.Errorf("sidecar = %+v, %v", sidecar, err)
		}
		if !strings.Contains(sidecar.Extra[0].Reason, "example.test/dist/manifests:v9.9.9") {
			t.Errorf("reason does not trace the artifact: %q", sidecar.Extra[0].Reason)
		}
	})

	t.Run("artifact missing a component", func(t *testing.T) {
		f.write("charts/flux/vendor/flux/Chart.yaml", "apiVersion: v2\nname: flux\nversion: 1.0.0\nappVersion: v9.9.9\n")
		bad := entry
		dm := *entry.Chart.Images.DistributionManifests
		dm.Components = []string{"kustomize-controller"}
		chart := *entry.Chart
		chart.Images.DistributionManifests = &dm
		bad.Chart = &chart
		wantErrContains(t, errOf(eng.DeriveDistribution(ctx, bad)), "flux:")
	})

	t.Run("artifact tag absent", func(t *testing.T) {
		f.write("charts/flux/vendor/flux/Chart.yaml", "apiVersion: v2\nname: flux\nversion: 1.0.0\nappVersion: v0.0.1\n")
		wantErrContains(t, errOf(eng.DeriveDistribution(ctx, entry)), "flux: pull")
	})

	t.Run("entries without the block are a no-op", func(t *testing.T) {
		f.artifactManifest("example.test/bundles/bundle", "")
		art, err := eng.Entry("bundle")
		if err != nil {
			t.Fatal(err)
		}
		extras, err := eng.DeriveDistribution(ctx, art)
		if err != nil || extras != nil {
			t.Errorf("DeriveDistribution(artifact) = %v, %v", extras, err)
		}
	})
}

func TestApplyTracksErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushChart(f.host+"/upstream", "demo", "1.0.0", f.chartTgz("demo", "1.0.0", "1.0.0", appCanonical+":1.0.0"))
	runner := f.host + "/apps/runner"
	f.pushImage(runner+":2.320.0", testNow.AddDate(0, 0, -40))

	manifest := func(discovery, track string) string {
		return fmt.Sprintf(`apiVersion: mirror.patchy.bitwisemedia.uk/v1alpha1
kind: Chart
name: demo
chart:
  repo: oci://%s/upstream
  name: demo
  version: "1.0.0"
%simages:
  track:
%s`, f.host, discovery, track)
	}
	vals := "discovery:\n  valuesFiles: [values/discovery.yaml]\n"
	tests := []struct {
		name, discovery, track, values, wantErr string
	}{
		{"missing image", vals, "    - valuesPath: .image\n", "image: x\n", "images.track[0] missing image"},
		{"missing path", vals, "    - image: " + runner + "\n", "image: x\n", "images.track[0] missing valuesPath"},
		{"no values file anywhere", "", "    - image: " + runner + "\n      valuesPath: .image\n", "", "has no valuesFile"},
		{"values file absent", vals,
			"    - image: " + runner + "\n      valuesPath: .image\n      valuesFile: values/nope.yaml\n",
			"image: x\n", "read values file values/nope.yaml"},
		{"path does not resolve", vals, "    - image: " + runner + "\n      valuesPath: .other\n", "image: x\n",
			`".other" does not resolve`},
		{"unparseable current ref", vals,
			"    - image: " + runner + "\n      valuesPath: .image\n", "image: \"ghcr.io/app:\"\n",
			`current pin "ghcr.io/app:"`},
		{"non-semver pin without constraint", vals, "    - image: " + runner + "\n      valuesPath: .image\n",
			"image: latest\n", "derive constraint"},
		{"repository absent", vals,
			"    - image: example.test/apps/absent\n      valuesPath: .image\n      versionConstraint: \">=1\"\n",
			"image: example.test/apps/absent:1.0.0\n", "demo:"},
		{"nothing satisfies", vals,
			"    - image: " + runner + "\n      valuesPath: .image\n      versionConstraint: \">=9.0.0\"\n",
			"image: " + runner + ":2.320.0\n", runner},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.RemoveAll(filepath.Join(f.root, "charts", "demo"))
			f.write("charts/demo/manifest.yaml", manifest(tc.discovery, tc.track))
			if tc.values != "" {
				f.write("charts/demo/values/discovery.yaml", tc.values)
			}
			eng := f.engine()
			entry, err := eng.Entry("demo")
			if err != nil {
				t.Fatal(err)
			}
			_, err = eng.ApplyTracks(ctx, entry)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ApplyTracks = %v, want error containing %q", err, tc.wantErr)
			}
			// checkTracks errors must also fail the read-only plan.
			if _, err := eng.CheckUpdates(ctx, []spec.Entry{entry}); err == nil {
				t.Error("CheckUpdates should surface the same track error")
			}
		})
	}
}

func TestApplyTracksAlreadyPinnedAndZeroCooldown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushChart(f.host+"/upstream", "demo", "1.0.0", f.chartTgz("demo", "1.0.0", "1.0.0", appCanonical+":1.0.0"))
	runner := f.host + "/apps/runner"
	f.pushImage(runner+":2.320.0", testNow.AddDate(0, 0, -40))
	f.pushImage(runner+":2.321.0", testNow.AddDate(0, 0, -1))

	write := func(cooldown string) {
		f.write("charts/demo/manifest.yaml", fmt.Sprintf(`apiVersion: mirror.patchy.bitwisemedia.uk/v1alpha1
kind: Chart
name: demo
chart:
  repo: oci://%s/upstream
  name: demo
  version: "1.0.0"
images:
  track:
    - image: %s
      valuesFile: values/pins.yaml
      valuesPath: .runner
      versionConstraint: ">=2.0.0 <3.0.0"
%s`, f.host, runner, cooldown))
	}
	f.write("charts/demo/values/pins.yaml", "runner: "+runner+":2.320.0\n")

	// Global 3-day cooldown: 2.321.0 is too fresh; the pin holds.
	write("")
	eng := f.engine()
	entry, _ := eng.Entry("demo")
	plans, err := eng.ApplyTracks(ctx, entry)
	if err != nil || len(plans) != 1 || plans[0].Changed() || plans[0].ValuesFile != "values/pins.yaml" {
		t.Fatalf("plans = %+v, err = %v", plans, err)
	}
	if got := f.read("charts/demo/values/pins.yaml"); got != "runner: "+runner+":2.320.0\n" {
		t.Errorf("unchanged pick rewrote the file: %q", got)
	}

	// Per-rule zero cooldown tracks head.
	write("      cooldownDays: 0\n")
	eng = f.engine()
	entry, _ = eng.Entry("demo")
	plans, err = eng.ApplyTracks(ctx, entry)
	if err != nil || len(plans) != 1 || plans[0].Selected != "2.321.0" {
		t.Fatalf("plans = %+v, err = %v", plans, err)
	}
	if got := f.read("charts/demo/values/pins.yaml"); got != "runner: "+runner+":2.321.0\n" {
		t.Errorf("values = %q", got)
	}
}

// errOf drops a call's value, keeping its error, so a (T, error) call can
// feed wantErrContains directly.
func errOf[T any](_ T, err error) error { return err }
