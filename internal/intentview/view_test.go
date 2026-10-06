// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentview

import (
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

func phaseLog(phases ...v1alpha1.IntentPhase) []v1alpha1.IntentPhaseTime {
	at := metav1.NewTime(time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC))
	out := make([]v1alpha1.IntentPhaseTime, len(phases))
	for i, p := range phases {
		out[i] = v1alpha1.IntentPhaseTime{Phase: p, At: at}
	}
	return out
}

func TestColumnFor(t *testing.T) {
	cases := []struct {
		name  string
		phase v1alpha1.IntentPhase
		log   []v1alpha1.IntentPhaseTime
		want  Column
	}{
		{"new", "", nil, ColumnPlanning},
		{"pending", v1alpha1.IntentPending, nil, ColumnPlanning},
		{"planning", v1alpha1.IntentPlanning, nil, ColumnPlanning},
		{"awaiting approval", v1alpha1.IntentAwaitingApproval, nil, ColumnApproval},
		{"building", v1alpha1.IntentBuilding, nil, ColumnBuilding},
		{"in review", v1alpha1.IntentInReview, nil, ColumnReview},
		{"revising", v1alpha1.IntentRevising, nil, ColumnReview},
		{"merged", v1alpha1.IntentMerged, nil, ColumnDone},
		{"closed", v1alpha1.IntentClosed, nil, ColumnDone},
		{"failed", v1alpha1.IntentFailed, nil, ColumnDone},
		{"blocked from building stays in building", v1alpha1.IntentBlocked,
			phaseLog(v1alpha1.IntentPending, v1alpha1.IntentPlanning, v1alpha1.IntentAwaitingApproval,
				v1alpha1.IntentBuilding, v1alpha1.IntentBlocked), ColumnBuilding},
		{"blocked from review stays in review", v1alpha1.IntentBlocked,
			phaseLog(v1alpha1.IntentInReview, v1alpha1.IntentRevising, v1alpha1.IntentBlocked), ColumnReview},
		// The log is bounded: an intent whose from-phase scrolled out sits in
		// its own column rather than a guessed one.
		{"blocked with the from-phase lost", v1alpha1.IntentBlocked,
			phaseLog(v1alpha1.IntentBlocked), ColumnBlocked},
		{"an unknown phase", "Bogus", nil, ColumnBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{Phase: tc.phase, PhaseTimes: tc.log}}
			if got := ColumnFor(in); got != tc.want {
				t.Errorf("ColumnFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLimitsOf(t *testing.T) {
	zero, five := int32(0), int32(5)
	if got := LimitsOf(nil); got != (Limits{3, 2, 10_000_000}) {
		t.Errorf("LimitsOf(nil) = %+v", got)
	}
	p := &v1alpha1.Project{}
	p.Spec.Limits.MaxRevisions = &zero
	p.Spec.Limits.MaxCheckFixes = &five
	p.Spec.Limits.MaxCostMicroUSD = 2_500_000
	// Zero revisions is meaningful (no revise rounds), not "unset".
	if got := LimitsOf(p); got != (Limits{0, 5, 2_500_000}) {
		t.Errorf("LimitsOf = %+v", got)
	}
}

func TestMicroUSDAndFormat(t *testing.T) {
	for in, want := range map[string]int64{
		"": 0, "0": 0, "1": 1_000_000, "1.5": 1_500_000, "0.000001": 1, "12.3456789": 12_345_678,
		"-1": 0, "x": 0, "1.x": 0, "9223372036854.775807": 9223372036854775807, "9223372036855": 0,
	} {
		if got := MicroUSD(in); got != want {
			t.Errorf("MicroUSD(%q) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[int64]string{0: "$0.00", 1: "$0.00", 12_345_678: "$12.34", 10_000_000: "$10.00", -5: "$0.00"} {
		if got := FormatUSD(in); got != want {
			t.Errorf("FormatUSD(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRunReasonNeverCarriesDetail(t *testing.T) {
	private := "0/3 nodes are available: node ip-10-0-1-23.ec2.internal; image " +
		"111122223333.dkr.ecr.us-east-1.amazonaws.com/patchy/app@sha256:abc"
	for _, outcome := range []string{"unschedulable", "evicted", "runtime_error", "image_required", "aborted", "weird"} {
		run := &v1alpha1.IntentRun{Status: v1alpha1.IntentRunStatus{Outcome: outcome, Detail: private}}
		got := RunReason(run)
		if !strings.HasPrefix(got, outcome) {
			t.Errorf("RunReason(%s) = %q, want it to start with the outcome", outcome, got)
		}
		for _, leak := range []string{"ip-10-0-1-23", "111122223333", "dkr.ecr", "nodes are available"} {
			if strings.Contains(got, leak) {
				t.Errorf("RunReason(%s) = %q leaks %q from the run's detail", outcome, got, leak)
			}
		}
	}
	if got := RunReason(&v1alpha1.IntentRun{}); got != "" {
		t.Errorf("RunReason of a run with no outcome = %q, want empty", got)
	}
	if got := RunReason(&v1alpha1.IntentRun{Status: v1alpha1.IntentRunStatus{Outcome: "ok"}}); got != "ok: completed" {
		t.Errorf("RunReason(ok) = %q", got)
	}
}

func TestBlockedReasonsUsePublicWordingOnly(t *testing.T) {
	leak := "image 111122223333.dkr.ecr.us-east-1.amazonaws.com/x rejected on node ip-10-0-1-23"
	cond := func(typ, reason string) metav1.Condition {
		return metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: reason, Message: leak}
	}
	in := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{
		Phase:     v1alpha1.IntentBlocked,
		Revisions: 3, CheckFixes: 2,
		Usage: v1alpha1.IntentUsage{CostMicroUSD: 10_250_000},
		Conditions: []metav1.Condition{
			cond(v1alpha1.ConditionResourcesUnavailable, "Unschedulable"),
			cond(v1alpha1.ConditionImageRequired, "RepositoryImageRejected"),
			cond(v1alpha1.ConditionBudgetExhausted, "CostCeilingReached"),
			cond(v1alpha1.ConditionRevisionLimitReached, "MaxRevisions"),
			cond(v1alpha1.ConditionChecksFailing, "MaxCheckFixes"),
			cond(v1alpha1.ConditionBranchConflict, "SomethingNew"),
			cond(v1alpha1.ConditionApprovalRejected, "NotBlocking"),
			{Type: v1alpha1.ConditionUnsupportedRepositories, Status: metav1.ConditionFalse, Message: leak},
		},
	}}
	got := BlockedReasons(in, Limits{MaxRevisions: 3, MaxCheckFixes: 2, MaxCostMicroUSD: 10_000_000})
	want := []string{
		"spent $10.25 of the $10.00 cost ceiling",
		"the repository's agent image was rejected",
		"the intent branch is in a state patchy will not build on",
		"the revision limit is reached (3 of 3)",
		"the check-fix limit is reached (2 of 2)",
		"no node in the cluster could fit the agent",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BlockedReasons =\n%q\nwant\n%q", got, want)
	}
	for _, r := range got {
		if strings.Contains(r, "111122223333") || strings.Contains(r, "ip-10-") {
			t.Errorf("reason %q leaks a condition message", r)
		}
	}
}

func TestSafeURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://github.com/acme/app/pull/3":       true,
		"https://intent-7.preview.example.com/":    true,
		"http://github.com/acme/app/pull/3":        false,
		"javascript:alert(1)":                      false,
		"https://user:pass@github.com/":            false,
		"https:///nohost":                          false,
		"//github.com/acme":                        false,
		"data:text/html,<script>alert(1)</script>": false,
		"https:opaque":                             false,
		"":                                         false,
	} {
		if got := SafeURL(raw); (got != "") != ok {
			t.Errorf("SafeURL(%q) = %q, want accepted=%v", raw, got, ok)
		}
	}
}

func TestDigestHex(t *testing.T) {
	for in, want := range map[string]string{
		"sha256:0123456789abcdef0123": "0123456789ab",
		"111122223333.dkr.ecr.us-east-1.amazonaws.com/app@sha256:fedcba9876543210": "fedcba987654",
		"registry.example/app:latest": "",
		"sha256:short":                "",
		"sha256:ZZZZZZZZZZZZZZZZ":     "",
	} {
		if got := DigestHex(in); got != want {
			t.Errorf("DigestHex(%q) = %q, want %q", in, got, want)
		}
	}
}

// For any input: Text's output is valid UTF-8, holds no character that
// renders as nothing (the escape that starts a terminal sequence included),
// and is at most max bytes plus the cut marker.
func TestTextProperty(t *testing.T) {
	runes := []rune{'a', ' ', '\n', '\t', '\r', 0x1b, 0x07, 0x202e, 0x200b, 0xe0041, 0xfe0f, 'é', '😀', 0xfffd}
	cfg := &quick.Config{
		MaxCount: 3000,
		Rand:     rand.New(rand.NewSource(20261006)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			b := make([]rune, r.Intn(40))
			for i := range b {
				b[i] = runes[r.Intn(len(runes))]
			}
			s := string(b)
			if r.Intn(8) == 0 {
				s += "\xff\xfe" // invalid UTF-8
			}
			args[0] = reflect.ValueOf(s)
			args[1] = reflect.ValueOf(r.Intn(30))
		},
	}
	prop := func(s string, max int) bool {
		got := Text(s, max)
		if !utf8.ValidString(got) {
			return false
		}
		if max > 0 && len(got) > max+len("…") {
			return false
		}
		for _, r := range got {
			if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestPureImports keeps the projection free of clients and controllers: the
// standard library, api/v1alpha1, internal/templates and apimachinery types.
func TestPureImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{
		"github.com/bitwise-media-group/patchy/api/v1alpha1",
		"github.com/bitwise-media-group/patchy/internal/templates",
		"k8s.io/apimachinery/",
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if !strings.Contains(first, ".") {
				continue
			}
			ok := false
			for _, a := range allowed {
				if path == a || (strings.HasSuffix(a, "/") && strings.HasPrefix(path, a)) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s imports %s: intentview stays pure", name, path)
			}
		}
	}
}
