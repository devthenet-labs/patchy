// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/generic"
	"github.com/bitwise-media-group/patchy/internal/ghas"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/wiz"
	pkggeneric "github.com/bitwise-media-group/patchy/pkg/generic"
)

// findingWithAlerts is a dismissed finding carrying alerts from the given
// sources, keyed by source id.
func findingWithAlerts(repo string, alerts map[string][]string) *v1alpha1.Finding {
	fnd := projectable(v1alpha1.PhaseDismissed)
	fnd.Spec.Alerts = nil
	if repo == "" {
		fnd.Spec.Repository = nil
	} else {
		fnd.Spec.Repository.Name = repo
	}
	for src, ids := range alerts {
		for _, id := range ids {
			fnd.Spec.Alerts = append(fnd.Spec.Alerts, v1alpha1.Alert{ID: id, Source: src})
		}
	}
	return fnd
}

func writebackReconciler(funcs *interceptor.Funcs, objs ...client.Object) *FindingReconciler {
	c := receiverClient(funcs, objs...)
	return &FindingReconciler{Client: c, Creds: NewCreds(c), Namespace: "patchy"}
}

// GHAS alerts are dismissed through the code-scanning Integration's client,
// by alert number, in the finding's repository.
func TestResolveAlertsGHAS(t *testing.T) {
	t.Run("dismisses each alert number", func(t *testing.T) {
		tracker := newFakeTracker()
		r := writebackReconciler(nil, testIntegration())
		var asked []ghclient.Repo
		r.ClientFor = func(_ context.Context, _ *v1alpha1.Integration, repo ghclient.Repo) (trackerClient, error) {
			asked = append(asked, repo)
			return tracker, nil
		}
		fnd := findingWithAlerts("acme/orders", map[string][]string{ghas.ID: {"9", "12"}})
		if err := r.resolveAlerts(t.Context(), fnd); err != nil {
			t.Fatalf("resolveAlerts() = %v", err)
		}
		if !slices.Equal(tracker.dismissed, []int{9, 12}) {
			t.Errorf("dismissed = %v, want [9 12]", tracker.dismissed)
		}
		if len(asked) != 1 || asked[0] != (ghclient.Repo{Owner: "acme", Name: "orders"}) {
			t.Errorf("client asked for %v, want acme/orders once", asked)
		}
	})

	t.Run("a repo-less finding dismisses nothing", func(t *testing.T) {
		tracker := newFakeTracker()
		r := writebackReconciler(nil, testIntegration())
		r.ClientFor = func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
			return tracker, nil
		}
		if err := r.resolveAlerts(t.Context(), findingWithAlerts("", map[string][]string{ghas.ID: {"9"}})); err != nil {
			t.Fatalf("resolveAlerts() = %v", err)
		}
		if len(tracker.dismissed) != 0 {
			t.Errorf("dismissed = %v, want none", tracker.dismissed)
		}
	})

	t.Run("an unreadable credential is an error", func(t *testing.T) {
		r := writebackReconciler(nil, testIntegration()) // no Secret, real Creds
		err := r.resolveAlerts(t.Context(), findingWithAlerts("acme/orders", map[string][]string{ghas.ID: {"9"}}))
		if err == nil || !strings.Contains(err.Error(), "get integration secret") {
			t.Errorf("resolveAlerts() = %v, want the credential failure", err)
		}
	})

	t.Run("a gone integration is skipped", func(t *testing.T) {
		r := writebackReconciler(nil)
		r.ClientFor = func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
			t.Fatal("client built with no code-scanning integration")
			return nil, nil
		}
		fnd := findingWithAlerts("acme/orders", map[string][]string{ghas.ID: {"9"}})
		if err := r.resolveAlerts(t.Context(), fnd); err != nil {
			t.Errorf("resolveAlerts() = %v, want nil", err)
		}
	})

	t.Run("ambiguous integrations are an error", func(t *testing.T) {
		a, b := testIntegration(), testIntegration()
		b.Name = "gh-2"
		r := writebackReconciler(nil, a, b)
		err := r.resolveAlerts(t.Context(), findingWithAlerts("acme/orders", map[string][]string{ghas.ID: {"9"}}))
		if !errors.Is(err, ErrAmbiguousIntegration) {
			t.Errorf("resolveAlerts() = %v, want ErrAmbiguousIntegration", err)
		}
	})
}

// fakeWiz is an in-process Wiz tenant: an OAuth token endpoint and the
// GraphQL endpoint, recording each rejected issue.
type fakeWiz struct {
	mu       sync.Mutex
	srv      *httptest.Server
	rejected []string
	clientID string
}

func newFakeWiz(t *testing.T) *fakeWiz {
	t.Helper()
	f := &fakeWiz{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("client_secret") != "wiz-secret" {
			respondJSON(w, http.StatusUnauthorized, `{"error":"access_denied"}`)
			return
		}
		f.mu.Lock()
		f.clientID = r.PostForm.Get("client_id")
		f.mu.Unlock()
		respondJSON(w, http.StatusOK, `{"access_token":"wiz-access","expires_in":3600}`)
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer wiz-access" {
			respondJSON(w, http.StatusUnauthorized, `{}`)
			return
		}
		var body struct {
			Variables struct {
				ID    string `json:"id"`
				Patch struct {
					Status string `json:"status"`
				} `json:"patch"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		if body.Variables.Patch.Status == "REJECTED" {
			f.rejected = append(f.rejected, body.Variables.ID)
		}
		f.mu.Unlock()
		respondJSON(w, http.StatusOK, `{"data":{"updateIssue":{"issue":{"id":"`+body.Variables.ID+`"}}}}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func wizWriteBackIntegration(api *v1alpha1.WizAPI) *v1alpha1.Integration {
	integ := testWizIntegration(true, false)
	integ.Spec.Wiz.API = api
	return integ
}

func wizAPISecret(id, secret string) *corev1.Secret {
	s := wizSecret("token")
	s.Data[wiz.KeyClientID] = []byte(id)
	s.Data[wiz.KeyClientSecret] = []byte(secret)
	return s
}

func TestResolveAlertsWiz(t *testing.T) {
	t.Run("rejects each issue through the Wiz API", func(t *testing.T) {
		w := newFakeWiz(t)
		integ := wizWriteBackIntegration(&v1alpha1.WizAPI{
			Endpoint: w.srv.URL + "/graphql", TokenURL: w.srv.URL + "/oauth/token",
		})
		r := writebackReconciler(nil, integ, wizAPISecret("wiz-id", "wiz-secret"))
		fnd := findingWithAlerts("", map[string][]string{wiz.IssuesID: {"issue-1", "issue-2"}})
		if err := r.resolveAlerts(t.Context(), fnd); err != nil {
			t.Fatalf("resolveAlerts() = %v", err)
		}
		slices.Sort(w.rejected)
		if !slices.Equal(w.rejected, []string{"issue-1", "issue-2"}) || w.clientID != "wiz-id" {
			t.Errorf("rejected %v as %q, want both issues as wiz-id", w.rejected, w.clientID)
		}
	})

	t.Run("without an API block ingestion stays one-way", func(t *testing.T) {
		r := writebackReconciler(nil, wizWriteBackIntegration(nil), wizSecret("token"))
		r.WizAPI = func(context.Context, *v1alpha1.Integration) (wiz.IssueRejecter, error) {
			t.Fatal("Wiz client built with write-back unconfigured")
			return nil, nil
		}
		if err := r.resolveAlerts(t.Context(), findingWithAlerts("", map[string][]string{wiz.IssuesID: {"i"}})); err != nil {
			t.Errorf("resolveAlerts() = %v, want nil", err)
		}
	})

	t.Run("missing API credentials are an error", func(t *testing.T) {
		integ := wizWriteBackIntegration(&v1alpha1.WizAPI{Endpoint: "https://wiz.invalid/graphql"})
		r := writebackReconciler(nil, integ, wizSecret("token"))
		err := r.resolveAlerts(t.Context(), findingWithAlerts("", map[string][]string{wiz.IssuesID: {"i"}}))
		if err == nil || !strings.Contains(err.Error(), wiz.KeyClientID) {
			t.Errorf("resolveAlerts() = %v, want the missing-credential error", err)
		}
	})

	t.Run("no wiz issues integration is skipped", func(t *testing.T) {
		r := writebackReconciler(nil)
		if err := r.resolveAlerts(t.Context(), findingWithAlerts("", map[string][]string{wiz.IssuesID: {"i"}})); err != nil {
			t.Errorf("resolveAlerts() = %v, want nil", err)
		}
	})
}

func genericResolverIntegration(name, url string) *v1alpha1.Integration {
	return testGenericIntegration(name, func(g *v1alpha1.GenericIntegration) {
		g.Source.Resolver = &v1alpha1.GenericResolver{Enabled: true, URL: url, Timeout: metav1.Duration{}}
	})
}

// A generic source's verdict is POSTed to its resolver, signed with that
// Integration's own secret.
func TestResolveAlertsGeneric(t *testing.T) {
	t.Run("posts a signed resolve request", func(t *testing.T) {
		var (
			mu  sync.Mutex
			got []pkggeneric.ResolveRequest
			bad int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			defer mu.Unlock()
			if r.Header.Get(pkggeneric.SignatureHeader) != generic.Sign([]byte("wh-secret"), body) {
				bad++
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var req pkggeneric.ResolveRequest
			_ = json.Unmarshal(body, &req)
			got = append(got, req)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(srv.Close)

		r := writebackReconciler(nil,
			genericResolverIntegration("warehouse", srv.URL), genericSecret("warehouse", "wh-secret"))
		fnd := findingWithAlerts("", map[string][]string{"warehouse": {"wh-1", "wh-2"}})
		if err := r.resolveAlerts(t.Context(), fnd); err != nil {
			t.Fatalf("resolveAlerts() = %v", err)
		}
		if bad != 0 || len(got) != 1 {
			t.Fatalf("resolver saw %d valid and %d badly signed requests, want 1 and 0", len(got), bad)
		}
		req := got[0]
		ids := make([]string, 0, len(req.Alerts))
		for _, a := range req.Alerts {
			ids = append(ids, a.ID)
		}
		if req.Integration != "warehouse" || !slices.Equal(ids, []string{"wh-1", "wh-2"}) {
			t.Errorf("request = %+v, want warehouse's two alerts", req)
		}
	})

	t.Run("an unreadable signing secret is an error", func(t *testing.T) {
		r := writebackReconciler(nil, genericResolverIntegration("warehouse", "https://wh.invalid/resolve"))
		err := r.resolveAlerts(t.Context(), findingWithAlerts("", map[string][]string{"warehouse": {"wh-1"}}))
		if err == nil || !strings.Contains(err.Error(), "get integration secret") {
			t.Errorf("resolveAlerts() = %v, want the secret failure", err)
		}
	})

	t.Run("a failed integration read is an error naming it", func(t *testing.T) {
		r := writebackReconciler(&interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*v1alpha1.Integration); ok {
					return errors.New("cache unavailable")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		err := r.resolveAlerts(t.Context(), findingWithAlerts("", map[string][]string{"warehouse": {"wh-1"}}))
		if err == nil || !strings.Contains(err.Error(), "get generic integration patchy/warehouse") {
			t.Errorf("resolveAlerts() = %v, want the read failure naming the integration", err)
		}
	})

	t.Run("a non-generic integration of that name has no write-back", func(t *testing.T) {
		other := testIntegration()
		other.Name = "warehouse"
		r := writebackReconciler(nil, other)
		fnd := findingWithAlerts("", map[string][]string{"warehouse": {"wh-1"}})
		if err := r.resolveAlerts(t.Context(), fnd); err != nil {
			t.Errorf("resolveAlerts() = %v, want nil", err)
		}
	})
}

// One source failing does not stop the others being told.
func TestResolveAlertsContinuesPastFailure(t *testing.T) {
	tracker := newFakeTracker()
	r := writebackReconciler(nil, testIntegration(), genericResolverIntegration("warehouse", "https://wh.invalid/resolve"))
	r.ClientFor = func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
		return tracker, nil
	}
	fnd := findingWithAlerts("acme/orders", map[string][]string{ghas.ID: {"9"}, "warehouse": {"wh-1"}})
	err := r.resolveAlerts(t.Context(), fnd)
	if err == nil || !strings.Contains(err.Error(), "get integration secret") {
		t.Errorf("resolveAlerts() = %v, want the generic source's failure", err)
	}
	if !slices.Equal(tracker.dismissed, []int{9}) {
		t.Errorf("dismissed = %v, want the ghas alert dismissed anyway", tracker.dismissed)
	}
}
