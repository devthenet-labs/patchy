// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// TestPreviewHost: a Preview's URL is linked only when it is https and a
// bare lowercase DNS name whose first label is the intent's name (and the
// Preview's host label), under a suffix of at least two labels.
func TestPreviewHost(t *testing.T) {
	const intent = "hello-web-13"
	tests := []struct {
		url, label string
		want       string
	}{
		{"https://hello-web-13.preview.patchy.example.com", intent, "hello-web-13.preview.patchy.example.com"},
		{"https://hello-web-13.preview.example.com/", intent, "hello-web-13.preview.example.com"},
		{"https://hello-web-13.example.com", intent, "hello-web-13.example.com"},
		{"http://hello-web-13.preview.example.com", intent, ""},
		{"https://user@hello-web-13.preview.example.com", intent, ""},
		{"https://hello-web-13.preview.example.com@evil.example", intent, ""},
		{"https://hello-web-13.preview.example.com:8443", intent, ""},
		{"https://hello-web-13.preview.example.com/login", intent, ""},
		{"https://hello-web-13.preview.example.com//", intent, ""},
		{"https://hello-web-13.preview.example.com?next=evil", intent, ""},
		{"https://hello-web-13.preview.example.com/?x", intent, ""},
		{"https://hello-web-13.preview.example.com#top", intent, ""},
		{"https://Hello-Web-13.preview.example.com", intent, ""},
		{"https://hello-web-13.PREVIEW.example.com", intent, ""},
		{"https://evil.example", intent, ""},
		{"https://other-1.preview.example.com", intent, ""},
		{"https://hello-web-130.preview.example.com", intent, ""},
		{"https://hello-web-13", intent, ""},
		{"https://hello-web-13.com", intent, ""},
		{"https://hello-web-13..example.com", intent, ""},
		{"https://hello-web-13.-x.example.com", intent, ""},
		{"https://hello-web-13.x-.example.com", intent, ""},
		{"https://hello-web-13.preview.example.123", intent, ""},
		{"https://hello-web-13.bücher.example", intent, ""},
		{"https://hello-web-13.xn--bcher-kva.example", intent, "hello-web-13.xn--bcher-kva.example"},
		{"https://hello-web-13.preview.example.com\n", intent, ""},
		{" https://hello-web-13.preview.example.com", intent, ""},
		{"https://hello-web-13.preview.example.com%2f", intent, ""},
		{"https://hello-web-13." + strings.Repeat("a", 64) + ".example.com", intent, ""},
		{"https://hello-web-13." + strings.Repeat(strings.Repeat("a", 60)+".", 4) + "com", intent, ""},
		{"", intent, ""},
		{"https://hello-web-13.preview.example.com", "other", ""},
	}
	for _, tt := range tests {
		host, ok := previewHost(tt.url, intent, tt.label)
		if host != tt.want || ok != (tt.want != "") {
			t.Errorf("previewHost(%q, label %q) = %q, %v; want %q", tt.url, tt.label, host, ok, tt.want)
		}
	}
}

// previewFixture is a one-repository Project previewing its app, an Intent
// in review at head, and its Preview Ready at that head.
func previewFixture(head string) (*v1alpha1.Project, *v1alpha1.Intent, *v1alpha1.Preview) {
	proj := testProject()
	proj.Spec.Preview = &appPreview
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS, UID: types.UID("intent-uid")},
		Status: v1alpha1.IntentStatus{Phase: v1alpha1.IntentInReview,
			PullRequests: []v1alpha1.IntentPullRequest{{Repository: appRepoURL, Number: 1, State: prOpen,
				HeadSHA: head}}},
	}
	desired, _ := v1alpha1.DesiredPreviewComponents(proj, in)
	pv := &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: testNS, Generation: 2},
		Spec: v1alpha1.PreviewSpec{IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID},
			HostLabel: in.Name, Components: desired},
		Status: v1alpha1.PreviewStatus{Phase: v1alpha1.PreviewReady, ObservedGeneration: 2,
			URL: "https://target-1.preview.example.com",
			Components: []v1alpha1.PreviewComponentStatus{{Name: "app", Revision: head,
				ImageID: "registry.example/app@sha256:1"}}},
	}
	return proj, in, pv
}

// TestViewPreview: the preview is live only when the Preview is this
// Intent's, current, Ready at exactly the derived revisions, and at a host
// that is linked; Failed and Expired at the current spec are unavailable;
// anything else that exists for the Intent is being deployed.
func TestViewPreview(t *testing.T) {
	head := strings.Repeat("a", 40)
	moved := strings.Repeat("b", 40)
	live := previewView{state: templates.PreviewLive, host: "target-1.preview.example.com",
		components: []previewComponent{{repository: appRepoURL, path: "/", revision: head}}}
	updating := previewView{state: templates.PreviewUpdating, components: live.components}
	unavailable := func(reason string) previewView {
		return previewView{state: templates.PreviewUnavailable, reason: reason, components: live.components}
	}
	tests := []struct {
		name   string
		mutate func(*v1alpha1.Project, *v1alpha1.Intent, *v1alpha1.Preview) *v1alpha1.Preview
		want   previewView
	}{
		{"live", nil, live},
		{"ready at an older generation", func(_ *v1alpha1.Project, _ *v1alpha1.Intent,
			pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.ObservedGeneration = 1
			return pv
		}, updating},
		{"ready serving a stale revision", func(_ *v1alpha1.Project, _ *v1alpha1.Intent,
			pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.Components[0].Revision = moved
			return pv
		}, updating},
		{"ready without its components", func(_ *v1alpha1.Project, _ *v1alpha1.Intent,
			pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.Components = nil
			return pv
		}, updating},
		{"the head moved, the spec not yet", func(_ *v1alpha1.Project, in *v1alpha1.Intent,
			pv *v1alpha1.Preview) *v1alpha1.Preview {
			in.Status.PullRequests[0].HeadSHA = moved
			return pv
		}, previewView{state: templates.PreviewUpdating,
			components: []previewComponent{{repository: appRepoURL, path: "/", revision: moved}}}},
		{"pending", func(_ *v1alpha1.Project, _ *v1alpha1.Intent, pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.Phase, pv.Status.URL, pv.Status.Components = v1alpha1.PreviewPending, "", nil
			return pv
		}, updating},
		{"queued", func(_ *v1alpha1.Project, _ *v1alpha1.Intent, pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.Phase = v1alpha1.PreviewQueued
			return pv
		}, updating},
		{"deploying", func(_ *v1alpha1.Project, _ *v1alpha1.Intent, pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.Phase = v1alpha1.PreviewDeploying
			return pv
		}, updating},
		{"failed", func(_ *v1alpha1.Project, _ *v1alpha1.Intent, pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.Phase, pv.Status.URL, pv.Status.Message = v1alpha1.PreviewFailed, "", "quota exceeded"
			return pv
		}, unavailable(templates.PreviewFailed)},
		{"failed at the old head", func(_ *v1alpha1.Project, in *v1alpha1.Intent,
			pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.Phase = v1alpha1.PreviewFailed
			in.Status.PullRequests[0].HeadSHA = moved
			return pv
		}, previewView{state: templates.PreviewUpdating,
			components: []previewComponent{{repository: appRepoURL, path: "/", revision: moved}}}},
		{"expired", func(_ *v1alpha1.Project, _ *v1alpha1.Intent, pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.Phase, pv.Status.URL = v1alpha1.PreviewExpired, ""
			return pv
		}, unavailable(templates.PreviewExpired)},
		{"ready at a foreign host", func(_ *v1alpha1.Project, _ *v1alpha1.Intent,
			pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Status.URL = "https://evil.example"
			return pv
		}, unavailable(templates.PreviewUnlinkable)},
		{"ready under another host label", func(_ *v1alpha1.Project, _ *v1alpha1.Intent,
			pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Spec.HostLabel = "other-1"
			return pv
		}, unavailable(templates.PreviewUnlinkable)},
		{"being deleted", func(_ *v1alpha1.Project, _ *v1alpha1.Intent, pv *v1alpha1.Preview) *v1alpha1.Preview {
			now := metav1.Now()
			pv.DeletionTimestamp = &now
			return pv
		}, previewView{}},
		{"another Intent's", func(_ *v1alpha1.Project, _ *v1alpha1.Intent, pv *v1alpha1.Preview) *v1alpha1.Preview {
			pv.Spec.IntentRef.UID = "earlier-intent-uid"
			return pv
		}, previewView{}},
		{"none", func(*v1alpha1.Project, *v1alpha1.Intent, *v1alpha1.Preview) *v1alpha1.Preview { return nil },
			previewView{}},
		{"a Project without previews", func(proj *v1alpha1.Project, _ *v1alpha1.Intent,
			pv *v1alpha1.Preview) *v1alpha1.Preview {
			proj.Spec.Preview = nil
			return pv
		}, previewView{}},
		{"an ended Intent", func(_ *v1alpha1.Project, in *v1alpha1.Intent, pv *v1alpha1.Preview) *v1alpha1.Preview {
			in.Status.Phase = v1alpha1.IntentMerged
			return pv
		}, previewView{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proj, in, pv := previewFixture(head)
			if tt.mutate != nil {
				pv = tt.mutate(proj, in, pv)
			}
			if got := viewPreview(proj, in, pv); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("viewPreview = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// previewCase is one Preview the property draws: its phase, whether its
// status is about its current spec and serves the derived revisions, and its
// URL.
type previewCase struct {
	phase    v1alpha1.PreviewPhase
	observed bool
	serves   bool
	specNow  bool
	url      string
}

// previewURLs are URLs a Preview's status could hold, most of them not ones
// patchy links.
var previewURLs = []string{
	"https://target-1.preview.example.com", "https://target-1.preview.example.com/",
	"https://target-1.a.b", "", "http://target-1.preview.example.com", "https://evil.example",
	"https://target-1.preview.example.com/x", "https://target-1.preview.example.com:443",
	"https://u@target-1.preview.example.com", "https://target-1.preview.example.com?x",
	"https://target-1.preview.example.com#x", "https://TARGET-1.preview.example.com", "https://target-1",
	"https://target-10.preview.example.com", "javascript:alert(1)", "https://target-1.preview.example.com\n#3",
	"https://target-1.preview.example.com)](https://evil.example", "https://target-1.@octocat.example",
}

func previewCaseConfig(seed int64) *quick.Config {
	phases := []v1alpha1.PreviewPhase{"", v1alpha1.PreviewPending, v1alpha1.PreviewQueued,
		v1alpha1.PreviewDeploying, v1alpha1.PreviewReady, v1alpha1.PreviewFailed, v1alpha1.PreviewExpired}
	return &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(previewCase{
				phase: phases[r.Intn(len(phases))], observed: r.Intn(4) > 0, serves: r.Intn(4) > 0,
				specNow: r.Intn(4) > 0, url: previewURLs[r.Intn(len(previewURLs))],
			})
		},
	}
}

// TestViewPreviewProperty: over Previews in every phase, current or not,
// serving the derived revisions or not, at every kind of URL, the preview is
// live exactly when it is Ready, has observed its spec, its spec and its
// components are the derived ones, and its URL is a bare https host under
// the intent's label; and the status comment and pull request comment
// rendered from it name an https address exactly when it is live.
func TestViewPreviewProperty(t *testing.T) {
	head := strings.Repeat("c", 40)
	var failure string
	holds := func(pc previewCase) bool {
		proj, in, pv := previewFixture(head)
		pv.Status.Phase, pv.Status.URL = pc.phase, pc.url
		if !pc.observed {
			pv.Status.ObservedGeneration = 1
		}
		if !pc.serves {
			pv.Status.Components[0].Revision = strings.Repeat("d", 40)
		}
		if !pc.specNow {
			pv.Spec.Components[0].Revision = strings.Repeat("e", 40)
		}
		_, linkable := previewHost(pc.url, in.Name, in.Name)
		wantLive := pc.phase == v1alpha1.PreviewReady && pc.observed && pc.serves && pc.specNow && linkable
		v := viewPreview(proj, in, pv)
		if (v.state == templates.PreviewLive) != wantLive {
			failure = fmt.Sprintf("%+v: state %q, want live %v", pc, v.state, wantLive)
			return false
		}
		p := &pass{in: in, proj: proj, preview: v}
		status, err := templates.RenderIntentStatusComment(templates.IntentStatusComment{Namespace: testNS,
			Intent: in.Name, Phase: string(in.Status.Phase), Preview: p.statusPreview()})
		if err != nil {
			failure = err.Error()
			return false
		}
		c := p.previewComment(v, in.Status.PullRequests[0], &v1alpha1.EffectivePreviews(proj)[0], false, false)
		comment, err := templates.RenderIntentPreviewComment(c)
		if err != nil {
			failure = err.Error()
			return false
		}
		for name, body := range map[string]string{"status comment": status, "pull request comment": comment} {
			if strings.Contains(body, "https://") != wantLive {
				failure = fmt.Sprintf("%+v: %s names an https address: %v, want %v:\n%s", pc, name,
					strings.Contains(body, "https://"), wantLive, body)
				return false
			}
			if wantLive && !strings.Contains(body, "(https://target-1.") {
				failure = fmt.Sprintf("%+v: %s does not link the preview:\n%s", pc, name, body)
				return false
			}
		}
		return true
	}
	if err := quick.Check(holds, previewCaseConfig(20261005)); err != nil {
		t.Errorf("%v\n%s", err, failure)
	}
}
