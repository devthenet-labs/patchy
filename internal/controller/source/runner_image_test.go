// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/artifact"
	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// tarball builds the gzip-compressed archive the forge serves: every file
// under one prefix directory.
func tarball(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"README.md", runnerimage.AgentYAMLPath, runnerimage.DevcontainerPath} {
		data, ok := files[name]
		if !ok {
			continue
		}
		hdr := &tar.Header{Name: "acme-orders-abc123/" + name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// fakeResolver answers Resolve with canned results and records its calls;
// observe runs inside Resolve so a test can read the Repository mid-flight.
type fakeResolver struct {
	resolved runnerimage.Resolved
	err      error
	calls    []imageref.Ref
	observe  func()
}

func (f *fakeResolver) Resolve(_ context.Context, ref imageref.Ref) (runnerimage.Resolved, error) {
	f.calls = append(f.calls, ref)
	if f.observe != nil {
		f.observe()
	}
	if f.err != nil {
		return runnerimage.Resolved{}, f.err
	}
	res := f.resolved
	if res.Image == "" {
		res.Image = ref.Repository + "@sha256:" + strings.Repeat("ab", 32)
	}
	return res, nil
}

const (
	yamlDecl = "image: ghcr.io/acme/go-env:1.26\n"
	dcDecl   = `{ "image": "ghcr.io/acme/dev-env:2", // editor image
  "customizations": { "vscode": {} }, }`
	dcBuild = `{"build": {"dockerfile": "Dockerfile"}, "features": {}}`
)

// imageHarness is harness plus the runner-image configuration.
func imageHarness(t *testing.T, gh *fakeForgeClient, fr *fakeResolver) (*RepositoryReconciler, client.Client) {
	t.Helper()
	r, c := harness(t, gh, testForge("gh"), testRepository())
	policy, err := runnerimage.NewPolicy([]string{"ghcr.io/acme/"})
	if err != nil {
		t.Fatal(err)
	}
	// Handoff, not the default: most cases here assert the stall, and
	// TestRunnerImageOnRejectDefault covers the fallback explicitly.
	r.Images = &RunnerImages{Policy: policy, Resolver: fr, OnReject: OnRejectHandoff}
	r.Now = func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }
	return r, c
}

// reconcileErr is reconcile without the fatal on error, for the backoff
// paths.
func reconcileErr(t *testing.T, r *RepositoryReconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "patchy", Name: repoName},
	})
}

func condition(t *testing.T, repo *v1alpha1.Repository, typ string) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(repo.Status.Conditions, typ)
}

func TestRunnerImageDeclarations(t *testing.T) {
	resolved := runnerimage.Resolved{
		Image:      "ghcr.io/acme/go-env@sha256:" + strings.Repeat("cd", 32),
		SearchPath: []string{"/usr/local/go/bin", "/usr/bin"},
		Verified:   true,
	}
	cases := []struct {
		name         string
		files        map[string]string
		wantDeclared string
		wantManifest string
		wantImage    string
		wantMessage  string
		wantCalls    int
	}{
		{"agent.yaml only", map[string]string{runnerimage.AgentYAMLPath: yamlDecl},
			"ghcr.io/acme/go-env:1.26", runnerimage.AgentYAMLPath, resolved.Image, "", 1},
		{"devcontainer only records its manifest", map[string]string{runnerimage.DevcontainerPath: dcDecl},
			"ghcr.io/acme/dev-env:2", runnerimage.DevcontainerPath, resolved.Image, "", 1},
		{"agent.yaml wins over devcontainer",
			map[string]string{runnerimage.AgentYAMLPath: yamlDecl, runnerimage.DevcontainerPath: dcDecl},
			"ghcr.io/acme/go-env:1.26", runnerimage.AgentYAMLPath, resolved.Image, "", 1},
		{"valid agent.yaml beside a build-based devcontainer",
			map[string]string{runnerimage.AgentYAMLPath: yamlDecl, runnerimage.DevcontainerPath: dcBuild},
			"ghcr.io/acme/go-env:1.26", runnerimage.AgentYAMLPath, resolved.Image, "", 1},
		{"build-based devcontainer is not applicable", map[string]string{runnerimage.DevcontainerPath: dcBuild},
			"", runnerimage.DevcontainerPath, "", "builds its image (`build`); patchy does not build images", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: tarball(t, tc.files)}
			fr := &fakeResolver{resolved: resolved}
			r, c := imageHarness(t, gh, fr)

			reconcile(t, r)

			repo := getRepo(t, c)
			ri := repo.Status.RunnerImage
			if ri == nil {
				t.Fatalf("status.runnerImage = nil, want a record")
			}
			if ri.Declared != tc.wantDeclared || ri.Manifest != tc.wantManifest || ri.Image != tc.wantImage {
				t.Errorf("runnerImage = %+v, want declared %q manifest %q image %q",
					ri, tc.wantDeclared, tc.wantManifest, tc.wantImage)
			}
			if ri.Rejected != "" || !strings.Contains(ri.Message, tc.wantMessage) {
				t.Errorf("rejected/message = %q/%q, want no rejection and message containing %q",
					ri.Rejected, ri.Message, tc.wantMessage)
			}
			if tc.wantImage != "" && (ri.SearchPath != "/usr/local/go/bin:/usr/bin" || !ri.Verified) {
				t.Errorf("searchPath/verified = %q/%v", ri.SearchPath, ri.Verified)
			}
			if ri.ResolvedAt == nil || !ri.ResolvedAt.Time.Equal(r.Now()) {
				t.Errorf("resolvedAt = %v, want the clock seam's time", ri.ResolvedAt)
			}
			if len(fr.calls) != tc.wantCalls {
				t.Errorf("resolver calls = %d, want %d", len(fr.calls), tc.wantCalls)
			}
			if tc.wantCalls > 0 && fr.calls[0].String() != tc.wantDeclared {
				t.Errorf("resolver got %s, want the declared %s", fr.calls[0], tc.wantDeclared)
			}
			if !meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
				t.Errorf("Ready = %+v, want True", condition(t, repo, v1alpha1.ConditionReady))
			}
			if condition(t, repo, v1alpha1.ConditionStalled) != nil {
				t.Errorf("Stalled = %+v, want absent", condition(t, repo, v1alpha1.ConditionStalled))
			}
			if repo.Status.Artifact == nil || repo.Status.ResolvedSHA != "abc123" {
				t.Errorf("artifact/sha = %+v/%q, want both pinned", repo.Status.Artifact, repo.Status.ResolvedSHA)
			}
		})
	}
}

func TestRunnerImageNoneOrDisabled(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		enabled bool
	}{
		{"no declaration", map[string]string{"README.md": "hi"}, true},
		{"feature off ignores a declaration", map[string]string{runnerimage.AgentYAMLPath: yamlDecl}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: tarball(t, tc.files)}
			fr := &fakeResolver{}
			r, c := imageHarness(t, gh, fr)
			if !tc.enabled {
				r.Images = nil
			}
			reconcile(t, r)
			repo := getRepo(t, c)
			if repo.Status.RunnerImage != nil {
				t.Errorf("status.runnerImage = %+v, want nil", repo.Status.RunnerImage)
			}
			if len(fr.calls) != 0 {
				t.Errorf("resolver called %d times", len(fr.calls))
			}
			if !meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
				t.Errorf("Ready = %+v, want True", condition(t, repo, v1alpha1.ConditionReady))
			}
		})
	}
}

func TestRunnerImageRejections(t *testing.T) {
	cases := []struct {
		name         string
		files        map[string]string
		resolver     *fakeResolver
		wantReason   string
		wantMessage  string
		wantDeclared string
		wantCalls    int
	}{
		{"invalid agent.yaml never falls through",
			map[string]string{runnerimage.AgentYAMLPath: "image: x\nbuild: .\n", runnerimage.DevcontainerPath: dcDecl},
			&fakeResolver{}, "InvalidDeclaration", "has a `build` key", "", 0},
		{"invalid reference", map[string]string{runnerimage.AgentYAMLPath: "image: 'ghcr.io/acme/app:bad tag'\n"},
			&fakeResolver{}, "InvalidReference", "contains whitespace", "ghcr.io/acme/app:bad tag", 0},
		{"not allowlisted", map[string]string{runnerimage.AgentYAMLPath: "image: ghcr.io/acme-evil/app:1\n"},
			&fakeResolver{}, "NotAllowlisted", "not under an allowlisted registry path (ghcr.io/acme/)",
			"ghcr.io/acme-evil/app:1", 0},
		{"devcontainer image outside the allowlist is a rejection too",
			map[string]string{runnerimage.DevcontainerPath: `{"image": "docker.io/library/golang:1.26"}`},
			&fakeResolver{}, "NotAllowlisted", "not under an allowlisted registry path", "docker.io/library/golang:1.26", 0},
		{"resolver rejection carries its reason", map[string]string{runnerimage.AgentYAMLPath: yamlDecl},
			&fakeResolver{err: &runnerimage.Rejection{Reason: "Unsigned", Message: "image `x` carries no signature"}},
			"Unsigned", "carries no signature", "ghcr.io/acme/go-env:1.26", 1},
		{"resolver rejection without a reason is labelled by stage", map[string]string{runnerimage.AgentYAMLPath: yamlDecl},
			&fakeResolver{err: &runnerimage.Rejection{Message: "image ENV sets `PATCHY_X`"}},
			"Resolve", "image ENV sets", "ghcr.io/acme/go-env:1.26", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: tarball(t, tc.files)}
			r, c := imageHarness(t, gh, tc.resolver)

			reconcile(t, r)

			repo := getRepo(t, c)
			stalled := condition(t, repo, v1alpha1.ConditionStalled)
			if stalled == nil || stalled.Status != metav1.ConditionTrue || stalled.Reason != v1alpha1.ReasonRunnerImageRejected {
				t.Fatalf("Stalled = %+v, want True/RunnerImageRejected", stalled)
			}
			ready := condition(t, repo, v1alpha1.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonRunnerImageRejected {
				t.Errorf("Ready = %+v, want False/RunnerImageRejected", ready)
			}
			ri := repo.Status.RunnerImage
			if ri == nil || ri.Rejected != tc.wantReason || ri.Image != "" || ri.Declared != tc.wantDeclared {
				t.Fatalf("runnerImage = %+v, want rejected %s, no image, declared %q", ri, tc.wantReason, tc.wantDeclared)
			}
			if !strings.Contains(ri.Message, tc.wantMessage) || ri.Message != stalled.Message {
				t.Errorf("message = %q (stalled %q), want containing %q and mirrored", ri.Message, stalled.Message, tc.wantMessage)
			}
			if ri.ResolvedAt == nil {
				t.Error("resolvedAt unset on a rejection")
			}
			// The artifact is retained so a revived finding has a tree to run on.
			if repo.Status.Artifact == nil || repo.Status.ResolvedSHA != "abc123" {
				t.Errorf("artifact/sha = %+v/%q, want retained", repo.Status.Artifact, repo.Status.ResolvedSHA)
			}
			if _, ok := r.Artifacts.Get("patchy/" + repoName); !ok {
				t.Error("artifact dropped from the store on rejection")
			}
			if len(tc.resolver.calls) != tc.wantCalls {
				t.Errorf("resolver calls = %d, want %d", len(tc.resolver.calls), tc.wantCalls)
			}
		})
	}
}

// TestRunnerImageOnRejectDefault: under onReject default — also what an
// unset policy means — a rejection is recorded but the Repository stays
// Ready, so the finding runs on the default image with no human step.
func TestRunnerImageOnRejectDefault(t *testing.T) {
	for _, policy := range []string{OnRejectDefault, ""} {
		t.Run("policy="+policy, func(t *testing.T) {
			gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
				tarball: tarball(t, map[string]string{runnerimage.AgentYAMLPath: "image: ghcr.io/acme-evil/app:1\n"})}
			r, c := imageHarness(t, gh, &fakeResolver{})
			r.Images.OnReject = policy

			reconcile(t, r)

			repo := getRepo(t, c)
			if !meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
				t.Errorf("Ready = %+v, want True under onReject=default", condition(t, repo, v1alpha1.ConditionReady))
			}
			if condition(t, repo, v1alpha1.ConditionStalled) != nil {
				t.Errorf("Stalled = %+v, want absent", condition(t, repo, v1alpha1.ConditionStalled))
			}
			ri := repo.Status.RunnerImage
			if ri == nil || ri.Rejected != "NotAllowlisted" || ri.Image != "" {
				t.Errorf("runnerImage = %+v, want the rejection recorded with no image", ri)
			}
			// A later reconcile keeps it Ready and never re-resolves.
			reconcile(t, r)
			repo = getRepo(t, c)
			if !meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
				t.Error("Ready flipped on the second reconcile")
			}
		})
	}
}

func TestRunnerImageTransientFailureBacksOffWithoutRefetch(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{runnerimage.AgentYAMLPath: yamlDecl})}
	fr := &fakeResolver{err: errors.New("registry unreachable")}
	r, c := imageHarness(t, gh, fr)

	if _, err := reconcileErr(t, r); err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("Reconcile = %v, want the transient error for backoff", err)
	}
	repo := getRepo(t, c)
	ready := condition(t, repo, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonRunnerImageResolveFailed {
		t.Errorf("Ready = %+v, want False/RunnerImageResolveFailed", ready)
	}
	if repo.Status.RunnerImage != nil {
		t.Errorf("runnerImage = %+v, want nil until resolved", repo.Status.RunnerImage)
	}
	if repo.Status.Artifact == nil {
		t.Fatal("artifact not persisted before the registry call")
	}
	if condition(t, repo, v1alpha1.ConditionStalled) != nil {
		t.Error("a transient failure must not stall")
	}

	// The registry recovers: the retry resolves without a second download.
	fr.err = nil
	reconcile(t, r)
	repo = getRepo(t, c)
	if repo.Status.RunnerImage == nil || repo.Status.RunnerImage.Image == "" {
		t.Errorf("runnerImage = %+v after recovery, want pinned", repo.Status.RunnerImage)
	}
	if !meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
		t.Errorf("Ready = %+v, want True after recovery", condition(t, repo, v1alpha1.ConditionReady))
	}
	if gh.tarballCalls != 1 {
		t.Errorf("tarball downloads = %d, want 1: resolution never costs a re-download", gh.tarballCalls)
	}
	if len(fr.calls) != 2 {
		t.Errorf("resolver calls = %d, want 2 (one failed, one succeeded)", len(fr.calls))
	}
}

func TestRunnerImagePinnedOnce(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{runnerimage.AgentYAMLPath: yamlDecl})}
	first := "ghcr.io/acme/go-env@sha256:" + strings.Repeat("11", 32)
	fr := &fakeResolver{resolved: runnerimage.Resolved{Image: first, SearchPath: []string{"/bin"}}}
	r, c := imageHarness(t, gh, fr)

	reconcile(t, r)
	fr.resolved.Image = "ghcr.io/acme/go-env@sha256:" + strings.Repeat("22", 32) // the tag moved
	reconcile(t, r)

	repo := getRepo(t, c)
	if repo.Status.RunnerImage == nil || repo.Status.RunnerImage.Image != first {
		t.Errorf("runnerImage = %+v after a second reconcile, want the first pin %s", repo.Status.RunnerImage, first)
	}
	if len(fr.calls) != 1 {
		t.Errorf("resolver calls = %d, want 1 (pin exactly once)", len(fr.calls))
	}

	// A restart empties the store: the artifact is re-fetched, the image
	// is not re-resolved.
	fresh, err := artifact.NewStore(t.TempDir(), "http://arts.local")
	if err != nil {
		t.Fatal(err)
	}
	r.Artifacts = fresh
	reconcile(t, r)
	repo = getRepo(t, c)
	if repo.Status.RunnerImage.Image != first || len(fr.calls) != 1 {
		t.Errorf("after restart: image %s, resolver calls %d; want %s and 1",
			repo.Status.RunnerImage.Image, len(fr.calls), first)
	}
	if gh.tarballCalls != 2 {
		t.Errorf("tarball downloads = %d, want 2 (one per store)", gh.tarballCalls)
	}
}

func TestRunnerImageRejectionSurvivesRestart(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{runnerimage.AgentYAMLPath: yamlDecl})}
	fr := &fakeResolver{err: &runnerimage.Rejection{Reason: "Oversized", Message: "too big"}}
	r, c := imageHarness(t, gh, fr)
	reconcile(t, r)

	fresh, err := artifact.NewStore(t.TempDir(), "http://arts.local")
	if err != nil {
		t.Fatal(err)
	}
	r.Artifacts = fresh
	fr.err = nil // even a now-passing registry does not un-stall a pinned rejection
	reconcile(t, r)

	repo := getRepo(t, c)
	stalled := condition(t, repo, v1alpha1.ConditionStalled)
	if stalled == nil || stalled.Status != metav1.ConditionTrue || stalled.Message != "too big" {
		t.Errorf("Stalled = %+v after restart, want True with the recorded message", stalled)
	}
	if meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
		t.Error("Ready = True on a rejected repository after restart")
	}
	if len(fr.calls) != 1 {
		t.Errorf("resolver calls = %d, want 1: a rejection is pinned too", len(fr.calls))
	}
	if _, ok := fresh.Get("patchy/" + repoName); !ok {
		t.Error("artifact not re-fetched into the fresh store")
	}
}

// TestRunnerImageTwoPhaseStatusWrite reads the Repository from inside the
// resolver: the artifact, SHA and forge are already persisted with
// Ready=False / RunnerImageResolving before the registry is asked.
func TestRunnerImageTwoPhaseStatusWrite(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{runnerimage.AgentYAMLPath: yamlDecl})}
	fr := &fakeResolver{}
	r, c := imageHarness(t, gh, fr)
	var mid *v1alpha1.Repository
	fr.observe = func() { mid = getRepo(t, c) }

	reconcile(t, r)

	if mid == nil {
		t.Fatal("resolver never ran")
	}
	ready := condition(t, mid, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonRunnerImageResolving {
		t.Errorf("mid-resolution Ready = %+v, want False/RunnerImageResolving", ready)
	}
	if !strings.Contains(ready.Message, "ghcr.io/acme/go-env:1.26") ||
		!strings.Contains(ready.Message, runnerimage.AgentYAMLPath) {
		t.Errorf("mid-resolution message = %q, want the declared image and file", ready.Message)
	}
	if mid.Status.Artifact == nil || mid.Status.ResolvedSHA != "abc123" || mid.Status.Forge == nil {
		t.Errorf("mid-resolution status = %+v, want artifact, sha and forge persisted", mid.Status)
	}
	if mid.Status.RunnerImage != nil {
		t.Errorf("mid-resolution runnerImage = %+v, want nil", mid.Status.RunnerImage)
	}
	final := getRepo(t, c)
	if !meta.IsStatusConditionTrue(final.Status.Conditions, v1alpha1.ConditionReady) || final.Status.RunnerImage == nil {
		t.Errorf("final status = %+v, want Ready with the pin", final.Status)
	}
}

func TestRunnerImageMessageTruncated(t *testing.T) {
	long := strings.Repeat("x", maxMessageBytes+100)
	if got := truncate(long); len(got) > maxMessageBytes || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate: len %d, suffix %q", len(got), got[len(got)-3:])
	}
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate(short) = %q", got)
	}
}

// storedPath is where the store keeps the tarball for the reconciled
// Repository: dir is the directory the store was built over.
func storedPath(t *testing.T, r *RepositoryReconciler, dir string) string {
	t.Helper()
	info, ok := r.Artifacts.Get("patchy/" + repoName)
	if !ok {
		t.Fatal("no stored artifact")
	}
	return filepath.Join(dir, path.Base(info.URL))
}

// TestRunnerImageUndeclaredTreeReadOnce: a tree that declares nothing leaves
// status.runnerImage nil, so the pin-once guard never closes for it. A later
// reconcile must neither re-walk the stored archive nor change the status —
// a walk slower than a second used to re-stamp lastFetchedAt, and the status
// write re-queued the Repository forever.
func TestRunnerImageUndeclaredTreeReadOnce(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{"README.md": "hi"})}
	fr := &fakeResolver{}
	r, c := imageHarness(t, gh, fr)
	dir := t.TempDir()
	store, err := artifact.NewStore(dir, "http://arts.local")
	if err != nil {
		t.Fatal(err)
	}
	r.Artifacts = store
	clock := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return clock }

	reconcile(t, r)
	before := getRepo(t, c)
	if before.Status.RunnerImage != nil || !meta.IsStatusConditionTrue(before.Status.Conditions, v1alpha1.ConditionReady) {
		t.Fatalf("first pass status = %+v, want Ready with no runner image", before.Status)
	}

	// A second walk would now fail on the archive, and the clock has moved
	// on as it does when the walk takes longer than a second.
	if err := os.WriteFile(storedPath(t, r, dir), []byte("not a gzip stream"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Second)
	if _, err := reconcileErr(t, r); err != nil {
		t.Fatalf("second reconcile re-read the stored archive: %v", err)
	}
	after := getRepo(t, c)
	if !equality.Semantic.DeepEqual(before.Status, after.Status) {
		t.Errorf("second reconcile changed the status:\nbefore %+v\nafter  %+v", before.Status, after.Status)
	}
	if gh.tarballCalls != 1 || len(fr.calls) != 0 {
		t.Errorf("tarball downloads = %d, resolver calls = %d; want 1 and 0", gh.tarballCalls, len(fr.calls))
	}
}

// TestRunnerImageUnreadableArtifactRefetched: a stored tarball that is not a
// readable archive (an auth proxy's HTML page served as 200) is the
// artifact's fault, not the declaration's: it is dropped, so the retry
// downloads it again instead of re-reading the same bytes forever.
func TestRunnerImageUnreadableArtifactRefetched(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: "<html>proxy login</html>"}
	fr := &fakeResolver{}
	r, c := imageHarness(t, gh, fr)

	if _, err := reconcileErr(t, r); err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("Reconcile = %v, want the unreadable-archive error for backoff", err)
	}
	repo := getRepo(t, c)
	ready := condition(t, repo, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonRunnerImageResolveFailed {
		t.Errorf("Ready = %+v, want False/RunnerImageResolveFailed", ready)
	}
	if condition(t, repo, v1alpha1.ConditionStalled) != nil {
		t.Error("an unreadable artifact must not stall")
	}
	if _, ok := r.Artifacts.Get("patchy/" + repoName); ok {
		t.Error("the unreadable artifact is still stored, so the retry would re-read it")
	}

	gh.tarball = tarball(t, map[string]string{runnerimage.AgentYAMLPath: yamlDecl})
	reconcile(t, r)
	repo = getRepo(t, c)
	if gh.tarballCalls != 2 {
		t.Errorf("tarball downloads = %d, want 2 (the retry re-fetches)", gh.tarballCalls)
	}
	if repo.Status.RunnerImage == nil || repo.Status.RunnerImage.Image == "" ||
		!meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
		t.Errorf("status after the re-fetch = %+v, want Ready with the pin", repo.Status)
	}
}

// TestRunnerImageLongRejectionFitsTheConditionCap: a rejection message quotes
// the declaration, which the committer controls (up to 64 KiB). The CRD caps
// a condition message at 32768 bytes, so an untruncated Stalled message fails
// the whole status write with 422 and the reconcile retries forever with no
// reason recorded anywhere.
func TestRunnerImageLongRejectionFitsTheConditionCap(t *testing.T) {
	long := "ghcr.io/acme/" + strings.Repeat("a", 40000) + " x"
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{runnerimage.AgentYAMLPath: "image: '" + long + "'\n"})}
	r, c := imageHarness(t, gh, &fakeResolver{})

	reconcile(t, r)

	repo := getRepo(t, c)
	stalled := condition(t, repo, v1alpha1.ConditionStalled)
	if stalled == nil || stalled.Reason != v1alpha1.ReasonRunnerImageRejected {
		t.Fatalf("Stalled = %+v, want RunnerImageRejected", stalled)
	}
	ri := repo.Status.RunnerImage
	if ri == nil || ri.Rejected != "InvalidReference" {
		t.Fatalf("runnerImage = %+v, want the InvalidReference rejection", ri)
	}
	for field, got := range map[string]string{
		"Stalled message": stalled.Message, "runnerImage.message": ri.Message, "runnerImage.declared": ri.Declared,
	} {
		if len(got) > maxMessageBytes {
			t.Errorf("%s is %d bytes, over the %d-byte cap", field, len(got), maxMessageBytes)
		}
	}
	if stalled.Message != ri.Message {
		t.Errorf("Stalled message %q is not mirrored into runnerImage.message %q", stalled.Message, ri.Message)
	}
}

// blockingResolver never answers: a registry that accepts the connection and
// then stalls.
type blockingResolver struct{}

func (blockingResolver) Resolve(ctx context.Context, _ imageref.Ref) (runnerimage.Resolved, error) {
	<-ctx.Done()
	return runnerimage.Resolved{}, ctx.Err()
}

// TestRunnerImageResolveDeadline: resolution runs on the single Repository
// worker, so a registry that never answers must cost a bounded, transient
// failure rather than block every Repository behind it.
func TestRunnerImageResolveDeadline(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{runnerimage.AgentYAMLPath: yamlDecl})}
	r, c := imageHarness(t, gh, &fakeResolver{})
	r.Images.Resolver = blockingResolver{}
	r.Images.ResolveTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := reconcileErr(t, r)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Reconcile = %v, want the deadline as a transient error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Reconcile still blocked on a registry that never answers")
	}
	repo := getRepo(t, c)
	ready := condition(t, repo, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonRunnerImageResolveFailed {
		t.Errorf("Ready = %+v, want False/RunnerImageResolveFailed", ready)
	}
	if repo.Status.RunnerImage != nil || condition(t, repo, v1alpha1.ConditionStalled) != nil {
		t.Errorf("status = %+v, want no pin and no stall on a timeout", repo.Status)
	}
}
