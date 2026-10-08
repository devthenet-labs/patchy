// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

const (
	relayIssuer = "https://preview-auth.acme.test"
	dexIssuer   = "https://dex.acme.test"
	dexClient   = "patchy-preview-auth"
)

// dexAuthQuery is what a single-connector Dex carries over to its
// connector's endpoint; githubAuthorize is where that endpoint sends a
// registered client and redirect URI (live Dex v2.45.1).
const (
	dexAuthQuery    = "client_id=" + dexClient + "&redirect_uri=x&response_type=code&scope=openid&state=s"
	githubAuthorize = "https://github.com/login/oauth/authorize?client_id=gh&state=req"
)

// answer is one canned HTTP answer, or the error the request fails with.
type answer struct {
	status   int
	location string
	body     string
	err      error
}

// fakeWeb answers by host and path (the query is recorded, not matched);
// any other URL fails as if nothing answered there.
type fakeWeb struct {
	mu      sync.Mutex
	answers map[string]answer
	asked   []*url.URL
}

func (f *fakeWeb) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, req.URL)
	a, ok := f.answers[req.URL.Host+req.URL.Path]
	switch {
	case !ok:
		return nil, fmt.Errorf("dial tcp: lookup %s: no such host", req.URL.Host)
	case a.err != nil:
		return nil, a.err
	}
	h := http.Header{}
	if a.location != "" {
		h.Set("Location", a.location)
	}
	return &http.Response{StatusCode: a.status, Header: h, Body: io.NopCloser(strings.NewReader(a.body))}, nil
}

// probeRedirect is the ALB's redirect of an unauthenticated request on
// label's host to the relay, for slot's client.
func probeRedirect(label string, slot int) answer {
	return answer{status: http.StatusFound, location: relayIssuer + "/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {previewauth.ClientID(slot)},
		"redirect_uri": {"https://" + label + "." + suffix + "/oauth2/idpresponse"}, "scope": {"openid"},
		"state": {"opaque"},
	}.Encode()}
}

// signInWorld is newWorld with preview sign-in required: the relay's
// ConfigMap, preview-controller requiring it, one Ready Preview of the
// Project in slot 1 beside one still deploying and one of another Project,
// and a web where the relay, Dex and every host answer as they should.
func signInWorld(t *testing.T) (*world, *fakeWeb) {
	w := newWorld(t)
	w.setting(previewController, keyAuthRequired, "true")
	w.objs = append(w.objs, w.configMap("patchy-preview-auth-config", previewAuth, map[string]string{
		keyAuthIssuer: relayIssuer, keyAuthDexIssuer: dexIssuer, keyAuthDexClientID: dexClient,
	}))
	preview := func(name, project string, phase v1alpha1.PreviewPhase, slot int32) *v1alpha1.Preview {
		return &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec:   v1alpha1.PreviewSpec{Project: project, HostLabel: name},
			Status: v1alpha1.PreviewStatus{Phase: phase, Slot: &slot}}
	}
	w.objs = append(w.objs, preview("shop-7", "shop", v1alpha1.PreviewReady, 1),
		preview("shop-8", "shop", v1alpha1.PreviewDeploying, 0), preview("blog-3", "blog", v1alpha1.PreviewReady, 0))
	web := &fakeWeb{answers: map[string]answer{
		"preview-auth.acme.test/.well-known/openid-configuration": {status: http.StatusOK,
			body: `{"issuer":"` + relayIssuer + `","authorization_endpoint":"` + relayIssuer + `/authorize"}`},
		"dex.acme.test/.well-known/openid-configuration": {status: http.StatusOK,
			body: `{"issuer":"` + dexIssuer + `","authorization_endpoint":"` + dexIssuer + `/auth"}`},
		"dex.acme.test/auth":          {status: http.StatusFound, location: "/auth/github?" + dexAuthQuery},
		"dex.acme.test/auth/github":   {status: http.StatusFound, location: githubAuthorize},
		"placeholder." + suffix + "/": probeRedirect("placeholder", 0),
		"shop-7." + suffix + "/":      probeRedirect("shop-7", 1),
		"shop-8." + suffix + "/":      {status: http.StatusOK, body: "the app, unprotected"},
		"blog-3." + suffix + "/":      {status: http.StatusOK, body: "another Project's app"},
	}}
	return w, web
}

// runWeb is world.run with the fake web.
func (w *world) runWeb(web HTTPDoer) Report {
	w.t.Helper()
	objs := append([]client.Object{w.project}, w.objs...)
	for _, f := range w.forges {
		objs = append(objs, f)
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
		WithInterceptorFuncs(noSecrets(w.t)).Build()
	report, err := Run(context.Background(), Config{
		Reader: c, Namespace: testNamespace, Project: w.project.Name, GitHub: w.github,
		Resolver: w.resolver, DialTLS: w.dialTLS, HTTP: web,
	})
	if err != nil {
		w.t.Fatalf("Run: %v", err)
	}
	return report
}

// authLines are a report's preview sign-in lines.
func authLines(r Report) []Check {
	var out []Check
	for _, c := range r.Checks {
		if strings.HasPrefix(c.Name, "preview-auth") {
			out = append(out, c)
		}
	}
	return out
}

// TestPreviewAuthOff: without the relay's ConfigMap and with nothing
// required, the report holds no sign-in line and nothing is fetched.
func TestPreviewAuthOff(t *testing.T) {
	w := newWorld(t)
	web := &fakeWeb{}
	r := w.runWeb(web)
	if got := authLines(r); len(got) != 0 {
		t.Errorf("sign-in lines while it is off:\n%s", dump(Report{Checks: got}))
	}
	if len(web.asked) != 0 {
		t.Errorf("fetched %v while sign-in is off", web.asked)
	}
}

// TestPreviewAuthAllPass: the relay answers, Dex accepts the redirect URI,
// and the placeholder and the Project's one Ready Preview redirect to the
// relay for their own slot; a Preview still deploying and another
// Project's are not probed.
func TestPreviewAuthAllPass(t *testing.T) {
	w, web := signInWorld(t)
	r := w.runWeb(web)
	got := authLines(r)
	want := []string{CheckPreviewAuth, CheckPreviewAuthDex, CheckPreviewAuthHost, CheckPreviewAuthHost}
	if len(got) != len(want) {
		t.Fatalf("%d sign-in lines, want %d:\n%s", len(got), len(want), dump(Report{Checks: got}))
	}
	for i, c := range got {
		if c.Name != want[i] || c.Status != checkreport.Pass {
			t.Errorf("line %d: %s %s: %s", i, c.Name, c.Status, c.Reason)
		}
	}
	if !strings.Contains(got[0].Reason, relayIssuer) ||
		!strings.Contains(got[1].Reason, "redirect URI "+relayIssuer+"/dex/callback") ||
		!strings.Contains(got[2].Reason, "placeholder."+suffix+" (slot 0)") ||
		!strings.Contains(got[3].Reason, "shop-7."+suffix+" (slot 1)") ||
		!strings.Contains(got[3].Reason, previewauth.ClientID(1)) {
		t.Errorf("reasons:\n%s", dump(Report{Checks: got}))
	}
	if n := r.Failed(); n != 0 {
		t.Errorf("Failed = %d:\n%s", n, dump(r))
	}
	for _, u := range web.asked {
		if u.Host == "dex.acme.test" && u.Path == "/auth" {
			q := u.Query()
			if q.Get("client_id") != dexClient || q.Get("redirect_uri") != relayIssuer+"/dex/callback" ||
				q.Get("response_type") != "code" || q.Get("scope") != "openid" {
				t.Errorf("Dex was asked %s", u)
			}
		}
		if strings.HasPrefix(u.Host, "shop-8.") || strings.HasPrefix(u.Host, "blog-3.") {
			t.Errorf("probed %s", u)
		}
	}
}

// TestDexCheckStaysOnDexHost: the Dex check follows a single-connector
// Dex's redirect to its connector endpoint, carrying the query, and never
// the connector's redirect off Dex's host.
func TestDexCheckStaysOnDexHost(t *testing.T) {
	w, web := signInWorld(t)
	w.runWeb(web)
	var paths []string
	for _, u := range web.asked {
		if u.Host == "github.com" {
			t.Errorf("followed Dex's redirect off its host to %s", u)
		}
		if u.Host == "dex.acme.test" {
			paths = append(paths, u.Path)
		}
		if u.Path == "/auth/github" && u.Query().Get("client_id") != dexClient {
			t.Errorf("the connector endpoint was asked %s", u)
		}
	}
	if got := strings.Join(paths, " "); got != "/.well-known/openid-configuration /auth /auth/github" {
		t.Errorf("asked Dex for %s", got)
	}
}

// TestPreviewAuthFailures: each part of sign-in that is wrong fails or
// skips its own line, with a reason that says what to fix.
func TestPreviewAuthFailures(t *testing.T) {
	timeout := answer{err: timeoutErr{}}
	tests := map[string]struct {
		change func(*world, *fakeWeb)
		check  string
		status checkreport.Status
		want   []string
	}{
		"permit stage": {
			change: func(w *world, _ *fakeWeb) { w.setting(previewController, keyAuthRequired, "") },
			check:  CheckPreviewAuthHost, status: checkreport.Skip, want: []string{"previewAuth.stage=permit"},
		},
		"required without the relay": {
			change: func(w *world, _ *fakeWeb) {
				w.drop(func(o client.Object) bool { return o.GetName() == "patchy-preview-auth-config" })
			},
			check: CheckPreviewAuth, status: checkreport.Fail, want: []string{"previewAuth.enabled"},
		},
		"issuer not https": {
			change: func(w *world, _ *fakeWeb) { w.setting(previewAuth, keyAuthIssuer, "http://relay.acme.test") },
			check:  CheckPreviewAuth, status: checkreport.Fail, want: []string{"not an https origin"},
		},
		"relay unreachable": {
			change: func(_ *world, f *fakeWeb) {
				delete(f.answers, "preview-auth.acme.test/.well-known/openid-configuration")
			},
			check: CheckPreviewAuth, status: checkreport.Fail, want: []string{"does not answer",
				"reachable from the internet"},
		},
		"relay route serves something else": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["preview-auth.acme.test/.well-known/openid-configuration"] = answer{status: 404}
			},
			check: CheckPreviewAuth, status: checkreport.Fail, want: []string{"answered 404", "preview-auth Service"},
		},
		"another issuer's discovery": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["preview-auth.acme.test/.well-known/openid-configuration"] = answer{status: 200,
					body: `{"issuer":"https://other.test","authorization_endpoint":"https://other.test/authorize"}`}
			},
			check: CheckPreviewAuth, status: checkreport.Fail, want: []string{`"https://other.test"`},
		},
		"redirect URI not registered": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth"] = answer{status: http.StatusBadRequest, body: "<html><head>" +
					"<title>dex</title></head><body><h2>Bad Request</h2><p>Unregistered redirect_uri (&#34;" +
					relayIssuer + "/dex/callback&#34;).</p></body></html>"}
			},
			check: CheckPreviewAuthDex, status: checkreport.Fail, want: []string{"refuses redirect URI " + relayIssuer +
				"/dex/callback", "Unregistered redirect_uri", "only per-install IdP setup"},
		},
		"no such client": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth"] = answer{status: http.StatusNotFound,
					body: "<p>Invalid client_id (&#34;patchy-preview-auth&#34;).</p>"}
			},
			check: CheckPreviewAuthDex, status: checkreport.Fail, want: []string{"Dex has no client " + dexClient,
				"Invalid client_id"},
		},
		"single connector, redirect URI not registered": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth/github"] = answer{status: http.StatusBadRequest,
					body: "<h2>Bad Request</h2><p>Unregistered redirect_uri.</p>"}
			},
			check: CheckPreviewAuthDex, status: checkreport.Fail, want: []string{"refuses redirect URI " + relayIssuer +
				"/dex/callback", "Unregistered redirect_uri"},
		},
		"single connector, no such client": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth/github"] = answer{status: http.StatusNotFound,
					body: "<h2>Not Found</h2><p>Invalid client_id.</p>"}
			},
			check: CheckPreviewAuthDex, status: checkreport.Fail, want: []string{"Dex has no client " + dexClient,
				"Invalid client_id"},
		},
		"Dex redirects straight off its host": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth"] = answer{status: http.StatusFound, location: githubAuthorize}
				delete(f.answers, "dex.acme.test/auth/github")
			},
			check: CheckPreviewAuthDex, status: checkreport.Pass, want: []string{"https://github.com"},
		},
		"Dex redirects by absolute URL on its host": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth"] = answer{status: http.StatusFound,
					location: dexIssuer + "/auth/github?" + dexAuthQuery}
			},
			check: CheckPreviewAuthDex, status: checkreport.Pass, want: []string{"https://github.com"},
		},
		"Dex redirect loop": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth/github"] = answer{status: http.StatusFound, location: "/auth?" + dexAuthQuery}
			},
			check: CheckPreviewAuthDex, status: checkreport.Skip, want: []string{"redirect loop"},
		},
		"Dex redirects too often": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth/github"] = answer{status: http.StatusFound, location: "/hop1"}
				for i := 1; i <= dexMaxHops; i++ {
					f.answers[fmt.Sprintf("dex.acme.test/hop%d", i)] = answer{status: http.StatusFound,
						location: fmt.Sprintf("/hop%d", i+1)}
				}
			},
			check: CheckPreviewAuthDex, status: checkreport.Skip, want: []string{"more than", "redirects"},
		},
		"Dex redirects to another scheme on its host": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth"] = answer{status: http.StatusFound, location: "http://dex.acme.test/auth/github"}
				f.answers["dex.acme.test/auth/github"] = answer{status: http.StatusBadRequest,
					body: "<p>Unregistered redirect_uri.</p>"}
			},
			check: CheckPreviewAuthDex, status: checkreport.Pass, want: []string{"http://dex.acme.test"},
		},
		"Dex shows its connector page": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/auth"] = answer{status: http.StatusOK, body: "Log in with GitHub"}
			},
			check: CheckPreviewAuthDex, status: checkreport.Pass, want: []string{"answered 200"},
		},
		"Dex unreachable": {
			change: func(_ *world, f *fakeWeb) { f.answers["dex.acme.test/.well-known/openid-configuration"] = timeout },
			check:  CheckPreviewAuthDex, status: checkreport.Skip, want: []string{"cannot reach Dex"},
		},
		"not Dex's issuer": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["dex.acme.test/.well-known/openid-configuration"] = answer{status: 404}
			},
			check: CheckPreviewAuthDex, status: checkreport.Fail, want: []string{"is " + dexIssuer + " Dex's issuer?"},
		},
		"no Dex settings": {
			change: func(w *world, _ *fakeWeb) { w.setting(previewAuth, keyAuthDexClientID, "") },
			check:  CheckPreviewAuthDex, status: checkreport.Fail, want: []string{"sets no"},
		},
		"host serves the app": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["shop-7."+suffix+"/"] = answer{status: http.StatusOK, body: "the app"}
			},
			check: CheckPreviewAuthHost, status: checkreport.Fail, want: []string{"shop-7." + suffix, "answered 200",
				"describe-rules"},
		},
		"host redirects for another slot": {
			change: func(_ *world, f *fakeWeb) { f.answers["shop-7."+suffix+"/"] = probeRedirect("shop-7", 0) },
			check:  CheckPreviewAuthHost, status: checkreport.Fail, want: []string{previewauth.ClientID(1)},
		},
		"host behind the allowlist": {
			change: func(_ *world, f *fakeWeb) { f.answers["shop-7."+suffix+"/"] = timeout },
			check:  CheckPreviewAuthHost, status: checkreport.Skip, want: []string{"preview.inboundCIDRs"},
		},
		"host TLS refused": {
			change: func(_ *world, f *fakeWeb) {
				f.answers["shop-7."+suffix+"/"] = answer{err: errors.New("tls: failed to verify certificate")}
			},
			check: CheckPreviewAuthHost, status: checkreport.Fail, want: []string{"failed to verify"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w, web := signInWorld(t)
			tc.change(w, web)
			r := w.runWeb(web)
			for _, c := range authLines(r) {
				if c.Name == tc.check && c.Status == tc.status && containsAll(c.Reason, tc.want) {
					return
				}
			}
			t.Errorf("no %s %s line with %q:\n%s", tc.status, tc.check, tc.want, dump(Report{Checks: authLines(r)}))
		})
	}
}

func containsAll(s string, want []string) bool {
	for _, w := range want {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}

// TestExcerpt quotes the sentence holding the key, bounded in runes.
func TestExcerpt(t *testing.T) {
	tests := []struct{ text, key, want string }{
		{"dex Bad Request Unregistered redirect_uri (x).", "redirect_uri",
			"dex Bad Request Unregistered redirect_uri (x)."},
		{"Title. Invalid client_id (y).", "client_id", "Invalid client_id (y)."},
		{"no key here", "client_id", "no key here"},
		{strings.Repeat("é", 200), "", strings.Repeat("é", excerptRunes) + "..."},
	}
	for _, tc := range tests {
		if got := excerpt(tc.text, tc.key); got != tc.want {
			t.Errorf("excerpt(%q, %q) = %q, want %q", tc.text, tc.key, got, tc.want)
		}
	}
	if got := pageText([]byte("<p>a\n  <b>b</b></p>\tc")); got != "a b c" {
		t.Errorf("pageText = %q", got)
	}
}
