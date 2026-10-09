// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// TestGitHubFailsClosed: every method of the production GitHub, called on a
// repository no Forge covers or whose Forge's credential cannot be read,
// returns an error and mints no token: nothing is ever called on GitHub
// without a credential the Forge grants.
func TestGitHubFailsClosed(t *testing.T) {
	covered := &v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "github"},
		Spec: v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub, Orgs: []string{"acme"},
			SecretRef: v1alpha1.LocalSecretReference{Name: "missing"}},
	}
	// An empty Secret: the Forge resolves, but no credential is in it.
	emptySecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "empty"}}
	brokenCred := covered.DeepCopy()
	brokenCred.Name = "broken"
	brokenCred.Spec.Orgs = []string{"broken"}
	brokenCred.Spec.SecretRef.Name = "empty"
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(covered, brokenCred, emptySecret).Build()

	cases := []struct {
		name string
		url  string
		// resolves: the repository has a Forge, so the methods that mint
		// no token (Resolve, BotLogin) may succeed or fail on the Secret.
		resolves bool
	}{
		{"no forge covers it", "https://github.com/someone-else/repo", false},
		{"the secret is missing", appRepoURL, true},
		{"the secret holds no credential", "https://github.com/broken/repo", true},
	}
	ghType := reflect.TypeFor[GitHub]()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewForgeGitHub(forge.NewStore(c), testNS).(*forgeGitHub)
			for i := range ghType.NumMethod() {
				m := ghType.Method(i)
				if tc.resolves && (m.Name == "Resolve" || m.Name == "BotLogin") {
					continue
				}
				args := []reflect.Value{reflect.ValueOf(t.Context()), reflect.ValueOf(tc.url)}
				for j := 2; j < m.Type.NumIn(); j++ {
					args = append(args, reflect.Zero(m.Type.In(j)))
				}
				out := reflect.ValueOf(g).MethodByName(m.Name).Call(args)
				last := out[len(out)-1]
				if last.IsNil() {
					t.Errorf("GitHub.%s on %s succeeded", m.Name, tc.url)
				}
				for _, v := range out[:len(out)-1] {
					if k := v.Kind(); (k == reflect.Pointer || k == reflect.Slice || k == reflect.Map) && !v.IsNil() {
						t.Errorf("GitHub.%s on %s returned a value beside its error: %v", m.Name, tc.url, v)
					}
				}
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if len(g.creds) != 0 || len(g.clients) != 0 {
				t.Errorf("tokens minted with no credential: %d credentials, %d clients", len(g.creds), len(g.clients))
			}
		})
	}
}
