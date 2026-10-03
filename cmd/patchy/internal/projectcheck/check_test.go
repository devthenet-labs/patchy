// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

const (
	testNamespace = "patchy"
	suffix        = "preview.acme.test"
	lbHost        = "k8s-preview-0123.us-east-1.elb.amazonaws.com"
	intentsURL    = "https://github.com/acme/intents"
)

// sha is a 40-hex commit SHA built from one hex digit.
func sha(c string) string { return strings.Repeat(c, 40) }

// fakeGitHub serves heads and files from memory, by repository URL.
type fakeGitHub struct {
	heads map[string]Head
	files map[string]map[string]string
	// fileErr fails every file read.
	fileErr error
}

func (g *fakeGitHub) Head(_ context.Context, repoURL string) (Head, error) {
	h, ok := g.heads[repoURL]
	if !ok {
		return Head{}, fmt.Errorf("GitHub answered 404 Not Found for %s (anonymously)", repoURL)
	}
	return h, nil
}

func (g *fakeGitHub) File(_ context.Context, repoURL, ref, path string, limit int64) ([]byte, int64, bool, error) {
	if g.fileErr != nil {
		return nil, 0, false, g.fileErr
	}
	if h := g.heads[repoURL]; h.SHA != ref {
		return nil, 0, false, fmt.Errorf("read %s at %s, not at the head %s", path, ref, h.SHA)
	}
	data, ok := g.files[repoURL][path]
	if !ok {
		return nil, 0, false, nil
	}
	if int64(len(data)) > limit {
		return nil, int64(len(data)), true, nil
	}
	return []byte(data), int64(len(data)), true, nil
}

// fakeResolver answers from a fixed table; any other name does not exist.
type fakeResolver map[string][]string

func (r fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if addrs, ok := r[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// timeoutErr is a dial that got no answer.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// servingCert is a TLSDialer whose handshake presents a certificate for
// *.<suffix>.
func servingCert(_ context.Context, _ string) (tls.ConnectionState, error) {
	return tls.ConnectionState{PeerCertificates: []*x509.Certificate{{
		DNSNames: []string{"*." + suffix},
		Issuer:   pkix.Name{CommonName: "Amazon RSA 2048 M02"},
		NotAfter: time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC),
	}}}, nil
}

// world is one cluster, GitHub, registry and network for Run to check.
type world struct {
	t        *testing.T
	host     string // the in-memory registry's host
	project  *v1alpha1.Project
	forges   []*v1alpha1.Forge
	objs     []client.Object
	github   *fakeGitHub
	resolver fakeResolver
	dialTLS  TLSDialer
}

// agentImage and previewRepo are where repository key's images live.
func (w *world) agentImage(key string) string { return w.host + "/patchy/app-envs/" + key + ":toolchain-v1" }
func (w *world) previewRepo(key string) string { return w.host + "/patchy/previews/" + key }

// newWorld is a Project with three repositories, web and api previewed and
// lib not, where every check passes.
func newWorld(t *testing.T) *world {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, host: u.Host, github: &fakeGitHub{heads: map[string]Head{}, files: map[string]map[string]string{}}}
	w.project = &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: testNamespace, Generation: 3},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: intentsURL,
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
		},
		Status: v1alpha1.ProjectStatus{Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Validated", ObservedGeneration: 3,
				Message: "the repositories resolve, the App is installed on them, and the labels exist"},
			{Type: v1alpha1.ConditionIntentNameConflict, Status: metav1.ConditionFalse, Reason: "NoConflict",
				Message: "no trigger-labelled issue's intent name is held elsewhere"},
		}},
	}
	w.github.heads[intentsURL] = Head{Branch: "main", SHA: sha("0")}
	for i, key := range []string{"web", "api", "lib"} {
		repoURL := "https://github.com/acme/shop-" + key
		repo := v1alpha1.ProjectRepository{Name: key, URL: repoURL}
		if key != "lib" {
			repo.Preview = &v1alpha1.ProjectRepositoryPreview{
				ProjectPreview: v1alpha1.ProjectPreview{ImageRepository: w.previewRepo(key), Port: 8080,
					ReadinessPath: "/healthz"},
				Path: "/" + key,
			}
			if key == "web" {
				repo.Preview.Path = "/"
			}
		}
		w.project.Spec.Repositories = append(w.project.Spec.Repositories, repo)
		head := sha(fmt.Sprint(i + 1))
		w.github.heads[repoURL] = Head{Branch: "main", SHA: head}
		w.github.files[repoURL] = map[string]string{".patchy/agent.yaml": "image: " + w.agentImage(key) + "\n"}
		w.push(w.agentImage(key), "PATH=/usr/local/bin:/usr/bin:/bin")
		if repo.Preview != nil {
			w.push(w.previewRepo(key)+":sha-"+head, "PATH=/usr/bin")
		}
	}
	w.forges = []*v1alpha1.Forge{testForge("github", metav1.ConditionTrue, "acme")}
	w.objs = []client.Object{
		w.configMap("patchy-source-controller-config", "source-controller", map[string]string{
			keyRepositoryImages:   "true",
			keyImageRegistries:    w.host + "/patchy/app-envs/",
			keyImageAllowUnsigned: "true",
		}),
		w.configMap("patchy-intent-controller-config", "intent-controller", map[string]string{
			keyIntentPreviewsEnabled: "true",
		}),
		w.configMap("patchy-preview-controller-config", "preview-controller", map[string]string{
			keyPreviewImagePrefix: w.host + "/patchy/previews/",
			keyPreviewHostSuffix:  suffix,
		}),
		&networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Namespace: placeholderNamespace, Name: placeholderName},
			Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
				Ingress: []networkingv1.IngressLoadBalancerIngress{{Hostname: lbHost}},
			}},
		},
	}
	w.resolver = fakeResolver{
		"shop-0." + suffix: {"198.51.100.8", "198.51.100.7"},
		lbHost:             {"198.51.100.7", "198.51.100.8"},
	}
	w.dialTLS = servingCert
	return w
}

// testForge is a Forge covering github.com, limited to orgs when given.
func testForge(name string, ready metav1.ConditionStatus, orgs ...string) *v1alpha1.Forge {
	return &v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub, Orgs: orgs,
			SecretRef: v1alpha1.LocalSecretReference{Name: name + "-app"}},
		Status: v1alpha1.ForgeStatus{Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: ready, Reason: "CredentialValid"},
		}},
	}
}

// configMap is a controller's chart-rendered settings.
func (w *world) configMap(name, controller string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace,
			Labels: map[string]string{labelName: controller, labelPartOf: partOf}},
		Data: data,
	}
}

// setting changes one controller ConfigMap's key; an empty value deletes it.
func (w *world) setting(controller, key, value string) {
	for _, o := range w.objs {
		cm, ok := o.(*corev1.ConfigMap)
		if !ok || cm.Labels[labelName] != controller {
			continue
		}
		if value == "" {
			delete(cm.Data, key)
		} else {
			cm.Data[key] = value
		}
	}
}

// drop removes the objects match picks.
func (w *world) drop(match func(client.Object) bool) {
	kept := w.objs[:0]
	for _, o := range w.objs {
		if !match(o) {
			kept = append(kept, o)
		}
	}
	w.objs = kept
}

// push writes a linux/amd64 image with env to the in-memory registry.
func (w *world) push(ref string, env ...string) {
	w.t.Helper()
	img, err := mutate.ConfigFile(mutate.MediaType(empty.Image, types.OCIManifestSchema1), &v1.ConfigFile{
		OS: "linux", Architecture: "amd64", Config: v1.Config{Env: env},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	tag, err := name.NewTag(ref)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		w.t.Fatal(err)
	}
}

// repo returns the Project's repository with the key.
func (w *world) repo(key string) *v1alpha1.ProjectRepository {
	for i := range w.project.Spec.Repositories {
		if w.project.Spec.Repositories[i].Name == key {
			return &w.project.Spec.Repositories[i]
		}
	}
	w.t.Fatalf("no repository %s", key)
	return nil
}

// run checks the world's Project, failing the test on any Secret read: the
// check must never hold the App's private key.
func (w *world) run() Report {
	w.t.Helper()
	objs := append([]client.Object{w.project}, w.objs...)
	for _, f := range w.forges {
		objs = append(objs, f)
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
		WithInterceptorFuncs(noSecrets(w.t)).Build()
	report, err := Run(context.Background(), Config{
		Reader: c, Namespace: testNamespace, Project: w.project.Name, GitHub: w.github,
		Resolver: w.resolver, DialTLS: w.dialTLS,
	})
	if err != nil {
		w.t.Fatalf("Run: %v", err)
	}
	return report
}

// noSecrets fails the test on any read of a Secret.
func noSecrets(t *testing.T) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				t.Errorf("the check read Secret %s", key)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.SecretList); ok {
				t.Error("the check listed Secrets")
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// line finds one check of a report, by name and repository.
func line(t *testing.T, r Report, check, repository string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == check && c.Repository == repository {
			return c
		}
	}
	t.Fatalf("no %s line for %q in:\n%s", check, repository, dump(r))
	return Check{}
}

// expect asserts one check's status and that its reason holds every want.
func expect(t *testing.T, r Report, check, repository string, status checkreport.Status, want ...string) {
	t.Helper()
	c := line(t, r, check, repository)
	if c.Status != status {
		t.Errorf("%s %s = %s, want %s: %s", check, repository, c.Status, status, c.Reason)
	}
	for _, s := range want {
		if !strings.Contains(c.Reason, s) {
			t.Errorf("%s %s reason lacks %q: %s", check, repository, s, c.Reason)
		}
	}
}

func dump(r Report) string {
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%s %s %s %s\n", c.Status, c.Name, c.Repository, c.Reason)
	}
	return b.String()
}

// TestRunAllPass: a Project whose every check passes reports one line per
// check and repository, every repository of it, and nothing fails.
func TestRunAllPass(t *testing.T) {
	w := newWorld(t)
	r := w.run()
	if r.Project != "shop" || r.Namespace != testNamespace {
		t.Errorf("report names %s/%s", r.Namespace, r.Project)
	}
	want := []struct{ check, repository string }{
		{CheckReady, ""}, {CheckIntentNames, ""},
		{CheckForge, IntentRepository}, {CheckForge, "web"}, {CheckForge, "api"}, {CheckForge, "lib"},
		{CheckLabels, ""},
		{CheckAgentImage, "web"}, {CheckAgentImage, "api"}, {CheckAgentImage, "lib"},
		{CheckPreviews, ""}, {CheckPreviewImage, "web"}, {CheckPreviewImage, "api"},
		{CheckPreviewDNS, ""}, {CheckPreviewTLS, ""},
	}
	if len(r.Checks) != len(want) {
		t.Errorf("%d checks, want %d:\n%s", len(r.Checks), len(want), dump(r))
	}
	for i, c := range r.Checks {
		if i < len(want) && (c.Name != want[i].check || c.Repository != want[i].repository) {
			t.Errorf("check %d is %s %q, want %s %q", i, c.Name, c.Repository, want[i].check, want[i].repository)
		}
		if c.Status != checkreport.Pass {
			t.Errorf("%s %s = %s: %s", c.Name, c.Repository, c.Status, c.Reason)
		}
	}
	if n := r.Failed(); n != 0 {
		t.Errorf("Failed = %d", n)
	}
	expect(t, r, CheckReady, "", checkreport.Pass, "Validated: the repositories resolve")
	expect(t, r, CheckForge, "api", checkreport.Pass, "Forge github covers acme/shop-api and is Ready")
	expect(t, r, CheckLabels, "", checkreport.Pass, `"patchy:shop" (trigger) and "patchy:approved" (approve)`,
		"acme/intents")
	expect(t, r, CheckAgentImage, "lib", checkreport.Pass, w.agentImage("lib"), "@sha256:",
		".patchy/agent.yaml at main@333333333333", "unsigned images allowed", "not source-controller's")
	expect(t, r, CheckPreviews, "", checkreport.Pass, "<intent>."+suffix, w.host+"/patchy/previews/")
	expect(t, r, CheckPreviewImage, "api", checkreport.Pass, w.previewRepo("api")+":sha-"+sha("2"),
		"the head of main")
	expect(t, r, CheckPreviewDNS, "", checkreport.Pass, "shop-0."+suffix+" resolves to the preview load balancer "+
		lbHost+" (198.51.100.7, 198.51.100.8)")
	expect(t, r, CheckPreviewTLS, "", checkreport.Pass, "*."+suffix, "Amazon RSA 2048 M02", "2027-05-01")
}

// TestRunShorthandPreview: spec.preview previews the one repository, by its
// key.
func TestRunShorthandPreview(t *testing.T) {
	w := newWorld(t)
	web := *w.repo("web")
	w.project.Spec.Preview = &web.Preview.ProjectPreview
	web.Preview = nil
	w.project.Spec.Repositories = []v1alpha1.ProjectRepository{web}
	r := w.run()
	expect(t, r, CheckPreviewImage, "web", checkreport.Pass, w.previewRepo("web")+":sha-"+sha("1"))
	if n := r.Failed(); n != 0 {
		t.Errorf("Failed = %d:\n%s", n, dump(r))
	}
}

func TestRunVerdict(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.Project)
		ready  checkreport.Status
		labels checkreport.Status
		want   []string
	}{
		{"not reported", func(p *v1alpha1.Project) { p.Status.Conditions = nil }, checkreport.Fail,
			checkreport.Skip, []string{"intent-controller has not reported on this Project"}},
		{"stale", func(p *v1alpha1.Project) { p.Generation = 4 }, checkreport.Fail, checkreport.Skip,
			[]string{"Ready describes generation 3 and the Project is at generation 4"}},
		{"app not installed", func(p *v1alpha1.Project) {
			p.Status.Conditions[0].Status = metav1.ConditionFalse
			p.Status.Conditions[0].Reason = v1alpha1.ReasonAppNotInstalled
			p.Status.Conditions[0].Message = "the App cannot act on https://github.com/acme/shop-api with " +
				"contents: write: 404"
		}, checkreport.Fail, checkreport.Skip, []string{"AppNotInstalled: the App cannot act on"}},
		{"label refused", func(p *v1alpha1.Project) {
			p.Status.Conditions[0].Status = metav1.ConditionFalse
			p.Status.Conditions[0].Reason = v1alpha1.ReasonAppNotInstalled
			p.Status.Conditions[0].Message = `the label "patchy:shop" cannot be created on ` + intentsURL + ": 403"
		}, checkreport.Fail, checkreport.Fail, []string{`the label "patchy:shop" cannot be created`}},
		{"suspended", func(p *v1alpha1.Project) { p.Spec.Suspend = true }, checkreport.Fail, checkreport.Pass,
			[]string{"spec.suspend is true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.mutate(w.project)
			r := w.run()
			expect(t, r, CheckReady, "", tc.ready, tc.want...)
			expect(t, r, CheckLabels, "", tc.labels)
			if tc.labels == checkreport.Fail {
				expect(t, r, CheckLabels, "", checkreport.Fail, tc.want...)
			}
		})
	}
}

func TestRunIntentNames(t *testing.T) {
	w := newWorld(t)
	w.project.Status.Conditions[1] = metav1.Condition{Type: v1alpha1.ConditionIntentNameConflict,
		Status: metav1.ConditionTrue, Reason: "NameHeld", Message: "issue #7's name shop-7 is held"}
	expect(t, w.run(), CheckIntentNames, "", checkreport.Fail, "issue #7's name shop-7 is held")

	w = newWorld(t)
	w.project.Status.Conditions = w.project.Status.Conditions[:1]
	expect(t, w.run(), CheckIntentNames, "", checkreport.Skip, "not reported yet")
}

func TestRunForges(t *testing.T) {
	t.Run("one repository's org is not covered", func(t *testing.T) {
		w := newWorld(t)
		w.repo("lib").URL = "https://github.com/other/shop-lib"
		r := w.run()
		expect(t, r, CheckForge, "lib", checkreport.Fail, "no forge matches repository", "namespace patchy")
		expect(t, r, CheckForge, "web", checkreport.Pass)
	})
	t.Run("ambiguous", func(t *testing.T) {
		w := newWorld(t)
		w.forges = append(w.forges, testForge("second", metav1.ConditionTrue, "acme"))
		expect(t, w.run(), CheckForge, IntentRepository, checkreport.Fail, "ambiguous forge match", "github, second")
	})
	t.Run("most constrained wins", func(t *testing.T) {
		w := newWorld(t)
		w.forges = append(w.forges, testForge("any", metav1.ConditionTrue))
		expect(t, w.run(), CheckForge, "web", checkreport.Pass, "Forge github covers")
	})
	t.Run("not ready", func(t *testing.T) {
		w := newWorld(t)
		w.forges[0].Status.Conditions[0].Status = metav1.ConditionFalse
		w.forges[0].Status.Conditions[0].Reason = "CredentialInvalid"
		w.forges[0].Status.Conditions[0].Message = "the App key is not a PEM key"
		expect(t, w.run(), CheckForge, "api", checkreport.Fail, "it is not Ready: CredentialInvalid: the App key")
	})
	t.Run("not validated", func(t *testing.T) {
		w := newWorld(t)
		w.forges[0].Status.Conditions = nil
		expect(t, w.run(), CheckForge, "api", checkreport.Fail, "has not validated its credential yet")
	})
}

func TestRunAgentImage(t *testing.T) {
	no := false
	cases := []struct {
		name   string
		mutate func(*world)
		key    string
		status checkreport.Status
		want   []string
	}{
		{"none declared, one required", func(w *world) { delete(w.github.files[w.repo("api").URL], ".patchy/agent.yaml") },
			"api", checkreport.Fail, []string{"declares no agent image", "requireRepositoryImage", "ImageRequired"}},
		{"none declared, none required", func(w *world) {
			delete(w.github.files[w.repo("api").URL], ".patchy/agent.yaml")
			w.project.Spec.RequireRepositoryImage = &no
		}, "api", checkreport.Skip, []string{"default runner image"}},
		{"devcontainer fallback", func(w *world) {
			delete(w.github.files[w.repo("api").URL], ".patchy/agent.yaml")
			w.github.files[w.repo("api").URL][".devcontainer/devcontainer.json"] = `{"image": "` + w.agentImage("api") + `"}`
		}, "api", checkreport.Pass, []string{".devcontainer/devcontainer.json at main@"}},
		{"refused declaration", func(w *world) { w.github.files[w.repo("lib").URL][".patchy/agent.yaml"] = "image: [\n" },
			"lib", checkreport.Fail, []string{".patchy/agent.yaml at main@333333333333 is refused"}},
		{"not allowlisted", func(w *world) {
			w.setting(sourceController, keyImageRegistries, "ghcr.io/acme/")
		}, "web", checkreport.Fail, []string{"fails source-controller's policy", "allowlist", "ghcr.io/acme/"}},
		{"not published", func(w *world) {
			w.github.files[w.repo("web").URL][".patchy/agent.yaml"] = "image: " + w.host + "/patchy/app-envs/web:v2\n"
		}, "web", checkreport.Fail, []string{"is not published", "MANIFEST_UNKNOWN"}},
		{"repository images off, one required", func(w *world) {
			w.setting(sourceController, keyRepositoryImages, "false")
		}, "web", checkreport.Fail, []string{"does not resolve repository-declared images",
			"agent.repositoryImages.enabled"}},
		{"repository images off, none required", func(w *world) {
			w.setting(sourceController, keyRepositoryImages, "false")
			w.project.Spec.RequireRepositoryImage = &no
		}, "web", checkreport.Skip, []string{"runs use the default runner image"}},
		{"no source-controller settings", func(w *world) {
			w.drop(func(o client.Object) bool { return o.GetLabels()[labelName] == sourceController })
		}, "web", checkreport.Skip, []string{"no source-controller ConfigMap in namespace patchy", "patchy check image"}},
		{"unparsable policy", func(w *world) { w.setting(sourceController, keyImageMaxBytes, "lots") },
			"web", checkreport.Skip, []string{"cannot read source-controller's policy", `"lots"`}},
		{"signature required, no key", func(w *world) {
			w.setting(sourceController, keyImageAllowUnsigned, "")
		}, "web", checkreport.Skip, []string{"neither " + keyImageAllowUnsigned}},
		{"default branch unreadable", func(w *world) { delete(w.github.heads, w.repo("lib").URL) },
			"lib", checkreport.Skip, []string{"cannot read the default branch", "404"}},
		{"declaration unreadable", func(w *world) { w.github.fileErr = errors.New("GitHub answered 502") },
			"api", checkreport.Skip, []string{"cannot read the declaration at main@222222222222: GitHub answered 502"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.mutate(w)
			expect(t, w.run(), CheckAgentImage, tc.key, tc.status, tc.want...)
		})
	}
}

// TestRunAgentImageSignature: with a cosign key required, an unsigned image
// fails and the reason says the key came from source-controller.
func TestRunAgentImageSignature(t *testing.T) {
	w := newWorld(t)
	w.setting(sourceController, keyImageAllowUnsigned, "")
	w.setting(sourceController, keyImageCosignKeyFile, "/etc/patchy/repository-image/cosign.pub")
	w.objs = append(w.objs, w.configMap("patchy-repository-image-key", sourceController,
		map[string]string{cosignKeyData: cosignKey(t)}))
	expect(t, w.run(), CheckAgentImage, "web", checkreport.Fail, "fails source-controller's policy", "signature")

	w = newWorld(t)
	w.setting(sourceController, keyImageAllowUnsigned, "")
	w.setting(sourceController, keyImageCosignKeyFile, "/etc/patchy/repository-image/cosign.pub")
	expect(t, w.run(), CheckAgentImage, "web", checkreport.Pass, "its cosign key ConfigMap was not found")
}

// cosignKey is a fresh P-256 public key's PEM, as cosign generate-key-pair
// writes it.
func cosignKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestRunPreviews(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*world)
		check  string
		key    string
		status checkreport.Status
		want   []string
	}{
		{"previews off in intent-controller", func(w *world) {
			w.setting(intentController, keyIntentPreviewsEnabled, "false")
		}, CheckPreviews, "", checkreport.Fail, []string{keyIntentPreviewsEnabled, "previewController.enabled"}},
		{"no preview-controller", func(w *world) {
			w.drop(func(o client.Object) bool { return o.GetLabels()[labelName] == previewController })
		}, CheckPreviews, "", checkreport.Fail, []string{"no preview-controller ConfigMap"}},
		{"outside the prefix", func(w *world) {
			w.repo("api").Preview.ImageRepository = "ghcr.io/acme/patchy/previews/api"
		}, CheckPreviewImage, "api", checkreport.Fail, []string{"is not under preview-controller's image prefix"}},
		{"leaf preview-controller refuses", func(w *world) {
			w.repo("api").Preview.ImageRepository = w.previewRepo("api-")
		}, CheckPreviewImage, "api", checkreport.Fail, []string{`"api-" after the prefix`}},
		{"head not published", func(w *world) {
			w.github.heads[w.repo("api").URL] = Head{Branch: "trunk", SHA: sha("a")}
		}, CheckPreviewImage, "api", checkreport.Fail, []string{"sha-" + sha("a") + ", the head of trunk, is not " +
			"published (it holds 1 other sha-<commit> tag)", "PREVIEW_PUBLISH_ENABLED"}},
		{"no such repository", func(w *world) {
			w.repo("api").Preview.ImageRepository = w.previewRepo("nothing")
		}, CheckPreviewImage, "api", checkreport.Fail, []string{"does not exist", "NAME_UNKNOWN"}},
		{"head unreadable, repository reachable", func(w *world) {
			delete(w.github.heads, w.repo("web").URL)
		}, CheckPreviewImage, "web", checkreport.Skip, []string{"is reachable and holds 1 sha-<commit> tag,",
			"default branch is unknown"}},
		{"head unreadable, repository missing", func(w *world) {
			delete(w.github.heads, w.repo("web").URL)
			w.repo("web").Preview.ImageRepository = w.previewRepo("nothing")
		}, CheckPreviewImage, "web", checkreport.Skip, []string{"cannot read the default branch",
			"your registry credentials cannot read"}},
		{"refused configuration", func(w *world) {
			w.project.Spec.Preview = &w.repo("web").Preview.ProjectPreview
		}, CheckPreviews, "", checkreport.Fail, []string{"one the schema refuses"}},
		{"DNS missing", func(w *world) { delete(w.resolver, "shop-0."+suffix) },
			CheckPreviewDNS, "", checkreport.Fail, []string{"does not resolve", "*." + suffix}},
		{"DNS elsewhere", func(w *world) { w.resolver["shop-0."+suffix] = []string{"203.0.113.9"} },
			CheckPreviewDNS, "", checkreport.Fail, []string{"resolves to 203.0.113.9, but the preview load balancer"}},
		{"placeholder missing", func(w *world) {
			w.drop(func(o client.Object) bool { return o.GetName() == placeholderName })
		}, CheckPreviewDNS, "", checkreport.Pass, []string{"not compared with the preview load balancer",
			"cannot read the placeholder Ingress"}},
		{"placeholder without an address", func(w *world) {
			for _, o := range w.objs {
				if ing, ok := o.(*networkingv1.Ingress); ok {
					ing.Status = networkingv1.IngressStatus{}
				}
			}
		}, CheckPreviewDNS, "", checkreport.Pass, []string{"has no load balancer address yet"}},
		{"TLS timeout", func(w *world) {
			w.dialTLS = func(context.Context, string) (tls.ConnectionState, error) { return tls.ConnectionState{}, timeoutErr{} }
		}, CheckPreviewTLS, "", checkreport.Skip, []string{"preview.inboundCIDRs"}},
		{"TLS deadline", func(w *world) {
			w.dialTLS = func(ctx context.Context, _ string) (tls.ConnectionState, error) {
				return tls.ConnectionState{}, context.DeadlineExceeded
			}
		}, CheckPreviewTLS, "", checkreport.Skip, []string{"no answer from shop-0." + suffix + ":443"}},
		{"TLS untrusted", func(w *world) {
			w.dialTLS = func(context.Context, string) (tls.ConnectionState, error) {
				return tls.ConnectionState{}, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
			}
		}, CheckPreviewTLS, "", checkreport.Fail, []string{"not trusted for it", "*." + suffix}},
		{"TLS wrong name", func(w *world) {
			w.dialTLS = func(context.Context, string) (tls.ConnectionState, error) {
				return tls.ConnectionState{}, x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"other.test"}},
					Host: "shop-0." + suffix}
			}
		}, CheckPreviewTLS, "", checkreport.Fail, []string{"not trusted for it"}},
		{"TLS refused", func(w *world) {
			w.dialTLS = func(context.Context, string) (tls.ConnectionState, error) {
				return tls.ConnectionState{}, errors.New("connection refused")
			}
		}, CheckPreviewTLS, "", checkreport.Fail, []string{"connection refused"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.mutate(w)
			expect(t, w.run(), tc.check, tc.key, tc.status, tc.want...)
		})
	}
}

// TestRunNoPreview: a Project that previews nothing skips the preview path,
// and never resolves or dials a host.
func TestRunNoPreview(t *testing.T) {
	w := newWorld(t)
	for i := range w.project.Spec.Repositories {
		w.project.Spec.Repositories[i].Preview = nil
	}
	w.resolver = nil
	w.dialTLS = func(context.Context, string) (tls.ConnectionState, error) {
		t.Error("dialed a preview host for a Project without previews")
		return tls.ConnectionState{}, nil
	}
	r := w.run()
	expect(t, r, CheckPreviews, "", checkreport.Skip, "previews no repository")
	expect(t, r, CheckPreviewDNS, "", checkreport.Skip, "no preview")
	expect(t, r, CheckPreviewTLS, "", checkreport.Skip, "no preview")
	for _, c := range r.Checks {
		if c.Name == CheckPreviewImage {
			t.Errorf("preview-image line for %s without a preview", c.Repository)
		}
	}
	if n := r.Failed(); n != 0 {
		t.Errorf("Failed = %d:\n%s", n, dump(r))
	}
}

// TestRunSharedConfigMap: kustomize's one patchy-config ConfigMap
// configures every controller.
func TestRunSharedConfigMap(t *testing.T) {
	w := newWorld(t)
	shared := map[string]string{}
	for _, o := range w.objs {
		if cm, ok := o.(*corev1.ConfigMap); ok {
			for k, v := range cm.Data {
				shared[k] = v
			}
		}
	}
	w.drop(func(o client.Object) bool { _, ok := o.(*corev1.ConfigMap); return ok })
	w.objs = append(w.objs, w.configMap("patchy-config", sharedName, shared))
	r := w.run()
	if n := r.Failed(); n != 0 {
		t.Errorf("Failed = %d:\n%s", n, dump(r))
	}
	expect(t, r, CheckAgentImage, "web", checkreport.Pass, "policy from patchy/patchy-config")
}

// TestRunProjectUnreadable: a missing Project is the API server's error,
// so the CLI can exit with its not-found code.
func TestRunProjectUnreadable(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).Build()
	_, err := Run(context.Background(), Config{Reader: c, Namespace: testNamespace, Project: "absent"})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Run = %v, want not found", err)
	}
}

// TestReportLinesAndPrintable: a reason from outside the CLI is made
// inert, and the Project-wide checks show "-" for their repository.
func TestReportLinesAndPrintable(t *testing.T) {
	var r Report
	r.add(CheckReady, "", checkreport.Fail, "Bad: \x1b[2Jcleared")
	r.add(CheckForge, "web", checkreport.Pass, "fine")
	if got := r.Checks[0].Reason; got != `Bad: \x1b[2Jcleared` {
		t.Errorf("reason = %q", got)
	}
	lines := r.Lines()
	if lines[0].Cells[1] != "-" || lines[1].Cells[1] != "web" {
		t.Errorf("lines = %+v", lines)
	}
	if r.Failed() != 1 {
		t.Errorf("Failed = %d", r.Failed())
	}
}
