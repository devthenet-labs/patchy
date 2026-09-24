// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"k8s.io/apimachinery/pkg/util/validation"
)

// githubLabelMaxLength is GitHub's own limit on a label name.
const githubLabelMaxLength = 50

// labelSafe reports why name is not usable both as an object name and,
// untruncated, as a label value — or "" when it is.
func labelSafe(name string) string {
	if errs := validation.IsValidLabelValue(name); len(errs) > 0 {
		return strings.Join(errs, "; ")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return strings.Join(errs, "; ")
	}
	return ""
}

func TestIntentNames(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"intent", IntentName("target", 1), "target-1"},
		{"plan run", IntentRunName("target-1", IntentStagePlan, 2, "ignored", 1), "target-1-plan-r2-a1"},
		{"build run", IntentRunName("target-1", IntentStageBuild, 2, "shop", 1), "target-1-bld-r2-shop-a1"},
		{"revise run", IntentRunName("target-1", IntentStageRevise, 3, "shop", 2), "target-1-rev3-shop-a2"},
		{"unknown stage", IntentRunName("target-1", "deploy", 1, "shop", 1), ""},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s name = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestIntentNameBudget pins the name budget's arithmetic: with every part at
// its bound, the build run's name is exactly the 63-character label-value
// limit, every other derived name fits inside it, and the derived trigger
// label fits inside GitHub's label limit.
func TestIntentNameBudget(t *testing.T) {
	project := strings.Repeat("p", MaxProjectNameLength)
	key := strings.Repeat("k", MaxRepositoryKeyLength)
	intent := IntentName(project, MaxIntentIssueNumber)
	if len(intent) != 33 {
		t.Errorf("longest intent name is %d characters, want 33", len(intent))
	}
	names := map[string]string{
		"intent": intent,
		"plan":   IntentRunName(intent, IntentStagePlan, MaxIntentRound, "", MaxIntentRunAttempt),
		"build":  IntentRunName(intent, IntentStageBuild, MaxIntentRound, key, MaxIntentRunAttempt),
		"revise": IntentRunName(intent, IntentStageRevise, MaxIntentRound, key, MaxIntentRunAttempt),
	}
	if got := len(names["build"]); got != validation.LabelValueMaxLength {
		t.Errorf("longest build run name is %d characters, want exactly the label limit %d (the budget is spent)",
			got, validation.LabelValueMaxLength)
	}
	for what, name := range names {
		if why := labelSafe(name); why != "" {
			t.Errorf("longest %s name %q is not label-safe: %s", what, name, why)
		}
	}
	p := &Project{}
	p.Name = project
	if got := ProjectTriggerLabel(p); len(got) > githubLabelMaxLength {
		t.Errorf("derived trigger %q is %d characters, over GitHub's %d", got, len(got), githubLabelMaxLength)
	}
}

func TestProjectTriggerLabel(t *testing.T) {
	p := &Project{}
	p.Name = "target"
	if got := ProjectTriggerLabel(p); got != "patchy:target" {
		t.Errorf("derived trigger = %q, want patchy:target", got)
	}
	p.Spec.Labels.Trigger = "intent:target"
	if got := ProjectTriggerLabel(p); got != "intent:target" {
		t.Errorf("explicit trigger = %q, want intent:target", got)
	}
}

// runParts is one IntentRun name's parts, every one inside the name budget:
// a valid Project name, issue, stage, round, repository key and attempt.
type runParts struct {
	Project string
	Issue   int64
	Stage   IntentStage
	Round   int32
	Key     string
	Attempt int32
}

const (
	dnsAlnum  = "abcdefghijklmnopqrstuvwxyz0123456789"
	dnsInside = dnsAlnum + "-"
)

// genName returns a random name of 1..maxLen characters from alnum ends and
// `inside` in between, retried until valid reports it valid.
func genName(r *rand.Rand, maxLen int, inside string, valid func(string) bool) string {
	for {
		b := make([]byte, 1+r.Intn(maxLen))
		for i := range b {
			set := inside
			if i == 0 || i == len(b)-1 {
				set = dnsAlnum
			}
			b[i] = set[r.Intn(len(set))]
		}
		if s := string(b); valid(s) {
			return s
		}
	}
}

// Generate implements quick.Generator. Project names are object names (DNS
// subdomains, dots allowed); repository keys are DNS labels. Small values are
// favoured so collisions between two generated runs are actually tried.
func (runParts) Generate(r *rand.Rand, _ int) reflect.Value {
	stages := []IntentStage{IntentStagePlan, IntentStageBuild, IntentStageRevise}
	small := func(limit int) int { // 1..limit, a third of the time 1..3
		if r.Intn(3) == 0 {
			return 1 + r.Intn(min(3, limit))
		}
		return 1 + r.Intn(limit)
	}
	p := runParts{
		Project: genName(r, MaxProjectNameLength, dnsInside+".", func(s string) bool {
			return len(validation.IsDNS1123Subdomain(s)) == 0
		}),
		Issue: int64(small(MaxIntentIssueNumber)),
		Stage: stages[r.Intn(len(stages))],
		Round: int32(small(MaxIntentRound)),
		Key: genName(r, MaxRepositoryKeyLength, dnsInside, func(s string) bool {
			return len(validation.IsDNS1123Label(s)) == 0
		}),
		Attempt: int32(small(MaxIntentRunAttempt)),
	}
	return reflect.ValueOf(p)
}

// name is the run's IntentRunName.
func (p runParts) name() string {
	return IntentRunName(IntentName(p.Project, p.Issue), p.Stage, p.Round, p.Key, p.Attempt)
}

// identity is what the name must distinguish within one Intent: a plan run's
// name carries no repository key.
func (p runParts) identity() string {
	key := p.Key
	if p.Stage == IntentStagePlan {
		key = ""
	}
	return strings.Join([]string{
		string(p.Stage), strconv.Itoa(int(p.Round)), key, strconv.Itoa(int(p.Attempt)),
	}, "/")
}

// TestIntentRunNameProperty: for any parts inside the name budget, the Intent
// name and the run name are label-safe (so jobs never truncates them and a
// Job maps back to its run by the exact value), and within one Intent two
// runs share a name exactly when they are the same run — so a new round or
// attempt can never adopt another's run as its lease. Seeded, so the gate is
// deterministic.
func TestIntentRunNameProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20260924))}

	safe := func(p runParts) bool {
		for _, name := range []string{IntentName(p.Project, p.Issue), p.name()} {
			if why := labelSafe(name); why != "" {
				t.Logf("%+v: %q is not label-safe: %s", p, name, why)
				return false
			}
		}
		return true
	}
	if err := quick.Check(safe, cfg); err != nil {
		t.Error(err)
	}

	// b borrows a's intent, and — to try near-collisions, not just random
	// pairs — some of a's run parts, as chosen by the mask.
	unique := func(a, b runParts, mask uint8) bool {
		b.Project, b.Issue = a.Project, a.Issue
		if mask&1 != 0 {
			b.Stage = a.Stage
		}
		if mask&2 != 0 {
			b.Round = a.Round
		}
		if mask&4 != 0 {
			b.Key = a.Key
		}
		if mask&8 != 0 {
			b.Attempt = a.Attempt
		}
		if same, sameName := a.identity() == b.identity(), a.name() == b.name(); same != sameName {
			t.Logf("%+v and %+v: same run = %v, same name = %v (%q, %q)", a, b, same, sameName, a.name(), b.name())
			return false
		}
		return true
	}
	if err := quick.Check(unique, cfg); err != nil {
		t.Error(err)
	}
}
