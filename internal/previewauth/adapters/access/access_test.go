// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package access

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

// grants answers SubjectAccessReviews: user -> the Project names they may
// get projects/previews for ("" for every Project).
func newReviewer(t *testing.T, grants map[string][]string, seen *[]authorizationv1.ResourceAttributes,
	fail error) Reviewer {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, o client.Object, _ ...client.CreateOption) error {
			if fail != nil {
				return fail
			}
			sar := o.(*authorizationv1.SubjectAccessReview)
			ra := *sar.Spec.ResourceAttributes
			*seen = append(*seen, ra)
			sar.Status.Allowed = ra.Resource == "projects" && ra.Subresource == "previews" && ra.Verb == "get" &&
				ra.Namespace == "patchy" && slices.Contains(grants[sar.Spec.User], ra.Name)
			return nil
		},
	}).Build()
	return Reviewer{Projects: authz.NewProjectReviewer(c, "patchy", time.Minute)}
}

func review(user, project string) previewauth.AccessReview {
	return previewauth.AccessReview{Username: user, Groups: []string{"github:org:team"}, Project: project,
		Subresource: previewauth.SubresourcePreviews}
}

func TestAllowed(t *testing.T) {
	var seen []authorizationv1.ResourceAttributes
	r := newReviewer(t, map[string][]string{"github:all": {""}, "github:one": {"demo"}}, &seen, nil)
	var answers []bool
	r.Record = func(_ context.Context, ok bool) { answers = append(answers, ok) }
	ctx := context.Background()
	tests := []struct {
		user, project string
		want          bool
	}{
		{"github:all", "demo", true},
		{"github:all", "other", true},
		{"github:one", "demo", true},
		{"github:one", "other", false},
		{"github:none", "demo", false},
	}
	for _, tt := range tests {
		ok, err := r.Allowed(ctx, review(tt.user, tt.project))
		if err != nil || ok != tt.want {
			t.Errorf("%s on %s: %v, %v", tt.user, tt.project, ok, err)
		}
	}
	if len(answers) != len(tests) {
		t.Errorf("recorded %v", answers)
	}
	for _, ra := range seen {
		if ra.Subresource != "previews" {
			t.Errorf("review asked %+v", ra)
		}
	}
}

func TestAllowedFailsClosed(t *testing.T) {
	var seen []authorizationv1.ResourceAttributes
	r := newReviewer(t, nil, &seen, nil)
	bad := review("github:a", "demo")
	bad.Subresource = "transcripts"
	if ok, err := r.Allowed(context.Background(), bad); ok || err == nil {
		t.Fatalf("another subresource: %v, %v", ok, err)
	}
	if ok, err := r.Allowed(context.Background(), review("github:a", "")); ok || err == nil {
		t.Fatalf("no project: %v, %v", ok, err)
	}
	boom := errors.New("api down")
	r = newReviewer(t, nil, &seen, boom)
	if ok, err := r.Allowed(context.Background(), review("github:a", "demo")); ok || !errors.Is(err, boom) {
		t.Fatalf("api failure: %v, %v", ok, err)
	}
}
