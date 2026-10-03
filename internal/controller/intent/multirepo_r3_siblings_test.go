// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
)

// TestSiblingCrossLinkNeverWritesToADepartedRepository is the round-3
// regression of the sibling cross-link. Web's comment was refused (a locked
// conversation); the lock is lifted and then web leaves the Project, whose
// generation change re-arms the refused cross-link. Before the fix the next
// poll posted it on web's pull request, a write to a repository the Project
// no longer holds. Now nothing is written there, and SiblingsLinked turns
// True naming web's pull request as not linked. A cross-link whose
// repository no Forge resolves is recorded as refused, as GitHub's refusal
// is, rather than retried at every poll.
func TestSiblingCrossLinkNeverWritesToADepartedRepository(t *testing.T) {
	t.Run("a repository that left the project", func(t *testing.T) {
		e := newMultiEnv(t)
		e.gh.lockedPRs[2] = true
		name := e.inReviewMulti()
		e.settleActions(name)
		if c := meta.FindStatusCondition(e.get(name).Status.Conditions, v1alpha1.ConditionSiblingsLinked); c == nil ||
			c.Reason != ReasonSiblingsRefused {
			t.Fatalf("SiblingsLinked = %+v, want web's refusal recorded", c)
		}
		e.gh.mu.Lock()
		e.gh.lockedPRs[2] = false
		e.gh.mu.Unlock()
		webWrites := e.webPRWrites()

		e.dropRepository(webRepoURL)
		e.settleActions(name)
		if n := len(e.siblingComments(2)); n != 0 || e.webPRWrites() != webWrites {
			t.Errorf("%d siblings comments and %d new writes on web's pull request, want none", n,
				e.webPRWrites()-webWrites)
		}
		c := meta.FindStatusCondition(e.get(name).Status.Conditions, v1alpha1.ConditionSiblingsLinked)
		if c == nil || c.Status != metav1.ConditionTrue ||
			!strings.Contains(c.Message, webSlug+"#2 was not linked, its repository having left the project") {
			t.Errorf("SiblingsLinked = %+v, want True naming web's pull request as not linked", c)
		}
		if len(e.siblingComments(1)) != 1 {
			t.Errorf("siblings comments on app's pull request = %d, want its one", len(e.siblingComments(1)))
		}
	})
	t.Run("a repository no forge resolves", func(t *testing.T) {
		e := newMultiEnv(t)
		e.setRepoErrs(map[string]error{
			"CreatePullRequestComment acme/acme.web_app": fmt.Errorf("resolve: %w", forge.ErrNoMatch),
		})
		name := e.inReviewMulti()
		e.settleActions(name)
		c := meta.FindStatusCondition(e.get(name).Status.Conditions, v1alpha1.ConditionSiblingsLinked)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonSiblingsRefused ||
			!strings.Contains(c.Message, webSlug+"#2") || !strings.Contains(c.Message, "no forge matches") {
			t.Fatalf("SiblingsLinked = %+v, want web's cross-link recorded as not posted", c)
		}
		e.gh.mu.Lock()
		tries := e.gh.calls["CreateIssueComment"]
		e.gh.mu.Unlock()
		e.settleActions(name)
		e.gh.mu.Lock()
		tries = e.gh.calls["CreateIssueComment"] - tries
		e.gh.mu.Unlock()
		if tries != 0 {
			t.Errorf("%d comments tried with nothing changed, want none", tries)
		}
	})
}
