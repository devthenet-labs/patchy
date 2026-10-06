// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

// The intents views' wire types. Each mirrors an interface of the same name
// in ui/src/types.ts; TestIntentWireTypesMatchTypeScript keeps the two in
// lockstep. Every string in them that is not the server's own fixed wording
// has passed through intentview.Text, and every link through
// intentview.SafeURL.

// Me is GET /api/me: who the caller is and which Projects the intents views
// show them, at which tier. It exists only while the intents views are
// enabled, so its absence is how the SPA knows they are off.
type Me struct {
	Name     string          `json:"name"`
	LoggedIn bool            `json:"loggedIn"`
	Projects []ProjectAccess `json:"projects"`
}

// ProjectAccess is one Project the caller may see and how much of it.
type ProjectAccess struct {
	Name string `json:"name"`
	// Tier is "intents" (the board, timeline and run numbers) or
	// "transcripts" (plans, reports and transcripts too).
	Tier string `json:"tier"`
}

// IntentBoard is GET /api/intents: the granted Projects and their intents.
type IntentBoard struct {
	GeneratedAt string         `json:"generatedAt"`
	Projects    []BoardProject `json:"projects"`
	Intents     []IntentCard   `json:"intents"`
}

// BoardProject is one granted Project's header on the board.
type BoardProject struct {
	Name         string        `json:"name"`
	Tier         string        `json:"tier"`
	Suspended    bool          `json:"suspended,omitempty"`
	Repositories []ProjectRepo `json:"repositories,omitempty"`
	Limits       IntentLimits  `json:"limits"`
}

// ProjectRepo is one of a Project's repositories: its key and owner/name.
type ProjectRepo struct {
	Key  string `json:"key"`
	Slug string `json:"slug,omitempty"`
}

// IntentLimits are the Project's limits with the schema defaults applied,
// plus the attempts a stage's round gets.
type IntentLimits struct {
	MaxRevisions    int32 `json:"maxRevisions"`
	MaxCheckFixes   int32 `json:"maxCheckFixes"`
	MaxCostMicroUSD int64 `json:"maxCostMicroUSD"`
	MaxAttempts     int32 `json:"maxAttempts"`
}

// IntentCard is one intent on the board.
type IntentCard struct {
	Name     string `json:"name"`
	Project  string `json:"project"`
	Issue    int64  `json:"issue"`
	IssueURL string `json:"issueURL,omitempty"`
	Phase    string `json:"phase"`
	// Column is the board column (intentview.ColumnFor).
	Column string `json:"column"`
	// BlockedFrom is the phase a Blocked intent resumes to, when known.
	BlockedFrom string `json:"blockedFrom,omitempty"`
	// BlockedReasons are the public reasons, never a condition's message.
	BlockedReasons []string `json:"blockedReasons,omitempty"`
	// Suspended is spec.suspend: nothing launches, and the status may be
	// stale, since a suspended intent is not reconciled.
	Suspended bool `json:"suspended,omitempty"`
	// Summary is the current plan's one-line summary (agent-written, shown
	// as plain text; the issue's status comment carries it too).
	Summary      string        `json:"summary,omitempty"`
	Repositories []string      `json:"repositories,omitempty"`
	PullRequests []IntentPR    `json:"pullRequests,omitempty"`
	Preview      *PreviewLink  `json:"preview,omitempty"`
	RunningRuns  []RunningRun  `json:"runningRuns,omitempty"`
	Attempt      *AttemptCount `json:"attempt,omitempty"`
	// Revisions and CheckFixes are the review/command and check-fix rounds
	// started that count against the Project's limits, failed ones included
	// (intentview.RevisionRounds, CheckFixRounds): what the limits are
	// enforced with, not the completed rounds status.revisions counts.
	Revisions    int32  `json:"revisions"`
	CheckFixes   int32  `json:"checkFixes"`
	CostMicroUSD int64  `json:"costMicroUSD"`
	RequestedBy  string `json:"requestedBy,omitempty"`
	RequestedAt  string `json:"requestedAt,omitempty"`
	PhaseSince   string `json:"phaseSince,omitempty"`
	CompletedAt  string `json:"completedAt,omitempty"`
}

// IntentPR is one pull request patchy opened for an intent.
type IntentPR struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	URL        string `json:"url,omitempty"`
	State      string `json:"state,omitempty"`
	MergedAt   string `json:"mergedAt,omitempty"`
}

// PreviewLink is an intent's preview, when there is one.
type PreviewLink struct {
	Phase string `json:"phase,omitempty"`
	// URL is set only while the preview is Ready.
	URL string `json:"url,omitempty"`
	// Revision is the commit the preview serves, cut to 12 characters.
	Revision       string `json:"revision,omitempty"`
	LastDeployedAt string `json:"lastDeployedAt,omitempty"`
}

// RunningRun is one IntentRun whose agent is pending or running. A
// multi-repository build runs one per repository at once, so the board lists
// them all rather than the intent's single activeRun.
type RunningRun struct {
	Name       string `json:"name"`
	Stage      string `json:"stage"`
	Repository string `json:"repository,omitempty"`
	Round      int32  `json:"round"`
	Attempt    int32  `json:"attempt"`
	Phase      string `json:"phase"`
	StartedAt  string `json:"startedAt,omitempty"`
}

// AttemptCount is the attempt of the intent's newest run, as counted toward
// the attempts its round gets (intentview.CountedAttempt), against them.
type AttemptCount struct {
	Stage   string `json:"stage"`
	Current int32  `json:"current"`
	Max     int32  `json:"max"`
}

// IntentDetail is GET /api/intents/{name}: the card plus everything the
// timeline tells, at the caller's tier for its Project.
type IntentDetail struct {
	IntentCard
	Tier       string           `json:"tier"`
	Limits     IntentLimits     `json:"limits"`
	PhaseTimes []PhaseTime      `json:"phaseTimes,omitempty"`
	Input      *IntentInputView `json:"input,omitempty"`
	Plan       *IntentPlanView  `json:"plan,omitempty"`
	Approval   *IntentApproval  `json:"approval,omitempty"`
	Rounds     int32            `json:"rounds,omitempty"`
	Runs       []IntentRunRow   `json:"runs"`
	// PlanTexts are the plan revisions whose text a tier 2 reader can open.
	PlanTexts []int32 `json:"planTexts,omitempty"`
}

// IntentInputView is the issue snapshot the current plan was made from.
type IntentInputView struct {
	Revision int32  `json:"revision"`
	Digest   string `json:"digest"`
}

// IntentPlanView is the current plan's metadata; its text is tier 2.
type IntentPlanView struct {
	Revision     int32    `json:"revision"`
	Digest       string   `json:"digest"`
	Summary      string   `json:"summary,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
	PostedAt     string   `json:"postedAt,omitempty"`
	CommentURL   string   `json:"commentURL,omitempty"`
}

// IntentApproval is the accepted approval and exactly what it approved. It
// is a GitHub fact the controller recorded; the dashboard only shows it.
type IntentApproval struct {
	By           string `json:"by"`
	Source       string `json:"source"`
	At           string `json:"at"`
	PlanRevision int32  `json:"planRevision"`
	PlanDigest   string `json:"planDigest"`
	InputDigest  string `json:"inputDigest"`
}

// IntentRunRow is one IntentRun in the timeline.
type IntentRunRow struct {
	Name       string `json:"name"`
	Stage      string `json:"stage"`
	Trigger    string `json:"trigger,omitempty"`
	Repository string `json:"repository,omitempty"`
	Round      int32  `json:"round"`
	Attempt    int32  `json:"attempt"`
	Phase      string `json:"phase,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	// Reason is the outcome in public wording; never the run's detail.
	Reason       string `json:"reason,omitempty"`
	CreatedAt    string `json:"createdAt,omitempty"`
	StartedAt    string `json:"startedAt,omitempty"`
	FinishedAt   string `json:"finishedAt,omitempty"`
	CostMicroUSD int64  `json:"costMicroUSD,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	BaseSHA      string `json:"baseSHA,omitempty"`
	PushedCommit string `json:"pushedCommit,omitempty"`
	// ImageSource is "repository" or "default"; ImageDigest the first 12
	// hex of the image digest when it was pinned by one. The image reference
	// itself (its registry host) is never sent.
	ImageSource string `json:"imageSource,omitempty"`
	ImageDigest string `json:"imageDigest,omitempty"`
	// Transcript says whether a recorded conversation exists (tier 2 opens it).
	Transcript *TranscriptSummary `json:"transcript,omitempty"`
	Grant      *RunGrant          `json:"grant,omitempty"`
	Running    bool               `json:"running,omitempty"`
}

// RunGrant is what the run was granted.
type RunGrant struct {
	MaxTurns            int32 `json:"maxTurns,omitempty"`
	TokenBudget         int64 `json:"tokenBudget,omitempty"`
	TimeoutMilliseconds int64 `json:"timeoutMilliseconds,omitempty"`
}

// IntentRunDetail is GET /api/intents/{name}/runs/{run}: the run panel.
type IntentRunDetail struct {
	IntentRunRow
	Intent  string `json:"intent"`
	Project string `json:"project"`
	Tier    string `json:"tier"`
	// CountedAttempt is this run's attempt as counted toward the attempts
	// its round gets (intentview.CountedAttempt). Attempt is its ordinal,
	// which runs ahead once an attempt did not count (its agent never ran).
	CountedAttempt int32 `json:"countedAttempt"`
	// LastAttempt: if this attempt fails, the intent fails (or, for a
	// revise round, the round ends).
	LastAttempt bool         `json:"lastAttempt,omitempty"`
	Limits      IntentLimits `json:"limits"`
	// IntentCostMicroUSD is the intent's recorded spend so far, the figure
	// the cost ceiling is checked against before the next launch.
	IntentCostMicroUSD int64 `json:"intentCostMicroUSD"`
	// Job is the agent Job's clock, while the run is live and the server can
	// read it.
	Job *RunJobClock `json:"job,omitempty"`
	// Report is the run's report, tier 2 only, as plain text.
	Report string `json:"report,omitempty"`
}

// RunJobClock is the agent Job's clock and the limits that stop the run.
type RunJobClock struct {
	CreatedAt          string `json:"createdAt,omitempty"`
	StartedAt          string `json:"startedAt,omitempty"`
	DeadlineSeconds    int64  `json:"deadlineSeconds,omitempty"`
	IdleTimeoutSeconds int64  `json:"idleTimeoutSeconds,omitempty"`
}

// IntentPlanText is GET /api/intents/{name}/plans/{revision}: one plan
// revision's text, tier 2 only, as plain text.
type IntentPlanText struct {
	Intent   string `json:"intent"`
	Revision int32  `json:"revision"`
	Digest   string `json:"digest,omitempty"`
	Current  bool   `json:"current,omitempty"`
	Text     string `json:"text"`
}

// RunActivity is the run stream's activity event: what a live run is doing,
// derived from its transcript without any of its text. Every tier gets it.
type RunActivity struct {
	Turns int `json:"turns"`
	// LastAt is the newest turn's time: "no activity since".
	LastAt string `json:"lastAt,omitempty"`
	// OpenTool is the tool the agent called last and has had no result for
	// yet, with when it was called.
	OpenTool      string `json:"openTool,omitempty"`
	OpenToolSince string `json:"openToolSince,omitempty"`
	Live          bool   `json:"live"`
}

// StreamNotice is the run stream's unavailable and end events' payload.
type StreamNotice struct {
	Reason string `json:"reason,omitempty"`
}
