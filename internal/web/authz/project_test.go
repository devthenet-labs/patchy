// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package authz

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
)

// projectReview is one review the fake API server answered.
type projectReview struct {
	user, name, subresource string
}

// projectSARClient answers project reviews with allow and records each one.
func projectSARClient(t *testing.T, mu *sync.Mutex, seen *[]projectReview,
	allow func(user, name, subresource string) bool) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				sar, ok := obj.(*authorizationv1.SubjectAccessReview)
				if !ok {
					t.Fatalf("unexpected create of %T", obj)
				}
				ra := sar.Spec.ResourceAttributes
				if ra.Group != group || ra.Resource != ProjectResource || ra.Verb != "get" || ra.Namespace != "patchy" {
					t.Errorf("review = %+v, want get on projects in patchy", ra)
				}
				mu.Lock()
				*seen = append(*seen, projectReview{sar.Spec.User, ra.Name, ra.Subresource})
				mu.Unlock()
				sar.Status.Allowed = allow(sar.Spec.User, ra.Name, ra.Subresource)
				return nil
			},
		}).
		Build()
}

func TestProjectReviewerTiers(t *testing.T) {
	projects := []string{"alpha", "beta", "gamma"}
	cases := []struct {
		name  string
		allow func(user, name, subresource string) bool
		want  map[string]Tier
	}{
		{
			name:  "nothing granted",
			allow: func(string, string, string) bool { return false },
			want:  map[string]Tier{"alpha": TierNone, "beta": TierNone, "gamma": TierNone},
		},
		{
			name: "intents on alpha only",
			allow: func(_, name, sub string) bool {
				return name == "alpha" && sub == SubresourceIntents
			},
			want: map[string]Tier{"alpha": TierIntents, "beta": TierNone, "gamma": TierNone},
		},
		{
			name: "transcripts on beta, intents on alpha and beta",
			allow: func(_, name, sub string) bool {
				return (name == "alpha" && sub == SubresourceIntents) || name == "beta"
			},
			want: map[string]Tier{"alpha": TierIntents, "beta": TierTranscripts, "gamma": TierNone},
		},
		{
			// A transcripts grant without the intents one opens nothing: the
			// tiers are nested, and content without the board is not a tier.
			name:  "transcripts without intents",
			allow: func(_, name, sub string) bool { return name == "gamma" && sub == SubresourceTranscripts },
			want:  map[string]Tier{"alpha": TierNone, "beta": TierNone, "gamma": TierNone},
		},
		{
			name:  "namespace-wide intents, transcripts on gamma",
			allow: func(_, name, sub string) bool { return name == "" && sub == SubresourceIntents || name == "gamma" },
			want:  map[string]Tier{"alpha": TierIntents, "beta": TierIntents, "gamma": TierTranscripts},
		},
		{
			name:  "namespace-wide everything",
			allow: func(_, name, _ string) bool { return name == "" },
			want:  map[string]Tier{"alpha": TierTranscripts, "beta": TierTranscripts, "gamma": TierTranscripts},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []projectReview
			r := NewProjectReviewer(projectSARClient(t, &mu, &seen, tc.allow), "patchy", 0)
			got, err := r.Tiers(t.Context(), auth.Identity{Username: "github:dev"}, projects)
			if err != nil {
				t.Fatalf("Tiers: %v", err)
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("tiers = %v, want %v", got, tc.want)
			}
		})
	}
}

// A namespace-wide grant of both tiers is answered by the two nameless
// reviews alone, whatever the number of Projects.
func TestProjectReviewerNamespaceWideShortcut(t *testing.T) {
	var mu sync.Mutex
	var seen []projectReview
	r := NewProjectReviewer(projectSARClient(t, &mu, &seen,
		func(_, name, _ string) bool { return name == "" }), "patchy", 0)
	many := make([]string, 50)
	for i := range many {
		many[i] = "p" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := r.Tiers(t.Context(), auth.Identity{Username: "github:dev"}, many); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Errorf("reviews = %d (%v), want the 2 nameless ones", len(seen), seen)
	}
	for _, s := range seen {
		if s.name != "" {
			t.Errorf("review named %q, want only nameless reviews", s.name)
		}
	}
}

// The cache is keyed by identity, Project and subresource: a grant for alpha
// is never served for beta, a transcripts answer never for intents, and one
// user's grant never for another.
func TestProjectReviewerCacheKeys(t *testing.T) {
	var mu sync.Mutex
	var seen []projectReview
	allow := func(user, name, sub string) bool {
		return user == "github:alice" && name == "alpha"
	}
	r := NewProjectReviewer(projectSARClient(t, &mu, &seen, allow), "patchy", time.Minute)
	alice := auth.Identity{Username: "github:alice"}
	bob := auth.Identity{Username: "github:bob"}
	for range 3 {
		for _, q := range []struct {
			id      auth.Identity
			project string
			want    Tier
		}{
			{alice, "alpha", TierTranscripts},
			{alice, "beta", TierNone},
			{bob, "alpha", TierNone},
			{alice, "alpha", TierTranscripts},
		} {
			got, err := r.Tiers(t.Context(), q.id, []string{q.project})
			if err != nil {
				t.Fatal(err)
			}
			if got[q.project] != q.want {
				t.Errorf("%s on %s = %v, want %v", q.id.Username, q.project, got[q.project], q.want)
			}
		}
	}
	// Alice: 2 nameless + alpha (2) + beta (1); bob: 2 nameless + alpha (1).
	if len(seen) != 8 {
		t.Errorf("reviews = %d (%v), want 8 (every later query answered from the cache)", len(seen), seen)
	}
}

func TestProjectReviewerCacheExpires(t *testing.T) {
	var mu sync.Mutex
	var seen []projectReview
	granted := true
	r := NewProjectReviewer(projectSARClient(t, &mu, &seen,
		func(_, name, _ string) bool { return granted && name == "alpha" }), "patchy", time.Minute)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	id := auth.Identity{Username: "github:dev"}
	if got, _ := r.Tiers(t.Context(), id, []string{"alpha"}); got["alpha"] != TierTranscripts {
		t.Fatalf("tier = %v, want transcripts", got["alpha"])
	}
	granted = false
	if got, _ := r.Tiers(t.Context(), id, []string{"alpha"}); got["alpha"] != TierTranscripts {
		t.Errorf("tier within the TTL = %v, want the cached transcripts", got["alpha"])
	}
	now = now.Add(2 * time.Minute)
	if got, _ := r.Tiers(t.Context(), id, []string{"alpha"}); got["alpha"] != TierNone {
		t.Errorf("tier past the TTL = %v, want the revocation seen", got["alpha"])
	}
}

func TestFullProjects(t *testing.T) {
	got, err := FullProjects{}.Tiers(t.Context(), auth.Identity{}, []string{"a", "b"})
	if err != nil || got["a"] != TierTranscripts || got["b"] != TierTranscripts {
		t.Errorf("FullProjects = %v, %v", got, err)
	}
}
