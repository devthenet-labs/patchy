// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/ghapp"
	"github.com/bitwise-media-group/patchy/internal/ghsecret"
	"github.com/bitwise-media-group/patchy/internal/intentperm"
)

const (
	// setupCode is the code the fake GitHub hands back for a created App.
	setupCode = "c0de1234"
	// setupClientSecret is the OAuth secret GitHub's conversion carries,
	// which patchy must never keep or print.
	setupClientSecret = "oauth-client-secret-never-kept"
	// setupWebhookSecret is the webhook secret GitHub issues an App with a
	// webhook.
	setupWebhookSecret = "whsec-issued-by-github"
	setupWebhook       = "https://patchy.acme.test/github/webhooks"
)

// fakeAppGitHub is github.com for the manifest flow, on one httptest
// server: the "create a GitHub App" form the start page posts the manifest
// to, which sends the browser back to redirect_url with a code and the
// state, and the conversions endpoint the code is exchanged at.
type fakeAppGitHub struct {
	t   *testing.T
	srv *httptest.Server
	key string

	mu       sync.Mutex
	manifest ghapp.Manifest
	created  []string // the form paths posted to, with their query
	converts int      // every conversion asked for, accepted or not
	spent    bool     // setupCode has been converted
	// refuse answers every conversion with this status instead of an App.
	refuse int
	// noWebhookSecret leaves the webhook secret out of a conversion, as if
	// the person creating the App had turned its webhook off.
	noWebhookSecret bool
	// owner is the converted App's owner login; "" is acme.
	owner string
}

func newFakeAppGitHub(t *testing.T) *fakeAppGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeAppGitHub{t: t,
		key: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /organizations/{org}/settings/apps/new", g.create)
	mux.HandleFunc("POST /settings/apps/new", g.create)
	mux.HandleFunc("POST /app-manifests/{code}/conversions", g.convert)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// deps wires `setup github-app` to the fake: GitHub's web and API hosts,
// open as the browser, stdin as the terminal, and stdout never a terminal.
func (g *fakeAppGitHub) deps(open func(string) error, stdin io.Reader) *setupDeps {
	return &setupDeps{
		webURL:   g.srv.URL,
		apiURL:   g.srv.URL,
		open:     open,
		stdin:    stdin,
		client:   g.srv.Client(),
		tempDir:  g.t.TempDir(),
		terminal: func(io.Writer) bool { return false },
	}
}

// create is GitHub's form: it registers the posted manifest and sends the
// browser to its redirect_url with the code and the state it was given.
func (g *fakeAppGitHub) create(w http.ResponseWriter, r *http.Request) {
	var m ghapp.Manifest
	if err := json.Unmarshal([]byte(r.PostFormValue("manifest")), &m); err != nil {
		http.Error(w, "no manifest", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.manifest = m
	g.created = append(g.created, r.URL.RequestURI())
	g.mu.Unlock()
	back, err := url.Parse(m.RedirectURL)
	if err != nil || m.RedirectURL == "" {
		http.Error(w, "no redirect_url", http.StatusBadRequest)
		return
	}
	q := back.Query()
	q.Set("code", setupCode)
	q.Set("state", r.URL.Query().Get("state"))
	back.RawQuery = q.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// convert is the conversions endpoint: the code converts once, to an App
// with exactly the manifest's permissions and events.
func (g *fakeAppGitHub) convert(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.converts++
	if r.Header.Get("Authorization") != "" {
		g.t.Error("the conversion sent credentials: the endpoint needs none")
	}
	if r.PathValue("code") != setupCode || g.spent || g.refuse != 0 {
		status := g.refuse
		if status == 0 {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		return
	}
	g.spent = true
	var webhookSecret any
	if g.manifest.HookAttributes != nil && !g.noWebhookSecret {
		webhookSecret = setupWebhookSecret
	}
	events := g.manifest.DefaultEvents
	if events == nil {
		events = []string{}
	}
	owner := g.owner
	if owner == "" {
		owner = "acme"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": 4242, "slug": "patchy-acme", "name": g.manifest.Name,
		"html_url": g.srv.URL + "/apps/patchy-acme", "owner": map[string]any{"login": owner},
		"permissions": g.manifest.DefaultPermissions, "events": events,
		"client_id": "Iv1.0123456789", "client_secret": setupClientSecret,
		"webhook_secret": webhookSecret, "pem": g.key,
	})
}

// conversions is how many codes were brought to the conversions endpoint.
func (g *fakeAppGitHub) conversions() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.converts
}

// received is the manifest GitHub's form was posted, and where.
func (g *fakeAppGitHub) received() (ghapp.Manifest, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.manifest, slices.Clone(g.created)
}

var (
	startAction   = regexp.MustCompile(`<form id="manifest" method="post" action="([^"]*)">`)
	startManifest = regexp.MustCompile(`<input type="hidden" name="manifest" value="([^"]*)">`)
)

// submitStartPage does what the start page's script does in a browser:
// post its manifest to its form's action. follow decides whether the
// client follows GitHub's redirect back.
func submitStartPage(page []byte, follow bool) (*http.Response, error) {
	action, manifest := startAction.FindSubmatch(page), startManifest.FindSubmatch(page)
	if action == nil || manifest == nil {
		return nil, errors.New("the start page has no manifest form")
	}
	client := &http.Client{}
	if !follow {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client.PostForm(html.UnescapeString(string(action[1])),
		url.Values{"manifest": {html.UnescapeString(string(manifest[1]))}})
}

// browser is the person at the browser in the loopback flow: it loads the
// start page and lets it post the manifest; GitHub's redirect brings the
// browser back to the callback. With forge, a callback carrying another
// state is sent first, and must be refused. started records the start URL.
func (g *fakeAppGitHub) browser(forge bool, started *string) func(string) error {
	return func(start string) error {
		*started = start
		resp, err := http.Get(start)
		if err != nil {
			g.t.Errorf("load the start page: %v", err)
			return nil
		}
		page, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if forge {
			u, _ := url.Parse(start)
			forged, err := http.Get("http://" + u.Host + "/callback?code=evil&state=" + strings.Repeat("0", 64))
			if err != nil {
				g.t.Errorf("a callback under another state: %v", err)
				return nil
			}
			_ = forged.Body.Close()
			if forged.StatusCode != http.StatusBadRequest {
				g.t.Errorf("a callback under another state = %d, want 400", forged.StatusCode)
			}
		}
		back, err := submitStartPage(page, true)
		if err != nil {
			g.t.Errorf("submit the start page: %v", err)
			return nil
		}
		body, _ := io.ReadAll(back.Body)
		_ = back.Body.Close()
		if back.StatusCode != http.StatusOK || !strings.Contains(string(body), "Return to your terminal") {
			g.t.Errorf("the callback answered %d %q", back.StatusCode, body)
		}
		return nil
	}
}

// pasteReader is the person at a browser elsewhere in the --no-browser
// flow: once patchy reads the terminal, they open the page it wrote, post
// it, and paste the address GitHub sent them to.
type pasteReader struct {
	g   *fakeAppGitHub
	dir string
	r   io.Reader
	// first is pasted before the address, which goes without its scheme
	// when bare is set.
	first string
	bare  bool
}

func (p *pasteReader) Read(b []byte) (int, error) {
	if p.r == nil {
		pages, _ := filepath.Glob(filepath.Join(p.dir, "patchy-github-app-*.html"))
		if len(pages) != 1 {
			return 0, errors.New("no start page written")
		}
		page, err := os.ReadFile(pages[0])
		if err != nil {
			return 0, err
		}
		resp, err := submitStartPage(page, false)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		address := resp.Header.Get("Location")
		if p.bare {
			address = strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://")
		}
		p.r = strings.NewReader(p.first + address + "\n")
	}
	return p.r.Read(b)
}

// neverOpen is a browser that must not be opened.
func neverOpen(t *testing.T) func(string) error {
	return func(u string) error {
		t.Errorf("the browser was opened at %s", u)
		return nil
	}
}

// execSetup runs `patchy` with args against deps.
func execSetup(t *testing.T, deps *setupDeps, args ...string) (string, string, error) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	opts := &Options{Out: out, ErrOut: errOut, Output: "table", NoColor: true, setupDeps: deps}
	root := NewRoot(opts)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// readSecret parses a written Secret manifest and checks ghsecret accepts
// it as an App credential, as a Forge would read it.
func readSecret(t *testing.T, raw []byte) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := yaml.UnmarshalStrict(raw, &s); err != nil {
		t.Fatalf("not a Secret: %v\n%s", err, raw)
	}
	s.ResourceVersion = "1"
	if err := ghsecret.NewApps().Validate(&s, "", ""); err != nil {
		t.Errorf("ghsecret refuses the Secret: %v", err)
	}
	return &s
}

// leaks reports which of the App's secrets, in any spelling the Secret
// carries, appear in out.
func (g *fakeAppGitHub) leaks(out string) []string {
	body := strings.Split(strings.TrimSpace(g.key), "\n")[1]
	var found []string
	for _, secret := range []string{"PRIVATE KEY", body, base64.StdEncoding.EncodeToString([]byte(g.key))[200:260],
		setupWebhookSecret, base64.StdEncoding.EncodeToString([]byte(setupWebhookSecret)), setupClientSecret} {
		if strings.Contains(out, secret) {
			found = append(found, secret)
		}
	}
	return found
}

// checkManifest asserts GitHub received, once, at the organization's form
// with a fresh state, a private App manifest named patchy-acme holding
// exactly the table's permissions and events for features, redirecting to
// the loopback callback.
func (g *fakeAppGitHub) checkManifest(t *testing.T, features []intentperm.Feature) {
	t.Helper()
	m, created := g.received()
	want, _ := intentperm.ForApp(features...)
	perms := map[string]string{}
	for _, gr := range want.Grants {
		perms[gr.Permission] = gr.Access
	}
	if !maps.Equal(m.DefaultPermissions, perms) || !slices.Equal(m.DefaultEvents, want.Events) ||
		m.Public || m.Name != "patchy-acme" {
		t.Errorf("GitHub received %+v; the table says %v and %v", m, perms, want.Events)
	}
	if !strings.HasPrefix(m.RedirectURL, "http://127.0.0.1:") || !strings.HasSuffix(m.RedirectURL, "/callback") {
		t.Errorf("redirect_url %q is not the loopback callback", m.RedirectURL)
	}
	if len(created) != 1 || !regexp.MustCompile(`^/organizations/acme/settings/apps/new\?state=[0-9a-f]{64}$`).
		MatchString(created[0]) {
		t.Errorf("the form was posted to %v", created)
	}
}

// checkSecretFile asserts the file at path is a 0600 Secret patchy-github in
// namespace with exactly keys, the App's ID and key, and no OAuth secret.
func checkSecretFile(t *testing.T, g *fakeAppGitHub, path, namespace string, keys []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := readSecret(t, raw)
	if s.Name != ghapp.DefaultSecretName || s.Namespace != namespace ||
		!slices.Equal(slices.Sorted(maps.Keys(s.Data)), keys) || string(s.Data[ghapp.KeyAppID]) != "4242" ||
		string(s.Data[ghapp.KeyPrivateKey]) != g.key {
		t.Errorf("the Secret is %s/%s with %v", s.Namespace, s.Name, slices.Sorted(maps.Keys(s.Data)))
	}
	if strings.Contains(string(raw), setupClientSecret) {
		t.Error("the Secret carries the OAuth client secret")
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the Secret file mode is %o, want 600", info.Mode().Perm())
	}
}

// TestSetupGitHubAppBrowserFlow drives the whole loopback flow against the
// fake GitHub: the manifest GitHub receives is exactly the table's for the
// chosen features; a callback under another state is refused; the code is
// exchanged; the Secret file is 0600 with exactly the ghsecret keys; the
// private key, webhook secret and OAuth secret appear on no stream; and the
// callback server is gone afterwards.
func TestSetupGitHubAppBrowserFlow(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		features []intentperm.Feature
		keys     []string
	}{
		{"intents and checks", []string{"--intents", "--checks"},
			[]intentperm.Feature{intentperm.FeatureIntents, intentperm.FeatureChecks},
			[]string{ghapp.KeyAppID, ghapp.KeyPrivateKey}},
		{"security", []string{"--security", "--webhook-url", setupWebhook},
			[]intentperm.Feature{intentperm.FeatureSecurity},
			[]string{ghapp.KeyAppID, ghapp.KeyPrivateKey, ghapp.KeyWebhookSecret}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeAppGitHub(t)
			var started string
			out := filepath.Join(t.TempDir(), "app.secret.yaml")
			args := append([]string{"setup", "github-app", "--org", "acme", "-o", out, "-n", "patchy-system",
				"--timeout", "20s"}, tt.args...)
			stdout, stderr, err := execSetup(t, g.deps(g.browser(true, &started), nil), args...)
			if err != nil {
				t.Fatalf("setup: %v\n%s", err, stderr)
			}

			g.checkManifest(t, tt.features)
			checkSecretFile(t, g, out, "patchy-system", tt.keys)
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing: the Secret went to a file", stdout)
			}
			if leaked := g.leaks(stderr); len(leaked) > 0 {
				t.Errorf("stderr leaks %v:\n%s", leaked, stderr)
			}
			if !strings.Contains(stderr, g.srv.URL+"/apps/patchy-acme/installations/new") {
				t.Errorf("stderr lacks the install link:\n%s", stderr)
			}
			if _, err := http.Get(started); err == nil {
				t.Error("the callback server still answers after it took its code")
			}
		})
	}
}

// TestSetupGitHubAppWarnsWithoutWebhookSecret: a --security App GitHub
// issued no webhook secret still gets its Secret, without the key, and the
// person is told the Integration will refuse it and where to set one.
func TestSetupGitHubAppWarnsWithoutWebhookSecret(t *testing.T) {
	g := newFakeAppGitHub(t)
	g.noWebhookSecret = true
	var started string
	out := filepath.Join(t.TempDir(), "patchy-github.secret.yaml")
	_, stderr, err := execSetup(t, g.deps(g.browser(false, &started), nil), "setup", "github-app", "--org", "acme",
		"--security", "--webhook-url", setupWebhook, "-o", out, "--timeout", "20s")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, stderr)
	}
	checkSecretFile(t, g, out, "patchy", []string{ghapp.KeyAppID, ghapp.KeyPrivateKey})
	if !strings.Contains(stderr, "GitHub issued no webhook secret") ||
		!strings.Contains(stderr, g.srv.URL+"/organizations/acme/settings/apps/patchy-acme") {
		t.Errorf("stderr does not warn about the missing webhook secret:\n%s", stderr)
	}
}

// TestSetupGitHubAppStdout: -o - writes the Secret to stdout and nothing to
// disk, and stderr carries no secret; to a terminal it refuses before
// GitHub hears of anything.
func TestSetupGitHubAppStdout(t *testing.T) {
	t.Chdir(t.TempDir())
	g := newFakeAppGitHub(t)
	var started string
	stdout, stderr, err := execSetup(t, g.deps(g.browser(false, &started), nil),
		"setup", "github-app", "--org", "acme", "--intents", "-o", "-", "--timeout", "20s")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, stderr)
	}
	if s := readSecret(t, []byte(stdout)); string(s.Data[ghapp.KeyPrivateKey]) != g.key {
		t.Error("stdout's Secret does not carry the private key")
	}
	if leaked := g.leaks(stderr); len(leaked) > 0 {
		t.Errorf("stderr leaks %v:\n%s", leaked, stderr)
	}
	if entries, _ := os.ReadDir("."); len(entries) != 0 {
		t.Errorf("-o - wrote %v to disk", entries)
	}

	g = newFakeAppGitHub(t)
	deps := g.deps(neverOpen(t), nil)
	deps.terminal = func(io.Writer) bool { return true }
	_, _, err = execSetup(t, deps, "setup", "github-app", "--org", "acme", "--intents", "-o", "-")
	if exitCode(err) != ExitUsage || !strings.Contains(fmtErr(err), "terminal") {
		t.Errorf("-o - to a terminal = %v (exit %d), want a usage refusal", err, exitCode(err))
	}
	if _, created := g.received(); len(created) != 0 {
		t.Errorf("GitHub was asked to create an App: %v", created)
	}
}

// runSetupToFile runs `setup github-app ... -o -` with stdout a new file of
// mode perm, as a shell's `> file` makes one, and answers its path.
func runSetupToFile(t *testing.T, deps *setupDeps, perm os.FileMode) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "patchy-github.yaml")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(perm); err != nil { // perm exactly, whatever the umask
		t.Fatal(err)
	}
	errOut := &bytes.Buffer{}
	root := NewRoot(&Options{Out: f, ErrOut: errOut, Output: "table", NoColor: true, setupDeps: deps})
	root.SetOut(f)
	root.SetErr(errOut)
	root.SetArgs([]string{"setup", "github-app", "--org", "acme", "--intents", "-o", "-", "--timeout", "20s"})
	return path, root.ExecuteContext(context.Background())
}

// TestSetupGitHubAppStdoutFile: -o - with stdout redirected to a file other
// users can read (a shell's `> file` under the usual umask) is refused
// before GitHub hears of anything, leaving the file empty; a file only its
// owner can open takes the Secret.
func TestSetupGitHubAppStdoutFile(t *testing.T) {
	g := newFakeAppGitHub(t)
	path, err := runSetupToFile(t, g.deps(neverOpen(t), nil), 0o644)
	if exitCode(err) != ExitUsage || !strings.Contains(fmtErr(err), "other users can read") ||
		!strings.Contains(fmtErr(err), "-o <file>") {
		t.Errorf("-o - to a 0644 file = %v (exit %d), want a usage refusal", err, exitCode(err))
	}
	if _, created := g.received(); len(created) != 0 {
		t.Errorf("GitHub was asked to create an App: %v", created)
	}
	if raw, _ := os.ReadFile(path); len(raw) != 0 {
		t.Errorf("the refused file holds %q", raw)
	}

	if runtime.GOOS == "windows" {
		return // no owner-only mode to give the file
	}
	g = newFakeAppGitHub(t)
	var started string
	path, err = runSetupToFile(t, g.deps(g.browser(false, &started), nil), 0o600)
	if err != nil {
		t.Fatalf("-o - to a 0600 file: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := readSecret(t, raw); string(s.Data[ghapp.KeyPrivateKey]) != g.key {
		t.Error("the file's Secret does not carry the private key")
	}
}

// TestSetupGitHubAppPaste: --no-browser opens nothing and listens on no
// port; the person pastes the address GitHub sent them to, the code in it
// is exchanged, the start page is removed, and a user-owned App's Secret is
// written.
func TestSetupGitHubAppPaste(t *testing.T) {
	g := newFakeAppGitHub(t)
	deps := g.deps(neverOpen(t), nil)
	deps.stdin = &pasteReader{g: g, dir: deps.tempDir, first: "\nnot a code!\n", bare: true}
	out := filepath.Join(t.TempDir(), "patchy-github.secret.yaml")
	_, stderr, err := execSetup(t, deps, "setup", "github-app", "--user", "--intents", "--no-browser", "-o", out)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, stderr)
	}
	m, created := g.received()
	if len(created) != 1 || !strings.HasPrefix(created[0], "/settings/apps/new?state=") ||
		m.RedirectURL != g.srv.URL+"/settings/apps" || m.Name != "patchy" {
		t.Errorf("GitHub received %+v at %v", m, created)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	readSecret(t, raw)
	if pages, _ := filepath.Glob(filepath.Join(deps.tempDir, "*")); len(pages) != 0 {
		t.Errorf("the start page was left behind: %v", pages)
	}
	if leaked := g.leaks(stderr); len(leaked) > 0 {
		t.Errorf("stderr leaks %v:\n%s", leaked, stderr)
	}
	if n := strings.Count(stderr, "Paste the address again"); n != 2 {
		t.Errorf("an empty line and junk were asked again %d times, want 2:\n%s", n, stderr)
	}

	// An address from another attempt is refused and asked again; the
	// terminal closing then ends the run with where to clean up, and
	// nothing is exchanged.
	g = newFakeAppGitHub(t)
	deps = g.deps(neverOpen(t), strings.NewReader(g.srv.URL+"/settings/apps?code="+setupCode+"&state=other\n"))
	_, stderr, err = execSetup(t, deps, "setup", "github-app", "--user", "--intents", "--no-browser",
		"-o", filepath.Join(t.TempDir(), "x.yaml"))
	if !errors.Is(err, ghapp.ErrNoCode) || exitCode(err) != ExitError ||
		!strings.Contains(err.Error(), g.srv.URL+"/settings/apps") || !strings.Contains(stderr, "another attempt") {
		t.Errorf("another attempt's address, then EOF = %v (exit %d)\n%s", err, exitCode(err), stderr)
	}
	if g.conversions() != 0 {
		t.Error("a code from another attempt was exchanged")
	}
}

// TestSetupGitHubAppPasteRejectedBareCode: a bare paste GitHub does not
// accept (a word typed at the prompt) asks again instead of ending the run,
// since this run's own code is still unspent; the address pasted next is
// exchanged and the Secret written. Ended there instead, the run says to
// delete the App only as the ErrNoCode fallback, never over the junk.
func TestSetupGitHubAppPasteRejectedBareCode(t *testing.T) {
	g := newFakeAppGitHub(t)
	deps := g.deps(neverOpen(t), nil)
	deps.stdin = &pasteReader{g: g, dir: deps.tempDir, first: "yes\n"}
	out := filepath.Join(t.TempDir(), "patchy-github.secret.yaml")
	_, stderr, err := execSetup(t, deps, "setup", "github-app", "--org", "acme", "--intents", "--no-browser",
		"-o", out)
	if err != nil {
		t.Fatalf("a rejected bare paste, then the address: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "GitHub did not accept the code") ||
		!strings.Contains(stderr, "paste that address") {
		t.Errorf("stderr does not ask again after the rejected paste:\n%s", stderr)
	}
	if g.conversions() != 2 {
		t.Errorf("%d conversions, want 2: the junk, then the code", g.conversions())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	readSecret(t, raw)

	g = newFakeAppGitHub(t)
	deps = g.deps(neverOpen(t), strings.NewReader("acme\n"))
	_, stderr, err = execSetup(t, deps, "setup", "github-app", "--org", "acme", "--intents", "--no-browser",
		"-o", filepath.Join(t.TempDir(), "x.yaml"))
	if !errors.Is(err, ghapp.ErrNoCode) || errors.Is(err, ghapp.ErrCodeRejected) ||
		!strings.Contains(err.Error(), "--no-browser") {
		t.Errorf("a rejected bare paste, then EOF = %v\n%s", err, stderr)
	}
}

// TestSetupGitHubAppRefusesAnotherOwner: a code that converts to an App
// another account owns (a local process that read the state could slip
// one in) writes nothing and names where the person's own App is.
func TestSetupGitHubAppRefusesAnotherOwner(t *testing.T) {
	g := newFakeAppGitHub(t)
	g.owner = "mallory"
	var started string
	out := filepath.Join(t.TempDir(), "patchy-github.secret.yaml")
	_, stderr, err := execSetup(t, g.deps(g.browser(false, &started), nil), "setup", "github-app", "--org", "Acme",
		"--intents", "-o", out, "--timeout", "20s")
	if err == nil || !strings.Contains(err.Error(), `owned by "mallory", not by --org Acme`) ||
		exitCode(err) != ExitError {
		t.Fatalf("another owner's App = %v (exit %d)", err, exitCode(err))
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("another owner's credentials were written: %v", statErr)
	}
	if leaked := g.leaks(stderr + err.Error()); len(leaked) > 0 {
		t.Errorf("the refusal leaks %v", leaked)
	}

	// The owner's login is matched without regard to case.
	g = newFakeAppGitHub(t)
	g.owner = "ACME"
	if _, stderr, err := execSetup(t, g.deps(g.browser(false, &started), nil), "setup", "github-app", "--org",
		"acme", "--intents", "-o", filepath.Join(t.TempDir(), "s.yaml"), "--timeout", "20s"); err != nil {
		t.Errorf("an App owned by ACME for --org acme: %v\n%s", err, stderr)
	}
}

// failWriter is a stdout whose reader has gone.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }

// TestSetupGitHubAppStdoutLost: when the Secret cannot be written to
// stdout after GitHub created the App, the error says so and links the
// App's settings page, where a new key is generated.
func TestSetupGitHubAppStdoutLost(t *testing.T) {
	g := newFakeAppGitHub(t)
	var started string
	errOut := &bytes.Buffer{}
	opts := &Options{Out: failWriter{}, ErrOut: errOut, Output: "table", NoColor: true,
		setupDeps: g.deps(g.browser(false, &started), nil)}
	root := NewRoot(opts)
	root.SetErr(errOut)
	root.SetArgs([]string{"setup", "github-app", "--org", "acme", "--intents", "-o", "-", "--timeout", "20s"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "private key was not saved") ||
		!strings.Contains(err.Error(), g.srv.URL+"/organizations/acme/settings/apps/patchy-acme") {
		t.Errorf("a lost stdout = %v", err)
	}
}

// TestWriteStdoutSurvivesABrokenPipe runs a write to a stdout pipe whose
// reader has closed in a child process: without the SIGPIPE guard the
// runtime kills it before it can report anything.
func TestWriteStdoutSurvivesABrokenPipe(t *testing.T) {
	if os.Getenv("PATCHY_TEST_BROKEN_PIPE") == "1" {
		err := writeStdout(os.Stdout, []byte("apiVersion: v1\n"))
		fmt.Fprintf(os.Stderr, "write returned: %v\n", err)
		os.Exit(0)
	}
	if runtime.GOOS == "windows" {
		t.Skip("no SIGPIPE on windows")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriteStdoutSurvivesABrokenPipe$")
	cmd.Env = append(os.Environ(), "PATCHY_TEST_BROKEN_PIPE=1")
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	_ = w.Close()
	if runErr != nil || !strings.Contains(stderr.String(), "write returned: write /dev/stdout: broken pipe") {
		t.Errorf("the child = %v, stderr %q; want it alive to report EPIPE", runErr, stderr.String())
	}
}

// TestSetupGitHubAppRefusesOverwrite: an existing output file stops the
// flow before GitHub hears of anything, untouched; --force replaces it
// with a 0600 file.
func TestSetupGitHubAppRefusesOverwrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "patchy-github.secret.yaml")
	if err := os.WriteFile(out, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newFakeAppGitHub(t)
	_, _, err := execSetup(t, g.deps(neverOpen(t), nil), "setup", "github-app", "--org", "acme", "--intents",
		"-o", out)
	if !errors.Is(err, ghapp.ErrExists) || exitCode(err) != ExitError {
		t.Errorf("an existing file = %v (exit %d), want ErrExists, exit %d", err, exitCode(err), ExitError)
	}
	if got, _ := os.ReadFile(out); string(got) != "keep" {
		t.Errorf("the refused file was changed: %q", got)
	}
	if _, created := g.received(); len(created) != 0 {
		t.Errorf("GitHub was asked to create an App: %v", created)
	}

	var started string
	_, stderr, err := execSetup(t, g.deps(g.browser(false, &started), nil), "setup", "github-app", "--org", "acme",
		"--intents", "-o", out, "--force", "--timeout", "20s")
	if err != nil {
		t.Fatalf("setup --force: %v\n%s", err, stderr)
	}
	raw, _ := os.ReadFile(out)
	readSecret(t, raw)
	if info, _ := os.Stat(out); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the replaced file mode is %o, want 600", info.Mode().Perm())
	}
}

// TestSetupGitHubAppDryRun: --dry-run prints exactly the manifest Build
// makes, as JSON on stdout, and creates, opens and writes nothing.
func TestSetupGitHubAppDryRun(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tt := range []struct {
		args []string
		cfg  ghapp.Config
	}{
		{[]string{"--org", "acme", "--intents"}, ghapp.Config{Features: ghapp.Features{Intents: true},
			Name: "patchy-acme", HomepageURL: ghapp.DefaultHomepageURL}},
		{[]string{"--org", "acme", "--security", "--intents", "--checks", "--webhook-url", setupWebhook,
			"--name", "acme-patchy"}, ghapp.Config{Features: ghapp.Features{Security: true, Intents: true, Checks: true},
			Name: "acme-patchy", HomepageURL: ghapp.DefaultHomepageURL, WebhookURL: setupWebhook}},
		{[]string{"--user", "--intents"}, ghapp.Config{Features: ghapp.Features{Intents: true}, Name: "patchy",
			HomepageURL: ghapp.DefaultHomepageURL}},
	} {
		g := newFakeAppGitHub(t)
		stdout, stderr, err := execSetup(t, g.deps(neverOpen(t), nil),
			append([]string{"setup", "github-app", "--dry-run"}, tt.args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", tt.args, err, stderr)
		}
		m, err := ghapp.Build(tt.cfg)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.MarshalIndent(m, "", "  ")
		if stdout != string(want)+"\n" {
			t.Errorf("%v: stdout =\n%s\nwant\n%s", tt.args, stdout, want)
		}
		if !strings.Contains(stderr, "dry run: nothing created") {
			t.Errorf("%v: stderr = %q", tt.args, stderr)
		}
		if _, created := g.received(); len(created) != 0 {
			t.Errorf("%v: GitHub was asked to create an App", tt.args)
		}
	}
	if entries, _ := os.ReadDir("."); len(entries) != 0 {
		t.Errorf("--dry-run wrote %v", entries)
	}
}

// TestSetupGitHubAppUsage: every invocation the flow cannot serve exits 2
// before a browser opens or GitHub hears of anything.
func TestSetupGitHubAppUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no owner", []string{"--intents"}, "--org <organization>, or --user"},
		{"two owners", []string{"--org", "acme", "--user", "--intents"}, "exclusive"},
		{"not an org login", []string{"--org", "acme/x", "--intents"}, "not a GitHub organization login"},
		{"no feature", []string{"--org", "acme"}, "choose what the App is for"},
		{"checks alone", []string{"--org", "acme", "--checks"}, "--checks extends --intents"},
		{"checks without intents", []string{"--org", "acme", "--security", "--webhook-url", setupWebhook,
			"--checks"}, "--checks extends --intents"},
		{"security without a webhook", []string{"--org", "acme", "--security"}, "needs --webhook-url"},
		{"a webhook without security", []string{"--org", "acme", "--intents", "--webhook-url", setupWebhook},
			"receives webhook events"},
		{"http webhook", []string{"--org", "acme", "--security", "--webhook-url",
			"http://patchy.acme.test/github/webhooks"}, "not an https URL"},
		{"long name", []string{"--org", "acme", "--intents", "--name", strings.Repeat("a", 35)}, "longer than"},
		{"secret name", []string{"--org", "acme", "--intents", "--secret-name", "Patchy_GitHub"}, "--secret-name"},
		{"namespace", []string{"--org", "acme", "--intents", "-n", "Patchy"}, "-n"},
		{"timeout", []string{"--org", "acme", "--intents", "--timeout", "0s"}, "--timeout"},
		{"argument", []string{"--org", "acme", "--intents", "extra"}, "unknown command"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeAppGitHub(t)
			_, _, err := execSetup(t, g.deps(neverOpen(t), nil), append([]string{"setup", "github-app"}, tt.args...)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
			if tt.name != "argument" && exitCode(err) != ExitUsage {
				t.Errorf("exit code %d, want %d", exitCode(err), ExitUsage)
			}
			if _, created := g.received(); len(created) != 0 {
				t.Error("GitHub was asked to create an App")
			}
		})
	}
}

// TestSetupGitHubAppFailures: a code GitHub will not convert, and a browser
// that never comes back, each end with an error, no file, and (for the
// first) where to clean up the App GitHub may have created.
func TestSetupGitHubAppFailures(t *testing.T) {
	g := newFakeAppGitHub(t)
	g.refuse = http.StatusUnprocessableEntity
	var started string
	out := filepath.Join(t.TempDir(), "patchy-github.secret.yaml")
	_, _, err := execSetup(t, g.deps(g.browser(false, &started), nil), "setup", "github-app", "--org", "acme",
		"--intents", "-o", out, "--timeout", "20s")
	if !errors.Is(err, ghapp.ErrCodeRejected) || exitCode(err) != ExitError ||
		!strings.Contains(err.Error(), g.srv.URL+"/organizations/acme/settings/apps") {
		t.Errorf("a refused code = %v (exit %d)", err, exitCode(err))
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a file was written after a refused code: %v", statErr)
	}

	g = newFakeAppGitHub(t)
	idle := func(string) error { return nil }
	_, _, err = execSetup(t, g.deps(idle, nil), "setup", "github-app", "--org", "acme", "--intents", "-o", out,
		"--timeout", "50ms")
	if !errors.Is(err, ghapp.ErrNoCode) || exitCode(err) != ExitError ||
		!strings.Contains(err.Error(), g.srv.URL+"/organizations/acme/settings/apps") {
		t.Errorf("no code before the timeout = %v (exit %d), want ErrNoCode naming where to clean up", err,
			exitCode(err))
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a file was written without a code: %v", statErr)
	}
}

// TestSetupFeatureFlagsAreTheTable: `setup github-app` has one switch per
// feature of the permission table, named as the table names it, so the
// command's wording and the table cannot drift apart.
func TestSetupFeatureFlagsAreTheTable(t *testing.T) {
	cmd := newSetupGitHubAppCmd(&Options{})
	for _, f := range intentperm.Features() {
		if fl := cmd.Flags().Lookup(string(f)); fl == nil || fl.Value.Type() != "bool" {
			t.Errorf("no --%s switch for the table's feature %s", f, f)
		}
	}
}

// fmtErr is err's message, "" for nil.
func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
