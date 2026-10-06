// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// The multi-repository Project the tests plan over: the planning
// repository, which is the working tree, and an API named the way an
// operator may name a repository.
const (
	webURL = "https://github.com/devthenet-labs/marigold-web"
	apiURL = "https://github.com/devthenet-labs/Acme.Web_App"
)

// multiPlan is a plan over both repositories, as a multi-repository planner
// writes it.
const multiPlan = `---
summary: "Show the API's greeting on the home page"
repositories:
  - "` + webURL + `"
  - "` + apiURL + `"
new_dependencies: []
questions: []
confidence: 0.8
estimated_max_turns: 40
estimated_token_budget: 200000
---

## Approach

The API serves GET /api/greeting; the page fetches it.
`

// treesWorkspace is a plan Job's workspace as the prepare init leaves it
// for a two-repository Project: the working tree, the API's tree under
// repos/, and the manifest naming both.
func treesWorkspace(t *testing.T, out *bytes.Buffer) (Config, string) {
	t.Helper()
	cfg, ws := intentConfig(t, PhasePlan, out)
	cfg.Repo = "devthenet-labs/marigold-web"
	brokeredClaude(t, &cfg)
	api := filepath.Join(ws, TreesDir, "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(api, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, ws, fmt.Sprintf("web %s %s\napi %s %s\n", filepath.Join(ws, "repo"), webURL, api, apiURL))
	return cfg, ws
}

// brokeredClaude runs cfg's stages on the brokered claude harness, so the
// one command a stage runs carries its prompt (the fake harness's does not).
func brokeredClaude(t *testing.T, cfg *Config) {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("caller-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.InvestigateHarness, cfg.RemediateHarness, cfg.BrokerTokenFile = "claude", "claude", tokenFile
}

func writeManifest(t *testing.T, ws, manifest string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, "input", RepositoriesManifest), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// request is the plan's request, as the workspace holds it.
func request(t *testing.T, ws string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(ws, "input", "issue.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// planPrompt is the prompt the one stage command was given.
func planPrompt(t *testing.T, fx *fakeExec) string {
	t.Helper()
	if len(fx.specs) != 1 {
		t.Fatalf("commands run = %d, want the one plan command", len(fx.specs))
	}
	argv := fx.specs[0].Argv
	i := slices.Index(argv, "-p")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv lacks -p: %q", argv)
	}
	return argv[i+1]
}

// TestPlanOverTrees: a plan Job holding several trees tells the planner
// where each is, on the same read-only posture and directories as a
// one-repository plan, and an ok plan naming repositories among them is
// reported as written.
func TestPlanOverTrees(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := treesWorkspace(t, &out)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"reports/plan.md": multiPlan,
	}}}}
	ev := onlyEvent(t, cfg, fx, &out)
	if ev.Plan == nil || ev.Plan.Outcome != envelope.OutcomeOK {
		t.Fatalf("event = %+v, want an ok plan", ev)
	}
	if ev.Plan.ReportMarkdown != multiPlan || !slices.Equal(ev.Plan.Repositories, []string{webURL, apiURL}) {
		t.Errorf("plan = %+v, want the report as written", ev.Plan)
	}
	prompt := planPrompt(t, fx)
	want, err := templates.RenderPlanPrompt(templates.PlanPrompt{
		IssuePath: filepath.Join(ws, "input", "issue.md"), ReportPath: filepath.Join(ws, "reports", "plan.md"),
		Intent:        request(t, ws),
		BuildMaxTurns: cfg.RemediateManualMaxTurns, BuildTokenBudget: cfg.RemediateManualTokenBudget,
		Limits: ungrantedPlanLimits(cfg),
		Trees: []templates.PlanTree{
			{URL: webURL, Path: filepath.Join(ws, "repo")},
			{URL: apiURL, Path: filepath.Join(ws, TreesDir, "api")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if prompt != want {
		t.Errorf("prompt is not the plan prompt over the manifest's trees:\n%s\n--- want ---\n%s", prompt, want)
	}

	// The posture and directories are a one-repository plan's: the other
	// trees sit under the workspace the stage already adds.
	var single bytes.Buffer
	scfg, sws := intentConfig(t, PhasePlan, &single)
	brokeredClaude(t, &scfg)
	sfx := &fakeExec{steps: []step{{ws: sws, stdout: streamSuccess, writes: map[string]string{
		"reports/plan.md": goodPlan,
	}}}}
	onlyEvent(t, scfg, sfx, &single)
	if got, want := withoutPrompt(fx.specs[0].Argv, ws), withoutPrompt(sfx.specs[0].Argv, sws); !slices.Equal(got, want) {
		t.Errorf("argv over trees = %q, want a one-repository plan's %q", got, want)
	}
	if !slices.Contains(fx.specs[0].Argv, ws) {
		t.Errorf("argv = %q, want the workspace, which holds every tree, added", fx.specs[0].Argv)
	}
}

// withoutPrompt is argv with the prompt and the session id blanked and the
// workspace path made generic, for comparing two stages' invocations.
func withoutPrompt(argv []string, ws string) []string {
	out := slices.Clone(argv)
	for i := range out {
		if i > 0 && (out[i-1] == "-p" || out[i-1] == "--session-id") {
			out[i] = ""
		}
		out[i] = strings.ReplaceAll(out[i], ws, "/workspace")
	}
	return out
}

// TestPlanWithoutManifestUnchanged: a one-repository plan Job has no
// manifest, and its prompt is the one-repository prompt exactly.
func TestPlanWithoutManifestUnchanged(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := intentConfig(t, PhasePlan, &out)
	brokeredClaude(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"reports/plan.md": goodPlan,
	}}}}
	if ev := onlyEvent(t, cfg, fx, &out); ev.Plan == nil || ev.Plan.Outcome != envelope.OutcomeOK {
		t.Fatalf("event = %+v, want an ok plan", ev)
	}
	want, err := templates.RenderPlanPrompt(templates.PlanPrompt{
		IssuePath: filepath.Join(ws, "input", "issue.md"), ReportPath: filepath.Join(ws, "reports", "plan.md"),
		Intent:        request(t, ws),
		BuildMaxTurns: cfg.RemediateManualMaxTurns, BuildTokenBudget: cfg.RemediateManualTokenBudget,
		Limits: ungrantedPlanLimits(cfg),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := planPrompt(t, fx); got != want {
		t.Errorf("one-repository prompt changed:\n%s", got)
	}
}

// TestPlanManifestRefused: a manifest that breaks its contract, or names a
// tree the init did not extract, ends the stage with a fatal event before
// any agent runs: a planner must never plan blind, nor read a tree from
// anywhere but its own directory.
func TestPlanManifestRefused(t *testing.T) {
	line := func(key, path, url string) string { return key + " " + path + " " + url + "\n" }
	tests := []struct {
		name     string
		manifest func(ws string) string
		want     string
	}{
		{"a missing tree", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) +
				line("docs", filepath.Join(ws, TreesDir, "docs"), "https://github.com/devthenet-labs/docs")
		}, `the tree of "docs"`},
		{"one repository", func(ws string) string { return line("web", filepath.Join(ws, "repo"), webURL) },
			"at least 2"},
		{"empty", func(string) string { return "" }, "at least 2"},
		{"a short line", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) + "api " + filepath.Join(ws, TreesDir, "api") + "\n"
		}, `line 2 is not "<key> <path> <url>"`},
		{"a blank line", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) + "\n" +
				line("api", filepath.Join(ws, TreesDir, "api"), apiURL)
		}, `line 2 is not "<key> <path> <url>"`},
		{"a bad key", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) +
				line("API", filepath.Join(ws, TreesDir, "API"), apiURL)
		}, `key "API" is not a Project repository key`},
		{"a key listed twice", func(ws string) string {
			return line("api", filepath.Join(ws, "repo"), webURL) +
				line("api", filepath.Join(ws, TreesDir, "api"), apiURL)
		}, `key "api" is listed twice`},
		{"a tree outside its directory", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) + line("api", "/etc", apiURL)
		}, `"api" is at "/etc"`},
		{"a tree reached through ..", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) +
				line("api", filepath.Join(ws, TreesDir, "api")+"/../../repo", apiURL)
		}, `"api" is at`},
		{"the working tree not first", func(ws string) string {
			return line("api", filepath.Join(ws, TreesDir, "api"), apiURL) +
				line("web", filepath.Join(ws, "repo"), webURL)
		}, `"api" is at`},
		{"a URL of another shape", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) +
				line("api", filepath.Join(ws, TreesDir, "api"), "http://github.com/devthenet-labs/api")
		}, "is not an https://<host>/<owner>/<name> URL"},
		{"a repository listed twice", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) +
				line("api", filepath.Join(ws, TreesDir, "api"), "https://GITHUB.com/devthenet-labs/Marigold-Web.GIT")
		}, "is listed twice"},
		{"too many repositories", func(ws string) string {
			s := line("web", filepath.Join(ws, "repo"), webURL)
			for i := range report.PlanMaxRepositories {
				key := fmt.Sprintf("r%d", i)
				s += line(key, filepath.Join(ws, TreesDir, key), fmt.Sprintf("https://github.com/acme/r%d", i))
			}
			return s
		}, "over 8"},
		{"oversized", func(ws string) string {
			return line("web", filepath.Join(ws, "repo"), webURL) + strings.Repeat("#", manifestMaxBytes)
		}, "over 16384 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := treesWorkspace(t, &out)
			writeManifest(t, ws, tt.manifest(ws))
			fx := &fakeExec{}
			err := New(cfg, fx).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run() error = %v, want it to contain %q", err, tt.want)
			}
			evs := events(t, out.String())
			if len(evs) != 1 || evs[0].Type != envelope.TypeFatal || !strings.Contains(evs[0].Error, tt.want) {
				t.Errorf("events = %+v, want one fatal event naming %q", evs, tt.want)
			}
			if len(fx.specs) != 0 {
				t.Errorf("commands run = %d, want none: no agent may run", len(fx.specs))
			}
		})
	}
}

// TestPlanOutsideTheTrees: a plan naming a repository the Job did not hold
// is invalid, and its retry is told which and why; one naming them in
// another case or with .git is not. The manifest is read before the agent
// runs, so an agent that rewrites it changes nothing.
func TestPlanOutsideTheTrees(t *testing.T) {
	const evil = "https://github.com/devthenet-labs/evil"
	tests := []struct {
		name    string
		plan    string
		writes  map[string]string
		outcome envelope.Outcome
	}{
		{"a repository outside the trees", strings.Replace(multiPlan, apiURL, evil, 1), nil,
			envelope.OutcomeReportInvalid},
		{"the manifest rewritten to admit it", strings.Replace(multiPlan, apiURL, evil, 1),
			map[string]string{"input/" + RepositoriesManifest: "web x " + webURL + "\nevil y " + evil + "\n"},
			envelope.OutcomeReportInvalid},
		{"the trees in another spelling",
			strings.Replace(multiPlan, apiURL, "https://GitHub.com/DEVTHENET-LABS/acme.web_app.git", 1), nil,
			envelope.OutcomeOK},
		{"only some of the trees", strings.Replace(multiPlan, "  - \""+webURL+"\"\n", "", 1), nil,
			envelope.OutcomeOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := treesWorkspace(t, &out)
			writes := map[string]string{"reports/plan.md": tt.plan}
			for k, v := range tt.writes {
				writes[k] = v
			}
			p := onlyEvent(t, cfg, &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: writes}}}, &out).Plan
			if p == nil || p.Outcome != tt.outcome {
				t.Fatalf("plan = %+v, want outcome %q", p, tt.outcome)
			}
			if tt.outcome == envelope.OutcomeOK {
				return
			}
			for _, want := range []string{`repositories[1] "` + evil + `"`, webURL, apiURL,
				"exactly as the request lists it"} {
				if !strings.Contains(p.Detail, want) {
					t.Errorf("detail = %q, want it to say %q", p.Detail, want)
				}
			}

			// The retry quotes why.
			var retryOut bytes.Buffer
			retry, rws := treesWorkspace(t, &retryOut)
			retry.PreviousAttempt = &templates.PreviousAttempt{Attempt: 1, Outcome: string(p.Outcome), Detail: p.Detail}
			fx := &fakeExec{steps: []step{{ws: rws, stdout: streamSuccess, writes: map[string]string{
				"reports/plan.md": multiPlan,
			}}}}
			onlyEvent(t, retry, fx, &retryOut)
			if prompt := planPrompt(t, fx); !strings.Contains(prompt, `is not one of the repositories this plan `+
				`was made from`) {
				t.Errorf("the retry's prompt does not say why the plan was refused:\n%s", prompt)
			}
		})
	}
}

// buildConfig is a build Job's config handed plan as the approved plan, in
// the repository repo ("owner/name", as the controller's PATCHY_REPO spells
// it).
func buildConfig(t *testing.T, plan, repo string, out *bytes.Buffer) (Config, string) {
	t.Helper()
	cfg, ws := intentConfig(t, PhaseBuild, out)
	if err := os.WriteFile(filepath.Join(ws, "input", "investigation.md"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Repo = repo
	brokeredClaude(t, &cfg)
	return cfg, ws
}

// TestBuildScopedToItsRepository: a build of a multi-repository plan finds
// its own repository among the plan's, whatever the case or .git suffix,
// and is told it and its siblings, as the plan spells them.
func TestBuildScopedToItsRepository(t *testing.T) {
	for _, repo := range []string{
		"devthenet-labs/Acme.Web_App", "DEVTHENET-LABS/acme.web_app", "devthenet-labs/Acme.Web_App.git",
	} {
		t.Run(repo, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := buildConfig(t, multiPlan, repo, &out)
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess,
				writes:    map[string]string{"reports/build.md": goodBuild, "commit.sh": buildCommitScript},
				repoWrite: map[string]string{"app.js": "version();\n"},
			}}}
			ev := onlyEvent(t, cfg, fx, &out)
			if ev.Remediation == nil || ev.Remediation.Outcome != envelope.OutcomeOK || !ev.Remediation.Success {
				t.Fatalf("event = %+v, want a successful build", ev)
			}
			want, err := templates.RenderBuildPrompt(templates.BuildPrompt{
				PlanPath:         filepath.Join(ws, "input", "investigation.md"),
				ReportPath:       filepath.Join(ws, "reports", "build.md"),
				CommitScriptPath: filepath.Join(ws, "commit.sh"),
				Limits:           ungrantedBuildLimits(cfg),
				ThisRepository:   apiURL, Siblings: []string{webURL},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := planPrompt(t, fx); got != want {
				t.Errorf("build prompt is not scoped to %s:\n%s", apiURL, got)
			}
		})
	}
}

// TestBuildOneRepositoryUnscoped: a one-repository plan's build is told
// nothing of repositories, as before slice 3, whatever PATCHY_REPO says.
func TestBuildOneRepositoryUnscoped(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := buildConfig(t, goodPlan, "someone/else", &out)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess,
		writes:    map[string]string{"reports/build.md": goodBuild, "commit.sh": buildCommitScript},
		repoWrite: map[string]string{"app.js": "version();\n"},
	}}}
	onlyEvent(t, cfg, fx, &out)
	want, err := templates.RenderBuildPrompt(templates.BuildPrompt{
		PlanPath:         filepath.Join(ws, "input", "investigation.md"),
		ReportPath:       filepath.Join(ws, "reports", "build.md"),
		CommitScriptPath: filepath.Join(ws, "commit.sh"),
		Limits:           ungrantedBuildLimits(cfg),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := planPrompt(t, fx); got != want {
		t.Errorf("one-repository build prompt changed:\n%s", got)
	}
}

// TestBuildWithoutItsRepositoryRefused: a build of a multi-repository plan
// that finds its repository in none of the plan's entries, or in two (one
// owner/name on two hosts), cannot know which steps are its own, and is
// refused with a fatal event before any agent runs.
func TestBuildWithoutItsRepositoryRefused(t *testing.T) {
	twoHosts := strings.Replace(multiPlan, webURL, "https://ghe.example.com/devthenet-labs/Acme.Web_App", 1)
	tests := []struct {
		name, plan, repo, want string
	}{
		{"in none", multiPlan, "devthenet-labs/patchy-target", "2 of them are"},
		{"in none, by host", multiPlan, "github.com/devthenet-labs/Acme.Web_App", "0 of them are"},
		{"in two", twoHosts, "devthenet-labs/acme.web_app", "2 of them are"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, _ := buildConfig(t, tt.plan, tt.repo, &out)
			fx := &fakeExec{}
			err := New(cfg, fx).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "builds exactly one of them") {
				t.Fatalf("Run() error = %v, want the build refused", err)
			}
			evs := events(t, out.String())
			if len(evs) != 1 || evs[0].Type != envelope.TypeFatal {
				t.Errorf("events = %+v, want one fatal event", evs)
			}
			if len(fx.specs) != 0 {
				t.Errorf("commands run = %d, want none", len(fx.specs))
			}
		})
	}
}

func TestScopeBuild(t *testing.T) {
	tests := []struct {
		name     string
		urls     []string
		repo     string
		this     string
		siblings []string
		err      string
	}{
		{"one repository", []string{webURL}, "anyone/anything", "", nil, ""},
		{"no repository", nil, "acme/web", "", nil, ""},
		{"first of two", []string{webURL, apiURL}, "devthenet-labs/marigold-web", webURL, []string{apiURL}, ""},
		{"last of three", []string{webURL, "https://github.com/acme/docs", apiURL}, "devthenet-labs/acme.web_app",
			apiURL, []string{webURL, "https://github.com/acme/docs"}, ""},
		{"none", []string{webURL, apiURL}, "acme/other", "", nil, "0 of them"},
		{"an empty repository", []string{webURL, apiURL}, "", "", nil, "0 of them"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, err := scopeBuild(tt.urls, tt.repo)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("scopeBuild() error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scope.this != tt.this || !slices.Equal(scope.siblings, tt.siblings) {
				t.Errorf("scopeBuild() = %+v, want this %q, siblings %q", scope, tt.this, tt.siblings)
			}
		})
	}
}
