// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// PreviewSourceReconciler is the sole writer of Preview spec. It copies only
// operator Project configuration and the PR heads (or preview bases) already
// verified and recorded by the Intent reconciler; it never reads issue or
// agent text.
// A separate reconciler prevents preview errors from blocking an Intent's
// merge, status comment, or Finding flow. Disabled unless the operator
// explicitly enables it alongside the preview-controller.
type PreviewSourceReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
}

const previewTTL = 72 * time.Hour

func (r *PreviewSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var in v1alpha1.Intent
	if err := r.APIReader.Get(ctx, req.NamespacedName, &in); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var project v1alpha1.Project
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: in.Spec.Project},
		&project); err != nil && !kerrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	desired, ok := desiredPreview(&in, &project)
	var existing v1alpha1.Preview
	err := r.APIReader.Get(ctx, req.NamespacedName, &existing)
	if err != nil && !kerrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil && existing.Spec.IntentRef.UID != in.UID {
		return ctrl.Result{}, fmt.Errorf("preview %s belongs to a different Intent UID", existing.Name)
	}
	if !ok {
		if err == nil && existing.DeletionTimestamp.IsZero() {
			uid := existing.UID
			return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &existing,
				&client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}))
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if kerrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(&in, desired, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desired); err != nil && !kerrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if !existing.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if !reflect.DeepEqual(existing.Spec.Components, desired.Spec.Components) {
		existing.Spec.Components = desired.Spec.Components
		if err := r.Update(ctx, &existing); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// desiredPreview is the Intent's Preview, derived by
// v1alpha1.DesiredPreviewComponents: the one function preview-controller
// re-checks every Preview against before rendering it, so the writer never
// creates a Preview the controller would refuse or delete (which, requeued
// every second, would churn create and delete for as long as the state held).
func desiredPreview(in *v1alpha1.Intent, project *v1alpha1.Project) (*v1alpha1.Preview, bool) {
	components, ok := v1alpha1.DesiredPreviewComponents(project, in)
	if !ok {
		return nil, false
	}
	return &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace},
		Spec: v1alpha1.PreviewSpec{
			IntentRef:  v1alpha1.ObjectReference{Name: in.Name, UID: in.UID},
			HostLabel:  in.Name,
			Components: components,
			TTL:        metav1.Duration{Duration: previewTTL},
		},
	}, true
}

// previewBaseSHA is the only commit a preview base records: a full 40-hex
// SHA, as status.previewBases[].sha's schema holds it, whose image is the
// sha-<SHA> tag the app's main-branch publisher pushed.
var previewBaseSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// recordPreviewBases records, with the preview projection on (Settings.
// Previews), the commit each previewed repository the intent opened no pull
// request in runs in its preview: that repository's default-branch head, read
// on the first review pass, appended to status.previewBases once and never
// rewritten, so the preview does not move when main does. The preview then
// runs the sha-<SHA> image that repository's trusted main-branch publisher
// pushed for that commit (tagged main-<SHA> too, which retention keeps). It
// reads nothing unless a previewed repository has a pull request (there is
// no preview otherwise), and nothing under a repository's rate floor. It is
// best effort, like the cross-link: a read GitHub refuses or fails is logged
// and tried again at the next poll, and holds no phase back; until every
// base is recorded the intent simply has no Preview. changed reports a
// status write.
func (p *pass) recordPreviewBases(ctx context.Context) (changed bool, err error) {
	if phase := p.in.Status.Phase; !p.set.Previews ||
		phase != v1alpha1.IntentInReview && phase != v1alpha1.IntentRevising {
		return false, nil
	}
	var missing []v1alpha1.RepositoryPreview
	fromPR := false
	for _, rp := range v1alpha1.EffectivePreviews(p.proj) {
		switch {
		case p.pullRequest(rp.URL) != nil:
			fromPR = true
		case !slices.ContainsFunc(p.in.Status.PreviewBases, func(b v1alpha1.IntentPreviewBase) bool {
			return sameRepo(b.Repository, rp.URL)
		}):
			missing = append(missing, rp)
		}
	}
	if !fromPR || len(missing) == 0 {
		return false, nil
	}
	bases := slices.Clone(p.in.Status.PreviewBases)
	for _, rp := range missing {
		sha, ok := p.defaultHead(ctx, rp.URL)
		if !ok {
			continue
		}
		bases = append(bases, v1alpha1.IntentPreviewBase{Repository: rp.URL, SHA: sha})
	}
	if len(bases) == len(p.in.Status.PreviewBases) {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		for _, b := range bases {
			if !slices.ContainsFunc(cur.Status.PreviewBases, func(o v1alpha1.IntentPreviewBase) bool {
				return sameRepo(o.Repository, b.Repository)
			}) {
				cur.Status.PreviewBases = append(cur.Status.PreviewBases, b)
			}
		}
		return nil
	})
}

// defaultHead is the head of repoURL's default branch, read under its rate
// floor, as a preview base records it; ok is false when it could not be
// read now (the floor, a failed or refused read, a head that is not a 40-hex
// SHA), which is logged and left for the next poll.
func (p *pass) defaultHead(ctx context.Context, repoURL string) (sha string, ok bool) {
	warn := func(msg string, attrs ...slog.Attr) {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, msg, append([]slog.Attr{slog.String("intent", p.in.Name),
			slog.String("repository", repoURL)}, attrs...)...)
	}
	if above, err := p.rateOK(ctx, repoURL); err != nil || !above {
		if err != nil {
			warn("read a preview base's rate budget; retried at the next poll", slog.Any("error", err))
		}
		return "", false
	}
	branch, err := p.r.GitHub.DefaultBranch(ctx, repoURL)
	if err != nil {
		warn("read a preview base's default branch; retried at the next poll", slog.Any("error", err))
		return "", false
	}
	sha, err = p.r.GitHub.HeadSHA(ctx, repoURL, branch)
	if err != nil {
		warn("read a preview base's default-branch head; retried at the next poll", slog.Any("error", err))
		return "", false
	}
	if !previewBaseSHA.MatchString(sha) {
		warn("a preview base's default-branch head is not a 40-hex commit; no preview runs it",
			slog.String("head", sha))
		return "", false
	}
	return sha, true
}

// SetupWithManager watches Intents; the minute poll also picks up Project
// configuration changes without requiring a second Project informer.
func (r *PreviewSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Intent{}).
		Named("intent-preview-source").
		Complete(r)
}
