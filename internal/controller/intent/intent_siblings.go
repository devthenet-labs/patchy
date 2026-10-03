// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// SiblingsLinked reasons.
const (
	// ReasonSiblingsPosted: every pull request carries the comment.
	ReasonSiblingsPosted = "Posted"
	// ReasonSiblingsRefused: GitHub refused the comment on a pull request
	// (a locked conversation, a permission the App lost).
	ReasonSiblingsRefused = "Refused"
)

// linkSiblings posts, on each pull request of an intent that opened more than
// one, the comment listing the others (templates.RenderIntentSiblingsComment):
// once per pull request, a repeated pass adopting the one the App's bot
// posted by its marker. It runs only once the Intent is in review: the
// cross-link is cosmetic, so it never holds a phase back, and the pull
// requests are open, and their records written, before any of it is
// attempted. It is best effort. A transient failure is logged and the whole
// thing retried at the next poll of the pull requests; a refusal records
// SiblingsLinked False saying which pull request and why, once, and is
// retried all the same. Once every pull request carries the comment,
// SiblingsLinked is True and nothing is listed again. changed reports a
// status write.
func (p *pass) linkSiblings(ctx context.Context) (changed bool, err error) {
	prs := p.in.Status.PullRequests
	if phase := p.in.Status.Phase; phase != v1alpha1.IntentInReview && phase != v1alpha1.IntentRevising {
		return false, nil
	}
	if len(prs) < 2 || meta.IsStatusConditionTrue(p.in.Status.Conditions, v1alpha1.ConditionSiblingsLinked) {
		return false, nil
	}
	var refused []string
	for _, pr := range prs {
		err := p.linkSibling(ctx, pr, prs)
		switch {
		case err == nil:
		case ghclient.IsRefused(err):
			refused = append(refused, fmt.Sprintf("%s#%d: %v", repoSlug(pr.Repository), pr.Number, err))
		default:
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "link the intent's pull requests; retried at the next poll",
				slog.String("intent", p.in.Name), slog.String("repository", pr.Repository),
				slog.Int64("number", pr.Number), slog.Any("error", err))
			return false, nil
		}
	}
	status, reason, msg := metav1.ConditionTrue, ReasonSiblingsPosted,
		"every pull request of the intent carries the comment linking the others"
	if len(refused) > 0 {
		status, reason = metav1.ConditionFalse, ReasonSiblingsRefused
		msg = "GitHub refused the comment linking the intent's pull requests on " + strings.Join(refused, "; ") +
			"; patchy retries it at every poll of the pull requests, and nothing waits on it"
	}
	if c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionSiblingsLinked); c != nil &&
		c.Status == status && c.Reason == reason && c.Message == msg {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		setCondition(cur, v1alpha1.ConditionSiblingsLinked, status, reason, msg)
		return nil
	})
}

// linkSibling posts the siblings comment on pr unless the App's bot already
// did (postOnceOnPullRequest).
func (p *pass) linkSibling(ctx context.Context, pr v1alpha1.IntentPullRequest,
	prs []v1alpha1.IntentPullRequest) error {
	c := templates.IntentSiblingsComment{Namespace: p.in.Namespace, Intent: p.in.Name,
		Repository: repoSlug(pr.Repository)}
	for _, o := range prs {
		c.PullRequests = append(c.PullRequests, templates.IntentPullRequest{
			Repository: repoSlug(o.Repository), Number: o.Number, URL: o.URL, State: o.State,
		})
	}
	body, err := templates.RenderIntentSiblingsComment(c)
	if err != nil {
		return err
	}
	return p.postOnceOnPullRequest(ctx, pr, templates.SiblingsKey, body)
}
