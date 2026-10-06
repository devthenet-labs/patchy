// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// The preview fixtures: one intent's host, and the heads of its two pull
// requests.
const (
	testPreviewHost = "marigold-3.preview.patchy.example.com"
	testWebHead     = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	testAPIHead     = "9f1c0e5d2a7b4c3e8f6a1d0b9c8e7f6a5b4c3d2e"
)

// testInReview is the one-repository status comment the preview goldens
// add a preview to.
func testInReview(p *IntentPreview) IntentStatusComment {
	return IntentStatusComment{
		Namespace: "patchy", Intent: "target-1", Phase: "InReview",
		PlanRevision: 2, PlanURL: "https://github.com/devthenet-labs/intents/issues/1#issuecomment-99",
		Summary:    "Add GET /version returning {sha, built} as JSON",
		ApprovedBy: "peter", ApprovedRevision: 2,
		PullRequests: []IntentPullRequest{{
			Repository: "devthenet-labs/patchy-target", Number: 12,
			URL: "https://github.com/devthenet-labs/patchy-target/pull/12", State: "open",
		}},
		Preview:  p,
		Commands: []string{"cancel"},
	}
}

func onePreview(state, reason string) *IntentPreview {
	return &IntentPreview{State: state, Reason: reason, Host: "target-1.preview.patchy.example.com",
		Resource: "target-1", Components: []IntentPreviewComponent{
			{Repository: "devthenet-labs/patchy-target", Path: "/", Revision: testWebHead},
		}}
}

func twoPreview(state string) *IntentPreview {
	return &IntentPreview{State: state, Host: testPreviewHost, Resource: "marigold-3",
		Components: []IntentPreviewComponent{
			{Repository: testWeb, Path: "/", Revision: testWebHead},
			{Repository: testAPI, Path: "/api", Revision: testAPIHead},
		}}
}

func testPreviewComment(state, reason, path string) IntentPreviewComment {
	return IntentPreviewComment{Namespace: "patchy", Intent: "marigold-3", State: state, Reason: reason,
		Host: testPreviewHost, Resource: "marigold-3", Revision: testAPIHead, Path: path}
}

// TestIntentPreviewGoldens pins the status comment's preview line in each
// state, one repository and several, and the pull request's preview
// comment in each state.
func TestIntentPreviewGoldens(t *testing.T) {
	tests := []struct {
		name   string
		render func() (string, error)
	}{
		{"intent_status_in_review_preview.md", func() (string, error) {
			return RenderIntentStatusComment(testInReview(onePreview(PreviewLive, "")))
		}},
		{"intent_status_preview_updating.md", func() (string, error) {
			return RenderIntentStatusComment(testInReview(onePreview(PreviewUpdating, "")))
		}},
		{"intent_status_preview_failed.md", func() (string, error) {
			return RenderIntentStatusComment(testInReview(onePreview(PreviewUnavailable, PreviewFailed)))
		}},
		{"intent_status_preview_expired.md", func() (string, error) {
			return RenderIntentStatusComment(testInReview(onePreview(PreviewUnavailable, PreviewExpired)))
		}},
		{"intent_status_in_review_preview_repositories.md", func() (string, error) {
			prs := testMultiPRs()
			prs[0].State, prs[1].State = "open", "open"
			return RenderIntentStatusComment(IntentStatusComment{
				Namespace: "patchy", Intent: "marigold-3", Phase: "InReview",
				PlanRevision: 1, PlanURL: "https://github.com/devthenet-labs/intents/issues/3#issuecomment-7",
				Summary:    "Show the API's greeting on the home page",
				ApprovedBy: "peter", ApprovedRevision: 1,
				Repositories: []string{testWeb, testAPI},
				PullRequests: prs, Preview: twoPreview(PreviewLive), Commands: []string{"cancel"},
			})
		}},
		{"intent_status_preview_updating_repositories.md", func() (string, error) {
			return RenderIntentStatusComment(IntentStatusComment{
				Namespace: "patchy", Intent: "marigold-3", Phase: "Revising", PlanRevision: 1,
				Preview: twoPreview(PreviewUpdating),
			})
		}},
		// A Project may name its repositories anything; the names are
		// shown in code, so none is a mention or a reference.
		{"intent_status_preview_hostile.md", func() (string, error) {
			p := twoPreview(PreviewLive)
			p.Components[0].Repository = "@octocat #3"
			p.Components[1].Path = "/api`<b>`\n#4"
			return RenderIntentStatusComment(IntentStatusComment{
				Namespace: "patchy", Intent: "marigold-3", Phase: "InReview", Preview: p,
			})
		}},
		// A host that is not bare DNS labels is never linked.
		{"intent_status_preview_unlinkable.md", func() (string, error) {
			p := onePreview(PreviewLive, "")
			p.Host = "evil.example/@octocat"
			return RenderIntentStatusComment(testInReview(p))
		}},
		{"intent_preview_live.md", func() (string, error) {
			return RenderIntentPreviewComment(testPreviewComment(PreviewLive, "", ""))
		}},
		{"intent_preview_live_path.md", func() (string, error) {
			return RenderIntentPreviewComment(testPreviewComment(PreviewLive, "", "/api"))
		}},
		{"intent_preview_updating.md", func() (string, error) {
			return RenderIntentPreviewComment(testPreviewComment(PreviewUpdating, "", "/api"))
		}},
		{"intent_preview_failed.md", func() (string, error) {
			return RenderIntentPreviewComment(testPreviewComment(PreviewUnavailable, PreviewFailed, "/api"))
		}},
		{"intent_preview_expired.md", func() (string, error) {
			return RenderIntentPreviewComment(testPreviewComment(PreviewUnavailable, PreviewExpired, ""))
		}},
		{"intent_preview_unlinkable.md", func() (string, error) {
			c := testPreviewComment(PreviewLive, "", "")
			c.Host = "https://evil.example"
			return RenderIntentPreviewComment(c)
		}},
		{"intent_preview_gone.md", func() (string, error) {
			return RenderIntentPreviewComment(testPreviewComment(PreviewUnavailable, "", ""))
		}},
		{"intent_preview_removed.md", func() (string, error) {
			return RenderIntentPreviewComment(testPreviewComment(PreviewRemoved, "", "/api"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestStatusCommentWithoutPreviewUnchanged: no preview (nil, or a zero
// state) renders the status comment byte for byte as it was before previews
// were linked, so an intent without one, or with the flag off, never sees
// its comment change.
func TestStatusCommentWithoutPreviewUnchanged(t *testing.T) {
	base, err := RenderIntentStatusComment(testInReview(nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*IntentPreview{
		"zero":             {},
		"zero with a host": {Host: testPreviewHost, Components: twoPreview(PreviewLive).Components},
	} {
		got, err := RenderIntentStatusComment(testInReview(p))
		if err != nil {
			t.Fatal(err)
		}
		if got != base {
			t.Errorf("%s preview changed the status comment:\n%s\n--- want ---\n%s", name, got, base)
		}
	}
	if strings.Contains(base, "Preview") {
		t.Errorf("the status comment without a preview mentions one:\n%s", base)
	}
}

// TestPreviewCommentKey: the preview comment carries its own notice key,
// which no other intent notice uses, and is headed by its marker.
func TestPreviewCommentKey(t *testing.T) {
	for _, k := range []string{SummaryKey, PartialKey, SiblingsKey, UntrackedKey} {
		if k == PreviewKey {
			t.Errorf("notice key %q is used twice", k)
		}
	}
	for _, state := range []string{PreviewLive, PreviewUpdating, PreviewUnavailable, PreviewRemoved} {
		body, err := RenderIntentPreviewComment(testPreviewComment(state, "", ""))
		if err != nil {
			t.Fatal(err)
		}
		if want := NoticeMarker("patchy", "marigold-3", PreviewKey) + "\n"; !strings.HasPrefix(body, want) {
			t.Errorf("%s preview comment is not headed by %q:\n%s", state, want, body)
		}
	}
}

// previewHosts are hosts a Preview's status could name: the preview
// controller's own shape, and every way a host can carry more than a host.
var previewHosts = []string{
	testPreviewHost, "target-1.preview.example.com", "a.b", "x-1.y-2.z",
	"", "evil.example/@octocat", "https://evil.example", "a.b?c=d", "a.b#e", "a.b:8443", "u@a.b", "A.B",
	"a..b", "-a.b", "a-.b", "a", "a.b/", "a.b)](https://evil.example", "a.b\n#3", "a.b`", "a.b c",
	"exa\u0301mple.com", "a.b\u202e", "xn--bcher-kva.example", "1.2.3.4", strings.Repeat("a", 64) + ".b",
	"[::1]", "a.b\\", "a.b<", "a.b%2f",
}

// previewConfig generates previews in every state and reason, at hosts from
// previewHosts, with components and a resource name of markdown tokens.
func previewConfig(seed int64) *quick.Config {
	states := []string{"", PreviewLive, PreviewUpdating, PreviewUnavailable, PreviewRemoved, "bogus"}
	reasons := []string{"", PreviewFailed, PreviewExpired, PreviewUnlinkable, "bogus"}
	tokens := func(r *rand.Rand) string {
		var b strings.Builder
		for range r.Intn(8) {
			b.WriteString(markdownTokens[r.Intn(len(markdownTokens))])
		}
		return b.String()
	}
	return &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			p := IntentPreview{
				State: states[r.Intn(len(states))], Reason: reasons[r.Intn(len(reasons))],
				Host: previewHosts[r.Intn(len(previewHosts))], Resource: tokens(r),
			}
			for range r.Intn(4) {
				p.Components = append(p.Components, IntentPreviewComponent{
					Repository: tokens(r), Path: tokens(r), Revision: tokens(r),
				})
			}
			args[0] = reflect.ValueOf(p)
		},
	}
}

// TestIntentPreviewProperties: whatever a Preview's status and the Project
// hold, the status comment and the pull request comment link the preview
// only while it is live and its host is bare DNS labels, and then exactly
// at https://<host>; and neither carries raw HTML, a mention, an issue
// reference or a closing keyword, nor anything that renders as nothing.
func TestIntentPreviewProperties(t *testing.T) {
	var failure string
	check := func(name, body string, live bool, host string) bool {
		rest := cutMarker(body)
		// The one link a live preview has is taken out; what is left must
		// link nothing and name no address outside code.
		link := "[`" + host + "`](https://" + host + ")"
		linked := live && strings.Contains(rest, link)
		if linked {
			rest = strings.Replace(rest, link, "the preview", 1)
		}
		seen := parse(rest)
		switch {
		case live && !linked:
			failure = fmt.Sprintf("%s does not link the live preview at https://%s", name, host)
		case seen.links:
			failure = fmt.Sprintf("%s links something other than the live preview", name)
		case strings.Contains(seen.prose, "://") || strings.Contains(strings.ToLower(seen.prose), "www."):
			failure = fmt.Sprintf("%s names an address outside code", name)
		case seen.rawHTML:
			failure = fmt.Sprintf("%s carries raw HTML", name)
		case closingReference.MatchString(seen.prose):
			failure = fmt.Sprintf("%s has a closing keyword %q", name, closingReference.FindString(seen.prose))
		case liveReference.MatchString(seen.prose):
			failure = fmt.Sprintf("%s has a live reference %q", name, liveReference.FindString(seen.prose))
		case liveMention.MatchString(seen.prose):
			failure = fmt.Sprintf("%s mentions %q", name, liveMention.FindString(seen.prose))
		case strings.ContainsFunc(rest, unseen):
			failure = fmt.Sprintf("%s keeps a character that renders as nothing", name)
		default:
			return true
		}
		failure += ":\n" + body
		return false
	}
	holds := func(p IntentPreview) bool {
		live := p.State == PreviewLive && linkable(p.Host)
		status, err := RenderIntentStatusComment(IntentStatusComment{
			Namespace: "patchy", Intent: "marigold-3", Phase: "InReview", Preview: &p,
		})
		if err != nil {
			failure = fmt.Sprintf("status: %v", err)
			return false
		}
		if !check("status comment", status, live, p.Host) {
			return false
		}
		c := IntentPreviewComment{Namespace: "patchy", Intent: "marigold-3", State: p.State, Reason: p.Reason,
			Host: p.Host, Resource: p.Resource}
		if len(p.Components) > 0 {
			c.Revision, c.Path = p.Components[0].Revision, p.Components[0].Path
		}
		comment, err := RenderIntentPreviewComment(c)
		if err != nil {
			failure = fmt.Sprintf("comment: %v", err)
			return false
		}
		return check("pull request comment", comment, live, p.Host)
	}
	if err := quick.Check(holds, previewConfig(20261005)); err != nil {
		t.Errorf("%v\n%s", err, failure)
	}
}
