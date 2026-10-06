// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentview

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// Column is a board column.
type Column string

// The board columns, left to right.
const (
	ColumnPlanning Column = "planning"
	ColumnApproval Column = "approval"
	ColumnBuilding Column = "building"
	ColumnReview   Column = "review"
	// ColumnBlocked holds a Blocked intent whose from-phase is no longer in
	// its bounded phase log; every other Blocked intent stays in the column
	// of the phase it was blocked from, with a badge.
	ColumnBlocked Column = "blocked"
	ColumnDone    Column = "done"
)

// Columns lists the board columns in display order.
var Columns = []Column{ColumnPlanning, ColumnApproval, ColumnBuilding, ColumnReview, ColumnBlocked, ColumnDone}

// ColumnOf is the column a phase belongs in. Blocked has none of its own:
// ColumnFor resolves it from the phase it was blocked from.
func ColumnOf(p v1alpha1.IntentPhase) Column {
	switch p {
	case "", v1alpha1.IntentPending, v1alpha1.IntentPlanning:
		return ColumnPlanning
	case v1alpha1.IntentAwaitingApproval:
		return ColumnApproval
	case v1alpha1.IntentBuilding:
		return ColumnBuilding
	case v1alpha1.IntentInReview, v1alpha1.IntentRevising:
		return ColumnReview
	case v1alpha1.IntentMerged, v1alpha1.IntentClosed, v1alpha1.IntentFailed:
		return ColumnDone
	}
	return ColumnBlocked
}

// ColumnFor is the column an Intent belongs in: its phase's, or for a
// Blocked one the column of the phase it was blocked from
// (v1alpha1.IntentBlockedFrom). The phase log keeps only the newest
// MaxIntentPhaseTimes entries, so an intent blocked and resumed many times
// can lose its from-phase; it then sits in ColumnBlocked.
func ColumnFor(in *v1alpha1.Intent) Column {
	if in.Status.Phase != v1alpha1.IntentBlocked {
		return ColumnOf(in.Status.Phase)
	}
	from := v1alpha1.IntentBlockedFrom(in)
	if from == "" || from == v1alpha1.IntentBlocked {
		return ColumnBlocked
	}
	return ColumnOf(from)
}

// MaxAttempts is how many counted attempts one stage's round gets before the
// Intent fails. intent-controller hard-wires the same number
// (intent.DefaultMaxAttempts, with no flag); a test there pins the two.
const MaxAttempts int32 = 2

// Limits are a Project's intent limits with the schema defaults applied, for
// a Project the API server never defaulted.
type Limits struct {
	MaxRevisions    int32
	MaxCheckFixes   int32
	MaxCostMicroUSD int64
}

// LimitsOf reads p's limits, defaulting what is unset as the schema does.
func LimitsOf(p *v1alpha1.Project) Limits {
	l := Limits{
		MaxRevisions:    v1alpha1.DefaultMaxRevisions,
		MaxCheckFixes:   v1alpha1.DefaultMaxCheckFixes,
		MaxCostMicroUSD: v1alpha1.DefaultMaxCostMicroUSD,
	}
	if p == nil {
		return l
	}
	if v := p.Spec.Limits.MaxRevisions; v != nil {
		l.MaxRevisions = *v
	}
	if v := p.Spec.Limits.MaxCheckFixes; v != nil {
		l.MaxCheckFixes = *v
	}
	if v := p.Spec.Limits.MaxCostMicroUSD; v > 0 {
		l.MaxCostMicroUSD = v
	}
	return l
}

// MicroUSD parses a run's UsageSummary cost ("1.234567") into micro-USD the
// way intent-controller sums it into the Intent's spend: "" and anything
// unparseable are zero, never an error, so a run row and the total it feeds
// cannot disagree about one run. A test in internal/controller/intent pins it
// to the controller's own parser.
func MicroUSD(cost string) int64 {
	if cost == "" {
		return 0
	}
	whole, frac, _ := strings.Cut(cost, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 || w > math.MaxInt64/1_000_000 {
		return 0
	}
	frac = (frac + "000000")[:6]
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0
	}
	return w*1_000_000 + f
}

// FormatUSD renders micro-USD as dollars and cents, rounded down: "$1.23".
func FormatUSD(micro int64) string {
	if micro < 0 {
		micro = 0
	}
	cents := micro / 10_000
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

// outcomeText is the public wording of each run outcome the controllers
// record: the envelope's vocabulary and intent-controller's own. A run's
// detail never appears beside it.
var outcomeText = map[string]string{
	"ok":                  "completed",
	"runtime_error":       "the agent stopped with an error",
	"timeout":             "the run reached its time limit, or made no progress for too long",
	"budget_exceeded":     "the run used up its output-token budget",
	"report_missing":      "the agent wrote no report",
	"report_invalid":      "the agent's report could not be read",
	"commit_failed":       "the agent's changes could not be committed",
	"changeset_too_large": "the change was over the size limit",
	"image_incompatible":  "the repository's agent image cannot run the agent",
	"changeset_rejected":  "the change was refused before any push",
	"aborted":             "the run ended with no result",
	"image_required":      "the build needs an accepted repository agent image",
	"not_built":           "the agent reported it could not build the plan",
	"branch_exists":       "the intent branch exists at a commit patchy did not push",
	"hold_expired":        "the finished build expired while the intent was suspended",
	"push_refused":        "GitHub refused the push",
	"launch_refused":      "the cluster refused the agent Job",
	"head_moved":          "the pull request head moved during the run",
	"input_unavailable":   "the feedback it was given went away before the agent read it",
	"no_usable_feedback":  "the review or command carried no usable feedback",
	// These two repeat the issue's own wording (failedRunReason), pinned by
	// a test beside it.
	"unschedulable": "no node in the cluster could fit its agent",
	"evicted":       "the agent pod was evicted",
}

// OutcomeText is the public wording of a run outcome, or "" for one this
// package does not know (the outcome code is then shown alone).
func OutcomeText(outcome string) string { return outcomeText[outcome] }

// RunReason is a run's outcome in public wording: "<outcome>: <text>", the
// outcome alone when unknown, "" while the run has none. It never includes
// the run's detail.
func RunReason(run *v1alpha1.IntentRun) string {
	o := run.Status.Outcome
	if o == "" {
		return ""
	}
	o = Text(o, 64)
	if t := OutcomeText(o); t != "" {
		return o + ": " + t
	}
	return o
}

// BlockingConditions are the conditions that hold an Intent Blocked, in the
// order intent-controller lists them. A test in internal/controller/intent
// pins the copy to its own list.
var BlockingConditions = []string{
	v1alpha1.ConditionBudgetExhausted, v1alpha1.ConditionImageRequired, v1alpha1.ConditionBranchConflict,
	v1alpha1.ConditionRevisionLimitReached, v1alpha1.ConditionChecksFailing,
	v1alpha1.ConditionUnsupportedRepositories, v1alpha1.ConditionResourcesUnavailable,
}

// blockText is the public wording per blocking condition and reason; the ""
// reason is the condition's fallback.
var blockText = map[string]map[string]string{
	v1alpha1.ConditionImageRequired: {
		"NoRepositoryImage":        "the repository declares no agent image, and builds need one",
		"RepositoryImageRejected":  "the repository's agent image was rejected",
		"RepositoryImagesDisabled": "repository agent images are turned off on this cluster",
		"SandboxBreakerTripped":    "the sandbox breaker tripped: an agent image failed the sandbox probe",
		"DefaultImageRan":          "the build ran on the default image instead of the repository's",
		"":                         "the build needs an accepted repository agent image",
	},
	v1alpha1.ConditionBranchConflict: {
		"BranchExists":       "the intent branch exists at a commit patchy did not push",
		"ForeignPullRequest": "a pull request patchy did not open holds the intent branch",
		"BranchMissing":      "the intent branch vanished before its pull request opened",
		"BranchChanged":      "the intent branch moved away from the build's commit",
		"PullRequestRefused": "GitHub refused to open the pull request",
		"StaleRoundBranch":   "the intent branch holds an earlier round's commit",
		"":                   "the intent branch is in a state patchy will not build on",
	},
	v1alpha1.ConditionChecksFailing: {
		"RepeatedFailure": "the same check failure came back after a fix round",
		"":                "the pull request's checks keep failing",
	},
	v1alpha1.ConditionUnsupportedRepositories: {
		"MultiRepositoryOff":   "the project lists several repositories, and multi-repository intents are off",
		"RepositoryKeyChanged": "the project's repository keys changed during a build",
		"":                     "the project's repositories are not supported as configured",
	},
	v1alpha1.ConditionResourcesUnavailable: {
		"UnknownResourceClass": "the repository asks for a resource class the cluster does not define",
		"Unschedulable":        "no node in the cluster could fit the agent",
		"":                     "the agent's resources are not available",
	},
}

// BlockedReasons are the public reasons the True blocking conditions give,
// in BlockingConditions order, from fixed wording and the Intent's own
// counters only, never a condition's message.
func BlockedReasons(in *v1alpha1.Intent, l Limits) []string {
	var out []string
	for _, typ := range BlockingConditions {
		c := meta.FindStatusCondition(in.Status.Conditions, typ)
		if c == nil || c.Status != metav1.ConditionTrue {
			continue
		}
		out = append(out, blockReason(in, l, c))
	}
	return out
}

func blockReason(in *v1alpha1.Intent, l Limits, c *metav1.Condition) string {
	switch c.Type {
	case v1alpha1.ConditionBudgetExhausted:
		return fmt.Sprintf("spent %s of the %s cost ceiling", FormatUSD(in.Status.Usage.CostMicroUSD),
			FormatUSD(l.MaxCostMicroUSD))
	case v1alpha1.ConditionRevisionLimitReached:
		return fmt.Sprintf("the revision limit is reached (%d of %d)", in.Status.Revisions, l.MaxRevisions)
	case v1alpha1.ConditionChecksFailing:
		if c.Reason == "MaxCheckFixes" {
			return fmt.Sprintf("the check-fix limit is reached (%d of %d)", in.Status.CheckFixes, l.MaxCheckFixes)
		}
	}
	texts := blockText[c.Type]
	if t, ok := texts[c.Reason]; ok {
		return t
	}
	if t, ok := texts[""]; ok {
		return t
	}
	return Text(c.Type, 64)
}

// Text makes s safe to show as a plain text node: invalid UTF-8 replaced,
// line breaks normalised, every character that renders as nothing (bidi and
// zero-width controls, tag characters, C0 controls such as ESC) written as
// its code point (templates.VisibleText), and the result capped at max bytes
// on a rune boundary, with "…" marking a cut. Every string the dashboard
// ships that is not its own fixed wording passes through it.
func Text(s string, max int) string {
	s = templates.VisibleText(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// SafeURL returns raw when it is an absolute https URL with a host and no
// user information, else "". Links the dashboard renders come only from
// controller-written fields, and only through it.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	return raw
}

// RepoSlug is "owner/name" of a repository URL (https://host/owner/name,
// a .git suffix dropped), or "" when it is not one.
func RepoSlug(raw string) string {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimRight(raw, "/"), ".git"))
	if err != nil || u.Host == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return Text(parts[0]+"/"+parts[1], 200)
}

// DigestHex is the hex of a sha256 digest reference, cut to the first 12
// characters, for display; "" when ref holds none.
func DigestHex(ref string) string {
	_, hex, ok := strings.Cut(ref, "sha256:")
	if !ok || len(hex) < 12 {
		return ""
	}
	for _, r := range hex[:12] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return ""
		}
	}
	return hex[:12]
}
