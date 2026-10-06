// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// previewNotes is what syncPreviewComments remembers of one Intent between
// passes, by pull request ("owner/name#n"): the comment it posted or adopted,
// so a status write lost after posting never posts a second one even when the
// preview has moved on by the next pass; and the body digest GitHub last
// refused there (a locked conversation, a permission the App lost, a
// repository patchy can no longer reach), not tried again until what the
// comment would say changes.
type previewNotes struct {
	ids     map[string]int64
	refused map[string]string
}

// prKey names a pull request in previewNotes.
func prKey(pr v1alpha1.IntentPullRequest) string {
	return fmt.Sprintf("%s#%d", repoSlug(pr.Repository), pr.Number)
}

// notes reads (or, with f, changes) the Intent's previewNotes under the
// reconciler's lock.
func (p *pass) notes(f func(*previewNotes)) {
	p.r.memo(func() {
		n := p.r.previews[p.in.Name]
		if n == nil {
			n = &previewNotes{ids: map[string]int64{}, refused: map[string]string{}}
			p.r.previews[p.in.Name] = n
		}
		f(n)
	})
}

// previewRecord is one pull request's preview comment as a pass leaves it.
type previewRecord struct {
	repository string
	number     int64
	id         int64
	digest     string
}

// syncPreviewComments keeps one sticky comment on each previewed pull
// request saying where the intent's preview stands for its head
// (templates.IntentPreviewComment), with the preview projection on. It is
// posted only once the preview is first live at the pull request's head,
// found first among the App's bot's comments by its marker (so a lost write
// never posts twice), recorded by id and body digest on the pull request's
// record, and from then on edited in place, only when what it says changes:
// live at a new head, being redeployed, failed, expired, gone, and last
// removed once the intent has ended. A comment someone deleted is posted
// again only once the preview is live again.
//
// It is best effort, like the cross-link, and never holds a phase or a
// hand-off back. Nothing is written to a pull request whose repository left
// the Project. Each write asks the rate floor of the pull request's own
// repository first; under it, or on a failure that may pass, the comment is
// left as it is and retry reports it, for an ended intent to come back to. A
// refusal, or a repository patchy can no longer reach (unreachable), is
// logged and not tried again until the comment would say something else. A
// failed read of the Preview changes nothing (loadPreview). changed reports
// a status write; only errConflict and a render error are returned.
func (p *pass) syncPreviewComments(ctx context.Context) (changed, retry bool, err error) {
	if !p.set.Previews || len(p.in.Status.PullRequests) == 0 {
		return false, false, nil
	}
	view, ok := p.loadPreview(ctx)
	if !ok {
		return false, true, nil
	}
	var records []previewRecord
	for _, pr := range p.in.Status.PullRequests {
		rec, again, err := p.syncPreviewComment(ctx, view, pr)
		if err != nil {
			return false, false, err
		}
		retry = retry || again
		if rec != nil && (rec.id != pr.PreviewCommentID || rec.digest != pr.PreviewDigest) {
			records = append(records, *rec)
		}
	}
	if len(records) == 0 {
		return false, retry, nil
	}
	return true, retry, p.update(ctx, func(cur *v1alpha1.Intent) error {
		for _, rec := range records {
			for i := range cur.Status.PullRequests {
				if pr := &cur.Status.PullRequests[i]; sameRepo(pr.Repository, rec.repository) && pr.Number == rec.number {
					pr.PreviewCommentID, pr.PreviewDigest = rec.id, rec.digest
				}
			}
		}
		return nil
	})
}

// syncPreviewComment is syncPreviewComments for one pull request: the
// record its comment has once written (nil when nothing was written),
// whether to come back for it (its repository's rate floor, a failure that
// may pass), and a render error.
func (p *pass) syncPreviewComment(ctx context.Context, view previewView, pr v1alpha1.IntentPullRequest) (
	*previewRecord, bool, error) {
	key := prKey(pr)
	id, refused := pr.PreviewCommentID, ""
	p.notes(func(n *previewNotes) {
		if id == 0 {
			id = n.ids[key]
		}
		refused = n.refused[key]
	})
	previews := v1alpha1.EffectivePreviews(p.proj)
	var rp *v1alpha1.RepositoryPreview
	if i := slices.IndexFunc(previews, func(rp v1alpha1.RepositoryPreview) bool {
		return sameRepo(rp.URL, pr.Repository)
	}); i >= 0 {
		rp = &previews[i]
	}
	if rp == nil && id == 0 || p.leftProject(pr.Repository) {
		return nil, false, nil
	}
	c := p.previewComment(view, pr, rp, len(previews) > 1, terminal(p.in.Status.Phase))
	if id == 0 && c.State != templates.PreviewLive {
		return nil, false, nil // posted only once live
	}
	body, err := templates.RenderIntentPreviewComment(c)
	if err != nil {
		return nil, false, err
	}
	d := digest([]byte(body))
	if id != 0 && id == pr.PreviewCommentID && d == pr.PreviewDigest || refused == d {
		return nil, false, nil
	}
	above, err := p.rateOK(ctx, pr.Repository)
	if err == nil && !above {
		return nil, true, nil
	}
	if err == nil {
		id, err = p.writePreviewComment(ctx, pr, id, c.State == templates.PreviewLive, body)
	}
	if err != nil {
		return nil, p.previewWriteFailed(ctx, pr, d, err), nil
	}
	p.notes(func(n *previewNotes) {
		delete(n.refused, key)
		if id == 0 {
			delete(n.ids, key)
		} else {
			n.ids[key] = id
		}
	})
	if id == 0 {
		return &previewRecord{repository: pr.Repository, number: pr.Number}, false, nil
	}
	return &previewRecord{repository: pr.Repository, number: pr.Number, id: id, digest: d}, false, nil
}

// previewWriteFailed logs a preview comment write that failed, and reports
// whether to try it again later: a refusal, or a repository patchy can no
// longer reach (unreachable), is remembered against what the comment would
// have said (d) instead, and not tried again until that changes.
func (p *pass) previewWriteFailed(ctx context.Context, pr v1alpha1.IntentPullRequest, d string, err error) bool {
	attrs := []slog.Attr{slog.String("intent", p.in.Name), slog.String("repository", pr.Repository),
		slog.Int64("number", pr.Number), slog.Any("error", err)}
	if unreachable(err) {
		p.notes(func(n *previewNotes) { n.refused[prKey(pr)] = d })
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "the preview comment could not be written; tried again once it "+
			"would say something else", attrs...)
		return false
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "write the preview comment; retried at the next poll", attrs...)
	return true
}

// previewComment is the preview comment pr calls for now: removed once the
// intent has ended; gone when its repository is not previewed (any more) or
// the intent has no preview to show; else the preview's own state, at the
// revision its component runs for pr's repository, under its path when the
// preview serves more than one repository.
func (p *pass) previewComment(view previewView, pr v1alpha1.IntentPullRequest, rp *v1alpha1.RepositoryPreview,
	multi, ended bool) templates.IntentPreviewComment {
	c := templates.IntentPreviewComment{Namespace: p.in.Namespace, Intent: p.in.Name, Resource: p.in.Name,
		Revision: pr.HeadSHA}
	switch {
	case ended:
		c.State = templates.PreviewRemoved
		return c
	case rp == nil || view.state == "":
		c.State = templates.PreviewUnavailable
		return c
	}
	c.State, c.Reason, c.Host = view.state, view.reason, view.host
	for _, comp := range view.components {
		if sameRepo(comp.repository, rp.URL) {
			c.Revision = comp.revision
		}
	}
	if multi {
		c.Path = rp.Path
	}
	return c
}

// writePreviewComment writes body to pr's preview comment: edited in place
// when id names it, else adopted by its marker (edited if it says something
// else) or posted, but only when live: a comment deleted while the preview
// is not live is left deleted. It returns the comment's id, 0 when there is
// none.
func (p *pass) writePreviewComment(ctx context.Context, pr v1alpha1.IntentPullRequest, id int64, live bool,
	body string) (int64, error) {
	if id != 0 {
		err := p.r.GitHub.EditPullRequestComment(ctx, pr.Repository, id, body)
		if !ghclient.IsNotFound(err) {
			return id, err
		}
		// Deleted: posted again once live.
	}
	if !live {
		return 0, nil
	}
	c, err := p.findOwnOnPullRequest(ctx, pr, templates.PreviewKey)
	switch {
	case err != nil:
		return 0, err
	case c == nil:
		if c, err = p.r.GitHub.CreatePullRequestComment(ctx, pr.Repository, pr.Number, body); err != nil {
			return 0, err
		}
	case c.Body != body:
		if err := p.r.GitHub.EditPullRequestComment(ctx, pr.Repository, c.ID, body); err != nil {
			return 0, err
		}
	}
	return c.ID, nil
}
