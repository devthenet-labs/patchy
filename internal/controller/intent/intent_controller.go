// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
)

// projectIndex indexes Intents by spec.project, so a Project change finds
// its Intents without a full scan.
const projectIndex = "spec.project"

// clockSkew widens a look for patchy's own earlier comment when the moment it
// is anchored to is this controller's clock rather than GitHub's.
const clockSkew = 5 * time.Minute

// errConflict: a status write met a newer version; the pass stops and the
// next one starts from it.
var errConflict = errors.New("intent changed under the pass")

// IntentReconciler runs each Intent's phase machine: see the package doc.
type IntentReconciler struct {
	client.Client
	// APIReader reads the Intent, uncached, at the start of every pass: the
	// cache can lag this reconciler's own writes, and exactly-once rests on
	// never acting on a stale record of what was already done.
	APIReader client.Reader
	GitHub    GitHub
	Settings  Settings
	// Images is the repository-image launch policy the run scheduler
	// enforces; a Blocked intent reads it to know whether its block holds.
	Images runnerguard.Guard
	// Nudger delivers discovery's hand-offs of ended Intents.
	Nudger *Nudger
	Now    func() time.Time
	Log    *slog.Logger

	mu sync.Mutex
	// polled is when each Intent last polled its issue, prPolled its pull
	// requests, in memory: a restart polls once more, which costs requests
	// and nothing else.
	polled   map[string]time.Time
	prPolled map[string]time.Time
	// blockedAt is the Project generation each Blocked Intent was blocked
	// under: a Project changed since may lift an image block.
	blockedAt map[string]int64
}

func (r *IntentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *IntentReconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// memo reads or writes the reconciler's in-memory maps under its lock.
func (r *IntentReconciler) memo(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.polled == nil {
		r.polled, r.prPolled, r.blockedAt = map[string]time.Time{}, map[string]time.Time{}, map[string]int64{}
	}
	f()
}

func (r *IntentReconciler) forget(name string) {
	r.memo(func() {
		delete(r.polled, name)
		delete(r.prPolled, name)
		delete(r.blockedAt, name)
	})
}

// pass is one reconcile of one Intent: the Intent as the API server holds it,
// its Project, its runs, and what this pass has read from GitHub.
type pass struct {
	r    *IntentReconciler
	set  Settings
	in   *v1alpha1.Intent
	proj *v1alpha1.Project
	runs []*v1alpha1.IntentRun
	now  time.Time

	bot     string
	botRead bool
	// polled reports that this pass polled the issue: the poll was due, so
	// the phase's other polls (a blocked build's default branch) are too.
	polled bool
	// comments are the issue's comments listed since commentsSince this
	// pass (nil: none listed); own indexes patchy's own among them by their
	// marker line.
	comments      []*ghclient.Comment
	commentsSince time.Time
	listed        bool
	own           map[string]*ghclient.Comment
}

// Reconcile takes one Intent one step at a time: every step that changes the
// Intent writes its status and ends the pass, and the write starts the next.
func (r *IntentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	settings := r.Settings.withDefaults()
	var in v1alpha1.Intent
	if err := r.APIReader.Get(ctx, req.NamespacedName, &in); err != nil {
		if kerrors.IsNotFound(err) {
			r.forget(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !in.DeletionTimestamp.IsZero() || in.Spec.Suspend {
		// A suspended intent launches nothing and writes nothing to GitHub
		// until the spec is changed back, which re-queues it.
		return ctrl.Result{}, nil
	}
	var proj v1alpha1.Project
	if err := r.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: in.Spec.Project}, &proj); err != nil {
		if kerrors.IsNotFound(err) {
			r.log().LogAttrs(ctx, slog.LevelWarn, "intent's project is gone; the intent waits",
				slog.String("intent", in.Name), slog.String("project", in.Spec.Project))
			return ctrl.Result{RequeueAfter: settings.PollInterval}, nil
		}
		return ctrl.Result{}, err
	}
	p := &pass{r: r, set: settings, in: &in, proj: &proj, now: r.now()}
	res, err := p.run(ctx)
	if errors.Is(err, errConflict) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("intent %s: %w", in.Name, err)
	}
	return res, nil
}

// run is the pass: the first status, the usage, then either an ended
// intent's hand-off or the active intent's poll and phase step, and last the
// status comment.
func (p *pass) run(ctx context.Context) (ctrl.Result, error) {
	if p.in.Status.Phase == "" {
		return ctrl.Result{}, p.setPhase(ctx, v1alpha1.IntentPending, nil)
	}
	if err := p.loadRuns(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if changed, err := p.syncUsage(ctx); changed || err != nil {
		return ctrl.Result{}, err
	}
	if terminal(p.in.Status.Phase) {
		if p.r.Nudger.take(p.in.Name) {
			if stop, err := p.handOff(ctx); stop || err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, p.syncStatusComment(ctx)
	}

	if wait := p.pollWait(); wait <= 0 {
		stop, err := p.poll(ctx)
		if err != nil || stop {
			return ctrl.Result{}, err
		}
	}
	stop, err := p.step(ctx)
	if err != nil || stop {
		return ctrl.Result{}, err
	}
	if err := p.syncStatusComment(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: max(time.Second, p.nextWake())}, nil
}

// step does the phase's own work, the part driven by the resources rather
// than by a human.
func (p *pass) step(ctx context.Context) (bool, error) {
	switch p.in.Status.Phase {
	case v1alpha1.IntentPending:
		return p.pending(ctx)
	case v1alpha1.IntentPlanning:
		return p.planning(ctx)
	case v1alpha1.IntentBuilding:
		return p.building(ctx)
	case v1alpha1.IntentInReview:
		return p.review(ctx)
	case v1alpha1.IntentBlocked:
		return p.blocked(ctx)
	}
	return false, nil
}

// pollInterval is how often the intent's phase polls its issue.
func (p *pass) pollInterval() time.Duration {
	if p.in.Status.Phase == v1alpha1.IntentAwaitingApproval {
		return p.set.ApprovalPollInterval
	}
	return p.set.PollInterval
}

// pollWait is how long until the issue poll is due; zero or less means now.
func (p *pass) pollWait() time.Duration {
	var last time.Time
	p.r.memo(func() { last = p.r.polled[p.in.Name] })
	if last.IsZero() {
		return 0
	}
	return last.Add(p.pollInterval()).Sub(p.now)
}

// nextWake is how long until the next poll this intent owes.
func (p *pass) nextWake() time.Duration {
	wake := p.pollWait()
	if wake <= 0 {
		wake = p.pollInterval()
	}
	if p.in.Status.Phase == v1alpha1.IntentInReview {
		var last time.Time
		p.r.memo(func() { last = p.r.prPolled[p.in.Name] })
		if pr := last.Add(p.set.PRPollInterval).Sub(p.now); pr > 0 && pr < wake {
			wake = pr
		}
	}
	return wake
}

// update writes the Intent's status as mutate leaves it, over the version
// this pass read: a newer version is errConflict, never overwritten. On
// success the pass holds the written Intent.
func (p *pass) update(ctx context.Context, mutate func(*v1alpha1.Intent) error) error {
	cur := p.in.DeepCopy()
	if err := mutate(cur); err != nil {
		return err
	}
	cur.Status.ObservedGeneration = cur.Generation
	if err := p.r.Status().Update(ctx, cur); err != nil {
		if kerrors.IsConflict(err) {
			return errConflict
		}
		return err
	}
	p.in = cur
	return nil
}

// setPhase moves the Intent to phase to, with mutate's other changes, in one
// status write.
func (p *pass) setPhase(ctx context.Context, to v1alpha1.IntentPhase, mutate func(*v1alpha1.Intent)) error {
	from := p.in.Status.Phase
	err := p.update(ctx, func(cur *v1alpha1.Intent) error {
		if mutate != nil {
			mutate(cur)
		}
		return v1alpha1.SetIntentPhase(cur, to, p.now)
	})
	if err == nil && from != to {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "intent phase",
			slog.String("intent", p.in.Name), slog.String("from", string(from)), slog.String("to", string(to)))
	}
	return err
}

// enteredAt is when the Intent last entered its current phase.
func (p *pass) enteredAt() time.Time {
	for i := len(p.in.Status.PhaseTimes) - 1; i >= 0; i-- {
		if pt := p.in.Status.PhaseTimes[i]; pt.Phase == p.in.Status.Phase {
			return pt.At.Time
		}
	}
	return p.in.CreationTimestamp.Time
}

// setCondition sets one condition on cur.
func setCondition(cur *v1alpha1.Intent, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: cur.Generation,
	})
}

// loadRuns lists this Intent's runs, by its UID: a run a same-named earlier
// Intent left terminating is not this one's.
func (p *pass) loadRuns(ctx context.Context) error {
	var list v1alpha1.IntentRunList
	if err := p.r.List(ctx, &list, client.InNamespace(p.in.Namespace),
		client.MatchingLabels{v1alpha1.LabelIntent: p.in.Name}); err != nil {
		return err
	}
	p.runs = p.runs[:0]
	for i := range list.Items {
		if list.Items[i].Spec.IntentRef.UID == p.in.UID {
			p.runs = append(p.runs, &list.Items[i])
		}
	}
	slices.SortFunc(p.runs, func(a, b *v1alpha1.IntentRun) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	return nil
}

// syncUsage sums every run's reported usage onto the Intent.
func (p *pass) syncUsage(ctx context.Context) (bool, error) {
	var u v1alpha1.IntentUsage
	for _, run := range p.runs {
		s := run.Status.Usage
		u.CostMicroUSD += microUSD(s.CostUSD)
		u.InputTokens += s.InputTokens
		u.OutputTokens += s.OutputTokens
		u.CacheReadTokens += s.CacheReadTokens
		u.CacheCreationTokens += s.CacheCreationTokens
	}
	if u == p.in.Status.Usage {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Usage = u
		return nil
	})
}

// SetupWithManager registers the intent reconciler: Intents, their runs (a
// run's progress is an Intent's), Project changes (limits and suspension),
// and discovery's hand-offs.
func (r *IntentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.Intent{}, projectIndex,
		func(obj client.Object) []string {
			return []string{obj.(*v1alpha1.Intent).Spec.Project}
		}); err != nil {
		return fmt.Errorf("index %s: %w", projectIndex, err)
	}
	mapRun := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		name := obj.GetLabels()[v1alpha1.LabelIntent]
		if name == "" {
			return nil
		}
		return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
	})
	mapProject := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		var intents v1alpha1.IntentList
		if err := mgr.GetClient().List(ctx, &intents, client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{projectIndex: obj.GetName()}); err != nil {
			return nil
		}
		out := make([]ctrl.Request, 0, len(intents.Items))
		for i := range intents.Items {
			if !terminal(intents.Items[i].Status.Phase) {
				out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&intents.Items[i])})
			}
		}
		return out
	})
	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Intent{}).
		Watches(&v1alpha1.IntentRun{}, mapRun).
		Watches(&v1alpha1.Project{}, mapProject).
		Named("intent")
	if r.Nudger != nil {
		b = b.WatchesRawSource(r.Nudger.source())
	}
	return b.Complete(r)
}
