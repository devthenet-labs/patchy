// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// Project Ready reasons this controller sets beside the API's own.
const (
	// ReasonUnsupportedRepositories: slice 1 builds in exactly one
	// repository, and the Project lists another number.
	ReasonUnsupportedRepositories = "UnsupportedRepositories"
	// ReasonValidated: every check passed.
	ReasonValidated = "Validated"
	// ReasonNoConflict: no trigger-labelled issue's name is held elsewhere.
	ReasonNoConflict = "NoConflict"
	// ReasonNameHeld: an issue's Intent name is held by another repository's
	// issue.
	ReasonNameHeld = "NameHeld"
	// ReasonForgeSecretUnreadable: the covering Forge's credential Secret
	// cannot be read: missing, or not among the Secrets intent-controller's
	// RBAC names (secrets get is restricted by resourceNames).
	ReasonForgeSecretUnreadable = "ForgeSecretUnreadable"
)

// revalidateEvery re-checks a Ready Project's forge, installation and labels
// this often; a Project that is not Ready is re-checked once per poll
// interval, never more often, so a fix (the App installed) is picked up
// within one while a broken installation costs one check per interval. A
// change to the Project or to any Forge re-checks it on the next pass.
const revalidateEvery = 10 * time.Minute

// Label colors for the labels patchy creates; a human may restyle them.
const (
	triggerLabelColor = "5319e7"
	approveLabelColor = "0e8a16"
)

// maxConflictsNamed bounds the issues the IntentNameConflict message names.
const maxConflictsNamed = 5

// ProjectReconciler validates Projects and discovers their intents: see the
// package doc.
type ProjectReconciler struct {
	client.Client
	// APIReader reads the Intent a create met, uncached.
	APIReader client.Reader
	GitHub    GitHub
	Settings  Settings
	// Nudger hands a trigger on an ended Intent's issue to that Intent.
	Nudger *Nudger
	Now    func() time.Time
	Log    *slog.Logger

	mu    sync.Mutex
	polls map[string]*projectPoll
	// forgeChanges counts the Forge changes the watch has seen: a Project
	// validated before the latest is validated again on its next pass.
	forgeChanges atomic.Int64
}

// projectPoll is one Project's discovery state, in memory: a restart
// re-lists in full, which only costs the requests a 304 would have saved.
type projectPoll struct {
	etag   string
	issues []*ghclient.Issue
	// waiting are the issues a full listing could not act on yet (the
	// Project at its active limit, a name held or still terminating),
	// retried on every poll, 304 or not.
	waiting map[int]bool
	// conflicts are the issues whose Intent name another repository holds.
	conflicts map[int]string
	// polledAt is when discovery last listed the intent repository;
	// attemptedAt when a poll was last due, whether it listed or was
	// skipped (the Project not Ready or suspended, or the rate budget
	// under the floor). The next pass is paced from attemptedAt, so a
	// skipped poll waits a whole interval rather than retrying at once.
	polledAt    time.Time
	attemptedAt time.Time
	// validatedGen/validatedAt/validatedForges are the Project generation,
	// time and Forge change count the Project was last validated at.
	validatedGen    int64
	validatedAt     time.Time
	validatedForges int64
}

func (r *ProjectReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ProjectReconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (r *ProjectReconciler) poll(name string) *projectPoll {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.polls == nil {
		r.polls = map[string]*projectPoll{}
	}
	p, ok := r.polls[name]
	if !ok {
		p = &projectPoll{waiting: map[int]bool{}, conflicts: map[int]string{}}
		r.polls[name] = p
	}
	return p
}

// Reconcile validates one Project and, when it is Ready and not suspended
// and a poll is due, discovers its new intents.
func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	settings := r.Settings.withDefaults()
	var p v1alpha1.Project
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		if kerrors.IsNotFound(err) {
			r.mu.Lock()
			delete(r.polls, req.Name)
			r.mu.Unlock()
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	poll := r.poll(p.Name)
	cur := p.DeepCopy()
	now := r.now()

	if err := r.revalidate(ctx, &p, cur, poll, now, settings.PollInterval); err != nil {
		return ctrl.Result{}, fmt.Errorf("project %s: validate: %w", p.Name, err)
	}
	cur.Status.ObservedGeneration = p.Generation

	var intents v1alpha1.IntentList
	if err := r.APIReader.List(ctx, &intents, client.InNamespace(p.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	cur.Status.ActiveIntents = activeIntents(&intents, p.Name)

	due := poll.attemptedAt.IsZero() || now.Sub(poll.attemptedAt) >= settings.PollInterval
	if due {
		poll.attemptedAt = now
	}
	if meta.IsStatusConditionTrue(cur.Status.Conditions, v1alpha1.ConditionReady) && !p.Spec.Suspend && due {
		polled, created, err := r.discover(ctx, &p, poll, &intents, settings)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("project %s: discover: %w", p.Name, err)
		}
		if polled {
			at := metav1.NewTime(now)
			cur.Status.LastPolledAt = &at
			poll.polledAt = now
		}
		// The list predates the Intents this pass created.
		cur.Status.ActiveIntents += created
		setConflict(cur, poll)
	}

	if !equality.Semantic.DeepEqual(p.Status, cur.Status) {
		if err := r.Status().Update(ctx, cur); err != nil {
			if kerrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	// Paced from the last due pass, skipped or not: a Project that cannot
	// poll (not Ready, suspended, under the rate floor) is looked at once
	// per interval, never in a loop.
	next := max(time.Second, poll.attemptedAt.Add(settings.PollInterval).Sub(now))
	return ctrl.Result{RequeueAfter: next}, nil
}

// revalidate validates the Project into cur's Ready condition when a check is
// due: never checked by this process, the Project or a Forge changed since,
// revalidateEvery passed, or, for a Project that is not Ready, a poll
// interval.
func (r *ProjectReconciler) revalidate(ctx context.Context, p, cur *v1alpha1.Project, poll *projectPoll,
	now time.Time, interval time.Duration) error {
	ready := meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady)
	forges := r.forgeChanges.Load()
	since := now.Sub(poll.validatedAt)
	if !poll.validatedAt.IsZero() && poll.validatedGen == p.Generation && poll.validatedForges == forges &&
		since < revalidateEvery && (ready || since < interval) {
		return nil
	}
	cond, err := r.validate(ctx, p)
	if err != nil {
		return err
	}
	cond.ObservedGeneration = p.Generation
	meta.SetStatusCondition(&cur.Status.Conditions, cond)
	poll.validatedGen, poll.validatedAt, poll.validatedForges = p.Generation, now, forges
	return nil
}

// validate checks the Project and returns its Ready condition. An error is a
// transient failure to find out; a check that fails is a False condition.
func (r *ProjectReconciler) validate(ctx context.Context, p *v1alpha1.Project) (metav1.Condition, error) {
	notReady := func(reason, format string, args ...any) (metav1.Condition, error) {
		return metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse,
			Reason: reason, Message: fmt.Sprintf(format, args...)}, nil
	}
	if n := len(p.Spec.Repositories); n != 1 {
		return notReady(ReasonUnsupportedRepositories,
			"intents build in exactly one repository for now, and this Project lists %d", n)
	}
	trigger := v1alpha1.ProjectTriggerLabel(p)
	var projects v1alpha1.ProjectList
	if err := r.List(ctx, &projects, client.InNamespace(p.Namespace)); err != nil {
		return metav1.Condition{}, err
	}
	for i := range projects.Items {
		o := &projects.Items[i]
		if o.Name != p.Name && sameRepo(o.Spec.IntentRepository, p.Spec.IntentRepository) &&
			strings.EqualFold(v1alpha1.ProjectTriggerLabel(o), trigger) {
			return notReady(v1alpha1.ReasonAmbiguousIntentRepository,
				"Project %s uses the same intent repository and trigger label %q, so an issue could belong to either",
				o.Name, trigger)
		}
	}
	app := p.Spec.Repositories[0].URL
	for _, u := range []string{p.Spec.IntentRepository, app} {
		if _, _, err := forge.ParseRepoURL(u); err != nil {
			return notReady(v1alpha1.ReasonForgeUnresolved, "%v", err)
		}
		if err := r.GitHub.Resolve(ctx, u); err != nil {
			if forgeUnresolved(err) {
				return notReady(v1alpha1.ReasonForgeUnresolved, "%s: %v", u, err)
			}
			return metav1.Condition{}, err
		}
	}
	// The permissions intents use on each repository, each proven by
	// minting the scoped token itself.
	for _, check := range []struct {
		url   string
		perms ghclient.TokenPerms
		what  string
	}{
		{p.Spec.IntentRepository, issuesWrite, "issues: write"},
		{app, contentsWrite, "contents: write"},
		{app, pullsWrite, "pull requests: write"},
	} {
		if err := r.GitHub.Installed(ctx, check.url, check.perms); err != nil {
			if secretUnreadable(err) {
				return notReady(ReasonForgeSecretUnreadable, forgeSecretMessage, check.url, err)
			}
			if installationRefused(err) {
				return notReady(v1alpha1.ReasonAppNotInstalled,
					"the App cannot act on %s with %s: %v", check.url, check.what, err)
			}
			return metav1.Condition{}, err
		}
	}
	for _, l := range []struct{ name, color, description string }{
		{trigger, triggerLabelColor, "patchy: plan and build this issue in project " + p.Name},
		{approveLabel(p), approveLabelColor, "patchy: approve the posted plan"},
	} {
		if err := r.GitHub.EnsureLabel(ctx, p.Spec.IntentRepository, l.name, l.color, l.description); err != nil {
			if secretUnreadable(err) {
				return notReady(ReasonForgeSecretUnreadable, forgeSecretMessage, p.Spec.IntentRepository, err)
			}
			if installationRefused(err) {
				return notReady(v1alpha1.ReasonAppNotInstalled,
					"the label %q cannot be created on %s: %v", l.name, p.Spec.IntentRepository, err)
			}
			return metav1.Condition{}, err
		}
	}
	return metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonValidated,
		Message: "the repositories resolve, the App is installed on them, and the labels exist"}, nil
}

// forgeSecretMessage explains a ForgeSecretUnreadable Project: the
// repository, then the error, which names the Secret.
const forgeSecretMessage = "the credential Secret of the Forge covering %s cannot be read (%v); " +
	"intent-controller reads only the Secrets its RBAC names: add it to intentController.forgeSecrets " +
	"(Helm) or to the component's rbac.yaml (kustomize)"

// secretUnreadable reports a Forge credential Secret the API server will not
// hand over: missing, or not one intent-controller may read (its secrets
// get is restricted by resourceNames). Not a transient failure, so the
// Project reports it rather than retrying in the dark.
func secretUnreadable(err error) bool {
	return kerrors.IsForbidden(err) || kerrors.IsNotFound(err)
}

// discover polls the intent repository for open trigger-labelled issues and
// creates an Intent for each new one, returning how many it created. polled
// is false when the installation's rate budget is under the floor and the
// poll was skipped.
func (r *ProjectReconciler) discover(ctx context.Context, p *v1alpha1.Project, poll *projectPoll,
	intents *v1alpha1.IntentList, settings Settings) (polled bool, created int32, err error) {
	repo := p.Spec.IntentRepository
	if settings.RateLimitFloor > 0 {
		remaining, err := r.GitHub.RateRemaining(ctx, repo)
		if err != nil {
			return false, 0, err
		}
		if remaining < settings.RateLimitFloor {
			r.log().LogAttrs(ctx, slog.LevelWarn, "installation rate budget under the floor; discovery paused",
				slog.String("project", p.Name), slog.Int("remaining", remaining),
				slog.Int("floor", settings.RateLimitFloor))
			return false, 0, nil
		}
	}
	trigger := v1alpha1.ProjectTriggerLabel(p)
	otherTriggers, err := r.otherTriggers(ctx, p)
	if err != nil {
		return false, 0, err
	}
	list, err := r.GitHub.ListIssues(ctx, repo, []string{trigger}, poll.etag)
	if err != nil {
		return false, 0, err
	}
	full := !list.NotModified
	issues := poll.issues
	if full {
		// Every issue of a new listing waits until it is settled: a
		// failure part way through leaves the rest waiting, so the next
		// poll retries them even when GitHub answers 304.
		poll.etag, poll.issues = list.ETag, list.Issues
		poll.waiting, poll.conflicts = map[int]bool{}, map[int]string{}
		for _, is := range list.Issues {
			poll.waiting[is.Number] = true
		}
		issues = list.Issues
	} else {
		issues = slices.DeleteFunc(slices.Clone(issues), func(is *ghclient.Issue) bool { return !poll.waiting[is.Number] })
	}

	byName := make(map[string]*v1alpha1.Intent, len(intents.Items))
	byIssue := make(map[int64]*v1alpha1.Intent, len(intents.Items))
	for i := range intents.Items {
		in := &intents.Items[i]
		byName[in.Name] = in
		if sameRepo(in.Spec.Issue.Repository, repo) {
			byIssue[in.Spec.Issue.Number] = in
		}
	}
	active := activeIntents(intents, p.Name)
	for _, is := range issues {
		delete(poll.conflicts, is.Number)
		if is.Number < 1 || int64(is.Number) > v1alpha1.MaxIntentIssueNumber {
			delete(poll.waiting, is.Number)
			continue
		}
		name := v1alpha1.IntentName(p.Name, int64(is.Number))
		if existing := byName[name]; existing != nil {
			delete(poll.waiting, is.Number)
			r.existing(ctx, p, poll, existing, is, full)
			continue
		}
		if conflict, err := r.sharedIssue(ctx, p, poll, is, byIssue[int64(is.Number)], otherTriggers); err != nil {
			return false, created, err
		} else if conflict {
			continue
		}
		if active >= maxActiveIntents(p) {
			continue // waits
		}
		ok, err := r.create(ctx, p, poll, is, name)
		if err != nil {
			return false, created, err
		}
		if ok {
			active++
			created++
		}
	}
	return true, created, nil
}

// otherTriggers are the labels of Projects sharing this Project's intent
// repository. A single issue may carry only one of them.
func (r *ProjectReconciler) otherTriggers(ctx context.Context, p *v1alpha1.Project) ([]string, error) {
	var projects v1alpha1.ProjectList
	if err := r.APIReader.List(ctx, &projects, client.InNamespace(p.Namespace)); err != nil {
		return nil, err
	}
	var out []string
	for i := range projects.Items {
		other := &projects.Items[i]
		if other.Name != p.Name && sameRepo(other.Spec.IntentRepository, p.Spec.IntentRepository) {
			out = append(out, v1alpha1.ProjectTriggerLabel(other))
		}
	}
	return out, nil
}

// sharedIssue prevents two Projects from creating Intents for one issue.
// A second trigger on an already claimed issue is removed so it cannot
// start unexpectedly after the first Intent's TTL; two triggers seen before
// either claim wait for a human to choose one.
func (r *ProjectReconciler) sharedIssue(ctx context.Context, p *v1alpha1.Project, poll *projectPoll,
	is *ghclient.Issue, owner *v1alpha1.Intent, otherTriggers []string) (bool, error) {
	if owner != nil {
		poll.conflicts[is.Number] = fmt.Sprintf("Intent %s already owns this issue", owner.Name)
		if err := r.GitHub.RemoveLabel(ctx, p.Spec.IntentRepository, int64(is.Number),
			v1alpha1.ProjectTriggerLabel(p)); err != nil {
			return true, fmt.Errorf("remove the competing trigger from issue #%d: %w", is.Number, err)
		}
		delete(poll.waiting, is.Number)
		return true, nil
	}
	for _, other := range otherTriggers {
		if hasLabel(is, other) {
			poll.conflicts[is.Number] = fmt.Sprintf("multiple Project trigger labels (%s and %s)",
				v1alpha1.ProjectTriggerLabel(p), other)
			return true, nil
		}
	}
	return false, nil
}

// existing handles a trigger-labelled issue whose Intent name is taken: by
// this issue's own Intent (an ended one is nudged, on a full listing, to
// answer the trigger), by one still terminating (the issue waits), or by
// another repository's issue (a conflict, reported).
func (r *ProjectReconciler) existing(ctx context.Context, p *v1alpha1.Project, poll *projectPoll,
	in *v1alpha1.Intent, is *ghclient.Issue, full bool) {
	switch {
	case !sameRepo(in.Spec.Issue.Repository, p.Spec.IntentRepository):
		poll.waiting[is.Number] = true
		poll.conflicts[is.Number] = "Intent " + in.Name
		r.log().LogAttrs(ctx, slog.LevelWarn, "intent name held by another repository's issue",
			slog.String("project", p.Name), slog.Int("issue", is.Number), slog.String("intent", in.Name),
			slog.String("held_by", in.Spec.Issue.Repository))
	case !in.DeletionTimestamp.IsZero():
		poll.waiting[is.Number] = true
	case terminal(in.Status.Phase) && full && r.Nudger != nil:
		r.Nudger.Nudge(in.Namespace, in.Name)
	}
}

// create creates the Intent for a trigger-labelled issue with no Intent,
// when its trigger was applied since the issue was last closed. created is
// false when the issue has no such trigger, or its name turned out taken.
func (r *ProjectReconciler) create(ctx context.Context, p *v1alpha1.Project, poll *projectPoll,
	is *ghclient.Issue, name string) (bool, error) {
	events, err := r.GitHub.ListIssueEvents(ctx, p.Spec.IntentRepository, int64(is.Number))
	if err != nil {
		return false, err
	}
	trigger := triggerSinceClose(events, v1alpha1.ProjectTriggerLabel(p))
	if trigger == nil {
		// A reopened issue still carrying the label of an intent that ended:
		// it becomes an intent again only once someone applies the label
		// after reopening it, which changes the listing.
		delete(poll.waiting, is.Number)
		return false, nil
	}
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace},
		Spec: v1alpha1.IntentSpec{
			Project: p.Name,
			Issue: v1alpha1.IntentIssue{
				Repository: p.Spec.IntentRepository,
				Number:     int64(is.Number),
				URL:        is.HTMLURL,
			},
			RequestedBy: v1alpha1.IntentRequest{
				Login:   trigger.Actor.Login,
				At:      metav1.NewTime(trigger.CreatedAt),
				EventID: trigger.ID,
			},
		},
	}
	err = r.Create(ctx, in)
	if err == nil {
		delete(poll.waiting, is.Number)
		r.log().LogAttrs(ctx, slog.LevelInfo, "intent discovered",
			slog.String("project", p.Name), slog.String("intent", name), slog.Int("issue", is.Number),
			slog.String("requested_by", trigger.Actor.Login))
		return true, nil
	}
	if !kerrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create intent %s: %w", name, err)
	}
	var held v1alpha1.Intent
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: name}, &held); err != nil {
		if kerrors.IsNotFound(err) {
			return false, nil // deleted meanwhile: it waits, and the next poll creates it
		}
		return false, err
	}
	delete(poll.waiting, is.Number)
	r.existing(ctx, p, poll, &held, is, false)
	return false, nil
}

// triggerSinceClose is the newest labeled event for the trigger label that
// is newer than the issue's newest closed event, or nil. Discovery counts
// only a trigger applied since the issue was last closed.
func triggerSinceClose(events []*ghclient.IssueEvent, trigger string) *ghclient.IssueEvent {
	var labeled, closed *ghclient.IssueEvent
	for _, ev := range events {
		switch {
		case ev.Event == "labeled" && strings.EqualFold(ev.Label, trigger):
			if labeled == nil || !ev.CreatedAt.Before(labeled.CreatedAt) {
				labeled = ev
			}
		case ev.Event == "closed":
			if closed == nil || !ev.CreatedAt.Before(closed.CreatedAt) {
				closed = ev
			}
		}
	}
	if labeled == nil || labeled.ID < 1 || labeled.Actor.Login == "" {
		return nil
	}
	if closed != nil && !labeled.CreatedAt.After(closed.CreatedAt) {
		return nil
	}
	return labeled
}

// activeIntents counts the Project's non-terminal Intents.
func activeIntents(intents *v1alpha1.IntentList, project string) int32 {
	var n int32
	for i := range intents.Items {
		in := &intents.Items[i]
		if in.Spec.Project == project && in.DeletionTimestamp.IsZero() && !terminal(in.Status.Phase) {
			n++
		}
	}
	return n
}

// setConflict writes the IntentNameConflict condition from poll.
func setConflict(p *v1alpha1.Project, poll *projectPoll) {
	if len(poll.conflicts) == 0 {
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionIntentNameConflict, Status: metav1.ConditionFalse, Reason: ReasonNoConflict,
			Message: "no trigger-labelled issue's intent name is held elsewhere", ObservedGeneration: p.Generation,
		})
		return
	}
	issues := make([]int, 0, len(poll.conflicts))
	for n := range poll.conflicts {
		issues = append(issues, n)
	}
	slices.Sort(issues)
	var named []string
	for _, n := range issues[:min(len(issues), maxConflictsNamed)] {
		named = append(named, fmt.Sprintf("issue #%d (%s)", n, poll.conflicts[n]))
	}
	msg := fmt.Sprintf("the Project cannot discover %s; leave one Project trigger label per issue, or use a new "+
		"issue when another Intent owns it", strings.Join(named, ", "))
	if len(issues) > maxConflictsNamed {
		msg += fmt.Sprintf(", and %d more", len(issues)-maxConflictsNamed)
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionIntentNameConflict, Status: metav1.ConditionTrue, Reason: ReasonNameHeld,
		Message: msg, ObservedGeneration: p.Generation,
	})
}

// SetupWithManager registers the project reconciler; a Forge change re-queues
// every Project, since resolution may have changed.
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapForge := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		r.forgeChanges.Add(1)
		var projects v1alpha1.ProjectList
		if err := mgr.GetClient().List(ctx, &projects, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		out := make([]ctrl.Request, 0, len(projects.Items))
		for i := range projects.Items {
			out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&projects.Items[i])})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Project{}).
		Watches(&v1alpha1.Forge{}, mapForge, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("intent-project").
		Complete(r)
}
