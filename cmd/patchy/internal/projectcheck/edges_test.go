// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// cannedAPI answers each path with a fixed status, body and (optional)
// declared Content-Length.
type cannedAPI map[string]struct {
	status int
	body   string
	length int64
}

func (c cannedAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a, ok := c[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if a.length > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(a.length))
	}
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, a.body)
}

func cannedGitHub(t *testing.T, api cannedAPI) *HTTPGitHub {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return &HTTPGitHub{APIURL: srv.URL}
}

func TestHTTPGitHubHeadFailures(t *testing.T) {
	const repo = "/repos/acme/web"
	cases := []struct {
		name string
		api  cannedAPI
		want string
	}{
		{
			name: "repository answer without a default branch",
			api:  cannedAPI{repo: {body: `{"name":"web"}`}},
			want: "without a default branch",
		},
		{
			name: "repository answer that is not JSON",
			api:  cannedAPI{repo: {body: `<html>`}},
			want: "without a default branch",
		},
		{
			name: "repository read fails",
			api:  cannedAPI{repo: {status: http.StatusInternalServerError}},
			want: "500 Internal Server Error",
		},
		{
			name: "head commit read fails",
			api: cannedAPI{
				repo:                   {body: `{"default_branch":"main"}`},
				repo + "/commits/main": {status: http.StatusBadGateway},
			},
			want: "502 Bad Gateway",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := cannedGitHub(t, c.api)
			head, err := g.Head(context.Background(), "https://github.com/acme/web")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Head = %+v, %v; want error containing %q", head, err, c.want)
			}
			if head != (Head{}) {
				t.Errorf("head = %+v alongside an error", head)
			}
		})
	}
}

func TestHTTPGitHubFileEdges(t *testing.T) {
	const trees = "/repos/acme/web/git/trees/"
	const blobs = "/repos/acme/web/git/blobs/"
	ref := sha("c")
	listing := func(size int) string {
		return fmt.Sprintf(`{"truncated":false,"tree":[{"path":"agent.yaml","mode":"100644","type":"blob",`+
			`"sha":"b1","size":%d}]}`, size)
	}
	cases := []struct {
		name      string
		api       cannedAPI
		path      string
		limit     int64
		wantData  string
		wantSize  int64
		wantFound bool
		wantErr   string
	}{
		{
			name: "root-level file", path: "agent.yaml", limit: 100,
			api:      cannedAPI{trees + ref: {body: listing(5)}, blobs + "b1": {body: "hello"}},
			wantData: "hello", wantSize: 5, wantFound: true,
		},
		{
			name: "listing says too large", path: ".patchy/agent.yaml", limit: 4,
			api:      cannedAPI{trees + ref + ":.patchy": {body: listing(5000)}},
			wantSize: 5000, wantFound: true,
		},
		{
			name: "blob turns out larger than the listing said", path: ".patchy/agent.yaml", limit: 5,
			api: cannedAPI{
				trees + ref + ":.patchy": {body: listing(3)},
				blobs + "b1":             {body: "0123456789", length: 10},
			},
			wantSize: 10, wantFound: true,
		},
		{
			name: "tree read fails", path: ".patchy/agent.yaml", limit: 100,
			api:     cannedAPI{trees + ref + ":.patchy": {status: http.StatusForbidden}},
			wantErr: "403 Forbidden",
		},
		{
			name: "tree answer is not JSON", path: ".patchy/agent.yaml", limit: 100,
			api:     cannedAPI{trees + ref + ":.patchy": {body: "nope"}},
			wantErr: "without a tree",
		},
		{
			name: "blob read fails", path: ".patchy/agent.yaml", limit: 100,
			api: cannedAPI{
				trees + ref + ":.patchy": {body: listing(5)},
				blobs + "b1":             {status: http.StatusInternalServerError},
			},
			wantErr: "500 Internal Server Error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := cannedGitHub(t, c.api)
			data, size, found, err := g.File(context.Background(), "https://github.com/acme/web", ref, c.path, c.limit)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("File err = %v, want %q", err, c.wantErr)
				}
				if found || data != nil {
					t.Errorf("File = %q, found %v alongside an error", data, found)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != c.wantData || size != c.wantSize || found != c.wantFound {
				t.Errorf("File = %q, %d, %v; want %q, %d, %v", data, size, found, c.wantData, c.wantSize, c.wantFound)
			}
			if c.wantData == "" && data != nil {
				t.Errorf("over-limit file returned data %q", data)
			}
		})
	}
}

// TestHTTPGitHubTokenWithoutSource: a token whose source is unnamed is still
// said to have been sent, and a refusal carries the dotcom hint.
func TestHTTPGitHubTokenWithoutSource(t *testing.T) {
	g := cannedGitHub(t, cannedAPI{"/repos/acme/web": {status: http.StatusUnauthorized}})
	g.Token = "gho_x"
	_, err := g.Head(context.Background(), "https://github.com/acme/web")
	if err == nil || !strings.Contains(err.Error(), "(with your token)") ||
		!strings.Contains(err.Error(), "GH_TOKEN or GITHUB_TOKEN") {
		t.Fatalf("Head = %v", err)
	}
}

func TestHTTPGitHubTransportFailures(t *testing.T) {
	failing := &HTTPGitHub{APIURL: "https://api.example.test", Client: &http.Client{
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		}),
	}}
	if _, err := failing.Head(context.Background(), "https://github.com/acme/web"); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Head over a failing transport = %v", err)
	}

	broken := &HTTPGitHub{APIURL: "https://api.example.test", Client: &http.Client{
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(failingReader{}), Header: http.Header{}}, nil
		}),
	}}
	if _, err := broken.Head(context.Background(), "https://github.com/acme/web"); err == nil ||
		!strings.Contains(err.Error(), "body went away") {
		t.Errorf("Head with an unreadable body = %v", err)
	}

	badRoot := &HTTPGitHub{APIURL: "https://api.example.test/\x7f"}
	if _, err := badRoot.Head(context.Background(), "https://github.com/acme/web"); err == nil {
		t.Error("Head with an unparseable API root succeeded")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("body went away") }

func TestHostName(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"  ":                              "",
		"ghe.acme.test":                   "ghe.acme.test",
		"GHE.Acme.Test:8443":              "ghe.acme.test",
		"https://GHE.acme.test/some/path": "ghe.acme.test",
		"http://[::1":                     "",
	}
	for in, want := range cases {
		if got := hostName(in); got != want {
			t.Errorf("hostName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSmallHelpers(t *testing.T) {
	if shortSHA("abc") != "abc" || shortSHA(sha("d")) != strings.Repeat("d", 12) {
		t.Error("shortSHA")
	}
	if plural(1) != "" || plural(0) != "s" || plural(2) != "s" {
		t.Error("plural")
	}
	if got := orNone("", "patchy"); !strings.Contains(got, "namespace patchy") {
		t.Errorf("orNone(empty) = %q", got)
	}
	if got := orNone("patchy/intent-controller", "patchy"); got != "patchy/intent-controller" {
		t.Errorf("orNone(named) = %q", got)
	}
	cases := []struct {
		err  error
		want transport.ErrorCode
	}{
		{errors.New("dial tcp: refused"), ""},
		{&transport.Error{StatusCode: http.StatusNotFound}, "404"},
		{&transport.Error{StatusCode: http.StatusUnauthorized}, ""},
		{fmt.Errorf("wrapped: %w", &transport.Error{
			Errors: []transport.Diagnostic{{Code: transport.ManifestUnknownErrorCode}},
		}), transport.ManifestUnknownErrorCode},
	}
	for _, c := range cases {
		if got := registryCode(c.err); got != c.want {
			t.Errorf("registryCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	u := func(s string) *url.URL {
		parsed, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	ports := map[string]string{"https://x": "443", "HTTP://x": "80", "https://x:8443": "8443", "ftp://x": ""}
	for raw, want := range ports {
		if got := effectivePort(u(raw)); got != want {
			t.Errorf("effectivePort(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestKeychainAndFetch(t *testing.T) {
	r := &run{}
	if r.keychain() == nil {
		t.Fatal("no keychain without configuration")
	}
	kc := authn.NewMultiKeychain(authn.DefaultKeychain)
	r.cfg.Keychain = kc
	if r.keychain() != kc {
		t.Error("configured keychain not used")
	}
	if _, err := r.fetch(context.Background(), "UPPER/Case:bad tag"); err == nil {
		t.Error("fetch of an unparseable reference succeeded")
	}
}

// TestJudgeDex: each shape of Dex's final answer maps onto its verdict.
func TestJudgeDex(t *testing.T) {
	cases := []struct {
		name   string
		status int
		page   string
		want   checkreport.Status
		reason string
	}{
		{"accepted", http.StatusOK, "<html>login</html>", checkreport.Pass, "accepts client"},
		{"bad redirect", http.StatusBadRequest, "Unregistered redirect_uri", checkreport.Fail, "refuses redirect URI"},
		{"unknown client", http.StatusNotFound, "not found", checkreport.Fail, "has no client"},
		{"client id named", http.StatusBadRequest, "Invalid client_id", checkreport.Fail, "has no client"},
		{"other refusal", http.StatusForbidden, "denied", checkreport.Fail, "refused a sign-in"},
		{"server error", http.StatusBadGateway, "upstream", checkreport.Skip, "says nothing about the client"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &run{}
			r.judgeDex("https://dex.example.com", "preview-auth", "https://relay/dex/callback", c.status, c.page)
			expect(t, r.report, CheckPreviewAuthDex, "", c.want, c.reason)
		})
	}
}

// TestPreviewAuthHostsEdges: before sign-in is required nothing is probed;
// an unusable suffix skips; a failed Preview list still probes the
// placeholder; only the Project's Ready, slotted, live Previews are probed;
// a transport error fails the host.
func TestPreviewAuthHostsEdges(t *testing.T) {
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "patchy"}}

	r := &run{p: project}
	r.previewAuthHosts(context.Background(), "https://relay.example.com", "preview.example.com", false)
	expect(t, r.report, CheckPreviewAuthHost, "", checkreport.Skip, "stage=permit")

	r = &run{p: project}
	r.previewAuthHosts(context.Background(), "https://relay.example.com", "not a suffix", true)
	expect(t, r.report, CheckPreviewAuthHost, "", checkreport.Skip, "unusable")

	var probed []string
	web := doerFunc(func(req *http.Request) (*http.Response, error) {
		probed = append(probed, req.URL.Host)
		return nil, errors.New("tls: handshake failure")
	})
	failingList := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	r = &run{p: project, cfg: Config{Reader: failingList, HTTP: web}}
	r.previewAuthHosts(context.Background(), "https://relay.example.com", "preview.example.com", true)
	if len(r.report.Checks) != 2 {
		t.Fatalf("checks = %s", dump(r.report))
	}
	if c := r.report.Checks[0]; c.Status != checkreport.Skip || !strings.Contains(c.Reason, "only the placeholder") {
		t.Errorf("list failure line = %+v", c)
	}
	if c := r.report.Checks[1]; c.Status != checkreport.Fail || !strings.Contains(c.Reason, "handshake failure") {
		t.Errorf("transport failure line = %+v", c)
	}

	slot := func(n int32) *int32 { return &n }
	preview := func(name, projectName string, phase v1alpha1.PreviewPhase, s *int32) *v1alpha1.Preview {
		return &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
			Spec:   v1alpha1.PreviewSpec{Project: projectName, HostLabel: name},
			Status: v1alpha1.PreviewStatus{Phase: phase, Slot: s}}
	}
	reader := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(
		preview("demo-1", "demo", v1alpha1.PreviewReady, slot(1)),
		preview("demo-2", "demo", v1alpha1.PreviewDeploying, slot(0)),
		preview("demo-3", "demo", v1alpha1.PreviewReady, nil),
		preview("other-1", "other", v1alpha1.PreviewReady, slot(0)),
	).Build()
	probed = nil
	r = &run{p: project, cfg: Config{Reader: reader, HTTP: web}}
	r.previewAuthHosts(context.Background(), "https://relay.example.com", "preview.example.com", true)
	want := []string{"placeholder.preview.example.com", "demo-1.preview.example.com"}
	if strings.Join(probed, ",") != strings.Join(want, ",") {
		t.Errorf("probed = %v, want %v", probed, want)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPGetEdges(t *testing.T) {
	r := &run{cfg: Config{HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://next"}},
			Body: io.NopCloser(failingReader{})}, nil
	})}}
	if _, _, _, err := r.httpGet(context.Background(), "https://x/"); err == nil ||
		!strings.Contains(err.Error(), "read the answer") {
		t.Errorf("httpGet with an unreadable body = %v", err)
	}
	if _, _, _, err := r.httpGet(context.Background(), "https://x/\x7f"); err == nil {
		t.Error("httpGet of an unparseable URL succeeded")
	}
}

// TestNewHTTPDoerNeverFollows: the CLI's doer reports a redirect as the
// answer instead of following it.
func TestNewHTTPDoerNeverFollows(t *testing.T) {
	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			followed = true
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	resp, err := NewHTTPDoer().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/next" || followed {
		t.Errorf("status %d, Location %q, followed %v", resp.StatusCode, resp.Header.Get("Location"), followed)
	}
}

// TestDialTLSRefused: the CLI's dialer reports a host that is not there.
func TestDialTLSRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialTLS(ctx, "preview.invalid"); err == nil {
		t.Error("DialTLS with a cancelled context succeeded")
	}
}
