// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/keydir"
	"github.com/bitwise-media-group/patchy/internal/previewauth/fakedex"
)

// The installation TestPreviewSignIn renders: the relay's public issuer, the
// preview host suffix, and the Dex client the relay signs viewers in with.
const (
	signInIssuer     = "https://preview-auth.patchy.example.com"
	signInRelayHost  = "preview-auth.patchy.example.com"
	signInSuffix     = "preview.patchy.example.com"
	signInDexClient  = "patchy-preview-auth"
	signInDexSecret  = "e2e-dex-secret"
	signInLease      = "patchy-preview-auth-codes"
	signInSecretName = "patchy-preview-oidc-g1"
)

// TestPreviewSignIn drives the shipped preview-auth and preview-controller
// binaries together, as the chart's require stage runs them: the
// controller renders the slot's pinned sign-in annotations onto the live
// Preview's Ingress, and the relay signs a viewer in for that Preview
// through a TLS Dex (fakedex behind its own CA, trusted through the relay's
// --preview-auth-dex-ca-file) and the access review the envtest API server
// answers from real RBAC. A test client stands in for the ALB at exactly its
// two edges: the browser redirects it issues and its backchannel calls to
// /token and /userinfo. An unbound viewer is refused with a page, a code
// redeems once, and ending the Intent (so the Preview) ends refresh.
func TestPreviewSignIn(t *testing.T) {
	cl := startCluster(t)
	ctx := context.Background()
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}

	dex, err := fakedex.StartTLS(signInDexClient, signInDexSecret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dex.Close)
	keysDir, caFile, secretFile := signInFiles(t, dex)
	keys, err := keydir.Load(keysDir)
	if err != nil {
		t.Fatal(err)
	}
	grantPreviewViewer(t, cl, "github:alice")
	if err := cl.client.Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: signInLease, Namespace: namespace},
	}); err != nil {
		t.Fatal(err)
	}

	pinned := map[string]map[string]string{}
	for slot := range 2 {
		pinned[fmt.Sprintf("patchy-preview-%d", slot)] = pinnedSet(slot)
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		t.Fatal(err)
	}
	cl.controller(t, "preview-controller",
		"--preview-slot-count", "2",
		"--preview-image-prefix", "registry.example/patchy/previews/",
		"--preview-host-suffix", signInSuffix,
		"--preview-node-pool", "patchy-preview", "--preview-node-class", "patchy-preview",
		"--preview-taint-key", "patchy.devthe.net/preview-only",
		"--preview-poll-interval", "1s",
		"--preview-auth-required", "--preview-auth-annotations", string(pinnedJSON))
	relayAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.controller(t, "preview-auth", "--listen-addr", relayAddr,
		"--preview-auth-issuer", signInIssuer, "--preview-auth-host-suffix", signInSuffix,
		"--preview-auth-slot-count", "2", "--preview-auth-keys-dir", keysDir,
		"--preview-auth-dex-issuer-url", dex.URL, "--preview-auth-dex-client-id", signInDexClient,
		"--preview-auth-dex-client-secret-file", secretFile, "--preview-auth-dex-ca-file", caFile,
		"--preview-auth-username-prefix", "github:", "--preview-auth-groups-prefix", "github:",
		"--preview-auth-ledger-lease", signInLease,
		// The test client is every request's one address, and envtest has
		// no preview host to probe.
		"--preview-auth-rate-per-second", "0", "--preview-auth-probe-interval", "0s")

	in, p, pod := readyPreview(t, cl)

	// The live Preview's Ingress carries exactly slot 0's pinned set.
	var ingress networkingv1.Ingress
	if err := cl.client.Get(ctx, types.NamespacedName{Namespace: "patchy-preview-0", Name: "preview-demo-1"},
		&ingress); err != nil {
		t.Fatal(err)
	}
	for k, v := range pinnedSet(0) {
		if ingress.Annotations[k] != v {
			t.Errorf("Ingress annotation %s = %q, want %q", k, ingress.Annotations[k], v)
		}
	}

	alb := &albClient{t: t, relay: relayAddr, secret: keys.Ring.ClientSecret(0), base: dex.Client().Transport}

	// alice signs in through Dex once, and the code reaches the preview host.
	dex.SignIn(map[string]any{"sub": "dex-alice", "preferred_username": "alice", "groups": []any{"acme:qa"}})
	browser := alb.browser()
	code := alb.signIn(browser, "state-1", "nonce-1")
	tok := alb.redeem(code)
	if tok.TokenType != "Bearer" || tok.AccessToken == "" || tok.RefreshToken == "" || tok.ExpiresIn <= 0 {
		t.Fatalf("token response %+v", tok)
	}
	claims := idTokenClaims(t, tok.IDToken)
	if claims["iss"] != signInIssuer || claims["aud"] != previewauth.ClientID(0) || claims["nonce"] != "nonce-1" {
		t.Fatalf("ID token claims %v", claims)
	}
	sub, _ := claims["sub"].(string)
	for _, leak := range []string{"preferred_username", "email", "groups", "name"} {
		if _, ok := claims[leak]; ok {
			t.Errorf("the ID token carries %s", leak)
		}
	}
	if sub == "" || strings.Contains(sub, "alice") {
		t.Fatalf("ID token sub %q is not an opaque pairwise value", sub)
	}
	if status, body := alb.userinfo(tok.AccessToken); status != http.StatusOK ||
		strings.TrimSpace(body) != `{"sub":"`+sub+`"}` {
		t.Fatalf("userinfo: %d %s", status, body)
	}
	if status, body := alb.token(url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {callbackURL()}}); status != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Fatalf("a replayed code: %d %s", status, body)
	}
	before := dex.Authorizations()
	if second := alb.signIn(browser, "state-2", "nonce-2"); second == "" || dex.Authorizations() != before {
		t.Fatal("a second sign-in in the same browser went back to Dex")
	}
	refreshed := alb.refresh(tok.RefreshToken)
	if refreshed.status != http.StatusOK {
		t.Fatalf("refresh of a live Preview: %d %s", refreshed.status, refreshed.body)
	}

	// mallory signs in at Dex but is bound to nothing: a page, no code.
	dex.SignIn(map[string]any{"sub": "dex-mallory", "preferred_username": "mallory", "groups": []any{"acme:x"}})
	if status, location := alb.refusedSignIn(alb.browser()); status != http.StatusForbidden || location != "" {
		t.Fatalf("an unbound viewer: status %d, Location %q", status, location)
	}

	// The Intent ends, the Preview goes, and its tokens stop working. envtest
	// has no Deployment controller to delete the synthetic Pod, and slot
	// cleanup waits for it.
	if err := cl.client.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentMerged
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Preview deleted", func() bool {
		return apierrors.IsNotFound(cl.client.Get(ctx, client.ObjectKeyFromObject(p), &v1alpha1.Preview{}))
	})
	eventually(t, "refresh refused once the Preview is gone", func() bool {
		r := alb.refresh(tok.RefreshToken)
		return r.status == http.StatusBadRequest && strings.Contains(r.body, "invalid_grant")
	})
	if status, _ := alb.userinfo(tok.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("userinfo after the Preview ended: %d", status)
	}
}

// pinnedSet is slot's six sign-in annotations, as the chart renders them.
func pinnedSet(slot int) map[string]string {
	idp := fmt.Sprintf(`{"issuer":%q,"authorizationEndpoint":%q,"tokenEndpoint":%q,"userInfoEndpoint":%q,`+
		`"secretName":%q}`, signInIssuer, signInIssuer+"/authorize", signInIssuer+"/token",
		signInIssuer+"/userinfo", signInSecretName)
	return map[string]string{
		"alb.ingress.kubernetes.io/auth-type":                       "oidc",
		"alb.ingress.kubernetes.io/auth-idp-oidc":                   idp,
		"alb.ingress.kubernetes.io/auth-on-unauthenticated-request": "authenticate",
		"alb.ingress.kubernetes.io/auth-scope":                      "openid",
		"alb.ingress.kubernetes.io/auth-session-cookie":             previewauth.ALBCookieName(slot),
		"alb.ingress.kubernetes.io/auth-session-timeout":            "3600",
	}
}

// signInFiles writes what the chart mounts into the relay: the keys
// directory (a fresh master and signing key of generation 1), Dex's CA
// bundle and the relay's Dex client secret.
func signInFiles(t *testing.T, dex *fakedex.Server) (keysDir, caFile, secretFile string) {
	t.Helper()
	dir := t.TempDir()
	keysDir = filepath.Join(dir, "keys")
	if err := os.Mkdir(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	signing, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		filepath.Join(keysDir, keydir.FileMaster):     []byte(strings.Repeat("e2e0", 16)),
		filepath.Join(keysDir, keydir.FileGeneration): []byte("1"),
		filepath.Join(keysDir, keydir.FileSigningKey): pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(signing)}),
		filepath.Join(dir, "dex-ca.pem"): pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE",
			Bytes: dex.Certificate().Raw}),
		filepath.Join(dir, "dex-secret"): []byte(signInDexSecret + "\n"),
	}
	for path, data := range files {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return keysDir, filepath.Join(dir, "dex-ca.pem"), filepath.Join(dir, "dex-secret")
}

// grantPreviewViewer binds user to the preview-viewer role the chart renders
// (get on projects/previews, every Project), in the release namespace.
func grantPreviewViewer(t *testing.T, cl *cluster, user string) {
	t.Helper()
	ctx := context.Background()
	if err := cl.client.Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "patchy-preview-viewer"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{v1alpha1.GroupVersion.Group},
			Resources: []string{"projects/previews"}, Verbs: []string{"get"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "patchy-preview-viewers", Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "patchy-preview-viewer"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
	}); err != nil {
		t.Fatal(err)
	}
}

// readyPreview creates the demo Project, its Intent in review and the
// Preview intent-controller would write, then plays the Deployment
// controller and kubelet until preview-controller reports it Ready. It
// returns the synthetic Pod, which the test deletes before the Preview ends.
func readyPreview(t *testing.T, cl *cluster) (*v1alpha1.Intent, *v1alpha1.Preview, *corev1.Pod) {
	t.Helper()
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: namespace},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "demo", URL: "https://github.com/acme/demo"}},
			Preview: &v1alpha1.ProjectPreview{ImageRepository: "registry.example/patchy/previews/demo",
				Port: 8080, ReadinessPath: "/health"},
		}}
	if err := cl.client.Create(ctx, project); err != nil {
		t.Fatal(err)
	}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: namespace},
		Spec: v1alpha1.IntentSpec{Project: "demo",
			Issue:       v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 1},
			RequestedBy: v1alpha1.IntentRequest{Login: "octocat", At: metav1.Now(), EventID: 1}}}
	if err := cl.client.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentInReview
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{{
		Repository: "https://github.com/acme/demo", Number: 1, State: "open", HeadSHA: sha,
	}}
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	p := &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Intent",
			Name: in.Name, UID: in.UID, Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}}},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID}, Project: "demo", HostLabel: in.Name,
			Components: []v1alpha1.PreviewComponent{{Name: "demo",
				ImageRepository: "registry.example/patchy/previews/demo", Revision: sha,
				Port: 8080, ReadinessPath: "/health"}}, TTL: metav1.Duration{Duration: 72 * time.Hour},
		}}
	if err := cl.client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "patchy-preview-0", Name: "preview-demo-1"}
	var dep appsv1.Deployment
	eventually(t, "the preview Deployment in slot 0", func() bool { return cl.client.Get(ctx, key, &dep) == nil })
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1, 1
	if err := cl.client.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "preview-demo-1-pod", Namespace: key.Namespace,
		Labels: dep.Spec.Template.Labels}, Spec: dep.Spec.Template.Spec}
	if err := cl.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "demo", Ready: true, ImageID: "repo@sha256:123"}}
	if err := cl.client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Preview Ready behind its Ingress", func() bool {
		var current v1alpha1.Preview
		return cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current) == nil &&
			current.Status.Phase == v1alpha1.PreviewReady &&
			cl.client.Get(ctx, key, &networkingv1.Ingress{}) == nil
	})
	return in, p, pod
}

// callbackURL is the demo-1 host's ALB callback, the only redirect URI the
// relay sends its codes to.
func callbackURL() string { return "https://demo-1." + signInSuffix + "/oauth2/idpresponse" }

// albClient plays the preview ALB: it sends browsers to the relay's
// /authorize with slot 0's client, and calls /token and /userinfo on its
// backchannel with that client's secret. Requests for the relay's public
// host go to the relay's plain listener (the edge ALB's TLS is not under
// test); every other request is the browser's, to Dex over TLS.
type albClient struct {
	t      *testing.T
	relay  string
	secret string
	base   http.RoundTripper
}

// RoundTrip routes the relay's host to its listener.
func (a *albClient) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != signInRelayHost {
		return a.base.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = "http", a.relay
	return a.base.RoundTrip(r)
}

// browser is a fresh browser: its own cookie jar, redirects shown, not
// followed.
func (a *albClient) browser() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		a.t.Fatal(err)
	}
	return &http.Client{Transport: a, Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (a *albClient) authorizeURL(state, nonce string) string {
	return signInIssuer + "/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {previewauth.ClientID(0)}, "redirect_uri": {callbackURL()},
		"scope": {"openid"}, "state": {state}, "nonce": {nonce},
	}.Encode()
}

// get is one browser navigation.
func (a *albClient) get(b *http.Client, u string) (int, string, string) {
	a.t.Helper()
	resp, err := b.Get(u)
	if err != nil {
		a.t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

// signIn follows the browser from the ALB's redirect, through Dex if the
// browser has no relay session, back to the preview host's callback, and
// returns the code, checking the state comes back byte for byte.
func (a *albClient) signIn(b *http.Client, state, nonce string) string {
	a.t.Helper()
	status, location, body := a.get(b, a.authorizeURL(state, nonce))
	if status != http.StatusFound {
		a.t.Fatalf("authorize: %d %s", status, body)
	}
	if !strings.HasPrefix(location, callbackURL()+"?") {
		if status, location, body = a.get(b, location); status != http.StatusSeeOther {
			a.t.Fatalf("Dex: %d %s", status, body)
		}
		if status, location, body = a.get(b, location); status != http.StatusFound {
			a.t.Fatalf("the relay's Dex callback: %d %s", status, body)
		}
	}
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, callbackURL()+"?") {
		a.t.Fatalf("the code went to %q", location)
	}
	if got := u.Query().Get("state"); got != state {
		a.t.Fatalf("state %q came back as %q", state, got)
	}
	return u.Query().Get("code")
}

// refusedSignIn signs a browser in at Dex and returns the relay's answer to
// the callback, which must be a page, never a redirect.
func (a *albClient) refusedSignIn(b *http.Client) (int, string) {
	a.t.Helper()
	_, location, _ := a.get(b, a.authorizeURL("state-x", "nonce-x"))
	_, location, _ = a.get(b, location)
	status, location, _ := a.get(b, location)
	return status, location
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
}

// token is one backchannel call to /token with slot 0's client secret.
func (a *albClient) token(form url.Values) (int, string) {
	a.t.Helper()
	req, err := http.NewRequest(http.MethodPost, signInIssuer+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(previewauth.ClientID(0), a.secret)
	resp, err := (&http.Client{Transport: a, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (a *albClient) redeem(code string) tokenResponse {
	a.t.Helper()
	status, body := a.token(url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {callbackURL()}})
	if status != http.StatusOK {
		a.t.Fatalf("redeem: %d %s", status, body)
	}
	var tok tokenResponse
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		a.t.Fatal(err)
	}
	return tok
}

type refreshResult struct {
	status int
	body   string
}

func (a *albClient) refresh(refreshToken string) refreshResult {
	status, body := a.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
	return refreshResult{status: status, body: body}
}

// userinfo is the ALB's userinfo call with the access token.
func (a *albClient) userinfo(accessToken string) (int, string) {
	a.t.Helper()
	req, err := http.NewRequest(http.MethodGet, signInIssuer+"/userinfo", nil)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := (&http.Client{Transport: a, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// idTokenClaims reads an ID token's claims (its signature is the relay's
// concern and the httpapi flow test's, against the relay's JWKS).
func idTokenClaims(t *testing.T, raw string) map[string]any {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("ID token %q is not a JWS", raw)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}
