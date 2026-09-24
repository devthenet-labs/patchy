// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// startEnv boots an envtest API server with the generated CRDs installed and
// returns a client against it. Skipped without KUBEBUILDER_ASSETS (mise x --
// setup-envtest use 1.36.x -p path).
func startEnv(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest schema smoke")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../deploy/kustomize/base/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := patchyv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return c
}

func TestSchemaValidation(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()

	t.Run("integration one-of rejects missing provider block", func(t *testing.T) {
		bad := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "no-block", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider:  patchyv1.IntegrationProviderGitHub,
				SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
			},
		}
		if err := c.Create(ctx, bad); err == nil {
			t.Error("Create(integration without github block) = nil, want CEL rejection")
		}
	})

	t.Run("integration with matching block is accepted", func(t *testing.T) {
		good := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider:  patchyv1.IntegrationProviderGitHub,
				SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
				GitHub: &patchyv1.GitHubIntegration{
					Issues:             &patchyv1.GitHubIssues{Enabled: true},
					CodeScanningAlerts: &patchyv1.GitHubCodeScanningAlerts{Enabled: true},
				},
			},
		}
		if err := c.Create(ctx, good); err != nil {
			t.Errorf("Create(valid integration) = %v, want nil", err)
		}
	})

	t.Run("integration one-of rejects a mismatched provider block", func(t *testing.T) {
		bad := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "gcp-with-github-block", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider: patchyv1.IntegrationProviderGoogleCloud,
				GitHub:   &patchyv1.GitHubIntegration{},
			},
		}
		if err := c.Create(ctx, bad); err == nil {
			t.Error("Create(google-cloud integration with a github block) = nil, want CEL rejection")
		}
	})

	t.Run("google-cloud integration needs no secretRef", func(t *testing.T) {
		good := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "gcp", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider: patchyv1.IntegrationProviderGoogleCloud,
				GoogleCloud: &patchyv1.GoogleCloudIntegration{
					SecurityCommandCenter: &patchyv1.GoogleCloudSCC{
						Enabled:        true,
						Audience:       "https://patchy.example/google-cloud/webhooks",
						ServiceAccount: "scc-push@x.iam.gserviceaccount.com",
					},
				},
			},
		}
		if err := c.Create(ctx, good); err != nil {
			t.Errorf("Create(credential-less google-cloud integration) = %v, want nil", err)
		}
	})

	t.Run("github integration still requires a secretRef", func(t *testing.T) {
		bad := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "gh-no-secret", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider: patchyv1.IntegrationProviderGitHub,
				GitHub:   &patchyv1.GitHubIntegration{},
			},
		}
		if err := c.Create(ctx, bad); err == nil {
			t.Error("Create(github integration without secretRef) = nil, want CEL rejection")
		}
	})

	t.Run("wiz integration requires its provider block and secretRef", func(t *testing.T) {
		noBlock := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "wiz-no-block", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider:  patchyv1.IntegrationProviderWiz,
				SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
			},
		}
		if err := c.Create(ctx, noBlock); err == nil {
			t.Error("Create(wiz integration without wiz block) = nil, want CEL rejection")
		}
		noSecret := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "wiz-no-secret", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider: patchyv1.IntegrationProviderWiz,
				Wiz:      &patchyv1.WizIntegration{Issues: &patchyv1.WizIssues{Enabled: true}},
			},
		}
		if err := c.Create(ctx, noSecret); err == nil {
			t.Error("Create(wiz integration without secretRef) = nil, want CEL rejection")
		}
		good := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "wiz", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider:  patchyv1.IntegrationProviderWiz,
				SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
				Wiz: &patchyv1.WizIntegration{
					Issues: &patchyv1.WizIssues{Enabled: true},
					Defend: &patchyv1.WizDefend{Enabled: true},
				},
			},
		}
		if err := c.Create(ctx, good); err != nil {
			t.Errorf("Create(valid wiz integration) = %v, want nil", err)
		}
	})

	t.Run("generic integration requires its provider block, secretRef, and an unreserved name", func(t *testing.T) {
		testGenericIntegrationSchema(ctx, t, c)
	})

	t.Run("asset-inventory-only google-cloud integration is accepted", func(t *testing.T) {
		good := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "gcp-cai-only", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider: patchyv1.IntegrationProviderGoogleCloud,
				GoogleCloud: &patchyv1.GoogleCloudIntegration{
					CloudAssetInventory: &patchyv1.GoogleCloudAssetInventory{
						Enabled: true,
						Scope:   "organizations/123456789012",
					},
				},
			},
		}
		if err := c.Create(ctx, good); err != nil {
			t.Errorf("Create(asset-inventory-only integration) = %v, want nil", err)
		}
		badScope := &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: "gcp-bad-scope", Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider: patchyv1.IntegrationProviderGoogleCloud,
				GoogleCloud: &patchyv1.GoogleCloudIntegration{
					CloudAssetInventory: &patchyv1.GoogleCloudAssetInventory{
						Enabled: true,
						Scope:   "org-123",
					},
				},
			},
		}
		if err := c.Create(ctx, badScope); err == nil {
			t.Error("Create(asset inventory with malformed scope) = nil, want pattern rejection")
		}
	})

	t.Run("aws integration enforces exactly one inventory backend", func(t *testing.T) {
		testAWSIntegrationSchema(ctx, t, c)
	})

	t.Run("azure integration requires its provider block and no credential", func(t *testing.T) {
		testAzureIntegrationSchema(ctx, t, c)
	})

	t.Run("proxy url pattern on forge and integration", func(t *testing.T) {
		testProxySchema(ctx, t, c)
	})

	t.Run("finding rejects an unknown phase and accepts a legal one", func(t *testing.T) {
		f := &patchyv1.Finding{
			ObjectMeta: metav1.ObjectMeta{Name: "finding-abc123-1", Namespace: "default"},
			Spec: patchyv1.FindingSpec{
				IntegrationRef: patchyv1.LocalObjectReference{Name: "gh"},
				Source:         "github-code-scanning",
				Advisories:     []string{"CVE-2026-0001"},
				Severity:       patchyv1.LevelHigh,
			},
		}
		if err := c.Create(ctx, f); err != nil {
			t.Fatalf("Create(finding) = %v, want nil", err)
		}
		f.Status.Phase = "Bogus"
		if err := c.Status().Update(ctx, f); err == nil {
			t.Error("Status().Update(phase=Bogus) = nil, want enum rejection")
		}
		f.Status.Phase = patchyv1.PhaseOpened
		if err := c.Status().Update(ctx, f); err != nil {
			t.Errorf("Status().Update(phase=Opened) = %v, want nil", err)
		}
	})

	t.Run("investigation spec is immutable", func(t *testing.T) {
		inv := &patchyv1.Investigation{
			ObjectMeta: metav1.ObjectMeta{Name: "finding-abc123-1-inv-1", Namespace: "default"},
			Spec: patchyv1.InvestigationSpec{
				FindingRef: patchyv1.ObjectReference{Name: "finding-abc123-1"},
				Attempt:    1,
			},
		}
		if err := c.Create(ctx, inv); err != nil {
			t.Fatalf("Create(investigation) = %v, want nil", err)
		}
		inv.Spec.Attempt = 2
		if err := c.Update(ctx, inv); err == nil {
			t.Error("Update(investigation spec) = nil, want immutability rejection")
		}
	})

	t.Run("rollup scope is immutable", func(t *testing.T) {
		fr := &patchyv1.FindingRollup{
			ObjectMeta: metav1.ObjectMeta{Name: "total", Namespace: "default"},
			Spec: patchyv1.FindingRollupSpec{
				Scope: patchyv1.RollupScope{Type: patchyv1.ScopeTotal},
			},
		}
		if err := c.Create(ctx, fr); err != nil {
			t.Fatalf("Create(rollup) = %v, want nil", err)
		}
		fr.Spec.Scope.Type = patchyv1.ScopeRepository
		if err := c.Update(ctx, fr); err == nil {
			t.Error("Update(rollup scope) = nil, want immutability rejection")
		}
	})

	t.Run("usage cost pattern rejects non-decimal strings", func(t *testing.T) {
		inv := &patchyv1.Investigation{}
		if err := c.Get(ctx, client.ObjectKey{Name: "finding-abc123-1-inv-1", Namespace: "default"}, inv); err != nil {
			t.Fatalf("Get(investigation) = %v", err)
		}
		inv.Status.Confidence = "1.5"
		if err := c.Status().Update(ctx, inv); err == nil {
			t.Error("Status().Update(confidence=1.5) = nil, want pattern rejection")
		}
	})

	t.Run("repository status keeps the pinned runner image", func(t *testing.T) {
		testRepositoryRunnerImageSchema(ctx, t, c)
	})

	t.Run("run status runner image is enum-checked and round-trips on every carrier", func(t *testing.T) {
		testRunStatusRunnerImageSchema(ctx, t, c)
	})

	t.Run("run spec previous attempt is bounded and round-trips on both kinds", func(t *testing.T) {
		testPreviousAttemptSchema(ctx, t, c)
	})

	t.Run("evaluation spec is immutable and bounded", func(t *testing.T) {
		testEvaluationSchema(ctx, t, c)
	})

	t.Run("evaluation unit spec is immutable and its failure reason is an enum", func(t *testing.T) {
		testEvaluationUnitSchema(ctx, t, c)
	})

	t.Run("project is defaulted, bounded, and its labels differ", func(t *testing.T) {
		testProjectSchema(ctx, t, c)
	})

	t.Run("intent spec is immutable except suspend and its status is bounded", func(t *testing.T) {
		testIntentSchema(ctx, t, c)
	})

	t.Run("intent run spec is immutable and holds its stage invariants", func(t *testing.T) {
		testIntentRunSchema(ctx, t, c)
	})
}

// testRepositoryRunnerImageSchema writes a fully populated runner-image
// record through the status subresource and reads it back, so a field the
// generated CRD schema does not know (and the API server would silently
// prune) fails here rather than as a mysteriously empty status in a cluster.
func testRepositoryRunnerImageSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	repo := &patchyv1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "finding-abc123-1-src", Namespace: "default"},
		Spec:       patchyv1.RepositorySpec{URL: "https://github.com/acme/shop"},
	}
	if err := c.Create(ctx, repo); err != nil {
		t.Fatalf("Create(repository) = %v, want nil", err)
	}
	resolvedAt := metav1.NewTime(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	want := &patchyv1.RunnerImage{
		Declared:   "ghcr.io/acme/go-agent-env:1.26",
		Manifest:   ".patchy/agent.yaml",
		Image:      "ghcr.io/acme/go-agent-env@sha256:" + strings.Repeat("a", 64),
		SearchPath: "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
		Verified:   true,
		ResolvedAt: &resolvedAt,
	}
	repo.Status.ResolvedSHA = strings.Repeat("b", 40)
	repo.Status.RunnerImage = want.DeepCopy()
	if err := c.Status().Update(ctx, repo); err != nil {
		t.Fatalf("Status().Update(runnerImage) = %v, want nil", err)
	}
	got := &patchyv1.Repository{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(repo), got); err != nil {
		t.Fatalf("Get(repository) = %v", err)
	}
	if got.Status.RunnerImage == nil {
		t.Fatal("status.runnerImage = nil after update, want the record (schema pruned it?)")
	}
	// The API server round-trips timestamps in local time; compare the
	// instant, then the rest of the record verbatim.
	if !got.Status.RunnerImage.ResolvedAt.Equal(&resolvedAt) {
		t.Errorf("status.runnerImage.resolvedAt = %v, want %v", got.Status.RunnerImage.ResolvedAt, resolvedAt)
	}
	got.Status.RunnerImage.ResolvedAt, want.ResolvedAt = nil, nil
	if !reflect.DeepEqual(got.Status.RunnerImage, want) {
		t.Errorf("status.runnerImage = %+v, want %+v", got.Status.RunnerImage, want)
	}

	rejected := &patchyv1.RunnerImage{
		Declared: "docker.io/library/golang:1.26",
		Manifest: ".devcontainer/devcontainer.json",
		Rejected: "NotAllowlisted",
		Message:  "docker.io/library/ is not an allowlisted registry path",
	}
	got.Status.RunnerImage = rejected.DeepCopy()
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatalf("Status().Update(rejected runnerImage) = %v, want nil", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(repo), got); err != nil {
		t.Fatalf("Get(repository) = %v", err)
	}
	if !reflect.DeepEqual(got.Status.RunnerImage, rejected) {
		t.Errorf("status.runnerImage (rejected) = %+v, want %+v", got.Status.RunnerImage, rejected)
	}

	// Message is mirrored from free-form resolver/policy output; the schema
	// caps it so a hostile or runaway explanation cannot bloat the object.
	// 4096 bytes is the ceiling, one more is rejected.
	got.Status.RunnerImage.Message = strings.Repeat("m", 4096)
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatalf("Status().Update(runnerImage.message=4096 bytes) = %v, want nil", err)
	}
	got.Status.RunnerImage.Message = strings.Repeat("m", 4097)
	if err := c.Status().Update(ctx, got); err == nil {
		t.Error("Status().Update(runnerImage.message=4097 bytes) = nil, want maxLength rejection")
	}
}

// testRunStatusRunnerImageSchema exercises the RunnerImageRef schema on
// every status that carries one: the Investigation and Remediation run
// statuses and the Finding's investigation mirror. Each record is written
// through the status subresource and the read-back is compared against the
// value that was written (never against the update's own response), so a
// field the generated schema prunes fails here.
func testRunStatusRunnerImageSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	want := func() *patchyv1.RunnerImageRef {
		return &patchyv1.RunnerImageRef{
			Image:    "ghcr.io/acme/go-agent-env@sha256:" + strings.Repeat("a", 64),
			Source:   patchyv1.RunnerImageSourceRepository,
			Manifest: ".devcontainer/devcontainer.json",
		}
	}

	t.Run("investigation", func(t *testing.T) {
		inv := &patchyv1.Investigation{}
		if err := c.Get(ctx, client.ObjectKey{Name: "finding-abc123-1-inv-1", Namespace: "default"}, inv); err != nil {
			t.Fatalf("Get(investigation) = %v", err)
		}
		inv.Status.RunnerImage = want()
		inv.Status.RunnerImage.Source = "bogus"
		if err := c.Status().Update(ctx, inv); err == nil {
			t.Error("Status().Update(runnerImage.source=bogus) = nil, want enum rejection")
		}
		inv.Status.RunnerImage = want()
		if err := c.Status().Update(ctx, inv); err != nil {
			t.Fatalf("Status().Update(runnerImage.source=repository) = %v, want nil", err)
		}
		got := &patchyv1.Investigation{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(inv), got); err != nil {
			t.Fatalf("Get(investigation) = %v", err)
		}
		if !reflect.DeepEqual(got.Status.RunnerImage, want()) {
			t.Errorf("status.runnerImage = %+v, want %+v", got.Status.RunnerImage, want())
		}
	})

	t.Run("remediation", func(t *testing.T) {
		rem := &patchyv1.Remediation{
			ObjectMeta: metav1.ObjectMeta{Name: "finding-abc123-1-rem-1", Namespace: "default"},
			Spec: patchyv1.RemediationSpec{
				FindingRef:       patchyv1.ObjectReference{Name: "finding-abc123-1"},
				InvestigationRef: patchyv1.ObjectReference{Name: "finding-abc123-1-inv-1"},
				RepositoryRef:    patchyv1.LocalObjectReference{Name: "finding-abc123-1-src"},
				Attempt:          1,
			},
		}
		if err := c.Create(ctx, rem); err != nil {
			t.Fatalf("Create(remediation) = %v, want nil", err)
		}
		rem.Status.RunnerImage = want()
		rem.Status.RunnerImage.Source = "bogus"
		if err := c.Status().Update(ctx, rem); err == nil {
			t.Error("Status().Update(runnerImage.source=bogus) = nil, want enum rejection")
		}
		rem.Status.RunnerImage = want()
		if err := c.Status().Update(ctx, rem); err != nil {
			t.Fatalf("Status().Update(runnerImage) = %v, want nil", err)
		}
		got := &patchyv1.Remediation{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(rem), got); err != nil {
			t.Fatalf("Get(remediation) = %v", err)
		}
		if !reflect.DeepEqual(got.Status.RunnerImage, want()) {
			t.Errorf("status.runnerImage = %+v, want %+v", got.Status.RunnerImage, want())
		}
	})

	t.Run("finding investigation mirror", func(t *testing.T) {
		f := &patchyv1.Finding{}
		if err := c.Get(ctx, client.ObjectKey{Name: "finding-abc123-1", Namespace: "default"}, f); err != nil {
			t.Fatalf("Get(finding) = %v", err)
		}
		f.Status.Investigation = &patchyv1.InvestigationSummary{
			Name:        "finding-abc123-1-inv-1",
			Attempt:     1,
			RunnerImage: want(),
		}
		f.Status.Investigation.RunnerImage.Source = "bogus"
		if err := c.Status().Update(ctx, f); err == nil {
			t.Error("Status().Update(investigation.runnerImage.source=bogus) = nil, want enum rejection")
		}
		f.Status.Investigation.RunnerImage = want()
		if err := c.Status().Update(ctx, f); err != nil {
			t.Fatalf("Status().Update(investigation.runnerImage) = %v, want nil", err)
		}
		got := &patchyv1.Finding{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(f), got); err != nil {
			t.Fatalf("Get(finding) = %v", err)
		}
		if got.Status.Investigation == nil {
			t.Fatal("status.investigation = nil after update, want the summary")
		}
		if !reflect.DeepEqual(got.Status.Investigation.RunnerImage, want()) {
			t.Errorf("status.investigation.runnerImage = %+v, want %+v", got.Status.Investigation.RunnerImage, want())
		}
	})
}

// evalUnitPlan is a minimal valid UnitPlan for schema tests.
func evalUnitPlan() patchyv1.UnitPlan {
	return patchyv1.UnitPlan{
		Skill:     "workflow-commit",
		Tier:      2,
		Model:     "anthropic/claude-sonnet-5",
		Harnesses: []patchyv1.HarnessOption{{Harness: "claude", ModelID: "claude-sonnet-5"}},
		Workspace: patchyv1.WorkspaceRef{
			Digest:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			SizeBytes: 1024,
		},
	}
}

// testEvaluationSchema exercises the Evaluation CEL rules: spec immutability,
// the units bounds, the workspace digest pattern, and the tier range.
// testPreviousAttemptSchema pins spec.previousAttempt on both run kinds: at
// its bounds it is accepted and round-trips; one byte of outcome or detail
// past them, or a zero attempt, is rejected.
func testPreviousAttemptSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	atBounds := func() *patchyv1.PreviousAttempt {
		return &patchyv1.PreviousAttempt{
			Name: "finding-prev-1-rem-1", Attempt: 1,
			Outcome: strings.Repeat("o", 64), Detail: strings.Repeat("d", 4096),
		}
	}
	objects := func(n int, prev *patchyv1.PreviousAttempt) []client.Object {
		meta := func(kind string) metav1.ObjectMeta {
			return metav1.ObjectMeta{Name: fmt.Sprintf("finding-prev-%d-%s-2", n, kind), Namespace: "default"}
		}
		return []client.Object{
			&patchyv1.Investigation{ObjectMeta: meta("inv"), Spec: patchyv1.InvestigationSpec{
				FindingRef: patchyv1.ObjectReference{Name: "finding-prev"}, Attempt: 2, PreviousAttempt: prev,
			}},
			&patchyv1.Remediation{ObjectMeta: meta("rem"), Spec: patchyv1.RemediationSpec{
				FindingRef:       patchyv1.ObjectReference{Name: "finding-prev"},
				InvestigationRef: patchyv1.ObjectReference{Name: "finding-prev-inv-1"},
				RepositoryRef:    patchyv1.LocalObjectReference{Name: "finding-prev-src"},
				Attempt:          2, PreviousAttempt: prev,
			}},
		}
	}
	for _, obj := range objects(0, atBounds()) {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("Create(%T with previousAttempt at its bounds) = %v, want nil", obj, err)
		}
	}
	gotInv := &patchyv1.Investigation{}
	if err := c.Get(ctx, client.ObjectKey{Name: "finding-prev-0-inv-2", Namespace: "default"}, gotInv); err != nil {
		t.Fatalf("Get(investigation) = %v", err)
	}
	gotRem := &patchyv1.Remediation{}
	if err := c.Get(ctx, client.ObjectKey{Name: "finding-prev-0-rem-2", Namespace: "default"}, gotRem); err != nil {
		t.Fatalf("Get(remediation) = %v", err)
	}
	for _, got := range []*patchyv1.PreviousAttempt{gotInv.Spec.PreviousAttempt, gotRem.Spec.PreviousAttempt} {
		if !reflect.DeepEqual(got, atBounds()) {
			t.Errorf("spec.previousAttempt round-tripped as %.80v, want it verbatim", got)
		}
	}

	for i, mutate := range []func(*patchyv1.PreviousAttempt){
		func(p *patchyv1.PreviousAttempt) { p.Outcome += "o" },
		func(p *patchyv1.PreviousAttempt) { p.Detail += "d" },
		func(p *patchyv1.PreviousAttempt) { p.Attempt = 0 },
	} {
		prev := atBounds()
		mutate(prev)
		for _, obj := range objects(i+1, prev) {
			if err := c.Create(ctx, obj); err == nil {
				t.Errorf("Create(%T with out-of-bounds previousAttempt #%d) = nil, want rejection", obj, i+1)
			}
		}
	}
}

func testEvaluationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	eval := func(name string, units []patchyv1.UnitPlan) *patchyv1.Evaluation {
		return &patchyv1.Evaluation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       patchyv1.EvaluationSpec{Submitter: "dev@example.com", Units: units},
		}
	}
	good := eval("eval-ok", []patchyv1.UnitPlan{evalUnitPlan()})
	if err := c.Create(ctx, good); err != nil {
		t.Fatalf("Create(valid evaluation) = %v, want nil", err)
	}
	good.Spec.Units[0].Skill = "another-skill"
	if err := c.Update(ctx, good); err == nil {
		t.Error("Update(evaluation spec) = nil, want immutability rejection")
	}
	if err := c.Create(ctx, eval("eval-no-units", nil)); err == nil {
		t.Error("Create(evaluation without units) = nil, want MinItems rejection")
	}
	badDigest := evalUnitPlan()
	badDigest.Workspace.Digest = "not-a-digest"
	if err := c.Create(ctx, eval("eval-bad-digest", []patchyv1.UnitPlan{badDigest})); err == nil {
		t.Error("Create(evaluation with malformed digest) = nil, want pattern rejection")
	}
	badTier := evalUnitPlan()
	badTier.Tier = 3
	if err := c.Create(ctx, eval("eval-bad-tier", []patchyv1.UnitPlan{badTier})); err == nil {
		t.Error("Create(evaluation with tier 3) = nil, want range rejection")
	}
	noHarness := evalUnitPlan()
	noHarness.Harnesses = nil
	if err := c.Create(ctx, eval("eval-no-harness", []patchyv1.UnitPlan{noHarness})); err == nil {
		t.Error("Create(evaluation without harness options) = nil, want MinItems rejection")
	}
}

// testEvaluationUnitSchema exercises the EvaluationUnit CEL rules: spec
// immutability and the failure-reason enum.
func testEvaluationUnitSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	unit := &patchyv1.EvaluationUnit{
		ObjectMeta: metav1.ObjectMeta{Name: "eval-ok-u000", Namespace: "default"},
		Spec: patchyv1.EvaluationUnitSpec{
			EvaluationRef: patchyv1.ObjectReference{Name: "eval-ok"},
			Index:         0,
			Unit:          evalUnitPlan(),
		},
	}
	if err := c.Create(ctx, unit); err != nil {
		t.Fatalf("Create(evaluation unit) = %v, want nil", err)
	}
	unit.Spec.Index = 1
	if err := c.Update(ctx, unit); err == nil {
		t.Error("Update(evaluation unit spec) = nil, want immutability rejection")
	}
	fresh := &patchyv1.EvaluationUnit{}
	if err := c.Get(ctx, client.ObjectKey{Name: "eval-ok-u000", Namespace: "default"}, fresh); err != nil {
		t.Fatalf("Get(evaluation unit) = %v", err)
	}
	fresh.Status.Phase = patchyv1.RunFailed
	fresh.Status.Reason = "Bogus"
	if err := c.Status().Update(ctx, fresh); err == nil {
		t.Error("Status().Update(reason=Bogus) = nil, want enum rejection")
	}
	fresh.Status.Reason = patchyv1.UnitWorkspaceLost
	if err := c.Status().Update(ctx, fresh); err != nil {
		t.Errorf("Status().Update(reason=WorkspaceLost) = %v, want nil", err)
	}
}

// testGenericIntegrationSchema exercises the generic provider's CEL rules:
// the provider/block biconditional, the mandatory secretRef, the URL pattern,
// and the reserved-source-id name guard.
func testGenericIntegrationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	generic := func(name string, spec patchyv1.IntegrationSpec) *patchyv1.Integration {
		return &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spec,
		}
	}
	block := &patchyv1.GenericIntegration{
		Source: &patchyv1.GenericSource{
			Enabled:  true,
			Resolver: &patchyv1.GenericResolver{Enabled: true, URL: "https://warehouse.internal/resolve"},
		},
		Enhance: &patchyv1.GenericEnhancer{Enabled: true, URL: "https://warehouse.internal/enhance"},
	}
	if err := c.Create(ctx, generic("warehouse", patchyv1.IntegrationSpec{
		Provider:  patchyv1.IntegrationProviderGeneric,
		SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
		Generic:   block,
	})); err != nil {
		t.Errorf("Create(valid generic integration) = %v, want nil", err)
	}
	if err := c.Create(ctx, generic("generic-no-block", patchyv1.IntegrationSpec{
		Provider:  patchyv1.IntegrationProviderGeneric,
		SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
	})); err == nil {
		t.Error("Create(generic integration without generic block) = nil, want CEL rejection")
	}
	if err := c.Create(ctx, generic("generic-no-secret", patchyv1.IntegrationSpec{
		Provider: patchyv1.IntegrationProviderGeneric,
		Generic:  block,
	})); err == nil {
		t.Error("Create(generic integration without secretRef) = nil, want CEL rejection")
	}
	if err := c.Create(ctx, generic("generic-bad-url", patchyv1.IntegrationSpec{
		Provider:  patchyv1.IntegrationProviderGeneric,
		SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
		Generic: &patchyv1.GenericIntegration{
			Enhance: &patchyv1.GenericEnhancer{Enabled: true, URL: "warehouse.internal/enhance"},
		},
	})); err == nil {
		t.Error("Create(generic integration with schemeless URL) = nil, want pattern rejection")
	}
	if err := c.Create(ctx, generic("ghas", patchyv1.IntegrationSpec{
		Provider:  patchyv1.IntegrationProviderGeneric,
		SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
		Generic:   block,
	})); err == nil {
		t.Error("Create(generic integration named ghas) = nil, want reserved-name rejection")
	}
}

// testProxySchema exercises the ProxyConfig URL pattern on both kinds that
// carry it: a plain http/https proxy is accepted; embedded userinfo and
// non-HTTP schemes are rejected at admission.
func testProxySchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	forge := func(name, proxyURL string) *patchyv1.Forge {
		return &patchyv1.Forge{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: patchyv1.ForgeSpec{
				Provider:  patchyv1.ForgeProviderGitHub,
				Proxy:     &patchyv1.ProxyConfig{URL: proxyURL},
				SecretRef: patchyv1.LocalSecretReference{Name: "s"},
			},
		}
	}
	integration := func(name, proxyURL string) *patchyv1.Integration {
		return &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider:  patchyv1.IntegrationProviderGitHub,
				SecretRef: &patchyv1.LocalSecretReference{Name: "s"},
				GitHub: &patchyv1.GitHubIntegration{
					Proxy:  &patchyv1.ProxyConfig{URL: proxyURL},
					Issues: &patchyv1.GitHubIssues{Enabled: true},
				},
			},
		}
	}
	tests := []struct {
		name     string
		proxyURL string
		wantErr  bool
	}{
		{"valid http proxy", "http://proxy.corp.example:3128", false},
		{"valid https proxy", "https://proxy.corp.example", false},
		{"userinfo rejected", "http://user:pass@proxy.corp.example:3128", true},
		{"socks5 rejected", "socks5://proxy.corp.example:1080", true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.Create(ctx, forge(fmt.Sprintf("proxy-forge-%d", i), tt.proxyURL))
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(forge, proxy=%q) = %v, wantErr %v", tt.proxyURL, err, tt.wantErr)
			}
			err = c.Create(ctx, integration(fmt.Sprintf("proxy-integ-%d", i), tt.proxyURL))
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(integration, proxy=%q) = %v, wantErr %v", tt.proxyURL, err, tt.wantErr)
			}
		})
	}
}

// testAWSIntegrationSchema exercises the aws provider's CEL rules: the
// backend one-of, the view-ARN pattern, and the credential-less posture (no
// secretRef anywhere below).
func testAWSIntegrationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	aws := func(name string, rt *patchyv1.AWSResourceTags) *patchyv1.Integration {
		return &patchyv1.Integration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: patchyv1.IntegrationSpec{
				Provider: patchyv1.IntegrationProviderAWS,
				AWS:      &patchyv1.AWSIntegration{ResourceTags: rt},
			},
		}
	}
	aggregator := &patchyv1.AWSConfigAggregator{Name: "org", Region: "eu-west-2"}
	explorer := &patchyv1.AWSResourceExplorer{
		ViewARN: "arn:aws:resource-explorer-2:eu-west-2:123456789012:view/org/abc",
	}
	if err := c.Create(ctx, aws("aws-aggregator", &patchyv1.AWSResourceTags{
		Enabled: true, ConfigAggregator: aggregator,
	})); err != nil {
		t.Errorf("Create(aws integration, aggregator backend) = %v, want nil", err)
	}
	if err := c.Create(ctx, aws("aws-explorer", &patchyv1.AWSResourceTags{
		Enabled: true, ResourceExplorer: explorer,
	})); err != nil {
		t.Errorf("Create(aws integration, explorer backend) = %v, want nil", err)
	}
	if err := c.Create(ctx, aws("aws-both", &patchyv1.AWSResourceTags{
		Enabled: true, ConfigAggregator: aggregator, ResourceExplorer: explorer,
	})); err == nil {
		t.Error("Create(aws integration with both backends) = nil, want CEL rejection")
	}
	if err := c.Create(ctx, aws("aws-neither", &patchyv1.AWSResourceTags{
		Enabled: true,
	})); err == nil {
		t.Error("Create(aws integration with no backend) = nil, want CEL rejection")
	}
	if err := c.Create(ctx, aws("aws-bad-view", &patchyv1.AWSResourceTags{
		Enabled:          true,
		ResourceExplorer: &patchyv1.AWSResourceExplorer{ViewARN: "arn:aws:s3:::not-a-view"},
	})); err == nil {
		t.Error("Create(aws integration with malformed view ARN) = nil, want pattern rejection")
	}
}

// testAzureIntegrationSchema exercises the azure provider's CEL rules: the
// provider/block biconditional, and the credential-less posture (no secretRef
// anywhere below — Resource Graph is one service, so there is no backend
// one-of either).
func testAzureIntegrationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	good := &patchyv1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: "default"},
		Spec: patchyv1.IntegrationSpec{
			Provider: patchyv1.IntegrationProviderAzure,
			Azure: &patchyv1.AzureIntegration{
				ResourceTags: &patchyv1.AzureResourceTags{
					Enabled:         true,
					ManagementGroup: "platform-mg",
				},
			},
		},
	}
	if err := c.Create(ctx, good); err != nil {
		t.Errorf("Create(valid azure integration) = %v, want nil", err)
	}
	noBlock := &patchyv1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-no-block", Namespace: "default"},
		Spec: patchyv1.IntegrationSpec{
			Provider: patchyv1.IntegrationProviderAzure,
		},
	}
	if err := c.Create(ctx, noBlock); err == nil {
		t.Error("Create(azure integration without azure block) = nil, want CEL rejection")
	}
	strayBlock := &patchyv1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-stray-block", Namespace: "default"},
		Spec: patchyv1.IntegrationSpec{
			Provider: patchyv1.IntegrationProviderGoogleCloud,
			GoogleCloud: &patchyv1.GoogleCloudIntegration{
				CloudAssetInventory: &patchyv1.GoogleCloudAssetInventory{
					Enabled: true,
					Scope:   "organizations/123456789012",
				},
			},
			Azure: &patchyv1.AzureIntegration{
				ResourceTags: &patchyv1.AzureResourceTags{Enabled: true},
			},
		},
	}
	if err := c.Create(ctx, strayBlock); err == nil {
		t.Error("Create(google-cloud integration carrying an azure block) = nil, want CEL rejection")
	}
}
