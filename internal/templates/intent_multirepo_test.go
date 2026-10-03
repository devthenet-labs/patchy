// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// The multi-repository intent: a web front end and the API it calls, the
// second named the way an operator may name a repository (mixed case,
// punctuated), which patchy must carry exactly as the Project spells it.
const (
	testWebURL = "https://github.com/devthenet-labs/marigold-web"
	testAPIURL = "https://github.com/devthenet-labs/Acme.Web_App"
	testWeb    = "devthenet-labs/marigold-web"
	testAPI    = "devthenet-labs/Acme.Web_App"
)

// testMultiPlan is a plan over both repositories, as a multi-repository
// planner writes it.
var testMultiPlan = strings.Join([]string{
	"---",
	`summary: "Show the API's greeting on the home page"`,
	"repositories:",
	`  - "` + testWebURL + `"`,
	`  - "` + testAPIURL + `"`,
	`new_dependencies: []`,
	`questions: []`,
	"confidence: 0.8",
	"estimated_max_turns: 40",
	"estimated_token_budget: 200000",
	"---",
	"",
	"## Approach",
	"",
	"The API serves `GET /api/greeting` as `{\"message\": string}`; the page fetches it same-origin.",
	"",
	"## Steps",
	"",
	"### " + testAPIURL,
	"",
	"1. Add the handler.",
	"",
	"### " + testWebURL,
	"",
	"1. Fetch `/api/greeting` and fill `#greeting`.",
	"",
}, "\n")

func renderTestTreesPlanPrompt(prev *PreviousAttempt) (string, error) {
	return RenderPlanPrompt(PlanPrompt{
		IssuePath:        "/workspace/input/issue.md",
		ReportPath:       "/workspace/reports/plan.md",
		Intent:           testPlanRequest,
		BuildMaxTurns:    150,
		BuildTokenBudget: 800000,
		PreviousAttempt:  prev,
		Trees: []PlanTree{
			{URL: testWebURL, Path: "/workspace/repo"},
			{URL: testAPIURL, Path: "/workspace/repos/api"},
		},
	})
}

func renderTestSiblingBuildPrompt(prev *PreviousAttempt, siblings ...string) (string, error) {
	return RenderBuildPrompt(BuildPrompt{
		PlanPath:         "/workspace/input/investigation.md",
		ReportPath:       "/workspace/reports/build.md",
		CommitScriptPath: "/workspace/commit.sh",
		PreviousAttempt:  prev,
		ThisRepository:   testAPIURL,
		Siblings:         siblings,
	})
}

func testMultiPRs() []IntentPullRequest {
	return []IntentPullRequest{
		{Repository: testWeb, Number: 4, URL: "https://github.com/devthenet-labs/marigold-web/pull/4", State: "merged"},
		{Repository: testAPI, Number: 9, URL: "https://github.com/devthenet-labs/Acme.Web_App/pull/9", State: "closed"},
	}
}

// TestMultiRepoGoldens pins every multi-repository variant: the plan prompt
// over trees, the build prompt naming its repository and siblings, the plan
// comment's header, the status comment, the pull request body's footer, the
// siblings comment, the partial ending's notice, and the summary's CI-fix
// count.
func TestMultiRepoGoldens(t *testing.T) {
	plan := testPlanComment(testMultiPlan)
	plan.Summary = "Show the API's greeting on the home page"
	plan.NewDependencies = []string{"github.com/acme/jsonx v1.2.0 (" + testAPI + ")"}
	plan.Repositories = []string{testWeb, testAPI}
	tests := []struct {
		name   string
		render func() (string, error)
	}{
		{"prompt_plan_trees.md", func() (string, error) { return renderTestTreesPlanPrompt(nil) }},
		{"prompt_build_sibling.md", func() (string, error) { return renderTestSiblingBuildPrompt(nil, testWebURL) }},
		{"prompt_build_siblings_retry.md", func() (string, error) {
			return renderTestSiblingBuildPrompt(&PreviousAttempt{
				Attempt: 1, Outcome: "commit_failed", Detail: "working tree not clean after commit.sh:\nM go.sum",
			}, testWebURL, "https://github.com/devthenet-labs/marigold-docs")
		}},
		{"intent_plan_repositories.md", func() (string, error) { return RenderPlanComment(plan) }},
		{"intent_status_in_review_repositories.md", func() (string, error) {
			prs := testMultiPRs()
			prs[0].State, prs[1].State = "open", "open"
			return RenderIntentStatusComment(IntentStatusComment{
				Namespace: "patchy", Intent: "marigold-3", Phase: "InReview",
				PlanRevision: 1, PlanURL: "https://github.com/devthenet-labs/intents/issues/3#issuecomment-7",
				Summary:    "Show the API's greeting on the home page",
				ApprovedBy: "peter", ApprovedRevision: 1,
				Repositories: []string{testWeb, testAPI},
				PullRequests: prs, MaxRevisions: 3, Commands: []string{"cancel"},
			})
		}},
		{"intent_pr_body_repositories.md", func() (string, error) {
			return RenderIntentPRBody(IntentPRBody{
				IntentRepository: "devthenet-labs/intents", IssueNumber: 3,
				Summary:      "Show the API's greeting on the home page",
				PlanRevision: 1, PlanDigest: PlanDigest([]byte(testMultiPlan)), ApprovedBy: "peter",
				Repositories: []string{testWeb, testAPI},
			})
		}},
		{"intent_siblings.md", func() (string, error) {
			return RenderIntentSiblingsComment(IntentSiblingsComment{
				Namespace: "patchy", Intent: "marigold-3", Repository: testAPI, PullRequests: testMultiPRs(),
			})
		}},
		{"intent_notice_partial.md", func() (string, error) {
			prs := testMultiPRs()
			return RenderIntentPartialNotice(IntentPartialNotice{
				Namespace: "patchy", Intent: "marigold-3",
				Merged: prs[:1], Closed: prs[1:], Revisions: 1, CheckFixes: 2, CostMicroUSD: 2_345_678,
			})
		}},
		{"intent_summary_check_fixes.md", func() (string, error) {
			return RenderIntentSummaryComment(IntentSummaryComment{
				Namespace: "patchy", Intent: "preview-demo-5",
				PullRequests: []IntentPullRequest{{
					Repository: "devthenet-labs/patchy-preview-demo", Number: 9,
					URL: "https://github.com/devthenet-labs/patchy-preview-demo/pull/9", State: "merged",
				}},
				CheckFixes: 1, CostMicroUSD: 748_594,
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestOneRepositoryRendersUnchanged: every multi-repository field given one
// repository, or none, leaves its rendering byte-identical to the
// rendering without the field, which the one-repository goldens pin. A
// one-repository intent therefore reads exactly as before slice 3.
func TestOneRepositoryRendersUnchanged(t *testing.T) {
	type pair struct {
		name       string
		with, base func() (string, error)
	}
	one := []string{"devthenet-labs/patchy-target"}
	planComment := func(repos []string) func() (string, error) {
		return func() (string, error) {
			p := testPlanComment(testPlan)
			p.NewDependencies = []string{"github.com/acme/jsonx"}
			p.Repositories = repos
			return RenderPlanComment(p)
		}
	}
	status := func(repos []string) func() (string, error) {
		return func() (string, error) {
			return RenderIntentStatusComment(IntentStatusComment{
				Namespace: "patchy", Intent: "target-1", Phase: "Building", PlanRevision: 2,
				ApprovedBy: "peter", ApprovedRevision: 2, Repositories: repos,
			})
		}
	}
	body := func(repos []string) func() (string, error) {
		return func() (string, error) {
			return RenderIntentPRBody(IntentPRBody{
				IntentRepository: "devthenet-labs/intents", IssueNumber: 1, Summary: "Add /version",
				PlanRevision: 2, PlanDigest: PlanDigest([]byte(testPlan)), ApprovedBy: "peter", Repositories: repos,
			})
		}
	}
	planPrompt := func(trees []PlanTree) func() (string, error) {
		return func() (string, error) {
			return RenderPlanPrompt(PlanPrompt{
				IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
				Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Trees: trees,
			})
		}
	}
	buildPrompt := func(this string, siblings []string) func() (string, error) {
		return func() (string, error) {
			return RenderBuildPrompt(BuildPrompt{
				PlanPath: "/workspace/input/investigation.md", ReportPath: "/workspace/reports/build.md",
				CommitScriptPath: "/workspace/commit.sh", ThisRepository: this, Siblings: siblings,
			})
		}
	}
	for _, p := range []pair{
		{"plan comment, one repository", planComment(one), planComment(nil)},
		{"plan comment, a blank second", planComment([]string{one[0], " \n"}), planComment(nil)},
		{"status comment, one repository", status(one), status(nil)},
		{"pull request body, one repository", body(one), body(nil)},
		{"plan prompt, one tree", planPrompt([]PlanTree{{URL: testWebURL, Path: "/workspace/repo"}}),
			planPrompt(nil)},
		{"build prompt, siblings without this repository", buildPrompt("", []string{testWebURL}),
			buildPrompt("", nil)},
	} {
		with, err := p.with()
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		base, err := p.base()
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		if with != base {
			t.Errorf("%s renders differently from the one-repository rendering:\n%s\n--- want ---\n%s",
				p.name, with, base)
		}
	}
}

// TestPlanPromptTreesStatesTheRules: the multi-repository plan prompt says
// where each tree is, that only the current directory has git history, and
// how a plan over several repositories must be shaped for builds that each
// see one tree — around the request, which it still quotes in one fence,
// and the one-repository rules, which it keeps.
func TestPlanPromptTreesStatesTheRules(t *testing.T) {
	got, err := renderTestTreesPlanPrompt(nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := renderTestPlanPrompt(testPlanRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	// requestSection runs to the next heading the one-repository prompt has,
	// past the repositories section.
	if !strings.HasPrefix(requestSection(t, got), requestSection(t, base)+"\n\n## The repositories\n\n") {
		t.Error("the request is not quoted as in the one-repository prompt, then the repositories")
	}
	for _, want := range []string{
		"- `" + testWebURL + "`: the current directory, `/workspace/repo`",
		"- `" + testAPIURL + "`: `/workspace/repos/api`",
		"Only the current directory is a git repository",
		"read them\nwith Read, Glob and Grep",
		"by an agent of its own, in that repository's own image",
		"nothing of the other trees",
		"Name under `repositories` only the repositories that must change",
		"Group the steps under each repository's URL",
		"State each contract between the repositories",
		"its own test plan, with the command that runs its tests in that repository",
		"Name each new dependency with the repository whose image must carry it",
		"what the largest single repository's build needs",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan prompt over trees lacks %q", want)
		}
	}
	// Everything the one-repository prompt states, it still states.
	_, baseRules, _ := strings.Cut(base, "\n\n## How to plan")
	if !strings.HasSuffix(got, "\n\n## How to plan"+baseRules) {
		t.Error("the plan prompt over trees changed the rules after its repositories section")
	}
}

// TestBuildPromptSiblingsStatesTheRules: a build in one repository of a
// multi-repository plan is told which repository it builds, which it does
// not, and not to stand in for those; and it is still told every rule a
// one-repository build is.
func TestBuildPromptSiblingsStatesTheRules(t *testing.T) {
	for _, prev := range []*PreviousAttempt{nil, {Attempt: 1, Outcome: "timeout", Detail: "stage timed out"}} {
		got, err := renderTestSiblingBuildPrompt(prev, testWebURL)
		if err != nil {
			t.Fatal(err)
		}
		base, err := renderTestBuildPrompt(prev)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"This run builds one of them, the one in the current directory:\n`" + testAPIURL + "`.",
			"Build only the plan's steps for this repository",
			"run only this repository's tests",
			"is about this repository's pull request",
			"is built by a separate run, in its own image, and its tree is not here:\n\n- `" + testWebURL + "`\n",
			"Do not build, stub, mock or copy any part of the plan that belongs to another repository",
			"build to the contract the plan states",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("previous attempt %+v: sibling build prompt lacks %q", prev, want)
			}
		}
		// The section is the only difference from the one-repository prompt.
		head, _, _ := strings.Cut(base, "\n\n## ")
		_, tail, _ := strings.Cut(base, head)
		if !strings.HasPrefix(got, head+"\n\n## This repository\n") || !strings.HasSuffix(got, tail) {
			t.Errorf("previous attempt %+v: the sibling build prompt is not the one-repository prompt "+
				"with its section inserted:\n%s", prev, got)
		}
		if strings.Contains(got, "issue.md") {
			t.Errorf("previous attempt %+v: build prompt names the request file", prev)
		}
	}
}

// repositoryConfig generates repository lists: one to four names, each
// either an ordinary owner/name or agent-like markdown (markdownTokens) —
// the operator spells repositories, but the renderers must hold whatever a
// name holds.
func repositoryConfig(seed int64) *quick.Config {
	return &quick.Config{
		MaxCount: 1500,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			repos := make([]string, 1+r.Intn(4))
			for i := range repos {
				if r.Intn(3) == 0 {
					repos[i] = fmt.Sprintf("acme/app-%d", i)
					continue
				}
				var b strings.Builder
				for range 1 + r.Intn(6) {
					b.WriteString(markdownTokens[r.Intn(len(markdownTokens))])
				}
				repos[i] = b.String()
			}
			args[0] = reflect.ValueOf(repos)
		},
	}
}

// TestMultiRepoCommentProperties: whatever the repository names hold, the
// plan comment still passes checkPlanComment (its header sanitiser output,
// the plan verbatim) and names each repository in code; the status comment
// stays sanitiser output; and the pull request body, read as markdown and
// as plain text, still references the intent issue alone and mentions
// nobody.
func TestMultiRepoCommentProperties(t *testing.T) {
	var failure string
	holds := func(repos []string) bool {
		p := testPlanComment(testMultiPlan)
		p.NewDependencies = []string{"github.com/acme/jsonx"}
		p.Repositories = repos
		plan, err := RenderPlanComment(p)
		if err != nil {
			failure = fmt.Sprintf("plan: %v", err)
			return false
		}
		if msg := checkPlanComment(plan, testMultiPlan); msg != "" {
			failure = fmt.Sprintf("plan: %s\nrepositories %q\n%s", msg, repos, plan)
			return false
		}
		header, _, _, _ := planParts(plan)
		if items := repositoryItems(repos, false); items != nil &&
			!strings.Contains(header, "patchy opens one pull request in each of: "+strings.Join(items, ", ")+".") {
			failure = fmt.Sprintf("plan header does not list %q:\n%s", items, header)
			return false
		}
		status, err := RenderIntentStatusComment(IntentStatusComment{
			Namespace: "patchy", Intent: "target-1", Phase: "Building", Repositories: repos,
		})
		if err != nil {
			failure = fmt.Sprintf("status: %v", err)
			return false
		}
		if msg := checkSanitized(cutMarker(status)); msg != "" {
			failure = fmt.Sprintf("status: %s\nrepositories %q\n%s", msg, repos, status)
			return false
		}
		body, err := RenderIntentPRBody(IntentPRBody{
			IntentRepository: "devthenet-labs/intents", IssueNumber: 1, Summary: "Add /version",
			PlanRevision: 1, PlanDigest: PlanDigest([]byte(testMultiPlan)), ApprovedBy: "peter", Repositories: repos,
		})
		if err != nil {
			failure = fmt.Sprintf("pull request body: %v", err)
			return false
		}
		const partOf = "Part of devthenet-labs/intents#1\n"
		if msg := checkSanitized(strings.TrimPrefix(body, partOf)); !strings.HasPrefix(body, partOf) || msg != "" {
			failure = fmt.Sprintf("pull request body: %s\nrepositories %q\n%s", msg, repos, body)
			return false
		}
		if msg := checkPlainText(body, len("Part of devthenet-labs/intents")); msg != "" {
			failure = fmt.Sprintf("pull request body as plain text: %s\nrepositories %q\n%s", msg, repos, body)
			return false
		}
		return true
	}
	if err := quick.Check(holds, repositoryConfig(20261003)); err != nil {
		t.Errorf("%v\n%s", err, failure)
	}
}

// githubNameAlphabet is what GitHub allows in a repository name; an owner
// is the same less "." and "_".
const githubNameAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._"

func genGitHubName(r *rand.Rand, alphabet string) string {
	var b strings.Builder
	for range 1 + r.Intn(14) {
		b.WriteByte(alphabet[r.Intn(len(alphabet))])
	}
	return b.String()
}

// pullRequestConfig generates the pull request records an intent keeps: one
// to four, each in a repository GitHub could name (the Project's URLs are
// the operator's, and its pull requests' URLs GitHub's), now and then one
// of them spelled as an operator might, mixed case and punctuated, or twice
// in different case.
func pullRequestConfig(seed int64) *quick.Config {
	return &quick.Config{
		MaxCount: 1500,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			prs := make([]IntentPullRequest, 1+r.Intn(4))
			for i := range prs {
				repo := genGitHubName(r, githubNameAlphabet[:63]) + "/" + genGitHubName(r, githubNameAlphabet)
				switch r.Intn(6) {
				case 0:
					repo = testAPI
				case 1:
					if i > 0 {
						repo = strings.ToUpper(prs[0].Repository)
					}
				}
				number := int64(1 + r.Intn(5000))
				prs[i] = IntentPullRequest{Repository: repo, Number: number,
					URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)}
			}
			args[0] = reflect.ValueOf(prs)
		},
	}
}

// TestSiblingCommentProperties: the siblings comment and the partial
// ending's notice link patchy's own pull requests and nothing else: no raw
// HTML, no closing keyword before any reference, and no mention; and the
// siblings comment lists every pull request but those in the repository it
// is posted on.
func TestSiblingCommentProperties(t *testing.T) {
	var failure string
	check := func(name, body string) bool {
		seen := parse(cutMarker(body))
		switch {
		case seen.rawHTML:
			failure = fmt.Sprintf("%s carries raw HTML:\n%s", name, body)
		case closingReference.MatchString(seen.prose):
			failure = fmt.Sprintf("%s has a closing keyword with a reference %q:\n%s",
				name, closingReference.FindString(seen.prose), body)
		case liveMention.MatchString(seen.prose):
			failure = fmt.Sprintf("%s mentions %q:\n%s", name, liveMention.FindString(seen.prose), body)
		default:
			return true
		}
		return false
	}
	holds := func(prs []IntentPullRequest) bool {
		siblings, err := RenderIntentSiblingsComment(IntentSiblingsComment{
			Namespace: "patchy", Intent: "marigold-3", Repository: prs[0].Repository, PullRequests: prs,
		})
		if err != nil {
			failure = fmt.Sprintf("siblings: %v", err)
			return false
		}
		if !check("siblings comment", siblings) {
			return false
		}
		var want []string
		for _, pr := range prs {
			if !strings.EqualFold(pr.Repository, prs[0].Repository) {
				want = append(want, fmt.Sprintf("\n- [%s#%d](%s)", pr.Repository, pr.Number, pr.URL))
			}
		}
		if listed := strings.Count(siblings, "\n- ["); listed != len(want) {
			failure = fmt.Sprintf("siblings comment lists %d pull requests, want %d:\n%s", listed, len(want), siblings)
			return false
		}
		for _, w := range want {
			if !strings.Contains(siblings, w) {
				failure = fmt.Sprintf("siblings comment does not list %q:\n%s", w, siblings)
				return false
			}
		}
		partial, err := RenderIntentPartialNotice(IntentPartialNotice{
			Namespace: "patchy", Intent: "marigold-3", Merged: prs[:1], Closed: prs[1:],
		})
		if err != nil {
			failure = fmt.Sprintf("partial: %v", err)
			return false
		}
		return check("partial notice", partial)
	}
	if err := quick.Check(holds, pullRequestConfig(20261004)); err != nil {
		t.Errorf("%v\n%s", err, failure)
	}
}

// TestNoticeKeys: the multi-repository comments carry their own notice
// keys, so a repeated pass finds the one it posted, and none is another
// comment's.
func TestNoticeKeys(t *testing.T) {
	keys := map[string]bool{}
	for _, k := range []string{SummaryKey, PartialKey, SiblingsKey} {
		if keys[k] {
			t.Errorf("notice key %q is used twice", k)
		}
		keys[k] = true
	}
	body, err := RenderIntentSiblingsComment(IntentSiblingsComment{
		Namespace: "patchy", Intent: "marigold-3", Repository: testAPI, PullRequests: testMultiPRs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := NoticeMarker("patchy", "marigold-3", SiblingsKey) + "\n"; !strings.HasPrefix(body, want) {
		t.Errorf("siblings comment is not headed by %q:\n%s", want, body)
	}
	body, err = RenderIntentPartialNotice(IntentPartialNotice{Namespace: "patchy", Intent: "marigold-3"})
	if err != nil {
		t.Fatal(err)
	}
	if want := NoticeMarker("patchy", "marigold-3", PartialKey) + "\n"; !strings.HasPrefix(body, want) {
		t.Errorf("partial notice is not headed by %q:\n%s", want, body)
	}
}
