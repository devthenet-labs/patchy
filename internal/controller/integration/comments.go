// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// A post — a new issue, comment or notice — is the one tracker write that a
// repeat duplicates, and the projection decides on one from the informer
// cache, which can lag this reconciler's own last write: the reconcile queued
// behind a busy one (dirtied by an event during it, often that reconcile's
// own earlier annotation write) starts before the watch event of that
// reconcile's final write lands, and reads the Finding without it. So every
// post is first confirmed against the API server. controller-runtime never
// runs two reconciles of one key at once, so once that read shows nothing
// posted, no other projection of the finding can be mid-post.

// apiReader is the uncached reader: the manager's API reader in production
// (SetupWithManager refuses to run without one), the client itself for a
// reconciler driven directly over an uncached client.
func (r *FindingReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// latest re-reads the Finding from the API server, past the cache.
func (r *FindingReconciler) latest(ctx context.Context, fnd *v1alpha1.Finding) (*v1alpha1.Finding, error) {
	var cur v1alpha1.Finding
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
		return nil, fmt.Errorf("re-read finding: %w", err)
	}
	return &cur, nil
}

// issueComments keeps marker-headed comments — the enrichment and
// runner-image stickies, the stage reports — on one tracking issue through
// one projection pass. Each is recorded by id on status.tracking.comments
// when it is posted or first found, and edited by that id thereafter; the
// issue's comments are listed only for a marker with no id recorded (a post
// interrupted before its record, or a comment from before ids were kept).
// The Finding is re-read and the comments listed at most once per pass.
type issueComments struct {
	r       *FindingReconciler
	fnd     *v1alpha1.Finding
	tracker trackerClient
	repo    ghclient.Repo
	number  int

	fresh   *v1alpha1.Finding   // the API server's copy, once read
	listed  []*ghclient.Comment // the issue's comments, once listed
	didList bool
}

func (r *FindingReconciler) issueComments(
	fnd *v1alpha1.Finding, tracker trackerClient, repo ghclient.Repo, number int,
) *issueComments {
	return &issueComments{r: r, fnd: fnd, tracker: tracker, repo: repo, number: number}
}

// upsert keeps exactly one comment headed by body's first line on the issue,
// carrying body.
func (c *issueComments) upsert(ctx context.Context, body string) error {
	marker, _, _ := strings.Cut(body, "\n")
	digest := hashOf(body)
	rec, err := c.recorded(ctx, marker)
	if err != nil {
		return err
	}
	if rec != nil {
		if rec.Digest == digest {
			return nil
		}
		err := c.tracker.EditComment(ctx, c.repo, rec.ID, body)
		if err == nil {
			return c.record(ctx, v1alpha1.TrackedComment{Marker: marker, ID: rec.ID, Digest: digest})
		}
		if !ghclient.IsNotFound(err) {
			return err
		}
		// Deleted on the tracker: find or post it afresh below. A 404 because
		// the whole issue is gone resurfaces from the listing, where the
		// caller unlinks it.
	}
	existing, err := c.list(ctx)
	if err != nil {
		return err
	}
	if found := findSticky(existing, marker); found != nil {
		if found.Body != body {
			if err := c.tracker.EditComment(ctx, c.repo, found.ID, body); err != nil {
				return err
			}
		}
		return c.record(ctx, v1alpha1.TrackedComment{Marker: marker, ID: found.ID, Digest: digest})
	}
	id, err := c.tracker.CreateComment(ctx, c.repo, c.number, body)
	if err != nil {
		return err
	}
	return c.record(ctx, v1alpha1.TrackedComment{Marker: marker, ID: id, Digest: digest})
}

// recorded returns the comment recorded for marker. The cached Finding's
// "none" is confirmed against the API server before it is believed: it is
// the answer that leads to a post.
func (c *issueComments) recorded(ctx context.Context, marker string) (*v1alpha1.TrackedComment, error) {
	if rec := trackedComment(c.fnd.Status.Tracking, marker); rec != nil {
		return rec, nil
	}
	if c.fresh == nil {
		cur, err := c.r.latest(ctx, c.fnd)
		if err != nil {
			return nil, err
		}
		c.fresh = cur
	}
	return trackedComment(c.fresh.Status.Tracking, marker), nil
}

// list returns the issue's comments, listing them once per pass.
func (c *issueComments) list(ctx context.Context) ([]*ghclient.Comment, error) {
	if !c.didList {
		cs, err := c.tracker.ListComments(ctx, c.repo, c.number)
		if err != nil {
			return nil, err
		}
		c.listed, c.didList = cs, true
	}
	return c.listed, nil
}

// record writes rec onto status.tracking.comments (single writer:
// integration-controller). It reads past the cache: the comment exists now,
// and this record is what keeps the next projection from posting it again,
// so it must not be lost to a stale read's conflicts.
func (c *issueComments) record(ctx context.Context, rec v1alpha1.TrackedComment) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := c.r.apiReader().Get(ctx, client.ObjectKeyFromObject(c.fnd), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if cur.Status.Tracking == nil {
			return nil // unlinked meanwhile; the comment went with the issue
		}
		if !setTrackedComment(cur.Status.Tracking, rec) {
			c.fnd.Status.Tracking = cur.Status.Tracking
			return nil
		}
		if err := c.r.Status().Update(ctx, &cur); err != nil {
			return err
		}
		c.fnd.Status.Tracking = cur.Status.Tracking
		return nil
	})
	if err != nil {
		return fmt.Errorf("record tracking comment %q: %w", rec.Marker, err)
	}
	return nil
}

// trackedComment returns the comment recorded for marker, or nil.
func trackedComment(t *v1alpha1.TrackingStatus, marker string) *v1alpha1.TrackedComment {
	if t == nil {
		return nil
	}
	for i := range t.Comments {
		if t.Comments[i].Marker == marker {
			return &t.Comments[i]
		}
	}
	return nil
}

// setTrackedComment records rec by its marker, reporting whether anything
// changed.
func setTrackedComment(t *v1alpha1.TrackingStatus, rec v1alpha1.TrackedComment) bool {
	if cur := trackedComment(t, rec.Marker); cur != nil {
		if *cur == rec {
			return false
		}
		*cur = rec
		return true
	}
	t.Comments = append(t.Comments, rec)
	return true
}
