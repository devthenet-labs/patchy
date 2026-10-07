// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// The intent's preview as the intent reconciler reads it, for the issue's
// status comment and the pull requests' preview comments. Preview status is
// preview-controller's alone: this side only reads it, through the uncached
// API reader (one get per pass, never a watch), and only with the preview
// projection on, while the Intent is in a state its Preview exists in and
// its Project previews something. What it reads is another component's
// word, written under another ServiceAccount, so it is held to what the
// intent reconciler can check itself: the Preview is this Intent's (its
// UID), its status is about the spec it has now (observedGeneration), every
// component it reports Ready serves the revision v1alpha1.DesiredPreviewComponents
// derives from this Intent's own records, and its URL is the bare https host
// the Preview's host label names. Its status message, which can quote the
// cluster, is never read.

// previewView is what an intent's Preview says about it now: whether it is
// live, waiting for a free slot, being deployed or unavailable, and what it
// serves.
type previewView struct {
	// state is "" when the intent has no preview to show (none exists, it
	// is another Intent's or being deleted, the Intent wants none), else
	// templates.PreviewLive, PreviewUpdating, PreviewWaiting or
	// PreviewUnavailable.
	state string
	// reason is why an unavailable preview is not available:
	// templates.PreviewFailed, PreviewExpired or PreviewUnlinkable.
	reason string
	// host is the preview's host, checked by previewHost; set only while
	// it is live.
	host string
	// components are what the preview serves or deploys, in Project order.
	components []previewComponent
}

// previewComponent is one previewed repository and the commit its component
// runs.
type previewComponent struct {
	// repository is the Project repository's URL.
	repository string
	// path is where it is served on the preview host ("/" or "/api").
	path string
	// revision is the commit it runs: its pull request's head, or its
	// recorded preview base.
	revision string
}

// previewURLPattern is the only URL a preview is linked by: https, a host of
// lowercase letters, digits, dots and hyphens, and nothing else (no
// userinfo, port, path, query or fragment; a trailing slash at most).
var previewURLPattern = regexp.MustCompile(`^https://([a-z0-9.-]+)/?$`)

// dnsLabel is one lowercase DNS label.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// previewHost is the host of raw, a Preview's status.url, when it is one the
// intent's preview may be linked by: "https://" and a lowercase DNS name
// whose first label is the intent's name, which is also the Preview's host
// label, followed by at least two more labels (the operator's host suffix),
// none of them wrong in shape and the last not all digits. Anything else is
// not linked.
func previewHost(raw, intent, hostLabel string) (string, bool) {
	m := previewURLPattern.FindStringSubmatch(raw)
	if m == nil || hostLabel != intent {
		return "", false
	}
	host := m[1]
	labels := strings.Split(host, ".")
	if len(host) > 253 || len(labels) < 3 || labels[0] != intent {
		return "", false
	}
	for _, l := range labels {
		if !dnsLabel.MatchString(l) {
			return "", false
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", false
	}
	return host, true
}

// viewPreview is what pv, the Preview read for in, says about in's preview.
// It is live only when pv is in's (its UID), not being deleted, has observed
// its current spec, that spec is exactly the components derived from in's
// records, it is Ready with every component serving its derived revision,
// and its URL passes previewHost; a Ready one whose URL does not is
// unavailable (unlinkable). A Failed or Expired one at the current spec is
// unavailable, and a Queued one there waits for a free slot; any other is
// being deployed, the spec in.Status names included while the preview
// projection has yet to write it.
func viewPreview(proj *v1alpha1.Project, in *v1alpha1.Intent, pv *v1alpha1.Preview) previewView {
	desired, ok := v1alpha1.DesiredPreviewComponents(proj, in)
	if !ok || pv == nil || pv.Spec.IntentRef.UID != in.UID || !pv.DeletionTimestamp.IsZero() {
		return previewView{}
	}
	v := previewView{components: previewComponents(proj, desired)}
	current := pv.Status.ObservedGeneration == pv.Generation && slices.Equal(pv.Spec.Components, desired)
	switch {
	case !current:
		v.state = templates.PreviewUpdating
	case pv.Status.Phase == v1alpha1.PreviewReady && servesDesired(pv.Status.Components, desired):
		host, ok := previewHost(pv.Status.URL, in.Name, pv.Spec.HostLabel)
		if !ok {
			v.state, v.reason = templates.PreviewUnavailable, templates.PreviewUnlinkable
			break
		}
		v.state, v.host = templates.PreviewLive, host
	case pv.Status.Phase == v1alpha1.PreviewFailed:
		v.state, v.reason = templates.PreviewUnavailable, templates.PreviewFailed
	case pv.Status.Phase == v1alpha1.PreviewExpired:
		v.state, v.reason = templates.PreviewUnavailable, templates.PreviewExpired
	case pv.Status.Phase == v1alpha1.PreviewQueued:
		v.state = templates.PreviewWaiting
	default:
		v.state = templates.PreviewUpdating
	}
	return v
}

// servesDesired reports whether a Ready Preview's components serve exactly
// the desired ones, in order, each at its desired revision.
func servesDesired(got []v1alpha1.PreviewComponentStatus, desired []v1alpha1.PreviewComponent) bool {
	return slices.EqualFunc(got, desired, func(g v1alpha1.PreviewComponentStatus, d v1alpha1.PreviewComponent) bool {
		return g.Name == d.Name && g.Revision == d.Revision
	})
}

// previewComponents are desired's components with the repository each is
// of, by its key.
func previewComponents(proj *v1alpha1.Project, desired []v1alpha1.PreviewComponent) []previewComponent {
	previews := v1alpha1.EffectivePreviews(proj)
	out := make([]previewComponent, 0, len(desired))
	for _, d := range desired {
		c := previewComponent{path: v1alpha1.PreviewComponentPath(d), revision: d.Revision}
		if i := slices.IndexFunc(previews, func(rp v1alpha1.RepositoryPreview) bool { return rp.Name == d.Name }); i >= 0 {
			c.repository = previews[i].URL
		}
		out = append(out, c)
	}
	return out
}

// loadPreview reads the intent's Preview once per pass, and only with the
// preview projection on (Settings.Previews), while the Intent is in a state
// its Preview exists in (v1alpha1.IntentWantsPreview) and its Project
// previews something: otherwise nothing is read and there is no preview to
// show. ok is false when the read failed: there is then no link this pass
// (it fails closed), and the preview comments are left as they are.
func (p *pass) loadPreview(ctx context.Context) (previewView, bool) {
	if p.previewRead {
		return p.preview, p.previewOK
	}
	p.previewRead, p.previewOK, p.preview = true, true, previewView{}
	if !p.set.Previews || !v1alpha1.IntentWantsPreview(p.in) || len(v1alpha1.EffectivePreviews(p.proj)) == 0 {
		return p.preview, true
	}
	var pv v1alpha1.Preview
	err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: p.in.Name}, &pv)
	switch {
	case kerrors.IsNotFound(err):
	case err != nil:
		p.previewOK = false
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the intent's preview; no link is shown this pass",
			slog.String("intent", p.in.Name), slog.Any("error", err))
	default:
		p.preview = viewPreview(p.proj, p.in, &pv)
	}
	return p.preview, p.previewOK
}

// statusPreview is the status comment's preview: nil when there is none to
// show.
func (p *pass) statusPreview() *templates.IntentPreview {
	v := p.preview
	if v.state == "" {
		return nil
	}
	out := &templates.IntentPreview{State: v.state, Reason: v.reason, Host: v.host, Resource: p.in.Name}
	for _, c := range v.components {
		out.Components = append(out.Components, templates.IntentPreviewComponent{
			Repository: repoSlug(c.repository), Path: c.path, Revision: c.revision,
		})
	}
	return out
}
