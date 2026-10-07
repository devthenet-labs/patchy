// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package authz

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/internal/web/auth"
)

// Tier is how much of one Project's intents a caller may read. The tiers
// are nested: transcripts implies intents.
type Tier int

// The read tiers, each a SubjectAccessReview for get on a virtual
// subresource of projects.patchy.bitwisemedia.uk, by Project name.
const (
	// TierNone: the Project is invisible. Its intents answer 404, exactly
	// like an intent that does not exist.
	TierNone Tier = iota
	// TierIntents: get on projects/intents. The board, the timeline and a
	// run's numbers: phases, durations, recorded costs, outcomes in public
	// wording, pull request and preview links, digests.
	TierIntents
	// TierTranscripts: get on projects/intents and projects/transcripts. The
	// private content too: plan text, run reports and transcripts, persisted
	// and live.
	TierTranscripts
)

// The virtual subresources the tiers are reviewed on. No such subresource
// exists on the API server, so a rule naming one grants nothing to kubectl;
// like the custom verbs, the strings exist for access reviews alone.
const (
	ProjectResource        = "projects"
	SubresourceIntents     = "intents"
	SubresourceTranscripts = "transcripts"
	// SubresourcePreviews is get on projects/previews: may open the Project's
	// live previews. It is reviewed on its own through Allowed, never as a
	// tier: a preview viewer need not see the Project's intents, and an
	// intents reader is not a preview viewer.
	SubresourcePreviews = "previews"
)

// projectSubresources is every virtual subresource Allowed answers for.
var projectSubresources = map[string]bool{
	SubresourceIntents: true, SubresourceTranscripts: true, SubresourcePreviews: true,
}

// projectReviewConcurrency bounds the per-Project reviews one resolution runs
// at once.
const projectReviewConcurrency = 8

// FullProjects is the tier source for auth mode none: every Project, every
// tier. status-server only uses it behind its explicit development flag.
type FullProjects struct{}

// Tiers returns TierTranscripts for every Project.
func (FullProjects) Tiers(_ context.Context, _ auth.Identity, projects []string) (map[string]Tier, error) {
	out := make(map[string]Tier, len(projects))
	for _, p := range projects {
		out[p] = TierTranscripts
	}
	return out, nil
}

// ProjectReviewer resolves read tiers per Project through
// SubjectAccessReviews, cached briefly per identity, Project and
// subresource. It never reuses the findings Reviewer's namespace-wide
// grants: a caller who may view findings may see no Project at all.
type ProjectReviewer struct {
	client    client.Client
	namespace string
	ttl       time.Duration
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]resourceCached
}

// NewProjectReviewer builds a ProjectReviewer for the server's namespace.
// ttl <= 0 uses the package default.
func NewProjectReviewer(c client.Client, namespace string, ttl time.Duration) *ProjectReviewer {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &ProjectReviewer{
		client: c, namespace: namespace, ttl: ttl, now: time.Now,
		cache: make(map[string]resourceCached),
	}
}

// Tiers resolves the identity's tier for each named Project. A review with
// no name runs first for each subresource: a rule without resourceNames
// matches it, so a namespace-wide grant costs one review whatever the number
// of Projects. Only what it leaves undecided is reviewed per Project, a few
// at a time. A rule with resourceNames never matches the nameless review, so
// a grant for Project a can only ever answer a's own review.
func (r *ProjectReviewer) Tiers(ctx context.Context, id auth.Identity, projects []string) (map[string]Tier, error) {
	allIntents, err := r.allowed(ctx, id, "", SubresourceIntents)
	if err != nil {
		return nil, err
	}
	allTranscripts, err := r.allowed(ctx, id, "", SubresourceTranscripts)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Tier, len(projects))
	if allIntents && allTranscripts {
		for _, p := range projects {
			out[p] = TierTranscripts
		}
		return out, nil
	}

	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(projectReviewConcurrency)
	for _, p := range projects {
		g.Go(func() error {
			intents := allIntents
			if !intents {
				ok, err := r.allowed(gctx, id, p, SubresourceIntents)
				if err != nil {
					return err
				}
				intents = ok
			}
			tier := TierNone
			if intents {
				tier = TierIntents
				transcripts := allTranscripts
				if !transcripts {
					ok, err := r.allowed(gctx, id, p, SubresourceTranscripts)
					if err != nil {
						return err
					}
					transcripts = ok
				}
				if transcripts {
					tier = TierTranscripts
				}
			}
			mu.Lock()
			out[p] = tier
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// Allowed reports whether the identity holds get on projects/<subresource>
// for the named Project. The nameless review runs first, as in Tiers, so a
// grant for every Project costs one cached review; only when it is refused is
// the Project reviewed by name. An empty project or a subresource that is not
// one of the virtual subresources above is an error, never an answer: a
// caller that lost track of the Project must fail closed, not ask the
// namespace-wide question by accident.
func (r *ProjectReviewer) Allowed(ctx context.Context, id auth.Identity, project, subresource string) (bool, error) {
	if project == "" {
		return false, fmt.Errorf("access review get projects/%s: no Project named", subresource)
	}
	if !projectSubresources[subresource] {
		return false, fmt.Errorf("access review get projects/%s: not a Project subresource", subresource)
	}
	all, err := r.allowed(ctx, id, "", subresource)
	if err != nil || all {
		return all, err
	}
	return r.allowed(ctx, id, project, subresource)
}

// allowed runs (or answers from the cache) one review of get on
// projects/<subresource>, for one Project by name or, with name "", for
// every Project.
func (r *ProjectReviewer) allowed(ctx context.Context, id auth.Identity, name, subresource string) (bool, error) {
	key := cacheKey(id) + "\x00" + name + "\x00" + subresource
	r.mu.Lock()
	if hit, ok := r.cache[key]; ok && r.now().Before(hit.expires) {
		r.mu.Unlock()
		return hit.allowed, nil
	}
	r.mu.Unlock()

	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   id.Username,
			Groups: id.Groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   r.namespace,
				Group:       group,
				Resource:    ProjectResource,
				Subresource: subresource,
				Name:        name,
				Verb:        "get",
			},
		},
	}
	if err := r.client.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("access review get projects/%s %q for %s: %w", subresource, name, id.Username, err)
	}

	r.mu.Lock()
	if len(r.cache) >= cacheLimit {
		r.cache = make(map[string]resourceCached) // reset rather than evict piecemeal
	}
	r.cache[key] = resourceCached{allowed: sar.Status.Allowed, expires: r.now().Add(r.ttl)}
	r.mu.Unlock()
	return sar.Status.Allowed, nil
}
