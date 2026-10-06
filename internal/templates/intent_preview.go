// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"regexp"
	"strings"
)

// An intent's preview, as patchy tells reviewers about it: a line in the
// issue's status comment, and one sticky comment on each previewed pull
// request, edited in place as the preview changes. Both are rendered from
// plain values the intent controller derived from the Preview's status and
// its own records. The preview's address is linked only while the preview
// is live, and only when it is a bare https host; the preview controller's
// own words (a Preview's status message, which can quote the cluster) are
// never shown: a failure names the Preview resource instead.

// PreviewKey is the notice key of the sticky preview comment on each
// previewed pull request of an intent.
const PreviewKey = "preview"

// Preview states.
const (
	// PreviewLive: the preview serves the commits listed, at its host.
	PreviewLive = "live"
	// PreviewUpdating: the preview is being deployed (or redeployed) and is
	// not reachable yet.
	PreviewUpdating = "updating"
	// PreviewUnavailable: there is no reachable preview; Reason says why.
	PreviewUnavailable = "unavailable"
	// PreviewRemoved: the intent ended and its preview was removed. Only a
	// pull request's comment says this; the status comment just drops the
	// line.
	PreviewRemoved = "removed"
)

// Why an unavailable preview is not available.
const (
	// PreviewFailed: it could not be deployed.
	PreviewFailed = "failed"
	// PreviewExpired: it outlived its time to live without a new
	// deployment.
	PreviewExpired = "expired"
	// PreviewUnlinkable: it is deployed, but its address is not one patchy
	// links.
	PreviewUnlinkable = "unlinkable"
)

// IntentPreview is the intent's preview as the issue's status comment shows
// it. A zero State (or a nil *IntentPreview) shows nothing at all, so the
// status comment of an intent without a preview is byte-identical to what
// it was before previews were linked.
type IntentPreview struct {
	// State is PreviewLive, PreviewUpdating or PreviewUnavailable.
	State string
	// Reason is why an unavailable preview is not available: PreviewFailed,
	// PreviewExpired, PreviewUnlinkable, or "" when there is none now.
	Reason string
	// Host is the preview's host ("<intent>.<suffix>"), linked as
	// https://Host while the preview is live. A host that is not lowercase
	// DNS labels is never linked: the preview then shows as unlinkable.
	Host string
	// Resource names the Preview resource an operator reads a failure from.
	Resource string
	// Components are what the preview serves or deploys, one per previewed
	// repository, in Project order. With more than one, the comment lists
	// each; with one, it names its revision in the line.
	Components []IntentPreviewComponent
}

// IntentPreviewComponent is one repository a preview serves.
type IntentPreviewComponent struct {
	// Repository is "owner/name".
	Repository string
	// Path is where it is served on the preview host: "/" or "/api".
	Path string
	// Revision is the commit it serves: a full SHA, shown short.
	Revision string
}

// IntentPreviewComment is the sticky comment on one previewed pull request
// of an intent, headed by NoticeMarker(Namespace, Intent, PreviewKey). It is
// posted once the preview first serves the pull request's head, and edited
// as the preview changes.
type IntentPreviewComment struct {
	// Namespace and Intent name the Intent, for the marker.
	Namespace string
	Intent    string
	// State is PreviewLive, PreviewUpdating, PreviewUnavailable or
	// PreviewRemoved.
	State string
	// Reason is why an unavailable preview is not available, as
	// IntentPreview's.
	Reason string
	// Host is the preview's host, linked while live, as IntentPreview's.
	Host string
	// Resource names the Preview resource an operator reads a failure from.
	Resource string
	// Revision is the pull request's head: the commit its repository's
	// component serves or deploys.
	Revision string
	// Path is where the pull request's repository is served on the host,
	// given when the preview serves more than one repository; "" omits it.
	Path string
}

// previewHost is a host patchy links: lowercase DNS labels, at least two.
var previewHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// linkable reports whether host may be linked as https://host.
func linkable(host string) bool { return len(host) <= 253 && previewHost.MatchString(host) }

// previewLink renders host as the link to the preview: the host in code, so
// nothing in it is read as a reference.
func previewLink(host string) string { return "[" + code(host) + "](https://" + host + ")" }

// shown is a value on one line with every character that renders as
// nothing written as its code point, as a repository list shows names.
func shown(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(visibleText(s), "\n", " "))
}

// shortRevision renders a commit as code, shortened to twelve characters.
func shortRevision(rev string) string {
	rev = shown(rev)
	if len(rev) > 12 {
		rev = cutRunes(rev, 12)
	}
	return code(rev)
}

// settle is state and reason as rendered: a live preview whose host cannot
// be linked is unlinkable instead, so nothing but a bare https host is ever
// linked.
func settle(state, reason, host string) (string, string) {
	if state == PreviewLive && !linkable(host) {
		return PreviewUnavailable, PreviewUnlinkable
	}
	return state, reason
}

// unavailableClause says why a preview is unavailable, after its subject
// (deployed is "is deployed" after a sentence's subject, "deployed" after a
// label); "" when reason names no cause.
func unavailableClause(reason, resource, deployed string) string {
	switch reason {
	case PreviewFailed:
		return "could not be deployed" + recordedBy(resource, "why") + ". A new push deploys it again."
	case PreviewExpired:
		return "expired after its time to live without a new deployment. A new push deploys it again."
	case PreviewUnlinkable:
		return deployed + ", but its address is not one patchy links" + recordedBy(resource, "it") + "."
	}
	return ""
}

// recordedBy points an operator to the Preview resource: "; the Preview
// `name` records what".
func recordedBy(resource, what string) string {
	if resource = shown(resource); resource == "" {
		return ""
	}
	return "; the Preview " + code(resource) + " records " + what
}

// statusPreview is the status comment's preview line and list.
type statusPreview struct {
	Sentence   string
	Components []string
}

// renderStatusPreview is the status comment's preview, nil for none.
func renderStatusPreview(p *IntentPreview) *statusPreview {
	if p == nil || p.State == "" {
		return nil
	}
	state, reason := settle(p.State, p.Reason, p.Host)
	single := len(p.Components) == 1
	var s string
	switch state {
	case PreviewLive:
		s = previewLink(p.Host)
		if single {
			s += " serves " + shortRevision(p.Components[0].Revision) + "."
		}
	case PreviewUpdating:
		s = "being deployed"
		if single {
			s += " at " + shortRevision(p.Components[0].Revision)
		}
		s += "; the link appears here once it is ready."
	default:
		s = "not available."
		if clause := unavailableClause(reason, p.Resource, "deployed"); clause != "" {
			s = clause
		}
	}
	out := &statusPreview{Sentence: s}
	if !single {
		for _, c := range p.Components {
			out.Components = append(out.Components, fmt.Sprintf("%s: %s at %s",
				code(shown(c.Path)), code(shown(c.Repository)), shortRevision(c.Revision)))
		}
	}
	return out
}

// RenderIntentPreviewComment renders the sticky preview comment on one pull
// request.
func RenderIntentPreviewComment(c IntentPreviewComment) (string, error) {
	state, reason := settle(c.State, c.Reason, c.Host)
	head := "This pull request's head, " + shortRevision(c.Revision) + ","
	var s string
	switch state {
	case PreviewLive:
		s = head + " is live at " + previewLink(c.Host)
		if path := shown(c.Path); path != "" {
			s += ", under " + code(path)
		}
		s += "."
	case PreviewUpdating:
		s = head + " is being deployed to the intent's preview; the link appears here once it is ready."
	case PreviewRemoved:
		s = "The intent has ended, and its preview was removed."
	default:
		s = "There is no preview of this pull request now."
		if clause := unavailableClause(reason, c.Resource, "is deployed"); clause != "" {
			s = "The preview of this pull request's head, " + shortRevision(c.Revision) + ", " + clause
		}
	}
	return render("intent_preview.md.tmpl", struct {
		Marker   string
		Sentence string
		Removed  bool
	}{
		Marker:   NoticeMarker(c.Namespace, c.Intent, PreviewKey),
		Sentence: s,
		Removed:  state == PreviewRemoved,
	})
}
