// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/devharness"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/kube"
	pkggeneric "github.com/bitwise-media-group/patchy/pkg/generic"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

// grants answers SelfSubjectAccessReviews from a set of "resource/verb"
// strings and SelfSubjectReviews with user, the way an API server's
// authorizer and authenticator would. It records every review it answered.
type grants struct {
	allowed map[string]bool
	user    string
	failAll error
	asked   []authorizationv1.ResourceAttributes
}

func (g *grants) create(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	if g.failAll != nil {
		return g.failAll
	}
	switch review := obj.(type) {
	case *authorizationv1.SelfSubjectAccessReview:
		ra := review.Spec.ResourceAttributes
		g.asked = append(g.asked, *ra)
		review.Status.Allowed = g.allowed[ra.Resource+"/"+ra.Verb]
		review.Status.Reason = "test"
		return nil
	case *authenticationv1.SelfSubjectReview:
		review.Status.UserInfo.Username = g.user
		return nil
	}
	return c.Create(ctx, obj, opts...)
}

// reviewHarness is a harness whose access and identity questions go to the
// cluster (no accessFn/identityFn), answered by g.
func reviewHarness(t *testing.T, g *grants, objs ...client.Object) *harness {
	t.Helper()
	h := newHarness(t)
	h.client = fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: g.create}).Build()
	h.opts.WithEnv(&kubecfg.Env{Client: h.client, Namespace: testNamespace})
	h.opts.accessFn = nil
	h.opts.identityFn = nil
	return h
}

func TestCanISingleVerb(t *testing.T) {
	g := &grants{allowed: map[string]bool{"findings/approve": true, "integrations/backfill": true, "findings/get": true}}
	cases := []struct {
		verb     string
		wantOut  string
		wantCode int
		wantRes  string
	}{
		{"approve", "yes\n", ExitOK, "findings"},
		{"suspend", "no\n", ExitDenied, "findings"},
		{"backfill", "yes\n", ExitOK, "integrations"},
		{"reset", "no\n", ExitDenied, "integrations"},
		{"get", "yes\n", ExitOK, "findings"},
		{"list", "no\n", ExitDenied, "findings"},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			g.asked = nil
			h := reviewHarness(t, g)
			err := h.execRoot(t, "can-i", tc.verb)
			if got := exitCode(err); err != nil && got != tc.wantCode || err == nil && tc.wantCode != ExitOK {
				t.Fatalf("err = %v (exit %d), want exit %d", err, got, tc.wantCode)
			}
			if h.out.String() != tc.wantOut {
				t.Errorf("stdout = %q, want %q", h.out.String(), tc.wantOut)
			}
			if len(g.asked) != 1 {
				t.Fatalf("asked %d reviews, want 1", len(g.asked))
			}
			ra := g.asked[0]
			if ra.Resource != tc.wantRes || ra.Namespace != testNamespace || ra.Group != v1alpha1.GroupVersion.Group {
				t.Errorf("review attributes = %+v", ra)
			}
		})
	}
}

func TestCanIMatrix(t *testing.T) {
	g := &grants{user: "alice@acme.test", allowed: map[string]bool{
		"findings/get": true, "findings/list": true, "findings/retry": true, "integrations/replay": true,
	}}
	h := reviewHarness(t, g)
	if err := h.execRoot(t, "can-i", "-o", "markdown"); err != nil {
		t.Fatalf("can-i: %v", err)
	}
	got := h.out.String()
	for _, want := range []string{
		"## Grants in namespace patchy",
		"- **Identity:** alice@acme.test",
		"- **get:** yes",
		"- **retry:** yes",
		"- **approve:** no",
		"## Integrations",
		"- **replay:** yes",
		"- **reset:** no",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// Every verb on both resources was asked about, and nothing else.
	want := 2 + len(action.ActionVerbs) + 2 + len(action.IntegrationVerbs)
	if len(g.asked) != want {
		t.Errorf("asked %d reviews, want %d", len(g.asked), want)
	}
}

func TestCanIErrors(t *testing.T) {
	t.Run("unknown verb", func(t *testing.T) {
		h := reviewHarness(t, &grants{})
		err := h.execRoot(t, "can-i", "delete")
		if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), `unknown verb "delete"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("all namespaces", func(t *testing.T) {
		h := reviewHarness(t, &grants{})
		h.opts.env.Namespace = ""
		err := h.execRoot(t, "can-i")
		if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "single namespace") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("review fails", func(t *testing.T) {
		h := reviewHarness(t, &grants{failAll: errors.New("apiserver down")})
		err := h.execRoot(t, "can-i", "approve")
		if err == nil || !strings.Contains(err.Error(), "access review for approve: apiserver down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("matrix review fails", func(t *testing.T) {
		h := reviewHarness(t, &grants{failAll: errors.New("apiserver down")})
		err := h.execRoot(t, "can-i")
		if err == nil || !strings.Contains(err.Error(), "apiserver down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad format", func(t *testing.T) {
		h := reviewHarness(t, &grants{})
		if err := h.execRoot(t, "can-i", "-o", "xml"); exitCode(err) != ExitUsage {
			t.Fatalf("err = %v, want usage", err)
		}
	})
}

func TestCanICompletion(t *testing.T) {
	h := newHarness(t)
	cmd := newCanICmd(h.opts)
	got, dir := cmd.ValidArgsFunction(cmd, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v", dir)
	}
	for _, v := range append(append([]string{}, action.ActionVerbs...), action.IntegrationVerbs...) {
		if !slices.Contains(got, v) {
			t.Errorf("completion missing %q: %v", v, got)
		}
	}
	if more, _ := cmd.ValidArgsFunction(cmd, []string{"approve"}, ""); len(more) != 0 {
		t.Errorf("completed a second argument: %v", more)
	}
}

// TestActionRecordsClusterIdentity: spec.approval.by is the audit trail, so it
// must be the user the cluster authenticated, asked through a self-subject
// review.
func TestActionRecordsClusterIdentity(t *testing.T) {
	g := &grants{user: "alice@acme.test", allowed: map[string]bool{"findings/approve": true}}
	h := reviewHarness(t, g, testFinding("fnd-1", v1alpha1.PhaseAwaitingApproval))
	if err := h.execRoot(t, "approve", "finding", "fnd-1"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	f := h.finding(t, "fnd-1")
	if f.Spec.Approval == nil || f.Spec.Approval.By != "alice@acme.test" {
		t.Fatalf("approval = %+v, want it recorded under the cluster identity", f.Spec.Approval)
	}
}

func TestWhoamiFallsBackToUnknown(t *testing.T) {
	for name, g := range map[string]*grants{
		"review fails":   {failAll: errors.New("forbidden")},
		"empty username": {},
	} {
		t.Run(name, func(t *testing.T) {
			h := reviewHarness(t, g)
			h.opts.Verbose = true
			if got := whoami(context.Background(), h.opts, h.opts.env); got != "unknown" {
				t.Errorf("whoami = %q, want unknown", got)
			}
		})
	}
}

func TestActionNoTargets(t *testing.T) {
	h := newHarness(t)
	if err := h.execRoot(t, "suspend", "finding", "-l", "patchy.bitwisemedia.uk/severity=critical"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "No findings matched.") {
		t.Errorf("stderr = %q", h.errOut.String())
	}
}

func TestActionAccessReviewError(t *testing.T) {
	h := newHarness(t, testFinding("fnd-1", v1alpha1.PhaseQueued))
	h.opts.WithAccess(func(context.Context, *kubecfg.Env, string, string) (bool, error) {
		return false, errors.New("review exploded")
	})
	err := h.execRoot(t, "suspend", "finding", "fnd-1")
	if err == nil || !strings.Contains(err.Error(), "review exploded") {
		t.Fatalf("err = %v", err)
	}
	if h.finding(t, "fnd-1").Spec.Suspend {
		t.Error("finding suspended despite a failed access review")
	}
}

func TestActionNeedsTargets(t *testing.T) {
	h := newHarness(t)
	err := runAction(context.Background(), h.opts, &actionFlags{}, action.VerbSuspend, "finding", nil)
	if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "name at least one finding") {
		t.Fatalf("err = %v", err)
	}
}

// withStdin points os.Stdin at a file holding input for the test's duration.
func withStdin(t *testing.T, input string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
		_ = f.Close()
	})
}

// TestActionBulkConfirmation: a bulk write names its targets and waits for an
// explicit yes; anything else writes nothing.
func TestActionBulkConfirmation(t *testing.T) {
	cases := []struct {
		input   string
		applied bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"n\n", false},
		{"yes\n", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.input), func(t *testing.T) {
			h := newHarness(t, testFinding("fnd-1", v1alpha1.PhaseQueued), testFinding("fnd-2", v1alpha1.PhaseQueued))
			withStdin(t, tc.input)
			err := h.execRoot(t, "suspend", "finding", "fnd-1", "fnd-2")
			if !strings.Contains(h.errOut.String(), "About to suspend 2 findings:\n  fnd-1\n  fnd-2\nContinue? [y/N] ") {
				t.Errorf("prompt = %q", h.errOut.String())
			}
			if tc.applied {
				if err != nil {
					t.Fatalf("suspend: %v", err)
				}
			} else if err == nil || err.Error() != "cancelled" {
				t.Fatalf("err = %v, want cancelled", err)
			}
			for _, name := range []string{"fnd-1", "fnd-2"} {
				if got := h.finding(t, name).Spec.Suspend; got != tc.applied {
					t.Errorf("%s suspended = %v, want %v", name, got, tc.applied)
				}
			}
		})
	}
}

func writeTestKubeconfig(t *testing.T, namespace string) string {
	t.Helper()
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://cluster.invalid:6443"}
users:
- name: u
  user: {token: t}
contexts:
- name: ctx
  context: {cluster: c, user: u, namespace: %s}
current-context: ctx
`, namespace)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOptionsConnect(t *testing.T) {
	path := writeTestKubeconfig(t, "team-a")
	opts := &Options{Kubeconfig: path, Namespace: "override"}
	env, err := opts.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if env.Namespace != "override" {
		t.Errorf("Namespace = %q, want the flag's", env.Namespace)
	}
	again, err := opts.Connect()
	if err != nil || again != env {
		t.Errorf("second Connect = %p, %v; want the cached env %p", again, err, env)
	}

	bad := &Options{Kubeconfig: filepath.Join(t.TempDir(), "absent")}
	if _, err := bad.Connect(); err == nil {
		t.Error("Connect accepted a missing kubeconfig")
	}
}

// TestCommandsSurfaceConnectErrors: every cluster-reading verb stops at an
// unusable kubeconfig rather than carrying on with a nil client.
func TestCommandsSurfaceConnectErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	for _, args := range [][]string{
		{"get", "findings"},
		{"get", "all"},
		{"describe", "finding", "x"},
		{"review", "finding", "x"},
		{"browse", "finding", "x"},
		{"can-i"},
		{"approve", "finding", "x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			opts := &Options{Out: out, ErrOut: errOut}
			root := NewRoot(opts)
			root.SetOut(out)
			root.SetErr(errOut)
			root.SetArgs(append([]string{"--kubeconfig", missing}, args...))
			err := root.ExecuteContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), "kubernetes client config") {
				t.Fatalf("err = %v, want the kubeconfig failure", err)
			}
		})
	}
}

func TestNounCompletions(t *testing.T) {
	got, dir := nounCompletion(nil, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp || !slices.Contains(got, "findings") || slices.Contains(got, "all") {
		t.Errorf("nounCompletion = %v, %v", got, dir)
	}
	if more, _ := nounCompletion(nil, []string{"finding"}, ""); more != nil {
		t.Errorf("nounCompletion completed a name: %v", more)
	}
	got, _ = getNounCompletion(nil, nil, "")
	if len(got) == 0 || got[0] != "all" || !slices.Contains(got, "findings") {
		t.Errorf("getNounCompletion = %v, want all first", got)
	}
	if more, _ := getNounCompletion(nil, []string{"all"}, ""); more != nil {
		t.Errorf("getNounCompletion completed a name: %v", more)
	}
	if v, dir := noFileCompletion(nil, nil, ""); v != nil || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("noFileCompletion = %v, %v", v, dir)
	}
}

func TestDevEmitterJSONIsNDJSON(t *testing.T) {
	h := newHarness(t)
	h.opts.Output = "json"
	emit, err := devEmitter(h.opts)
	if err != nil {
		t.Fatalf("devEmitter: %v", err)
	}
	emit(devharness.Event{Kind: "listening", WebhookURL: "http://127.0.0.1:1/x"})
	emit(devharness.Event{Kind: "delivery", DeliveryID: "d1", Ingested: 2})
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want one per event:\n%s", len(lines), h.out.String())
	}
	var e devharness.Event
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil || e.Kind != "delivery" || e.Ingested != 2 {
		t.Errorf("line 2 = %q (%v)", lines[1], err)
	}
	if h.errOut.Len() != 0 {
		t.Errorf("json mode narrated on stderr: %q", h.errOut.String())
	}

	h.opts.Output = "xml"
	if _, err := devEmitter(h.opts); exitCode(err) != ExitUsage {
		t.Errorf("devEmitter(xml) err = %v, want usage", err)
	}
}

func TestDevEmitterRendersForHumans(t *testing.T) {
	cases := []struct {
		name  string
		event devharness.Event
		want  []string
	}{
		{"listening", devharness.Event{Kind: "listening", WebhookURL: "http://127.0.0.1:1/generic/x/webhooks"},
			[]string{"patchy: listening — POST signed findings payloads to http://127.0.0.1:1/generic/x/webhooks"}},
		{"delivery", devharness.Event{Kind: "delivery", DeliveryID: "d1", Ingested: 3, Note: "1 below floor"},
			[]string{"patchy: delivery d1: 3 finding(s) ingested", "patchy: note: 1 below floor"}},
		{"code finding", devharness.Event{Kind: "finding", Finding: &source.Finding{
			AlertNumber: 42, Severity: "high", Title: "SQLi", RuleID: "go/sqli",
			Advisories: []string{"CWE-89", "CVE-1"}, HTMLURL: "https://x.test/a/42",
			Repo: source.Repo{Owner: "acme", Name: "app"},
		}}, []string{"Finding #42", "Severity:", "high", "SQLi", "go/sqli", "CWE-89, CVE-1", "acme/app"}},
		{"cloud finding", devharness.Event{Kind: "finding", Finding: &source.Finding{
			AlertID: "scc-1", Title: "Open bucket",
			CloudResource: &source.CloudResource{Provider: "gcp", Name: "b1", Type: "bucket"},
		}}, []string{"Finding scc-1", "gcp b1 (bucket)"}},
		{"enhance", devharness.Event{Kind: "enhance", EnhanceResponse: &pkggeneric.EnhanceResponse{
			Owners:          []string{"team-a", "team-b"},
			Attributes:      map[string]string{"tier": "1", "env": "prod"},
			Repository:      &source.RepositoryRef{Provider: "github", Owner: "acme", Name: "app"},
			CommentMarkdown: "owned by **team-a**",
		}, Note: "n1"}, []string{"Enhance", "team-a, team-b", "env=prod, tier=1", "github acme/app",
			"owned by **team-a**", "n1"}},
		{"enhance without response", devharness.Event{Kind: "enhance", Err: "timeout"},
			[]string{"Enhance", "Error:", "timeout"}},
		{"resolve", devharness.Event{Kind: "resolve", ResolveRequest: &pkggeneric.ResolveRequest{
			Alerts:  []pkggeneric.AlertRef{{ID: "a1"}, {ID: "a2"}},
			Verdict: pkggeneric.Verdict{Kind: "false-positive", Reason: "unreachable"},
		}}, []string{"Resolve", "a1", "a2", "false-positive (unreachable)"}},
		{"error", devharness.Event{Kind: "error", DeliveryID: "d2", Err: "bad signature", Note: "check the secret"},
			[]string{"patchy: delivery d2 rejected: bad signature", "patchy: note: check the secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			emit, err := devEmitter(h.opts)
			if err != nil {
				t.Fatalf("devEmitter: %v", err)
			}
			emit(tc.event)
			if h.out.Len() != 0 {
				t.Errorf("human rendering wrote to stdout: %q", h.out.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(h.errOut.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, h.errOut.String())
				}
			}
		})
	}
}
