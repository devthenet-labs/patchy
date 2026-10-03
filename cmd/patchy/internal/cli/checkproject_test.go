// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/projectcheck"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// noGitHub is a GitHub the caller cannot read.
type noGitHub struct{}

func (noGitHub) Head(context.Context, string) (projectcheck.Head, error) {
	return projectcheck.Head{}, errors.New("GitHub answered 404 Not Found (anonymously)")
}

func (noGitHub) File(context.Context, string, string, string, int64) ([]byte, int64, bool, error) {
	return nil, 0, false, errors.New("unreachable")
}

// noDNS resolves nothing.
type noDNS struct{}

func (noDNS) LookupHost(context.Context, string) ([]string, error) {
	return nil, errors.New("no such host")
}

// checkProjectDepsFake reaches nothing beyond the fake cluster.
var checkProjectDepsFake = checkProjectDeps{
	github:   noGitHub{},
	resolver: noDNS{},
	dialTLS: func(context.Context, string) (tls.ConnectionState, error) {
		return tls.ConnectionState{}, errors.New("not dialed in tests")
	},
}

// readyProject is a Ready one-repository Project with no preview, whose
// repository need not declare an agent image.
func readyProject(name string) *v1alpha1.Project {
	no := false
	return &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository:       "https://github.com/acme/intents",
			Approvers:              v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:           []v1alpha1.ProjectRepository{{Name: "web", URL: "https://github.com/acme/web"}},
			RequireRepositoryImage: &no,
		},
		Status: v1alpha1.ProjectStatus{Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Validated", ObservedGeneration: 1,
				Message: "the repositories resolve, the App is installed on them, and the labels exist"},
			{Type: v1alpha1.ConditionIntentNameConflict, Status: metav1.ConditionFalse, Reason: "NoConflict",
				Message: "no conflict"},
		}},
	}
}

func readyForge() *v1alpha1.Forge {
	return &v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: testNamespace},
		Spec:       v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub, Orgs: []string{"acme"}},
		Status: v1alpha1.ForgeStatus{Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "CredentialValid"},
		}},
	}
}

// checkProjectHarness is a harness whose client fails the test on any
// Secret read, and answers fn first when given.
func checkProjectHarness(t *testing.T, fn interceptor.Funcs, objs ...client.Object) *harness {
	t.Helper()
	h := newHarness(t)
	get := fn.Get
	fn.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Secret); ok {
			t.Errorf("check project read Secret %s", key)
		}
		if get != nil {
			return get(ctx, c, key, obj, opts...)
		}
		return c.Get(ctx, key, obj, opts...)
	}
	fn.List = func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.SecretList); ok {
			t.Error("check project listed Secrets")
		}
		return c.List(ctx, list, opts...)
	}
	h.client = fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
		WithInterceptorFuncs(fn).Build()
	h.opts.WithEnv(&kubecfg.Env{Client: h.client, Namespace: testNamespace})
	return h
}

func TestCheckProjectTable(t *testing.T) {
	h := checkProjectHarness(t, interceptor.Funcs{}, readyProject("shop"), readyForge())
	if err := runCheckProject(context.Background(), h.opts, "shop", checkProjectDepsFake); err != nil {
		t.Fatalf("check project: %v\n%s", err, h.out)
	}
	out := h.out.String()
	for _, want := range []string{
		"PASS  ready         -                  Validated: the repositories resolve",
		"PASS  forge         intent-repository  Forge github covers acme/intents and is Ready",
		"PASS  forge         web                Forge github covers acme/web",
		"SKIP  agent-image   web                cannot read the default branch",
		"SKIP  previews      -                  the Project previews no repository",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCheckProjectFailureExitsOne(t *testing.T) {
	p := readyProject("shop")
	p.Status.Conditions[0].Status = metav1.ConditionFalse
	p.Status.Conditions[0].Reason = v1alpha1.ReasonForgeUnresolved
	p.Status.Conditions[0].Message = "https://github.com/acme/web: no forge matches repository"
	h := checkProjectHarness(t, interceptor.Funcs{}, p)
	err := runCheckProject(context.Background(), h.opts, "shop", checkProjectDepsFake)
	if err == nil || err.Error() != "3 checks failed" {
		t.Fatalf("check project error = %v, want 3 checks failed (ready, both forges)\n%s", err, h.out)
	}
	if code := exitCode(err); code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(h.out.String(), "FAIL  ready") {
		t.Errorf("output:\n%s", h.out)
	}
}

func TestCheckProjectStructured(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			h := checkProjectHarness(t, interceptor.Funcs{}, readyProject("shop"), readyForge())
			h.opts.Output = format
			if err := runCheckProject(context.Background(), h.opts, "shop", checkProjectDepsFake); err != nil {
				t.Fatal(err)
			}
			var report projectcheck.Report
			raw := h.out.Bytes()
			if format == "yaml" {
				var err error
				if raw, err = yaml.YAMLToJSON(raw); err != nil {
					t.Fatal(err)
				}
			}
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatalf("not a report: %v\n%s", err, h.out)
			}
			if report.Project != "shop" || report.Namespace != testNamespace || len(report.Checks) == 0 {
				t.Errorf("report = %+v", report)
			}
			for _, c := range report.Checks {
				if c.Status != checkreport.Pass && c.Status != checkreport.Skip {
					t.Errorf("%s %s = %s: %s", c.Name, c.Repository, c.Status, c.Reason)
				}
			}
		})
	}
}

func TestCheckProjectExitCodes(t *testing.T) {
	forbidden := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object,
		...client.GetOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "projects"},
			"shop", errors.New("RBAC"))
	}}
	cases := []struct {
		name  string
		fn    interceptor.Funcs
		setup func(*Options)
		want  int
	}{
		{"not found", interceptor.Funcs{}, nil, ExitNotFound},
		{"forbidden", forbidden, nil, ExitDenied},
		{"all namespaces", interceptor.Funcs{}, func(o *Options) { o.AllNamespaces = true }, ExitUsage},
		{"bad format", interceptor.Funcs{}, func(o *Options) { o.Output = "csv" }, ExitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := checkProjectHarness(t, tc.fn)
			if tc.setup != nil {
				tc.setup(h.opts)
			}
			err := runCheckProject(context.Background(), h.opts, "shop", checkProjectDepsFake)
			if code := exitCode(err); code != tc.want {
				t.Errorf("exit code = %d (%v), want %d", code, err, tc.want)
			}
		})
	}
}

// TestCheckProjectNeedsANamespace: a context with no namespace and no -n
// is a usage error, not a read of every namespace.
func TestCheckProjectNeedsANamespace(t *testing.T) {
	h := checkProjectHarness(t, interceptor.Funcs{})
	h.opts.WithEnv(&kubecfg.Env{Client: h.client})
	err := runCheckProject(context.Background(), h.opts, "shop", checkProjectDepsFake)
	if code := exitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d (%v), want %d", code, err, ExitUsage)
	}
}

func TestEnvToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	if tok, src := envToken("GH_TOKEN", "GITHUB_TOKEN"); tok != "from-github-token" || src != "GITHUB_TOKEN" {
		t.Errorf("envToken = %q, %q", tok, src)
	}
	t.Setenv("GH_TOKEN", "from-gh-token")
	if tok, src := envToken("GH_TOKEN", "GITHUB_TOKEN"); tok != "from-gh-token" || src != "GH_TOKEN" {
		t.Errorf("envToken = %q, %q", tok, src)
	}
	t.Setenv("GH_ENTERPRISE_TOKEN", "")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")
	if tok, src := envToken("GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"); tok != "" || src != "" {
		t.Errorf("envToken = %q, %q, want nothing", tok, src)
	}
}

// TestCheckCommandHelp: check is no longer wholly cluster-free, and says
// which of its nouns reads the cluster.
func TestCheckCommandHelp(t *testing.T) {
	out, err := execDev(t, "check", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"check image needs no cluster", "check project reads the", "project"} {
		if !strings.Contains(out, want) {
			t.Errorf("check --help lacks %q:\n%s", want, out)
		}
	}
}
