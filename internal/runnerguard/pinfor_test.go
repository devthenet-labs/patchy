// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerguard

import (
	"fmt"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

func acceptedImage() *v1alpha1.RunnerImage {
	return &v1alpha1.RunnerImage{
		Declared: "ghcr.io/acme/go-env:1.26", Manifest: ".patchy/agent.yaml",
		Image: pinned, SearchPath: "/usr/local/go/bin:/usr/bin:/bin", Verified: true,
	}
}

// TestPinFor: a launch requiring the repository's image gets it only when
// source-controller accepted one and the guard allows it; every other case
// names why, and leaves the spec as it was.
func TestPinFor(t *testing.T) {
	rejected := &v1alpha1.RunnerImage{
		Declared: "docker.io/evil/x:1", Manifest: ".patchy/agent.yaml",
		Rejected: "NotAllowlisted", Message: "not allowlisted",
	}
	// Never written by source-controller (a rejection carries no image),
	// but a pin must be accepted, not merely present.
	inconsistent := acceptedImage()
	inconsistent.Rejected = "Unsigned"
	tests := []struct {
		name     string
		guard    Guard
		ri       *v1alpha1.RunnerImage
		revived  bool
		wantSkip string
	}{
		{"accepted and allowed", Guard{Enabled: true}, acceptedImage(), false, ""},
		// The contract intent-controller relies on: a revival from Failed is a
		// new plan and a new approval, passed as not revived, so it builds in
		// the repository's image rather than blocking for good on
		// SkipRevivedWork, which no Project change could clear.
		{"an intent revived from Failed", Guard{Enabled: true}, acceptedImage(), false, ""},
		{"nothing declared", Guard{Enabled: true}, nil, false, SkipNoImage},
		{"not-applicable devcontainer", Guard{Enabled: true}, &v1alpha1.RunnerImage{
			Manifest: ".devcontainer/devcontainer.json", Message: "builds its image",
		}, false, SkipNoImage},
		{"rejected under onReject default", Guard{Enabled: true}, rejected, false, SkipRejected},
		{"rejected yet carrying an image", Guard{Enabled: true}, inconsistent, false, SkipRejected},
		{"kill switch off", Guard{}, acceptedImage(), false, SkipDisabled},
		{"breaker tripped", Guard{Enabled: true, Breaker: trippedBreaker()}, acceptedImage(), false, SkipBreaker},
		{"revived", Guard{Enabled: true}, acceptedImage(), true, SkipRevivedWork},
		{"nothing declared outranks the kill switch", Guard{}, nil, true, SkipNoImage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := jobs.Spec{Repo: "acme/orders"}
			skip := tt.guard.PinFor(&spec, repoWith(tt.ri), tt.revived)
			if skip != tt.wantSkip {
				t.Errorf("PinFor skip = %q, want %q", skip, tt.wantSkip)
			}
			want := jobs.Spec{Repo: "acme/orders"}
			if tt.wantSkip == "" {
				want.RunnerImage = pinned
				want.RunnerSearchPath = tt.ri.SearchPath
				want.RunnerImageManifest = tt.ri.Manifest
			}
			if spec != want {
				t.Errorf("spec = %+v, want %+v", spec, want)
			}
		})
	}
}

// TestPinForAgreesWithPin: over every guard, Repository and Finding a
// Finding launch can meet, PinFor with Revived(finding) copies exactly
// when Pin does, names Pin's reason when Pin names one (its own for a
// revival, which a Finding's wording does not fit), and where Pin copies
// nothing and says nothing — the Repository pinned no usable image, which a
// Finding runs the default image for — refuses with SkipNoImage or
// SkipRejected instead.
func TestPinForAgreesWithPin(t *testing.T) {
	guards := map[string]Guard{
		"enabled":  {Enabled: true},
		"disabled": {},
		"tripped":  {Enabled: true, Breaker: trippedBreaker()},
	}
	images := map[string]*v1alpha1.RunnerImage{
		"accepted": acceptedImage(),
		"none":     nil,
		"not applicable": {
			Manifest: ".devcontainer/devcontainer.json", Message: "builds its image",
		},
		"rejected": {Declared: "docker.io/evil/x:1", Manifest: ".patchy/agent.yaml", Rejected: "NotAllowlisted"},
	}
	findings := map[string]*v1alpha1.Finding{
		"fresh":   findingThrough(v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating),
		"handoff": findingThrough(v1alpha1.PhaseInvestigating, v1alpha1.PhaseHandedOff, v1alpha1.PhaseQueued),
		"retried": findingThrough(v1alpha1.PhaseInvestigating, v1alpha1.PhaseFailed, v1alpha1.PhaseEnhanced),
	}
	for gname, g := range guards {
		for iname, ri := range images {
			for fname, fnd := range findings {
				t.Run(fmt.Sprintf("%s/%s/%s", gname, iname, fname), func(t *testing.T) {
					repo := repoWith(ri)
					base := jobs.Spec{Repo: "acme/orders"}
					pinSpec, forSpec := base, base
					pin := g.Pin(&pinSpec, repo, fnd)
					got := g.PinFor(&forSpec, repo, Revived(fnd))
					switch {
					case pinSpec.RunnerImage != "":
						if got != "" || forSpec != pinSpec {
							t.Errorf("Pin copied; PinFor = %q with spec %+v, want \"\" with %+v", got, forSpec, pinSpec)
						}
					case pin == SkipRevived:
						if got != SkipRevivedWork || forSpec != base {
							t.Errorf("Pin = %q; PinFor = %q with spec %+v, want %q untouched",
								pin, got, forSpec, SkipRevivedWork)
						}
					case pin != "":
						if got != pin || forSpec != base {
							t.Errorf("Pin = %q; PinFor = %q with spec %+v, want the same reason untouched", pin, got, forSpec)
						}
					default:
						if (got != SkipNoImage && got != SkipRejected) || forSpec != base {
							t.Errorf("Pin pinned nothing; PinFor = %q with spec %+v, want a refusal untouched", got, forSpec)
						}
					}
				})
			}
		}
	}
}
