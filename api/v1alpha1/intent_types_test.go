// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// allIntentPhases is every phase of the enum, written out independently of
// the edge table so the tests below check the table against the design
// rather than against itself.
var allIntentPhases = []IntentPhase{
	IntentPending, IntentPlanning, IntentAwaitingApproval, IntentBuilding,
	IntentInReview, IntentRevising, IntentBlocked, IntentMerged,
	IntentClosed, IntentFailed,
}

// nonTerminalIntentPhases are the phases the design's "any non-terminal
// phase" edges leave from.
var nonTerminalIntentPhases = []IntentPhase{
	IntentPending, IntentPlanning, IntentAwaitingApproval, IntentBuilding,
	IntentInReview, IntentRevising, IntentBlocked,
}

// designIntentEdges is the design's edge list ("Intent phases"), spelled
// out edge by edge.
func designIntentEdges() map[[2]IntentPhase]bool {
	edges := map[[2]IntentPhase]bool{
		{"", IntentPending}:                      true, // creation
		{IntentPending, IntentPlanning}:          true, // approver's trigger
		{IntentPlanning, IntentAwaitingApproval}: true, // plan posted
		{IntentAwaitingApproval, IntentBuilding}: true, // approval accepted
		{IntentAwaitingApproval, IntentPlanning}: true, // replan
		{IntentBuilding, IntentInReview}:         true, // every PR open
		{IntentInReview, IntentRevising}:         true, // review/revise/check-fix round
		{IntentRevising, IntentInReview}:         true, // round done or failed
		{IntentInReview, IntentMerged}:           true, // every PR merged
		{IntentRevising, IntentMerged}:           true, // a human merged mid-round
		{IntentBlocked, IntentMerged}:            true, // a human merged while blocked
		{IntentPlanning, IntentFailed}:           true, // attempts exhausted / plan invalid twice
		{IntentBuilding, IntentFailed}:           true, // attempts exhausted
		{IntentFailed, IntentPlanning}:           true, // revival
		{IntentBlocked, IntentPending}:           true, // resume
		{IntentBlocked, IntentPlanning}:          true, // resume
		{IntentBlocked, IntentAwaitingApproval}:  true, // resume
		{IntentBlocked, IntentBuilding}:          true, // resume
		{IntentBlocked, IntentInReview}:          true, // resume
		{IntentBlocked, IntentRevising}:          true, // resume
		{IntentPending, IntentClosed}:            true, // non-approver trigger (and cancel)
	}
	for _, p := range nonTerminalIntentPhases {
		edges[[2]IntentPhase{p, IntentClosed}] = true // issue closed, /patchy cancel, PRs closed
		if p != IntentBlocked {
			edges[[2]IntentPhase{p, IntentBlocked}] = true // a limit or precondition
		}
	}
	for _, p := range allIntentPhases {
		edges[[2]IntentPhase{p, p}] = true // self-transitions are no-ops
	}
	return edges
}

// TestCanTransitionIntentMatchesDesign checks every (from, to) pair, the
// empty "new intent" phase included: exactly the design's edges are legal and
// everything else is refused.
func TestCanTransitionIntentMatchesDesign(t *testing.T) {
	want := designIntentEdges()
	froms := append([]IntentPhase{""}, allIntentPhases...)
	for _, from := range froms {
		for _, to := range allIntentPhases {
			if got, w := CanTransitionIntent(from, to), want[[2]IntentPhase{from, to}]; got != w {
				t.Errorf("CanTransitionIntent(%q, %q) = %v, want %v", from, to, got, w)
			}
		}
	}
}

func TestCanTransitionIntentExamples(t *testing.T) {
	cases := []struct {
		name string
		from IntentPhase
		to   IntentPhase
		want bool
	}{
		{"new intent is pending", "", IntentPending, true},
		{"new intent cannot skip to planning", "", IntentPlanning, false},
		{"planning cannot build without approval", IntentPlanning, IntentBuilding, false},
		{"pending cannot build without a plan", IntentPending, IntentBuilding, false},
		{"awaiting approval cannot open review", IntentAwaitingApproval, IntentInReview, false},
		{"building cannot merge before review", IntentBuilding, IntentMerged, false},
		{"planning cannot merge", IntentPlanning, IntentMerged, false},
		{"awaiting approval cannot merge", IntentAwaitingApproval, IntentMerged, false},
		{"a human merge mid-round completes the intent", IntentRevising, IntentMerged, true},
		{"a human merge while blocked completes the intent", IntentBlocked, IntentMerged, true},
		{"a failed round never fails the intent", IntentRevising, IntentFailed, false},
		{"review never fails the intent", IntentInReview, IntentFailed, false},
		{"merged is absorbing", IntentMerged, IntentPlanning, false},
		{"closed is absorbing", IntentClosed, IntentPlanning, false},
		{"failed revives only to planning", IntentFailed, IntentBuilding, false},
		{"failed is not closed again", IntentFailed, IntentClosed, false},
		{"failed is not blocked", IntentFailed, IntentBlocked, false},
		{"blocked cannot fail", IntentBlocked, IntentFailed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CanTransitionIntent(c.from, c.to); got != c.want {
				t.Errorf("CanTransitionIntent(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
			}
		})
	}
}

func TestIntentTerminal(t *testing.T) {
	want := map[IntentPhase]bool{IntentMerged: true, IntentClosed: true, IntentFailed: true}
	for _, p := range append([]IntentPhase{""}, allIntentPhases...) {
		if got := IntentTerminal(p); got != want[p] {
			t.Errorf("IntentTerminal(%q) = %v, want %v", p, got, want[p])
		}
	}
}

// TestIntentEdgeTableShape pins the table itself: every phase of the enum is
// a key, every edge targets a known phase, and exactly Merged and Closed are
// absorbing — a typo cannot orphan a phase or open an exit from a terminal.
func TestIntentEdgeTableShape(t *testing.T) {
	known := map[IntentPhase]bool{}
	for _, p := range allIntentPhases {
		known[p] = true
		if _, ok := intentTransitions[p]; !ok {
			t.Errorf("phase %q missing from the intent edge table", p)
		}
	}
	for from, tos := range intentTransitions {
		if from != "" && !known[from] {
			t.Errorf("intent edge table keys unknown phase %q", from)
		}
		for _, to := range tos {
			if !known[to] {
				t.Errorf("edge %q -> %q targets an unknown phase", from, to)
			}
		}
		absorbing := len(tos) == 0
		if want := from == IntentMerged || from == IntentClosed; absorbing != want {
			t.Errorf("phase %q absorbing = %v, want %v", from, absorbing, want)
		}
	}
}

// intentAt returns an Intent in phase p with no history.
func intentAt(p IntentPhase) *Intent {
	i := &Intent{}
	i.Status.Phase = p
	return i
}

func TestSetIntentPhaseLog(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	t.Run("creation appends the first phase time", func(t *testing.T) {
		i := &Intent{}
		if err := SetIntentPhase(i, IntentPending, now); err != nil {
			t.Fatalf("SetIntentPhase(new, Pending) error = %v", err)
		}
		want := []IntentPhaseTime{{Phase: IntentPending, At: metav1.NewTime(now)}}
		if i.Status.Phase != IntentPending || !reflect.DeepEqual(i.Status.PhaseTimes, want) {
			t.Errorf("status = phase %q times %v, want Pending with %v", i.Status.Phase, i.Status.PhaseTimes, want)
		}
		if i.Status.CompletedAt != nil {
			t.Errorf("completedAt = %v, want nil for a non-terminal phase", i.Status.CompletedAt)
		}
	})

	t.Run("illegal transition mutates nothing", func(t *testing.T) {
		i := intentAt(IntentPlanning)
		i.Status.PhaseTimes = []IntentPhaseTime{{Phase: IntentPlanning, At: metav1.NewTime(now)}}
		before := i.DeepCopy()
		if err := SetIntentPhase(i, IntentBuilding, now.Add(time.Minute)); err == nil {
			t.Fatal("SetIntentPhase(Planning, Building) error = nil, want an illegal-transition error")
		}
		if !reflect.DeepEqual(i, before) {
			t.Errorf("intent mutated on an illegal transition: %+v, want %+v", i.Status, before.Status)
		}
	})

	t.Run("self transition is a silent no-op", func(t *testing.T) {
		i := intentAt(IntentInReview)
		if err := SetIntentPhase(i, IntentInReview, now); err != nil {
			t.Fatalf("SetIntentPhase(InReview, InReview) error = %v", err)
		}
		if len(i.Status.PhaseTimes) != 0 {
			t.Errorf("phaseTimes = %v, want none for a self transition", i.Status.PhaseTimes)
		}
	})
}

// TestSetIntentPhaseCompletedAt pins the TTL contract: completedAt is
// stamped on entry to Merged, Closed and Failed, never on Blocked (a blocked
// intent waits for a human and must not expire), and cleared on revival.
func TestSetIntentPhaseCompletedAt(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		from, to      IntentPhase
		wantCompleted bool
	}{
		{IntentInReview, IntentMerged, true},
		{IntentRevising, IntentMerged, true},
		{IntentBlocked, IntentMerged, true},
		{IntentInReview, IntentClosed, true},
		{IntentPending, IntentClosed, true},
		{IntentPlanning, IntentFailed, true},
		{IntentBuilding, IntentFailed, true},
		{IntentInReview, IntentBlocked, false},
		{IntentAwaitingApproval, IntentBuilding, false},
	}
	for _, c := range cases {
		t.Run(string(c.from)+" to "+string(c.to), func(t *testing.T) {
			i := intentAt(c.from)
			if err := SetIntentPhase(i, c.to, now); err != nil {
				t.Fatalf("SetIntentPhase(%s, %s) error = %v", c.from, c.to, err)
			}
			var want *metav1.Time
			if c.wantCompleted {
				want = &metav1.Time{Time: now}
			}
			if !reflect.DeepEqual(i.Status.CompletedAt, want) {
				t.Errorf("completedAt = %v, want %v", i.Status.CompletedAt, want)
			}
		})
	}

	t.Run("revival clears completedAt", func(t *testing.T) {
		i := intentAt(IntentBuilding)
		if err := SetIntentPhase(i, IntentFailed, now); err != nil {
			t.Fatalf("SetIntentPhase(Building, Failed) error = %v", err)
		}
		if i.Status.CompletedAt == nil || !i.Status.CompletedAt.Time.Equal(now) {
			t.Fatalf("completedAt = %v after Failed, want %v", i.Status.CompletedAt, now)
		}
		later := now.Add(time.Hour)
		if err := SetIntentPhase(i, IntentPlanning, later); err != nil {
			t.Fatalf("SetIntentPhase(Failed, Planning) error = %v", err)
		}
		if i.Status.CompletedAt != nil {
			t.Errorf("completedAt = %v after revival, want nil", i.Status.CompletedAt)
		}
		want := []IntentPhaseTime{
			{Phase: IntentFailed, At: metav1.NewTime(now)},
			{Phase: IntentPlanning, At: metav1.NewTime(later)},
		}
		if !reflect.DeepEqual(i.Status.PhaseTimes, want) {
			t.Errorf("phaseTimes = %v, want %v", i.Status.PhaseTimes, want)
		}
	})
}

// TestSetIntentPhaseBoundsLog: replans are human-driven and so unbounded in
// number; the log keeps the newest MaxIntentPhaseTimes entries, so it can
// never outgrow the schema's MaxItems.
func TestSetIntentPhaseBoundsLog(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	i := &Intent{}
	if err := SetIntentPhase(i, IntentPending, now); err != nil {
		t.Fatal(err)
	}
	if err := SetIntentPhase(i, IntentPlanning, now); err != nil {
		t.Fatal(err)
	}
	// Bounce between Planning and AwaitingApproval well past the bound.
	next, other := IntentAwaitingApproval, IntentPlanning
	last := now
	for n := range 3 * MaxIntentPhaseTimes {
		last = now.Add(time.Duration(n+1) * time.Minute)
		if err := SetIntentPhase(i, next, last); err != nil {
			t.Fatalf("step %d: SetIntentPhase(%q) error = %v", n, next, err)
		}
		next, other = other, next
	}
	times := i.Status.PhaseTimes
	if len(times) != MaxIntentPhaseTimes {
		t.Fatalf("len(phaseTimes) = %d, want the bound %d", len(times), MaxIntentPhaseTimes)
	}
	if got := times[len(times)-1]; got.Phase != i.Status.Phase || !got.At.Time.Equal(last) {
		t.Errorf("newest entry = %+v, want %q at %v", got, i.Status.Phase, last)
	}
	if times[0].Phase == IntentPending {
		t.Error("oldest entry is still the creation entry, want it trimmed")
	}
}

func TestIntentBlockedFrom(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	history := func(phases ...IntentPhase) []IntentPhaseTime {
		out := make([]IntentPhaseTime, 0, len(phases))
		for _, p := range phases {
			out = append(out, IntentPhaseTime{Phase: p, At: at})
		}
		return out
	}
	cases := []struct {
		name  string
		phase IntentPhase
		times []IntentPhaseTime
		want  IntentPhase
	}{
		{"revision limit resumes review", IntentBlocked,
			history(IntentPending, IntentPlanning, IntentAwaitingApproval, IntentBuilding, IntentInReview, IntentBlocked),
			IntentInReview},
		{"missing image resumes the build", IntentBlocked,
			history(IntentAwaitingApproval, IntentBuilding, IntentBlocked), IntentBuilding},
		{"a second block resumes where it was entered", IntentBlocked,
			history(IntentBuilding, IntentBlocked, IntentBuilding, IntentInReview, IntentRevising, IntentBlocked),
			IntentRevising},
		{"not blocked", IntentInReview, history(IntentBuilding, IntentInReview), ""},
		{"no history", IntentBlocked, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := &Intent{}
			i.Status.Phase = c.phase
			i.Status.PhaseTimes = c.times
			if got := IntentBlockedFrom(i); got != c.want {
				t.Errorf("IntentBlockedFrom() = %q, want %q", got, c.want)
			}
			if got := IntentBlockedFrom(i); got != "" && !CanTransitionIntent(IntentBlocked, got) {
				t.Errorf("IntentBlockedFrom() = %q, which Blocked cannot legally resume to", got)
			}
		})
	}
}

// walkTarget picks the phase one step of a random walk attempts to move to.
// One step in sixteen attempts an arbitrary phase: often illegal, which
// exercises the error path, and occasionally absorbing. Every other step
// takes a legal non-self edge that is not Merged or Closed, so walks rarely
// absorb and run long enough to fill and trim the phase log.
func walkTarget(from IntentPhase, s uint8) IntentPhase {
	targets := append([]IntentPhase{""}, allIntentPhases...)
	if s < 16 {
		return targets[int(s)%len(targets)]
	}
	var moves []IntentPhase
	for _, to := range intentTransitions[from] {
		if to != IntentMerged && to != IntentClosed {
			moves = append(moves, to)
		}
	}
	if len(moves) == 0 {
		return targets[int(s)%len(targets)]
	}
	return moves[int(s)%len(moves)]
}

// TestSetIntentPhaseProperty drives SetIntentPhase with random 256-step walks
// of attempted transitions from a new Intent — long enough to fill the phase
// log past MaxIntentPhaseTimes, so the trim is exercised — and checks the
// invariants the TTL and the controllers rely on after every step:
//
//   - an illegal attempt returns an error and leaves the status untouched;
//   - completedAt is set exactly when the phase is terminal, at the entry
//     time of that phase;
//   - the phase log is bounded, ends with the current phase, and is itself a
//     legal path (consecutive entries are legal, non-self edges);
//   - Merged and Closed are absorbing: nothing ever leaves them;
//   - while Blocked, IntentBlockedFrom is the phase the block was entered
//     from (tracked by the walk itself, not read from the log), and Blocked
//     may legally resume to it — including after the log was trimmed; while
//     not Blocked, it is "".
//
// Seeded, so the gate is deterministic. It also asserts the walks reached
// the trim and a blocked resume check after one, so a generator change that
// stops exercising them fails instead of passing vacuously.
func TestSetIntentPhaseProperty(t *testing.T) {
	base := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	var walks, trimmedWalks, blockedAfterTrim int
	prop := func(steps [256]uint8) bool {
		walks++
		i := &Intent{}
		var blockedFrom IntentPhase // the model: where the current block was entered from
		trimmed := false
		for n, s := range steps {
			before := i.DeepCopy()
			to := walkTarget(before.Status.Phase, s)
			now := base.Add(time.Duration(n) * time.Second)
			err := SetIntentPhase(i, to, now)
			legal := CanTransitionIntent(before.Status.Phase, to)
			if (err == nil) != legal {
				t.Logf("step %d: SetIntentPhase(%q -> %q) err = %v, legal = %v", n, before.Status.Phase, to, err, legal)
				return false
			}
			if err != nil && !reflect.DeepEqual(i, before) {
				t.Logf("step %d: illegal %q -> %q mutated the intent", n, before.Status.Phase, to)
				return false
			}
			if IntentTerminal(before.Status.Phase) && len(intentTransitions[before.Status.Phase]) == 0 &&
				i.Status.Phase != before.Status.Phase {
				t.Logf("step %d: left absorbing phase %q for %q", n, before.Status.Phase, i.Status.Phase)
				return false
			}
			if !intentInvariantsHold(t, i) {
				t.Logf("step %d: after %q -> %q", n, before.Status.Phase, to)
				return false
			}
			if err == nil && to != before.Status.Phase && len(before.Status.PhaseTimes) == MaxIntentPhaseTimes {
				trimmed = true
			}
			if i.Status.Phase == IntentBlocked && before.Status.Phase != IntentBlocked {
				blockedFrom = before.Status.Phase
			}
			got := IntentBlockedFrom(i)
			if i.Status.Phase != IntentBlocked {
				if got != "" {
					t.Logf("step %d: IntentBlockedFrom = %q in phase %q, want \"\"", n, got, i.Status.Phase)
					return false
				}
				continue
			}
			if got != blockedFrom || !CanTransitionIntent(IntentBlocked, got) {
				t.Logf("step %d: IntentBlockedFrom = %q, want %q, a legal resume (trimmed log: %v)", n, got, blockedFrom, trimmed)
				return false
			}
			if trimmed {
				blockedAfterTrim++
			}
		}
		if trimmed {
			trimmedWalks++
		}
		return true
	}
	cfg := &quick.Config{
		MaxCount: 500,
		Rand:     rand.New(rand.NewSource(20260923)),
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
	if trimmedWalks < walks/10 {
		t.Errorf("%d of %d walks trimmed the phase log, want at least a tenth: the property no longer exercises the bound",
			trimmedWalks, walks)
	}
	if blockedAfterTrim == 0 {
		t.Error("no walk checked IntentBlockedFrom after a trim: the property no longer exercises it")
	}
}

// intentInvariantsHold reports whether the intent's phase bookkeeping is
// self-consistent, logging the first violation.
func intentInvariantsHold(t *testing.T, i *Intent) bool {
	t.Helper()
	st := i.Status
	if IntentTerminal(st.Phase) != (st.CompletedAt != nil) {
		t.Logf("phase %q terminal = %v but completedAt = %v", st.Phase, IntentTerminal(st.Phase), st.CompletedAt)
		return false
	}
	if len(st.PhaseTimes) > MaxIntentPhaseTimes {
		t.Logf("len(phaseTimes) = %d, over the bound %d", len(st.PhaseTimes), MaxIntentPhaseTimes)
		return false
	}
	if st.Phase == "" {
		if len(st.PhaseTimes) != 0 {
			t.Logf("no phase but phaseTimes = %v", st.PhaseTimes)
			return false
		}
		return true
	}
	last := st.PhaseTimes[len(st.PhaseTimes)-1]
	if last.Phase != st.Phase {
		t.Logf("newest phase time %q != phase %q", last.Phase, st.Phase)
		return false
	}
	if st.CompletedAt != nil && !st.CompletedAt.Equal(&last.At) {
		t.Logf("completedAt %v != terminal entry time %v", st.CompletedAt, last.At)
		return false
	}
	for k := 1; k < len(st.PhaseTimes); k++ {
		from, to := st.PhaseTimes[k-1].Phase, st.PhaseTimes[k].Phase
		if from == to || !CanTransitionIntent(from, to) {
			t.Logf("phaseTimes holds %q -> %q, not a legal non-self edge", from, to)
			return false
		}
	}
	return true
}
