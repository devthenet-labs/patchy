// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/resourceclass"
)

// ResourcesUnavailable reasons (an Intent's block) and the
// ResourceClassesResolved reasons (a Project's warning).
const (
	// ReasonUnknownResourceClass: the Project picks a resource class
	// intent-controller does not define. On an Intent, a run of that
	// repository waits Pending; on a Project, the condition names each such
	// repository.
	ReasonUnknownResourceClass = "UnknownResourceClass"
	// ReasonUnschedulable: a run's agent pod stayed unschedulable past
	// UnschedulableGrace, and the run was stopped.
	ReasonUnschedulable = "Unschedulable"
	// ReasonResourceClassesDefined: every class the Project picks is
	// defined.
	ReasonResourceClassesDefined = "Defined"
)

// UnschedulableGrace is how long a run's agent pod may stay unschedulable
// (no node fits its requests, and none that does has been added) before the
// run is stopped: long enough for a node autoscaler such as Karpenter or EKS
// Auto Mode to launch a node that fits, which takes a minute or two, short
// of the Job's deadline (90 minutes by default) a pod no node will ever fit
// would otherwise wait out while holding a slot. unschedulablePoll paces the
// looks at a pod that has not been scheduled yet: neither its scheduling nor
// its failing to be ever changes the Job, so a Job watch never sees it.
const (
	UnschedulableGrace = 10 * time.Minute
	unschedulablePoll  = 30 * time.Second
)

// classOf is the resource class proj picks for run's repository: "" for a
// plan run (plans always run on the default resources), and for a run whose
// repository picks none or that the Project no longer lists.
func classOf(proj *v1alpha1.Project, run *v1alpha1.IntentRun) string {
	if run.Spec.Stage != v1alpha1.IntentStageBuild && run.Spec.Stage != v1alpha1.IntentStageRevise {
		return ""
	}
	for _, repo := range proj.Spec.Repositories {
		if sameRepo(repo.URL, run.Spec.Repository.URL) {
			return repo.AgentResourceClass
		}
	}
	return ""
}

// runResources resolves the CPU and memory a run's Job gets: nil, the
// controller's default, when classOf picks none; the class's when it picks
// one the controller defines; and, when it picks one it does not, nil with
// unknown naming it, the run then waiting rather than launching.
func runResources(classes resourceclass.Set, proj *v1alpha1.Project, run *v1alpha1.IntentRun) (
	res *jobs.Resources, unknown string) {
	name := classOf(proj, run)
	if name == "" {
		return nil, ""
	}
	class, ok := classes.Lookup(name)
	if !ok {
		return nil, name
	}
	cpuReq, memReq, cpuLim, memLim := class.Strings()
	return &jobs.Resources{Class: name, CPURequest: cpuReq, MemoryRequest: memReq, CPULimit: cpuLim,
		MemoryLimit: memLim}, ""
}

// waitsOnClass reports a run that waits on a class the controller does not
// define: a build or revise run not yet launched (no Job), not settled,
// whose repository's class is unknown.
func waitsOnClass(classes resourceclass.Set, proj *v1alpha1.Project, run *v1alpha1.IntentRun) string {
	if run.Status.JobRef != nil || run.Status.Phase == v1alpha1.RunComplete ||
		run.Status.Phase == v1alpha1.RunFailed || !run.DeletionTimestamp.IsZero() {
		return ""
	}
	_, unknown := runResources(classes, proj, run)
	return unknown
}

// classWait is the first of runs that waits on an unknown class, and the
// class, or nil.
func (p *pass) classWait(runs []*v1alpha1.IntentRun) (*v1alpha1.IntentRun, string) {
	for _, run := range runs {
		if name := waitsOnClass(p.r.Classes, p.proj, run); name != "" {
			return run, name
		}
	}
	return nil, ""
}

// blockOnClass blocks the Intent when one of runs waits on an unknown class:
// that run stays Pending, holding no slot and spending no attempt (the run
// reconciler never grants it), while the other repositories' runs go on.
// The block lifts once no run waits (resourceBlockHolds). blocked is false,
// and nothing is written, when no run waits.
func (p *pass) blockOnClass(ctx context.Context, runs []*v1alpha1.IntentRun) (blocked bool, err error) {
	run, name := p.classWait(runs)
	if run == nil {
		return false, nil
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "a run waits on a resource class intent-controller does not define",
		slog.String("intent", p.in.Name), slog.String("run", run.Name), slog.String("class", name))
	return true, p.block(ctx, v1alpha1.ConditionResourcesUnavailable, ReasonUnknownResourceClass,
		fmt.Sprintf("the %s%s waits: the project picks the resource class %q for it, and intent-controller "+
			"defines no class of that name (defined: %s). Define it in the patchy chart's agent.resources.classes, "+
			"or change the repository's agentResourceClass on the project; the run launches once its class is "+
			"known, and no attempt is spent meanwhile",
			stageNoun(run), p.inRepository(run.Spec.Repository.URL), name, p.r.Classes.Describe()))
}

// stageNoun names a run's stage in a message: the build, the revise round,
// or the check-fix round.
func stageNoun(run *v1alpha1.IntentRun) string {
	switch {
	case run.Spec.Stage == v1alpha1.IntentStagePlan:
		return "plan"
	case run.Spec.Stage == v1alpha1.IntentStageRevise && run.Spec.Trigger == v1alpha1.IntentRunTriggerChecks:
		return "check-fix round"
	case run.Spec.Stage == v1alpha1.IntentStageRevise:
		return "revise round"
	}
	return "build"
}

// resourcesBlocked reports a failed run whose agent pod no node could fit:
// it blocks its Intent (ResourcesUnavailable, Unschedulable) rather than
// spend another attempt on a pod that would wait the same way.
func resourcesBlocked(run *v1alpha1.IntentRun) bool {
	return run.Status.Phase == v1alpha1.RunFailed && run.Status.Outcome == OutcomeUnschedulable
}

// blockUnschedulable blocks the Intent on a resourcesBlocked run, saying
// what lifts the block. The message reaches the intent issue's status
// comment, so the scheduler's words (which describe the cluster's nodes)
// stay on the run's detail, which it names.
func (p *pass) blockUnschedulable(ctx context.Context, run *v1alpha1.IntentRun) error {
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "a run's agent pod could not be scheduled; the intent is held",
		slog.String("intent", p.in.Name), slog.String("run", run.Name), slog.String("detail", run.Status.Detail))
	return p.block(ctx, v1alpha1.ConditionResourcesUnavailable, ReasonUnschedulable,
		fmt.Sprintf("the %s%s could not be scheduled: no node could fit its agent pod within %s, so it was "+
			"stopped before it ran, and no attempt was spent (run %s records what it asked for and the scheduler's "+
			"reason). Make room for it (nodes large enough for its requests) or pick a smaller resource class, then "+
			"update the project to retry; restarting intent-controller (a chart upgrade that changes the classes "+
			"does) retries it too", stageNoun(run), p.inRepository(run.Spec.Repository.URL), UnschedulableGrace,
			run.Name))
}

// resourceBlockHolds reports a ResourcesUnavailable block still in force: an
// unknown class while any run of the Intent still waits on one; an
// unschedulable pod until the Project changes or this process restarts
// (which may come with new classes), when the next attempt is made.
func (p *pass) resourceBlockHolds() bool {
	c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionResourcesUnavailable)
	if c == nil || c.Status != metav1.ConditionTrue {
		return false
	}
	switch c.Reason {
	case ReasonUnknownResourceClass:
		run, _ := p.classWait(p.runs)
		return run != nil
	case ReasonUnschedulable:
		var gen int64
		var known bool
		p.r.memo(func() { gen, known = p.r.blockedAt[p.in.Name] })
		return known && gen == p.proj.Generation
	}
	return false
}

// unschedulable judges a launched run's Job that has not finished: detail
// says why the run is to stop when its pod has stayed unschedulable past
// UnschedulableGrace, naming the scheduler's message and what the pod asked
// for; requeue is when to look again while its pod is not yet scheduled
// (nothing once it is, or once the agent started).
func unschedulable(st jobs.Status, now time.Time) (detail string, requeue time.Duration) {
	if st.Done || st.AgentStarted || st.Scheduled {
		return "", 0
	}
	if st.Unschedulable == "" {
		return "", unschedulablePoll
	}
	if waited := now.Sub(st.UnschedulableSince); waited < UnschedulableGrace {
		return "", min(UnschedulableGrace-waited, unschedulablePoll)
	}
	asked := st.Resources
	if st.ResourceClass != "" {
		asked = "resource class " + st.ResourceClass + ": " + asked
	}
	return fmt.Sprintf("no node could fit the agent pod for %s (it asked for %s): %s",
		UnschedulableGrace, asked, strings.TrimSpace(st.Unschedulable)), 0
}

// noResultDetail is base, the detail of a run whose agent reported no
// result, with why the agent stopped when the Job says (an OOM kill, an
// eviction, the deadline), and for an OOM kill what to change.
func noResultDetail(base string, st jobs.Status) string {
	why := st.Termination()
	if why == "" {
		return base
	}
	detail := base + ": " + why
	if st.AgentTerminated == "OOMKilled" {
		if st.ResourceClass != "" {
			detail += "; give the repository a resource class with more memory (agentResourceClass), or raise " +
				"the class's memory limit"
		} else {
			detail += "; pick a resource class with more memory for the repository (agentResourceClass), or raise " +
				"the chart's agent.resources.default memory limit"
		}
	}
	return detail
}

// resourceClassesCondition is the ResourceClassesResolved condition of a
// Project, given the classes defined, or nil when the Project picks no
// class (the condition is then absent).
func resourceClassesCondition(p *v1alpha1.Project, classes resourceclass.Set) *metav1.Condition {
	var picks, unknown []string
	for _, repo := range p.Spec.Repositories {
		if repo.AgentResourceClass == "" {
			continue
		}
		picks = append(picks, fmt.Sprintf("%s picks %s", repo.Name, repo.AgentResourceClass))
		if _, ok := classes.Lookup(repo.AgentResourceClass); !ok {
			unknown = append(unknown, fmt.Sprintf("repository %s (%s) picks %q", repo.Name, repo.URL,
				repo.AgentResourceClass))
		}
	}
	if len(picks) == 0 {
		return nil
	}
	if len(unknown) > 0 {
		return &metav1.Condition{Type: v1alpha1.ConditionResourceClassesResolved, Status: metav1.ConditionFalse,
			Reason: ReasonUnknownResourceClass, ObservedGeneration: p.Generation,
			Message: fmt.Sprintf("%s, which intent-controller does not define (defined: %s); the build, revise and "+
				"check-fix runs of each wait until the class is defined in the patchy chart's "+
				"agent.resources.classes or the pick is changed. Plans and the other repositories are unaffected",
				strings.Join(unknown, "; "), classes.Describe())}
	}
	return &metav1.Condition{Type: v1alpha1.ConditionResourceClassesResolved, Status: metav1.ConditionTrue,
		Reason: ReasonResourceClassesDefined, ObservedGeneration: p.Generation,
		Message: "every resource class the project picks is defined: " + strings.Join(picks, ", ")}
}

// setResourceClasses sets cur's ResourceClassesResolved condition, or
// removes it when the Project picks no class.
func setResourceClasses(cur *v1alpha1.Project, classes resourceclass.Set) {
	c := resourceClassesCondition(cur, classes)
	if c == nil {
		meta.RemoveStatusCondition(&cur.Status.Conditions, v1alpha1.ConditionResourceClassesResolved)
		return
	}
	meta.SetStatusCondition(&cur.Status.Conditions, *c)
}
