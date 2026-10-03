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
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// SiblingsLinked reasons.
const (
	// ReasonSiblingsPosted: every pull request carries the comment.
	ReasonSiblingsPosted = "Posted"
	// ReasonSiblingsRefused: the comment could not be posted on a pull
	// request, and asking again would meet the same answer: GitHub refused
	// it (a locked conversation, a permission the App lost), or patchy can
	// no longer reach its repository (no Forge covers it).
	ReasonSiblingsRefused = "Refused"
)

// siblingLinks is what linkSiblings remembers of one Intent between passes:
// the pull requests confirmed to carry the comment (by "owner/name#n"), never
// listed again, and the state the last refusal was met in (siblingsState),
// "" when there was none.
type siblingLinks struct {
	posted  map[string]bool
	refused string
}

// siblingsState is what a refused cross-link waits to change before it is
// tried again: the Project's generation (a permission restored is often a
// Project edit away) and every pull request's head.
func (p *pass) siblingsState() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", p.proj.Generation)
	for _, pr := range p.in.Status.PullRequests {
		fmt.Fprintf(&b, " %s#%d@%s", repoSlug(pr.Repository), pr.Number, pr.HeadSHA)
	}
	return b.String()
}

// linkSiblings posts, on each pull request of an intent that opened more than
// one, the comment listing the others (templates.RenderIntentSiblingsComment):
// once per pull request, a repeated pass adopting the one the App's bot
// posted by its marker. It runs only once the Intent is in review: the
// cross-link is cosmetic, so it never holds a phase back, and the pull
// requests are open, and their records written, before any of it is
// attempted. It is best effort. A transient failure is logged and the rest
// retried at the next poll of the pull requests. A refusal, or a repository
// patchy can no longer reach (unreachable), records SiblingsLinked False
// saying which pull request and why, once, and is tried again only once the
// Project changes or a pull request's head moves, never at every poll: a
// locked conversation would otherwise cost a refused write and a listing of
// every pull request each minute for the whole review. A pull request whose
// repository has left the Project is written nothing (nor listed): the
// condition names it, and the comment on the others still lists it. A pull
// request confirmed to carry the comment is not listed again. Once every one
// whose repository the Project still holds carries it, SiblingsLinked is True
// and nothing is listed again. changed reports a status write.
func (p *pass) linkSiblings(ctx context.Context) (changed bool, err error) {
	prs := p.in.Status.PullRequests
	if phase := p.in.Status.Phase; phase != v1alpha1.IntentInReview && phase != v1alpha1.IntentRevising {
		return false, nil
	}
	if len(prs) < 2 || meta.IsStatusConditionTrue(p.in.Status.Conditions, v1alpha1.ConditionSiblingsLinked) {
		return false, nil
	}
	state := p.siblingsState()
	links := siblingLinks{posted: map[string]bool{}}
	p.r.memo(func() {
		if known := p.r.siblings[p.in.Name]; known != nil {
			links.refused = known.refused
			for k := range known.posted {
				links.posted[k] = true
			}
		}
	})
	if links.refused == state {
		return false, nil
	}
	remember := func(refusedIn string) {
		links.refused = refusedIn
		p.r.memo(func() { p.r.siblings[p.in.Name] = &links })
	}
	var refused, left []string
	for _, pr := range prs {
		ref := fmt.Sprintf("%s#%d", repoSlug(pr.Repository), pr.Number)
		if links.posted[ref] {
			continue
		}
		if p.leftProject(pr.Repository) {
			left = append(left, ref)
			continue
		}
		err := p.linkSibling(ctx, pr, prs)
		switch {
		case err == nil:
			links.posted[ref] = true
		case unreachable(err):
			refused = append(refused, fmt.Sprintf("%s: %v", ref, err))
		default:
			remember(links.refused)
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "link the intent's pull requests; retried at the next poll",
				slog.String("intent", p.in.Name), slog.String("repository", pr.Repository),
				slog.Int64("number", pr.Number), slog.Any("error", err))
			return false, nil
		}
	}
	status, reason, msg := siblingsOutcome(refused, left)
	refusedIn := ""
	if len(refused) > 0 {
		refusedIn = state
	}
	if c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionSiblingsLinked); c != nil &&
		c.Status == status && c.Reason == reason && c.Message == msg {
		remember(refusedIn)
		return false, nil
	}
	// The refusal is remembered only once recorded: a lost write tries
	// again rather than leave it unsaid.
	remember(links.refused)
	if err := p.update(ctx, func(cur *v1alpha1.Intent) error {
		setCondition(cur, v1alpha1.ConditionSiblingsLinked, status, reason, msg)
		return nil
	}); err != nil {
		return false, err
	}
	remember(refusedIn)
	return true, nil
}

// siblingsOutcome is the SiblingsLinked condition once every pull request
// was tried: False naming each refused one ("owner/name#n: why") while any
// was, True otherwise, and either way naming each one left unlinked because
// its repository left the Project.
func siblingsOutcome(refused, left []string) (metav1.ConditionStatus, string, string) {
	status, reason, msg := metav1.ConditionTrue, ReasonSiblingsPosted,
		"every pull request of the intent carries the comment linking the others"
	if len(left) > 0 {
		msg = "every pull request of the intent whose repository the project still holds carries the comment " +
			"linking the others"
	}
	if len(refused) > 0 {
		status, reason = metav1.ConditionFalse, ReasonSiblingsRefused
		msg = "the comment linking the intent's pull requests could not be posted on " +
			strings.Join(refused, "; ") + "; patchy tries it again once the project changes or a pull " +
			"request's head moves, and nothing waits on it"
	}
	switch len(left) {
	case 0:
	case 1:
		msg += "; " + left[0] + " was not linked, its repository having left the project"
	default:
		msg += "; " + strings.Join(left, ", ") + " were not linked, their repositories having left the project"
	}
	return status, reason, msg
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
