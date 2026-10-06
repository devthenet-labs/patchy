// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package authz

import (
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
)

// TestProjectReviewerRealRBAC is the proof behind the per-Project tiers: the
// reviews run against a real kube-apiserver's RBAC authorizer, with Roles
// written in the grammar the docs and the chart's example roles use. It is
// what shows that resourceNames scope a virtual subresource by Project name,
// that a nameless review is answered only by a rule without resourceNames,
// and that the native projects resource grants no tier.
func TestProjectReviewerRealRBAC(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping the RBAC envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "patchy"}}); err != nil {
		t.Fatal(err)
	}

	rule := func(subresources []string, names ...string) rbacv1.PolicyRule {
		res := make([]string, len(subresources))
		for i, s := range subresources {
			res[i] = "projects/" + s
		}
		return rbacv1.PolicyRule{
			APIGroups: []string{group}, Resources: res, ResourceNames: names, Verbs: []string{"get"},
		}
	}
	roles := map[string][]rbacv1.PolicyRule{
		// Tier 1 on alpha.
		"alpha-intents": {rule([]string{"intents"}, "alpha")},
		// Tier 2 on beta.
		"beta-transcripts": {rule([]string{"intents", "transcripts"}, "beta")},
		// Tier 1 on every Project.
		"all-intents": {rule([]string{"intents"})},
		// Native get on the alpha Project: a configuration read, no tier.
		"alpha-native": {{
			APIGroups: []string{group}, Resources: []string{"projects"},
			ResourceNames: []string{"alpha"}, Verbs: []string{"get"},
		}},
	}
	for name, rules := range roles {
		if err := admin.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"}, Rules: rules,
		}); err != nil {
			t.Fatal(err)
		}
	}
	bindings := 0
	bind := func(role string, subject rbacv1.Subject) {
		t.Helper()
		bindings++
		if err := admin.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role + "-" + string(rune('a'+bindings)), Namespace: "patchy"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role},
			Subjects:   []rbacv1.Subject{subject},
		}); err != nil {
			t.Fatal(err)
		}
	}
	user := func(n string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: n}
	}
	groupSubject := func(n string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: n}
	}
	bind("alpha-intents", user("github:alice"))
	bind("beta-transcripts", groupSubject("github:acme:security"))
	bind("all-intents", user("github:carol"))
	bind("beta-transcripts", user("github:carol"))
	bind("alpha-native", user("github:dave"))

	projects := []string{"alpha", "beta", "gamma"}
	cases := []struct {
		name string
		id   auth.Identity
		want map[string]Tier
	}{
		{"tier 1 on one Project", auth.Identity{Username: "github:alice"},
			map[string]Tier{"alpha": TierIntents, "beta": TierNone, "gamma": TierNone}},
		{"tier 2 on one Project through a group", auth.Identity{Username: "github:erin",
			Groups: []string{"github:acme:security"}},
			map[string]Tier{"alpha": TierNone, "beta": TierTranscripts, "gamma": TierNone}},
		{"tier 1 everywhere, tier 2 on one", auth.Identity{Username: "github:carol"},
			map[string]Tier{"alpha": TierIntents, "beta": TierTranscripts, "gamma": TierIntents}},
		{"native project read grants no tier", auth.Identity{Username: "github:dave"},
			map[string]Tier{"alpha": TierNone, "beta": TierNone, "gamma": TierNone}},
		// The binding names the prefixed group: the same string unprefixed,
		// as a provider would send it, matches nothing.
		{"an unprefixed group matches nothing", auth.Identity{Username: "github:frank",
			Groups: []string{"acme:security"}},
			map[string]Tier{"alpha": TierNone, "beta": TierNone, "gamma": TierNone}},
		{"a stranger sees nothing", auth.Identity{Username: "github:mallory"},
			map[string]Tier{"alpha": TierNone, "beta": TierNone, "gamma": TierNone}},
	}
	r := NewProjectReviewer(admin, "patchy", 0)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Twice: the second answer comes from the cache and must agree.
			for range 2 {
				got, err := r.Tiers(ctx, tc.id, projects)
				if err != nil {
					t.Fatalf("Tiers: %v", err)
				}
				for _, p := range projects {
					if got[p] != tc.want[p] {
						t.Errorf("%s on %s = %v, want %v", tc.id.Username, p, got[p], tc.want[p])
					}
				}
			}
		})
	}
}
