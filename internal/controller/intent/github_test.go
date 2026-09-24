// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghsecret"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// TestCredentialsKeptBetweenCalls: the production GitHub reads a Forge's
// Secret once for a run of calls with one repository and permission set, not
// once per call, and reads it again once the credential is kept long enough,
// or when the Forge changes.
func TestCredentialsKeptBetweenCalls(t *testing.T) {
	ctx := context.Background()
	forgeObj := &v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "github"},
		Spec: v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub,
			SecretRef: v1alpha1.LocalSecretReference{Name: "cred"}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "cred"},
		Data:       map[string][]byte{ghsecret.KeyToken: []byte("ghp_dev")},
	}
	reads := 0
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(forgeObj, secret).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					reads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	clock := &fakeClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	g := NewForgeGitHub(forge.NewStore(c), testNS).(*forgeGitHub)
	g.now = clock.Now

	call := func() {
		t.Helper()
		for _, url := range []string{intentRepoURL, intentRepoURL, appRepoURL} {
			if _, _, err := g.client(ctx, url, issuesWrite); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := g.client(ctx, appRepoURL, contentsWrite); err != nil {
			t.Fatal(err)
		}
		if login, err := g.BotLogin(ctx, intentRepoURL); err != nil || login != "" {
			t.Fatalf("BotLogin = %q, %v; a personal access token has no bot", login, err)
		}
	}
	call()
	// One read each for (intents, issues), (app, issues), (app, contents)
	// and the bot login.
	if reads != 4 {
		t.Fatalf("%d Secret reads for the first calls, want 4", reads)
	}
	for range 5 {
		clock.Advance(time.Minute)
		call()
	}
	if reads != 4 {
		t.Errorf("%d Secret reads after repeated calls, want still 4", reads)
	}
	clock.Advance(credentialReread)
	call()
	if reads != 8 {
		t.Errorf("%d Secret reads once the credentials were kept %s, want 8", reads, credentialReread)
	}
	// A changed Forge is read again at once.
	var f v1alpha1.Forge
	if err := c.Get(ctx, client.ObjectKeyFromObject(forgeObj), &f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Orgs = []string{"acme"}
	if err := c.Update(ctx, &f); err != nil {
		t.Fatal(err)
	}
	call()
	if reads != 12 {
		t.Errorf("%d Secret reads after the Forge changed, want 12", reads)
	}
}
