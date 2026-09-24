// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

// testIntegration is a minimal github Integration.
func testIntegration(name string, mutate ...func(*v1alpha1.Integration)) *v1alpha1.Integration {
	i := &v1alpha1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
		Spec: v1alpha1.IntegrationSpec{
			Provider:  v1alpha1.IntegrationProviderGitHub,
			SecretRef: &v1alpha1.LocalSecretReference{Name: "creds"},
			GitHub:    &v1alpha1.GitHubIntegration{},
		},
	}
	for _, m := range mutate {
		m(i)
	}
	return i
}

// postAdmin drives one admin action through the full handler stack.
func postAdmin(t *testing.T, s *Server, verb string) (*http.Response, string) {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	res, err := http.Post(ts.URL+"/api/admin/"+verb, "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res, strings.TrimSpace(string(body))
}

func TestHandleAdminReplay(t *testing.T) {
	s := testServer(t, testIntegration("gh"), testIntegration("gh-suspended", func(i *v1alpha1.Integration) {
		i.Spec.Suspend = true
	}))
	s.auth, s.granter = stubAuth{id: operator}, stubGranter{grants: allGrants()}

	res, body := postAdmin(t, s, "replay")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", res.StatusCode, body)
	}

	var integ v1alpha1.Integration
	if err := mustClient(s).Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "gh"}, &integ); err != nil {
		t.Fatalf("get integration: %v", err)
	}
	rep := integ.Spec.Replay
	if rep == nil || rep.By != "op@acme.test" || !rep.At.Time.Equal(testClock) {
		t.Errorf("spec.replay = %+v, want stamped by op@acme.test at testClock", rep)
	}

	var suspended v1alpha1.Integration
	key := types.NamespacedName{Namespace: "patchy", Name: "gh-suspended"}
	if err := mustClient(s).Get(t.Context(), key, &suspended); err != nil {
		t.Fatalf("get suspended integration: %v", err)
	}
	if suspended.Spec.Replay != nil {
		t.Error("suspended integration got a replay request")
	}
}

func TestHandleAdminReplayNoIntegration(t *testing.T) {
	s := testServer(t)
	s.auth, s.granter = stubAuth{id: operator}, stubGranter{grants: allGrants()}
	if res, body := postAdmin(t, s, "replay"); res.StatusCode != http.StatusConflict {
		t.Errorf("status = %d (%s), want 409", res.StatusCode, body)
	}
}

func TestHandleAdminReset(t *testing.T) {
	inv := &v1alpha1.Investigation{ObjectMeta: metav1.ObjectMeta{Name: "inv-1", Namespace: "patchy"}}
	rollup := testRollup("total", "", "total")
	s := testServer(t, fullFinding(), inv, rollup, testIntegration("gh"))
	s.auth, s.granter = stubAuth{id: operator}, stubGranter{grants: allGrants()}

	res, body := postAdmin(t, s, "reset")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", res.StatusCode, body)
	}

	// The reset is a request, not an act: the integration-controller does
	// the deleting (it needs the Findings' issue numbers for the GitHub
	// cleanup), so everything must still be here, and the Integration must
	// carry the stamp.
	for _, list := range []client.ObjectList{
		&v1alpha1.FindingList{}, &v1alpha1.InvestigationList{}, &v1alpha1.FindingRollupList{},
	} {
		if err := mustClient(s).List(t.Context(), list, client.InNamespace("patchy")); err != nil {
			t.Fatalf("list %T: %v", list, err)
		}
		if n := len(asMap(t, list)["items"].([]any)); n == 0 {
			t.Errorf("%T emptied by the status server; deletion is the controller's job", list)
		}
	}
	var integs v1alpha1.IntegrationList
	if err := mustClient(s).List(t.Context(), &integs, client.InNamespace("patchy")); err != nil {
		t.Fatalf("list integrations: %v", err)
	}
	if len(integs.Items) != 1 {
		t.Fatalf("integrations = %d, want the configuration untouched", len(integs.Items))
	}
	req := integs.Items[0].Spec.Reset
	if req == nil || req.By != "op@acme.test" || !req.At.Time.Equal(testClock) {
		t.Errorf("spec.reset = %+v, want stamped by op@acme.test at testClock", req)
	}
}

func TestHandleAdminResetNoIntegrationFallsBack(t *testing.T) {
	inv := &v1alpha1.Investigation{ObjectMeta: metav1.ObjectMeta{Name: "inv-1", Namespace: "patchy"}}
	s := testServer(t, fullFinding(), inv, testRollup("total", "", "total"))
	s.auth, s.granter = stubAuth{id: operator}, stubGranter{grants: allGrants()}

	res, body := postAdmin(t, s, "reset")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", res.StatusCode, body)
	}

	// No Integration means no controller to consume the request and nothing
	// on GitHub to clean; the pipeline resources are deleted directly.
	for _, list := range []client.ObjectList{
		&v1alpha1.FindingList{}, &v1alpha1.InvestigationList{}, &v1alpha1.FindingRollupList{},
	} {
		if err := mustClient(s).List(t.Context(), list, client.InNamespace("patchy")); err != nil {
			t.Fatalf("list %T: %v", list, err)
		}
		if n := len(asMap(t, list)["items"].([]any)); n != 0 {
			t.Errorf("%T still holds %d items after fallback reset", list, n)
		}
	}
}

// TestHandleAdminResetFallbackLeavesIntentFlow: the fallback delete is the
// Finding flow's, as the controller's reset is. The intent flow shares the
// namespace, and its Projects, Intents, IntentRuns and Repositories (which
// never carry the Finding label) survive; the findings' Repositories go.
func TestHandleAdminResetFallbackLeavesIntentFlow(t *testing.T) {
	findingRepo := &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{
		Name: "fnd-1", Namespace: "patchy", Labels: map[string]string{v1alpha1.LabelFinding: "fnd-1"},
	}}
	intentRepo := &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{
		Name: "web-12-r0", Namespace: "patchy", Labels: map[string]string{v1alpha1.LabelIntent: "web-12"},
	}}
	s := testServer(t, fullFinding(), findingRepo, intentRepo,
		&v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "web-12", Namespace: "patchy"}},
		&v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: "web-12-plan-1", Namespace: "patchy"}},
		&v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "patchy"}},
	)
	s.auth, s.granter = stubAuth{id: operator}, stubGranter{grants: allGrants()}

	if res, body := postAdmin(t, s, "reset"); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", res.StatusCode, body)
	}

	var repos v1alpha1.RepositoryList
	if err := mustClient(s).List(t.Context(), &repos, client.InNamespace("patchy")); err != nil {
		t.Fatalf("list repositories: %v", err)
	}
	if len(repos.Items) != 1 || repos.Items[0].Name != intentRepo.Name {
		t.Errorf("repositories after reset = %v, want only the intent's", repos.Items)
	}
	for _, list := range []client.ObjectList{
		&v1alpha1.IntentList{}, &v1alpha1.IntentRunList{}, &v1alpha1.ProjectList{},
	} {
		if err := mustClient(s).List(t.Context(), list, client.InNamespace("patchy")); err != nil {
			t.Fatalf("list %T: %v", list, err)
		}
		if n := len(asMap(t, list)["items"].([]any)); n != 1 {
			t.Errorf("%T holds %d items after reset, want 1 (untouched)", list, n)
		}
	}
}

func TestHandleAdminAuthz(t *testing.T) {
	tests := []struct {
		name    string
		auth    stubAuth
		granter stubGranter
		verb    string
		want    int
	}{
		{"unauthenticated", stubAuth{}, stubGranter{}, "replay", http.StatusUnauthorized},
		{"granted actions but not admin", stubAuth{id: operator},
			stubGranter{grants: authz.Grants{View: true, Verbs: []string{"approve"}}}, "reset", http.StatusForbidden},
		{"unknown verb", stubAuth{id: operator}, stubGranter{grants: allGrants()}, "explode", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testServer(t, testIntegration("gh"))
			s.auth, s.granter = tt.auth, tt.granter
			if res, body := postAdmin(t, s, tt.verb); res.StatusCode != tt.want {
				t.Errorf("status = %d (%s), want %d", res.StatusCode, body, tt.want)
			}
		})
	}
}
