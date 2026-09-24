// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
)

// Names derived from an Intent or an IntentRun. Each is created under its
// name once and adopted on AlreadyExists only when its controller owner is
// the one creating it.
func inputConfigMapName(intent string, revision int32) string {
	return fmt.Sprintf("%s-input-r%d", intent, revision)
}

func planConfigMapName(intent string, revision int32) string {
	return fmt.Sprintf("%s-plan-r%d", intent, revision)
}

func runInputName(run string) string { return run + "-input" }

func runRepositoryName(run string) string { return run + "-src" }

// branchName is the branch every pull request of the intent is opened from.
func branchName(intent string) string { return v1alpha1.IntentBranchPrefix + intent }

// ConfigMap data keys.
const (
	// keyIssue is the handoff every run's input ConfigMap carries as the
	// Job's issue.md, and the input snapshot's rendered request.
	keyIssue = "issue.md"
	// keyInvestigation is a build run's approved plan, the Job's
	// investigation.md.
	keyInvestigation = "investigation.md"
	// keyPlan is the plan report exactly as the planner wrote it.
	keyPlan = "plan.md"
	// The input snapshot's parts, kept beside the rendered request so an
	// approval can re-render it from the issue as it is now.
	keyTitle        = "title"
	keyBody         = "body"
	keyRepositories = "repositories"
	keyComments     = "comments"
)

// digest is the sha256 digest of b, as every intent digest is written.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// normalizeRepoURL compares repository URLs the way forges do:
// case-insensitively, with a trailing slash and any .git suffix dropped.
func normalizeRepoURL(u string) string {
	u = strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/"))
	return strings.TrimSuffix(u, ".git")
}

func sameRepo(a, b string) bool { return normalizeRepoURL(a) == normalizeRepoURL(b) }

// repoSlug is "owner/name" of a repository URL, or the URL itself when it
// does not parse.
func repoSlug(u string) string {
	if _, repo, err := forge.ParseRepoURL(u); err == nil {
		return repo.String()
	}
	return u
}

// terminal reports the phases an Intent ends in.
func terminal(p v1alpha1.IntentPhase) bool { return v1alpha1.IntentTerminal(p) }

// microUSD parses a UsageSummary cost ("1.234567") into micro-USD, the unit
// intent usage is summed in; "" and anything unparseable are zero.
func microUSD(cost string) int64 {
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

// Settings are the controller-wide limits the reconcilers share.
type Settings struct {
	// Namespace is where Projects, Intents, IntentRuns, Repositories and
	// ConfigMaps live.
	Namespace string
	// AgentNamespace is where the agent Jobs run, recorded on JobRef.
	AgentNamespace string
	// PollInterval paces discovery and every non-terminal intent's poll of
	// its issue; ApprovalPollInterval an intent awaiting approval's;
	// PRPollInterval an intent in review's poll of its pull requests.
	PollInterval         time.Duration
	ApprovalPollInterval time.Duration
	PRPollInterval       time.Duration
	// RateLimitFloor pauses polling while the installation has fewer core
	// requests left than this.
	RateLimitFloor int
	// MaxAttempts bounds the counted attempts of one stage's round.
	MaxAttempts int32
	// Plan and Build are the per-stage ceilings: a Project's limits are
	// clamped to them, and the Job's stage configuration carries them.
	Plan, Build StageCeiling
}

// StageCeiling bounds one agent stage.
type StageCeiling struct {
	MaxTurns    int32
	TokenBudget int64
	Timeout     time.Duration
}

// Defaults the design sets.
const (
	DefaultPollInterval         = 60 * time.Second
	DefaultApprovalPollInterval = 30 * time.Second
	DefaultPRPollInterval       = 60 * time.Second
	DefaultRateLimitFloor       = 1000
	DefaultMaxAttempts          = 2
	DefaultTTL                  = 14 * 24 * time.Hour
)

// withDefaults fills unset settings.
func (s Settings) withDefaults() Settings {
	if s.PollInterval <= 0 {
		s.PollInterval = DefaultPollInterval
	}
	if s.ApprovalPollInterval <= 0 {
		s.ApprovalPollInterval = DefaultApprovalPollInterval
	}
	if s.PRPollInterval <= 0 {
		s.PRPollInterval = DefaultPRPollInterval
	}
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = DefaultMaxAttempts
	}
	return s
}

// grant is what one run of stage is granted: the Project's limit for the
// stage clamped by the controller's ceiling (a zero limit takes the
// ceiling), and the ceiling's wall clock.
func (s Settings) grant(p *v1alpha1.Project, stage v1alpha1.IntentStage) v1alpha1.IntentRunGrant {
	ceiling, limits := s.Plan, p.Spec.Limits.Plan
	if stage != v1alpha1.IntentStagePlan {
		ceiling, limits = s.Build, p.Spec.Limits.Build
	}
	turns := ceiling.MaxTurns
	if limits.MaxTurns > 0 && limits.MaxTurns < turns {
		turns = limits.MaxTurns
	}
	tokens := ceiling.TokenBudget
	if limits.TokenBudget > 0 && limits.TokenBudget < tokens {
		tokens = limits.TokenBudget
	}
	return v1alpha1.IntentRunGrant{
		MaxTurns:            turns,
		TokenBudget:         tokens,
		TimeoutMilliseconds: ceiling.Timeout.Milliseconds(),
	}
}

// Project settings with the schema defaults applied, for a Project the API
// server never defaulted (a fake client's).
func approveLabel(p *v1alpha1.Project) string {
	if p.Spec.Labels.Approve != "" {
		return p.Spec.Labels.Approve
	}
	return v1alpha1.DefaultApproveLabel
}

func maxActiveIntents(p *v1alpha1.Project) int32 {
	if p.Spec.Limits.MaxActiveIntents > 0 {
		return p.Spec.Limits.MaxActiveIntents
	}
	return v1alpha1.DefaultMaxActiveIntents
}

func maxCostMicroUSD(p *v1alpha1.Project) int64 {
	if p.Spec.Limits.MaxCostMicroUSD > 0 {
		return p.Spec.Limits.MaxCostMicroUSD
	}
	return v1alpha1.DefaultMaxCostMicroUSD
}

func requireRepositoryImage(p *v1alpha1.Project) bool {
	return p.Spec.RequireRepositoryImage == nil || *p.Spec.RequireRepositoryImage
}

// isApprover reports whether login is one of the Project's approvers,
// compared case-insensitively as GitHub compares logins.
func isApprover(p *v1alpha1.Project, login string) bool {
	for _, a := range p.Spec.Approvers.Logins {
		if strings.EqualFold(a, login) {
			return true
		}
	}
	return false
}
