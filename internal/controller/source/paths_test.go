// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/artifact"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

var errBoom = errors.New("boom")

var conflictErr = kerrors.NewConflict(schema.GroupResource{Resource: "x"}, "x", errors.New("stale"))

// failingForgeClient fails the call named by fail; the rest answer like
// fakeForgeClient.
type failingForgeClient struct {
	fakeForgeClient
	fail     string
	branches []string
}

func (f *failingForgeClient) DefaultBranch(ctx context.Context, repo ghclient.Repo) (string, error) {
	if f.fail == "DefaultBranch" {
		return "", errBoom
	}
	return f.fakeForgeClient.DefaultBranch(ctx, repo)
}

func (f *failingForgeClient) HeadSHA(ctx context.Context, repo ghclient.Repo, branch string) (string, error) {
	f.branches = append(f.branches, branch)
	if f.fail == "HeadSHA" {
		return "", errBoom
	}
	return f.fakeForgeClient.HeadSHA(ctx, repo, branch)
}

func (f *failingForgeClient) Tarball(ctx context.Context, repo ghclient.Repo, ref string) (io.ReadCloser, error) {
	if f.fail == "Tarball" {
		return nil, errBoom
	}
	return f.fakeForgeClient.Tarball(ctx, repo, ref)
}

// harnessWith is harness over any forgeClient with interceptors.
func harnessWith(t *testing.T, gh forgeClient, funcs interceptor.Funcs, objs ...client.Object) (
	*RepositoryReconciler, client.Client,
) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Repository{}, &v1alpha1.Forge{}).
		WithInterceptorFuncs(funcs).
		Build()
	store, err := artifact.NewStore(t.TempDir(), "http://arts.local")
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	return &RepositoryReconciler{
		Client:    c,
		Forges:    forge.NewStore(c),
		Artifacts: store,
		ClientFor: func(context.Context, *forge.Resolved) (forgeClient, error) { return gh, nil },
		Now:       func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) },
	}, c
}

func repoReq() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: repoName}}
}

// TestReconcileForgeFailures: a forge call that fails records Ready=False
// with the step's reason and returns the error for backoff; nothing is
// stored.
func TestReconcileForgeFailures(t *testing.T) {
	tests := []struct {
		fail       string
		wantReason string
	}{
		{"DefaultBranch", "ResolveFailed"},
		{"HeadSHA", "ResolveFailed"},
		{"Tarball", "FetchFailed"},
	}
	for _, tt := range tests {
		t.Run(tt.fail, func(t *testing.T) {
			gh := &failingForgeClient{
				fakeForgeClient: fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: "t"},
				fail:            tt.fail,
			}
			r, c := harnessWith(t, gh, interceptor.Funcs{}, testForge("gh"), testRepository())
			if _, err := r.Reconcile(t.Context(), repoReq()); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			repo := getRepo(t, c)
			cond := meta.FindStatusCondition(repo.Status.Conditions, v1alpha1.ConditionReady)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tt.wantReason ||
				cond.Message != "boom" {
				t.Errorf("Ready = %+v, want False/%s boom", cond, tt.wantReason)
			}
			if repo.Status.Artifact != nil || repo.Status.ResolvedSHA != "" {
				t.Errorf("status = sha %q artifact %+v, want neither", repo.Status.ResolvedSHA, repo.Status.Artifact)
			}
			if _, ok := r.Artifacts.Get("patchy/" + repoName); ok {
				t.Error("artifact stored despite the failure")
			}
		})
	}
}

// TestReconcileExplicitBranch: a Repository naming its branch is pinned to
// that branch's head without asking for the default branch.
func TestReconcileExplicitBranch(t *testing.T) {
	gh := &failingForgeClient{
		fakeForgeClient: fakeForgeClient{headSHA: "abc123", tarball: "t"},
		fail:            "DefaultBranch", // would fail if consulted
	}
	repo := testRepository()
	repo.Spec.Ref.Branch = "release-1"
	r, c := harnessWith(t, gh, interceptor.Funcs{}, testForge("gh"), repo)
	if _, err := r.Reconcile(t.Context(), repoReq()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(gh.branches) != 1 || gh.branches[0] != "release-1" {
		t.Errorf("HeadSHA branches = %v, want [release-1]", gh.branches)
	}
	if got := getRepo(t, c); got.Status.ResolvedSHA != "abc123" ||
		!meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady) {
		t.Errorf("status = %+v, want Ready at abc123", got.Status)
	}
}

// TestReconcileCredentialInvalid: without a ClientFor seam the reconciler
// builds its client from the Forge's Secret; a missing Secret is
// CredentialInvalid.
func TestReconcileCredentialInvalid(t *testing.T) {
	r, c := harnessWith(t, nil, interceptor.Funcs{}, testForge("gh"), testRepository())
	r.ClientFor = nil
	if _, err := r.Reconcile(t.Context(), repoReq()); !kerrors.IsNotFound(err) {
		t.Fatalf("Reconcile error = %v, want the missing Secret", err)
	}
	cond := meta.FindStatusCondition(getRepo(t, c).Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "CredentialInvalid" || cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v, want False/CredentialInvalid", cond)
	}
}

// TestReconcileSecondPassKeepsFetchTime: a pass that finds the artifact
// already stored does not download again or re-stamp LastFetchedAt.
func TestReconcileSecondPassKeepsFetchTime(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: "t"}
	r, c := harnessWith(t, gh, interceptor.Funcs{}, testForge("gh"), testRepository())
	if _, err := r.Reconcile(t.Context(), repoReq()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	first := getRepo(t, c).Status.Artifact.LastFetchedAt.DeepCopy()
	r.Now = func() time.Time { return first.Add(time.Hour) }
	if _, err := r.Reconcile(t.Context(), repoReq()); err != nil {
		t.Fatalf("Reconcile again: %v", err)
	}
	if gh.tarballCalls != 1 {
		t.Errorf("tarball calls = %d, want 1", gh.tarballCalls)
	}
	if got := getRepo(t, c).Status.Artifact.LastFetchedAt; !got.Equal(first) {
		t.Errorf("lastFetchedAt = %v, want the first fetch %v", got, first)
	}
}

// TestReconcileStatusWriteErrors: a conflict on the Ready write is
// requeued; another error is returned. A failed write while recording a
// failure is joined to the cause.
func TestReconcileStatusWriteErrors(t *testing.T) {
	writeFails := func(err error) interceptor.Funcs {
		return interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object,
			...client.SubResourceUpdateOption) error {
			return err
		}}
	}
	t.Run("ready conflict", func(t *testing.T) {
		gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: "t"}
		r, _ := harnessWith(t, gh, writeFails(conflictErr), testForge("gh"), testRepository())
		res, err := r.Reconcile(t.Context(), repoReq())
		if err != nil || !res.Requeue { //nolint:staticcheck // the reconciler under test sets Requeue
			t.Errorf("Reconcile = %+v, %v, want a requeue", res, err)
		}
	})
	t.Run("ready error", func(t *testing.T) {
		gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: "t"}
		r, _ := harnessWith(t, gh, writeFails(errBoom), testForge("gh"), testRepository())
		if _, err := r.Reconcile(t.Context(), repoReq()); !errors.Is(err, errBoom) {
			t.Errorf("Reconcile error = %v, want %v", err, errBoom)
		}
	})
	t.Run("failure write error joins the cause", func(t *testing.T) {
		gh := &failingForgeClient{fail: "HeadSHA"}
		writeErr := errors.New("write refused")
		r, _ := harnessWith(t, gh, writeFails(writeErr), testForge("gh"), testRepository())
		_, err := r.Reconcile(t.Context(), repoReq())
		if !errors.Is(err, errBoom) || !errors.Is(err, writeErr) {
			t.Errorf("Reconcile error = %v, want both the cause and the write error", err)
		}
	})
	t.Run("unresolvable conflict is quiet", func(t *testing.T) {
		r, _ := harnessWith(t, &fakeForgeClient{}, writeFails(conflictErr), testRepository())
		if _, err := r.Reconcile(t.Context(), repoReq()); err != nil {
			t.Errorf("Reconcile error = %v, want nil (waits for a Forge)", err)
		}
	})
	t.Run("stall error", func(t *testing.T) {
		gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: strings.Repeat("x", 64)}
		r, _ := harnessWith(t, gh, writeFails(errBoom), testForge("gh"), testRepository())
		r.MaxArtifactBytes = 16
		if _, err := r.Reconcile(t.Context(), repoReq()); !errors.Is(err, errBoom) {
			t.Errorf("Reconcile error = %v, want %v", err, errBoom)
		}
	})
}

// TestReconcileGetError: a transient read error is returned and the stored
// artifact is kept (only a confirmed absence drops it).
func TestReconcileGetError(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: "t"}
	failGet := false
	r, _ := harnessWith(t, gh, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch,
		key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*v1alpha1.Repository); ok && failGet {
			return errBoom
		}
		return c.Get(ctx, key, obj, opts...)
	}}, testForge("gh"), testRepository())
	if _, err := r.Reconcile(t.Context(), repoReq()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	failGet = true
	if _, err := r.Reconcile(t.Context(), repoReq()); !errors.Is(err, errBoom) {
		t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
	}
	if _, ok := r.Artifacts.Get("patchy/" + repoName); !ok {
		t.Error("artifact dropped on a transient read error")
	}
}

// TestReconcileDeletingDropsArtifact: a Repository being deleted (still
// held by a finalizer) has its stored artifact dropped at once.
func TestReconcileDeletingDropsArtifact(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: "t"}
	repo := testRepository()
	repo.Finalizers = []string{"patchy.bitwisemedia.uk/test"}
	r, c := harnessWith(t, gh, interceptor.Funcs{}, testForge("gh"), repo)
	if _, err := r.Reconcile(t.Context(), repoReq()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := c.Delete(t.Context(), getRepo(t, c)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Reconcile(t.Context(), repoReq()); err != nil {
		t.Fatalf("Reconcile deleting: %v", err)
	}
	if _, ok := r.Artifacts.Get("patchy/" + repoName); ok {
		t.Error("artifact kept for a deleting Repository")
	}
	if gh.tarballCalls != 1 {
		t.Errorf("tarball calls = %d, want no fetch for a deleting Repository", gh.tarballCalls)
	}
}

// TestForgeFailReason maps resolution errors onto condition reasons.
func TestForgeFailReason(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("x: %w", forge.ErrAmbiguous), v1alpha1.ReasonAmbiguous},
		{fmt.Errorf("x: %w", forge.ErrNoMatch), v1alpha1.ReasonNoForgeMatch},
		{errBoom, "ResolveFailed"},
	} {
		if got := forgeFailReason(tt.err); got != tt.want {
			t.Errorf("forgeFailReason(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

// TestRejectedLabelsAndUnwraps: a runner-image Rejection is wrapped with
// its own reason (or the stage when it has none) and still unwraps to the
// Rejection; any other error passes through untouched.
func TestRejectedLabelsAndUnwraps(t *testing.T) {
	labelled := &runnerimage.Rejection{Reason: "Unsigned", Message: "no signature"}
	bare := &runnerimage.Rejection{Message: "path not allowed"}
	for _, tt := range []struct {
		name       string
		cause      *runnerimage.Rejection
		wantReason string
	}{{"own reason", labelled, "Unsigned"}, {"stage label", bare, "Policy"}} {
		t.Run(tt.name, func(t *testing.T) {
			err := rejected("Policy", "ghcr.io/acme/env:1", ".patchy/agent.yaml", fmt.Errorf("check: %w", tt.cause))
			var rej *imageRejection
			if !errors.As(err, &rej) {
				t.Fatalf("rejected() = %T, want *imageRejection", err)
			}
			if rej.reason != tt.wantReason || rej.declared != "ghcr.io/acme/env:1" || rej.manifest != ".patchy/agent.yaml" {
				t.Errorf("rejection = %+v", rej)
			}
			var inner *runnerimage.Rejection
			if !errors.As(err, &inner) || inner != tt.cause {
				t.Errorf("errors.As(Rejection) = %v, want the original cause", inner)
			}
			if err.Error() != tt.cause.Message {
				t.Errorf("Error() = %q, want %q", err.Error(), tt.cause.Message)
			}
		})
	}
	if err := rejected("Policy", "", "", errBoom); err != errBoom { //nolint:errorlint // identity is the point
		t.Errorf("rejected(non-rejection) = %v, want it passed through", err)
	}
}

// TestForgeReconcileEdges: deleting and missing Forges are no-ops; a
// conflicting status write is requeued and any other write error returned.
func TestForgeReconcileEdges(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "patchy"},
		Data:       map[string][]byte{forge.SecretKeyToken: []byte("t")},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "gh"}}
	build := func(funcs interceptor.Funcs, objs ...client.Object) (*ForgeReconciler, client.Client) {
		c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
			WithStatusSubresource(&v1alpha1.Forge{}).WithInterceptorFuncs(funcs).Build()
		return &ForgeReconciler{Client: c, Forges: forge.NewStore(c)}, c
	}

	t.Run("missing", func(t *testing.T) {
		r, _ := build(interceptor.Funcs{})
		if res, err := r.Reconcile(t.Context(), req); err != nil || res != (ctrl.Result{}) {
			t.Errorf("Reconcile = %+v, %v, want empty", res, err)
		}
	})
	t.Run("deleting", func(t *testing.T) {
		f := testForge("gh")
		now := metav1.NewTime(time.Now())
		f.DeletionTimestamp = &now
		f.Finalizers = []string{"patchy.bitwisemedia.uk/test"}
		r, c := build(interceptor.Funcs{}, f, secret)
		if res, err := r.Reconcile(t.Context(), req); err != nil || res != (ctrl.Result{}) {
			t.Errorf("Reconcile = %+v, %v, want empty", res, err)
		}
		var got v1alpha1.Forge
		if err := c.Get(t.Context(), req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Status.Conditions) != 0 {
			t.Errorf("conditions = %+v, want none written", got.Status.Conditions)
		}
	})
	for name, tt := range map[string]struct {
		err         error
		wantErr     bool
		wantRequeue bool
	}{"conflict": {conflictErr, false, true}, "error": {errBoom, true, false}} {
		t.Run(name, func(t *testing.T) {
			r, _ := build(interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string,
				client.Object, ...client.SubResourceUpdateOption) error {
				return tt.err
			}}, testForge("gh"), secret)
			res, err := r.Reconcile(t.Context(), req)
			if (err != nil) != tt.wantErr || res.Requeue != tt.wantRequeue { //nolint:staticcheck // set by the reconciler
				t.Errorf("Reconcile = %+v, %v, want requeue %v err %v", res, err, tt.wantRequeue, tt.wantErr)
			}
		})
	}
}
