// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakeoidc"
	"github.com/bitwise-media-group/patchy/internal/transcript"
	"github.com/bitwise-media-group/patchy/internal/transcriptstore"
)

// TestStatusServerIntents drives the shipped status-server binary with the
// intents views on, end to end: sign-in through a real OIDC authorization
// code flow (fakeoidc standing in for Dex with its GitHub connector), the
// prefixed identity reaching SubjectAccessReviews, and those reviews
// answered by the envtest API server's real RBAC from Roles written in the
// documented grammar. One viewer per tier and Project, and a stranger.
//
// Set PATCHY_E2E_STATUS_HOLD to a duration to keep the cluster, the issuer
// and the server up after the assertions, for a manual browser check: the
// test logs the URL, the kubeconfig and the auth config to point a
// withui-built status-server at.
func TestStatusServerIntents(t *testing.T) {
	cl := startCluster(t)

	oidc, err := fakeoidc.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oidc.Close)
	oidc.AddUser(fakeoidc.User{Login: "viv", Name: "Viv Viewer"})
	oidc.AddUser(fakeoidc.User{Login: "rex", Name: "Rex Reader", Groups: []string{"acme:alpha-content"}})
	oidc.AddUser(fakeoidc.User{Login: "bea", Name: "Bea Beta"})
	oidc.AddUser(fakeoidc.User{Login: "nob", Name: "Nobody"})

	grantIntentTiers(t, cl)
	runs := fabricateIntents(t, cl)

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	authCfg := writeIntentsAuthConfig(t, oidc.URL, "http://"+listen+"/oauth2/callback")
	cl.controller(t, "status-server", "--listen-addr", listen, "--auth-config", authCfg,
		"--agent-namespace", agentsNS, "--intents-enabled")
	base := "http://" + listen

	viv, rex, bea, nob := signIn(t, base, oidc, "viv"), signIn(t, base, oidc, "rex"),
		signIn(t, base, oidc, "bea"), signIn(t, base, oidc, "nob")

	// Who sees which Project, at which tier.
	for who, want := range map[*http.Client]string{
		viv: "alpha:intents", rex: "alpha:transcripts", bea: "beta:intents", nob: "",
	} {
		var me struct {
			Name     string `json:"name"`
			Projects []struct {
				Name string `json:"name"`
				Tier string `json:"tier"`
			} `json:"projects"`
		}
		getJSON(t, who, base+"/api/me", &me)
		var got []string
		for _, p := range me.Projects {
			got = append(got, p.Name+":"+p.Tier)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s: /api/me projects = %v, want %q", me.Name, got, want)
		}
	}

	// The board holds only the caller's Projects.
	var board struct {
		Intents []struct {
			Name   string `json:"name"`
			Column string `json:"column"`
		} `json:"intents"`
	}
	getJSON(t, viv, base+"/api/intents", &board)
	if len(board.Intents) != 1 || board.Intents[0].Name != "alpha-1" || board.Intents[0].Column != "building" {
		t.Errorf("viv's board = %+v, want alpha-1 alone, in building", board.Intents)
	}

	// Another Project's intent is a 404, identical to a missing one.
	_, missing := status(t, bea, base+"/api/intents/alpha-404")
	for _, path := range []string{"/api/intents/alpha-1", "/api/intents/alpha-1/plans/1",
		"/api/intents/alpha-1/runs/" + runs.plan} {
		if code, body := status(t, bea, base+path); code != http.StatusNotFound || body != missing {
			t.Errorf("bea GET %s = %d %q, want the missing-intent 404", path, code, body)
		}
	}

	// Tier 2 content: the plan is refused at tier 1, served at tier 2.
	if code, _ := status(t, viv, base+"/api/intents/alpha-1/plans/1"); code != http.StatusForbidden {
		t.Errorf("viv's plan = %d, want 403", code)
	}
	code, body := status(t, rex, base+"/api/intents/alpha-1/plans/1")
	if code != http.StatusOK || !strings.Contains(body, "Add the health endpoint") {
		t.Errorf("rex's plan = %d %s", code, body)
	}

	// The stream of a collected run: turns for tier 2, activity alone for
	// tier 1.
	stream := base + "/api/intents/alpha-1/runs/" + runs.plan + "/stream"
	if _, body := status(t, viv, stream); strings.Contains(body, "event: turn") ||
		strings.Contains(body, "Reading the router") || !strings.Contains(body, "event: activity") {
		t.Errorf("viv's stream carried turns or no activity: %s", body)
	}
	if _, body := status(t, rex, stream); strings.Count(body, "event: turn") != 2 {
		t.Errorf("rex's stream = %s, want 2 turns", body)
	}

	// The run panel: public wording, no detail, no registry host.
	_, body = status(t, viv, base+"/api/intents/alpha-1/runs/"+runs.evicted)
	if !strings.Contains(body, `"reason":"evicted: the agent pod was evicted"`) ||
		strings.Contains(body, "ip-10-0-1-23") || strings.Contains(body, "111122223333") {
		t.Errorf("evicted run panel = %s", body)
	}

	// The browser envelope: a same-site request (a preview app's page) is
	// refused even with a session.
	req, _ := http.NewRequest(http.MethodGet, base+"/api/intents", nil)
	req.Header.Set("Sec-Fetch-Site", "same-site")
	if res, err := viv.Do(req); err != nil {
		t.Fatal(err)
	} else {
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("same-site GET = %d, want 403", res.StatusCode)
		}
		if res.Header.Get("Content-Security-Policy") == "" || res.Header.Get("Strict-Transport-Security") == "" {
			t.Error("the hardened envelope's CSP and HSTS are missing")
		}
	}

	// A posture that cannot serve the views refuses to start at all.
	refusesToStart(t, cl, "mode: none\n", "refuses auth mode none")
	refusesToStart(t, cl, "mode: anonymous\nanonymous:\n  username: viewer\n", "the intents views need auth mode oidc")
	refusesToStart(t, cl, "mode: oidc\noidc:\n  issuerURL: "+oidc.URL+"\n  clientID: c\n  clientSecret: s\n"+
		"  claims:\n    username: preferred_username\n", "usernamePrefix and oidc.claims.groupsPrefix")

	if hold := os.Getenv("PATCHY_E2E_STATUS_HOLD"); hold != "" {
		d, err := time.ParseDuration(hold)
		if err != nil {
			t.Fatalf("PATCHY_E2E_STATUS_HOLD: %v", err)
		}
		t.Logf("holding for %s: status server %s, issuer %s (users viv, rex, bea, nob), kubeconfig %s, auth config %s",
			d, base, oidc.URL, cl.kubeconfig, authCfg)
		time.Sleep(d)
	}
}

// intentRuns names the fabricated runs the assertions address.
type intentRuns struct {
	plan, evicted string
}

// grantIntentTiers writes the per-Project grants in the documented grammar:
// a Role with resourceNames on the virtual subresources, bound to the
// prefixed GitHub login or org:team group.
func grantIntentTiers(t *testing.T, cl *cluster) {
	t.Helper()
	ctx := context.Background()
	role := func(name, project string, subresources ...string) {
		res := make([]string, len(subresources))
		for i, s := range subresources {
			res[i] = "projects/" + s
		}
		if err := cl.client.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{"patchy.bitwisemedia.uk"}, Resources: res,
				ResourceNames: []string{project}, Verbs: []string{"get"}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(role string, subject rbacv1.Subject) {
		if err := cl.client.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role, Namespace: namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role},
			Subjects:   []rbacv1.Subject{subject},
		}); err != nil {
			t.Fatal(err)
		}
	}
	role("alpha-viewers", "alpha", "intents")
	role("alpha-content", "alpha", "intents", "transcripts")
	role("beta-viewers", "beta", "intents")
	bind("alpha-viewers", rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "github:viv"})
	bind("alpha-content", rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName,
		Name: "github:acme:alpha-content"})
	bind("beta-viewers", rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "github:bea"})
}

// fabricateIntents writes two Projects, an Intent in each, and the alpha
// intent's runs, plan and transcript, as intent-controller would.
func fabricateIntents(t *testing.T, cl *cluster) intentRuns {
	t.Helper()
	ctx := context.Background()
	for _, p := range []string{"alpha", "beta"} {
		if err := cl.client.Create(ctx, &v1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: p, Namespace: namespace},
			Spec: v1alpha1.ProjectSpec{
				IntentRepository: "https://github.com/acme/intents",
				Labels:           v1alpha1.ProjectLabels{Trigger: "patchy:" + p},
				Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
				Repositories:     []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/acme/" + p}},
			},
		}); err != nil {
			t.Fatalf("create project %s: %v", p, err)
		}
	}
	now := metav1.Now()
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	intent := func(project string, phase v1alpha1.IntentPhase) *v1alpha1.Intent {
		in := &v1alpha1.Intent{
			ObjectMeta: metav1.ObjectMeta{Name: project + "-1", Namespace: namespace},
			Spec: v1alpha1.IntentSpec{
				Project: project,
				Issue: v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 1,
					URL: "https://github.com/acme/intents/issues/1"},
				RequestedBy: v1alpha1.IntentRequest{Login: "octocat", At: now, EventID: 1},
			},
		}
		if err := cl.client.Create(ctx, in); err != nil {
			t.Fatalf("create intent: %v", err)
		}
		in.Status = v1alpha1.IntentStatus{
			Phase:      phase,
			PhaseTimes: []v1alpha1.IntentPhaseTime{{Phase: phase, At: now}},
			Input:      &v1alpha1.IntentInput{Revision: 1, Digest: digest("a"), ConfigMap: project + "-1-input-r1"},
			Plan: &v1alpha1.IntentPlan{Revision: 1, Digest: digest("b"), ConfigMap: project + "-1-plan-r1",
				Summary: "Add the health endpoint"},
			Usage: v1alpha1.IntentUsage{CostMicroUSD: 420_000},
		}
		if err := cl.client.Status().Update(ctx, in); err != nil {
			t.Fatalf("intent status: %v", err)
		}
		return in
	}
	alpha := intent("alpha", v1alpha1.IntentBuilding)
	intent("beta", v1alpha1.IntentAwaitingApproval)

	owner := func(kind, name string, uid types.UID) metav1.OwnerReference {
		yes := true
		return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: kind, Name: name,
			UID: uid, Controller: &yes}
	}
	if err := cl.client.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-1-plan-r1", Namespace: namespace,
			Labels:          map[string]string{v1alpha1.LabelIntent: "alpha-1"},
			OwnerReferences: []metav1.OwnerReference{owner("Intent", alpha.Name, alpha.UID)}},
		Data: map[string]string{"plan.md": "---\nsummary: Add the health endpoint\n---\n# Plan\n"},
	}); err != nil {
		t.Fatal(err)
	}

	run := func(name string, st v1alpha1.IntentRunStatus) *v1alpha1.IntentRun {
		r := &v1alpha1.IntentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
				Labels: map[string]string{v1alpha1.LabelIntent: alpha.Name}},
			Spec: v1alpha1.IntentRunSpec{
				IntentRef: v1alpha1.ObjectReference{Name: alpha.Name, UID: alpha.UID},
				Stage:     v1alpha1.IntentStagePlan, Round: 1, Attempt: 1,
				Repository: v1alpha1.IntentRunRepository{URL: "https://github.com/acme/alpha",
					RepositoryRef: v1alpha1.LocalObjectReference{Name: name + "-src"}},
				Inputs: v1alpha1.IntentRunInputs{ConfigMap: name + "-input", InputDigest: digest("a")},
			},
		}
		if err := cl.client.Create(ctx, r); err != nil {
			t.Fatalf("create run %s: %v", name, err)
		}
		r.Status = st
		if err := cl.client.Status().Update(ctx, r); err != nil {
			t.Fatalf("run status %s: %v", name, err)
		}
		return r
	}
	plan := run("alpha-1-plan-r1-a1", v1alpha1.IntentRunStatus{
		Phase: v1alpha1.RunComplete, Outcome: "ok", Report: "# Plan\n",
		Usage:      v1alpha1.UsageSummary{CostUSD: "0.42"},
		Transcript: &v1alpha1.TranscriptRef{Name: "alpha-1-plan-r1-a1-transcript", Turns: 2},
	})
	evicted := run("alpha-1-plan-r1-a2", v1alpha1.IntentRunStatus{
		Phase: v1alpha1.RunFailed, Outcome: "evicted",
		Detail: "the agent pod was evicted: The node ip-10-0-1-23.ec2.internal was low on resource: memory",
		RunnerImage: &v1alpha1.RunnerImageRef{Source: v1alpha1.RunnerImageSourceDefault,
			Image: "111122223333.dkr.ecr.us-east-1.amazonaws.com/patchy/claude@sha256:" + strings.Repeat("c", 64)},
	})
	cm, err := transcriptstore.ConfigMap(namespace, map[string]string{v1alpha1.LabelIntent: alpha.Name}, plan,
		metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "IntentRun"}, []transcript.Turn{
			{Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: "Reading the router."},
			{Seq: 2, Role: transcript.RoleAssistant, Kind: transcript.KindToolUse, Tool: "Read", Text: "router.go"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	return intentRuns{plan: plan.Name, evicted: evicted.Name}
}

// writeIntentsAuthConfig is a Dex-with-GitHub shaped oidc config: GitHub
// logins and org:team groups, both prefixed, over plain HTTP (insecure dev
// cookies).
func writeIntentsAuthConfig(t *testing.T, issuer, redirect string) string {
	t.Helper()
	cfg := fmt.Sprintf(`mode: oidc
insecure: true
oidc:
  issuerURL: %s
  clientID: patchy-status-e2e
  clientSecret: e2e-status-client-secret
  redirectURL: %s
  claims:
    username: preferred_username
    groups: groups
    usernamePrefix: "github:"
    groupsPrefix: "github:"
`, issuer, redirect)
	path := filepath.Join(t.TempDir(), "auth.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// signIn runs the authorization code flow for login and returns a client
// holding the session cookie. The issuer picks the user from login_hint,
// which the client adds as it follows the server's redirect to it.
func signIn(t *testing.T, base string, oidc *fakeoidc.Server, login string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	issuer, _ := url.Parse(oidc.URL)
	c := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host == issuer.Host && req.URL.Path == "/authorize" {
			q := req.URL.Query()
			q.Set("login_hint", login)
			req.URL.RawQuery = q.Encode()
		}
		if len(via) > 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}}
	res, err := c.Get(base + "/oauth2/authorize?originalPath=/")
	if err != nil {
		t.Fatalf("sign in %s: %v", login, err)
	}
	_ = res.Body.Close()
	u, _ := url.Parse(base)
	if !slices.ContainsFunc(jar.Cookies(u), func(ck *http.Cookie) bool { return ck.Name == "patchy-dev-auth" }) {
		t.Fatalf("sign in %s: no session cookie (final status %d)", login, res.StatusCode)
	}
	return c
}

func status(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func getJSON(t *testing.T, c *http.Client, url string, v any) {
	t.Helper()
	code, body := status(t, c, url)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", url, code, body)
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// refusesToStart runs status-server with the intents views on under auth
// config cfg and expects it to exit at startup with an error containing
// want, never to serve.
func refusesToStart(t *testing.T, cl *cluster, cfg, want string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, build(t, "status-server"), "serve", "--kubeconfig", cl.kubeconfig,
		"--namespace", namespace, "--listen-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"--health-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)), "--auth-config", path, "--intents-enabled")
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), want) {
		t.Errorf("status-server under %q: err=%v, output %s; want a startup refusal containing %q", cfg, err, out, want)
	}
}
