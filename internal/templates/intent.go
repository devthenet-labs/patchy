// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/report"
)

// This file renders what intent-controller writes to GitHub. The renderers
// take plain values, never the intent resource types, and every value an
// agent wrote (a plan, its summary, questions, dependencies) goes through
// Sanitize, SanitizeInline or code here, before any template sees it.
// Controller values that quote agent output (a refusal naming a changeset
// path) are fenced. Logins are rendered as code, never as mentions: patchy
// notifies nobody.

// MaxCommentBytes is the most a rendered comment may be. GitHub refuses a
// body of more than 65,536 characters, and a character is at least a byte,
// so a body within this many bytes is always accepted.
const MaxCommentBytes = 65536

// ErrCommentTooLarge reports a rendered comment GitHub would refuse.
// Sanitising expands agent text (every "<" gains a backslash), so a plan
// within its own byte bound can still render past GitHub's; the plan is then
// too large to put in front of an approver, and must not be cut short, since
// the approver has to see all of what the build agent reads.
var ErrCommentTooLarge = errors.New("rendered comment exceeds GitHub's comment size limit")

// PlanDigest is the digest a plan's approval binds: "sha256:" and the hex
// SHA-256 of the report exactly as stored.
func PlanDigest(plan []byte) string {
	sum := sha256.Sum256(plan)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// shortDigest is a digest's first twelve hex digits, with its algorithm.
func shortDigest(digest string) string {
	algo, hexits, ok := strings.Cut(digest, ":")
	if !ok {
		algo, hexits = "sha256", digest
	}
	if len(hexits) > 12 {
		hexits = hexits[:12]
	}
	return algo + ":" + hexits
}

// IntentStatusMarker heads an intent's sticky status comment, which
// intent-controller edits in place.
func IntentStatusMarker(namespace, intent string) string {
	return fmt.Sprintf("<!-- patchy:intent %s/%s -->", markerField(namespace), markerField(intent))
}

// PlanMarker heads the comment posting plan revision of an intent; digest
// is the plan's PlanDigest, shortened in the marker.
func PlanMarker(namespace, intent string, revision int32, digest string) string {
	return fmt.Sprintf("<!-- patchy:plan %s/%s r%d %s -->",
		markerField(namespace), markerField(intent), revision, markerField(shortDigest(digest)))
}

// NoticeMarker heads a notice; key names what the notice answers (the
// command's comment, the label event), so a restart finds the reply it
// already posted instead of posting a second.
func NoticeMarker(namespace, intent, key string) string {
	return fmt.Sprintf("<!-- patchy:notice %s/%s %s -->", markerField(namespace), markerField(intent), markerField(key))
}

// markerField keeps a marker's fields to characters that cannot end the
// HTML comment or the line: names, numbers and digests need no others.
func markerField(s string) string {
	return strings.Map(func(r rune) rune {
		if isASCIIAlnum(r) || strings.ContainsRune("._:/-", r) {
			return r
		}
		return '_'
	}, s)
}

// IntentStatusComment is an intent's sticky status comment on its issue:
// where the work stands, the plan, the pull requests, the spend and what can
// be done next.
type IntentStatusComment struct {
	// Namespace and Intent name the Intent, for the marker.
	Namespace string
	Intent    string
	// Phase is the Intent's phase, such as "AwaitingApproval".
	Phase string
	// PlanRevision is the latest plan's revision; 0 before one is posted.
	PlanRevision int32
	// PlanURL links the latest plan's comment.
	PlanURL string
	// Summary is the latest plan's summary as the planner wrote it.
	Summary string
	// ApprovedBy is the login whose approval started the build, and
	// ApprovedRevision the plan revision approved; empty before approval.
	ApprovedBy       string
	ApprovedRevision int32
	// PullRequests are the pull requests patchy opened.
	PullRequests []IntentPullRequest
	// Revisions and MaxRevisions count the revision rounds against the
	// Project's limit; MaxRevisions 0 omits the line.
	Revisions    int32
	MaxRevisions int32
	// CostMicroUSD is the reported spend so far and MaxCostMicroUSD the
	// Project's ceiling; both 0 omit the line.
	CostMicroUSD    int64
	MaxCostMicroUSD int64
	// Reason explains a Blocked or Failed phase in the controller's words.
	// It may quote what an agent produced (a refused changeset path), so it
	// is shown fenced.
	Reason string
	// Commands are the verbs an approver can use in this phase, such as
	// "approve"; each is shown as its /patchy command.
	Commands []string
}

// IntentPullRequest is one pull request an intent opened.
type IntentPullRequest struct {
	// Repository is "owner/name".
	Repository string
	Number     int64
	URL        string
	// State is "open", "merged" or "closed".
	State string
}

// phaseSentences say what each phase means to someone reading the issue.
var phaseSentences = map[string]string{
	"Pending":          "patchy has seen this issue and is checking who asked for the work.",
	"Planning":         "patchy is writing a plan, which it will post here for approval.",
	"AwaitingApproval": "the plan is posted and waits for an approver.",
	"Building":         "patchy is building the approved plan.",
	"InReview":         "the pull requests are open for review.",
	"Revising":         "patchy is revising the pull requests from review feedback.",
	"Blocked":          "patchy has stopped until the reason below is dealt with.",
	"Merged":           "every pull request was merged, and the work is complete.",
	"Closed":           "work on this intent has stopped.",
	"Failed":           "patchy could not complete the work.",
}

// RenderIntentStatusComment renders the sticky status comment, headed by
// IntentStatusMarker.
func RenderIntentStatusComment(c IntentStatusComment) (string, error) {
	prs := make([]statusPR, len(c.PullRequests))
	for i, pr := range c.PullRequests {
		prs[i] = statusPR{
			Ref:   fmt.Sprintf("%s#%d", oneLine(pr.Repository), pr.Number),
			URL:   oneLine(pr.URL),
			State: pr.State,
		}
	}
	return render("intent_status.md.tmpl", struct {
		Marker           string
		Phase            string
		Sentence         string
		PlanRevision     int32
		PlanURL          string
		Summary          string
		ApprovedBy       string
		ApprovedRevision int32
		PullRequests     []statusPR
		Revisions        int32
		MaxRevisions     int32
		Cost             string
		MaxCost          string
		Reason           string
		Commands         []string
	}{
		Marker:           IntentStatusMarker(c.Namespace, c.Intent),
		Phase:            oneLine(c.Phase),
		Sentence:         phaseSentences[c.Phase],
		PlanRevision:     c.PlanRevision,
		PlanURL:          oneLine(c.PlanURL),
		Summary:          SanitizeInline(c.Summary),
		ApprovedBy:       oneLine(c.ApprovedBy),
		ApprovedRevision: c.ApprovedRevision,
		PullRequests:     prs,
		Revisions:        c.Revisions,
		MaxRevisions:     c.MaxRevisions,
		Cost:             usd(c.CostMicroUSD),
		MaxCost:          usd(c.MaxCostMicroUSD),
		Reason:           strings.TrimRight(plainText(c.Reason), "\n"),
		Commands:         commands(c.Commands),
	})
}

type statusPR struct {
	Ref, URL, State string
}

// PlanComment is one plan revision posted for approval.
type PlanComment struct {
	// Namespace and Intent name the Intent, for the marker.
	Namespace string
	Intent    string
	// Revision is the plan's revision, from 1.
	Revision int32
	// Report is the plan report exactly as stored: the bytes the approval's
	// digest binds and the build agent reads. The comment shows all of it —
	// the frontmatter verbatim, the body sanitised — and nothing else of the
	// agent's.
	Report []byte
	// Summary, NewDependencies and Questions are the report's parsed
	// frontmatter fields, called out for the approver.
	Summary         string
	NewDependencies []string
	Questions       []string
	// ApproveLabel and TriggerLabel are the Project's labels.
	ApproveLabel string
	TriggerLabel string
}

// RenderPlanComment renders a plan for approval, headed by PlanMarker over
// the report's digest. It fails with ErrCommentTooLarge rather than post a
// plan cut short.
func RenderPlanComment(p PlanComment) (string, error) {
	digest := PlanDigest(p.Report)
	body := report.StripFrontmatter(string(p.Report))
	// StripFrontmatter only slices, so the frontmatter is what precedes it.
	frontmatter := strings.TrimRight(plainText(string(p.Report)[:len(p.Report)-len(body)]), "\n")
	var data []string
	if frontmatter != "" {
		data = codeBlock("yaml", strings.Split(frontmatter, "\n"))
	}
	deps := make([]string, 0, len(p.NewDependencies))
	for _, d := range p.NewDependencies {
		if d = code(strings.TrimSpace(plainText(d))); d != "" {
			deps = append(deps, d)
		}
	}
	questions := make([]string, 0, len(p.Questions))
	for _, q := range p.Questions {
		if q = SanitizeInline(q); q != "" {
			questions = append(questions, q)
		}
	}
	out, err := render("intent_plan.md.tmpl", struct {
		Marker          string
		Revision        int32
		Digest          string
		Summary         string
		Body            string
		Data            string
		NewDependencies []string
		Questions       []string
		ApproveLabel    string
		TriggerLabel    string
	}{
		Marker:          PlanMarker(p.Namespace, p.Intent, p.Revision, digest),
		Revision:        p.Revision,
		Digest:          shortDigest(digest),
		Summary:         SanitizeInline(p.Summary),
		Body:            strings.Trim(Sanitize(body), "\n"),
		Data:            strings.Join(data, "\n"),
		NewDependencies: deps,
		Questions:       questions,
		ApproveLabel:    oneLine(p.ApproveLabel),
		TriggerLabel:    oneLine(p.TriggerLabel),
	})
	if err != nil {
		return "", err
	}
	if len(out) > MaxCommentBytes {
		return "", fmt.Errorf("plan r%d renders to %d bytes, over %d: %w",
			p.Revision, len(out), MaxCommentBytes, ErrCommentTooLarge)
	}
	return out, nil
}

// NotAllowedNotice answers a command, or the label standing for one, from
// someone who may not use it: not one of the Project's approvers, or a bot.
type NotAllowedNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Actor is the login that made the command or added the label.
	Actor string
	// Bot reports that the actor is a bot account.
	Bot bool
	// Verb is the command's verb. Label is set instead when the command
	// arrived as a label: the trigger label, or the approve label.
	Verb  string
	Label string
	// LabelRemoved reports that patchy removed the label again.
	LabelRemoved bool
	// Closed reports that patchy will not work on the issue because of it:
	// the trigger came from someone who may not start work.
	Closed bool
}

// RenderNotAllowedNotice renders a NotAllowedNotice.
func RenderNotAllowedNotice(n NotAllowedNotice) (string, error) {
	return render("intent_notice_not_allowed.md.tmpl", struct {
		Marker       string
		Actor        string
		Bot          bool
		Command      string
		Label        string
		LabelRemoved bool
		Closed       bool
	}{
		Marker:       NoticeMarker(n.Namespace, n.Intent, n.Key),
		Actor:        oneLine(n.Actor),
		Bot:          n.Bot,
		Command:      command(n.Verb),
		Label:        oneLine(n.Label),
		LabelRemoved: n.LabelRemoved,
		Closed:       n.Closed,
	})
}

// NotAvailableNotice answers a command, or the label standing for one, that
// means nothing in the intent's current phase.
type NotAvailableNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Verb is the command's verb, or Label the label it arrived as.
	Verb  string
	Label string
	// Phase is the Intent's phase.
	Phase string
	// Available are the verbs the phase does offer, if any.
	Available []string
}

// RenderNotAvailableNotice renders a NotAvailableNotice.
func RenderNotAvailableNotice(n NotAvailableNotice) (string, error) {
	return render("intent_notice_not_available.md.tmpl", struct {
		Marker    string
		Command   string
		Label     string
		Phase     string
		Available []string
	}{
		Marker:    NoticeMarker(n.Namespace, n.Intent, n.Key),
		Command:   command(n.Verb),
		Label:     oneLine(n.Label),
		Phase:     oneLine(n.Phase),
		Available: commands(n.Available),
	})
}

// ApprovalRefusedNotice answers an approval patchy refused because what it
// would approve is no longer what the approver was shown: the plan comment
// was edited after patchy posted it, or the issue changed since the plan
// was made.
type ApprovalRefusedNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Actor is the approver.
	Actor string
	// Label is the approve label when the approval arrived as one.
	Label string
	// PlanRevision is the plan the approval was for.
	PlanRevision int32
	// PlanChanged and IssueChanged say what changed; at least one is set.
	PlanChanged  bool
	IssueChanged bool
	// LabelRemoved reports that patchy removed the approve label.
	LabelRemoved bool
	// TriggerLabel is the Project's trigger label, re-applied to replan.
	TriggerLabel string
}

// RenderApprovalRefusedNotice renders an ApprovalRefusedNotice.
func RenderApprovalRefusedNotice(n ApprovalRefusedNotice) (string, error) {
	return render("intent_notice_approval_refused.md.tmpl", struct {
		Marker       string
		Actor        string
		Label        string
		PlanRevision int32
		PlanChanged  bool
		IssueChanged bool
		LabelRemoved bool
		TriggerLabel string
		Replan       string
	}{
		Marker:       NoticeMarker(n.Namespace, n.Intent, n.Key),
		Actor:        oneLine(n.Actor),
		Label:        oneLine(n.Label),
		PlanRevision: n.PlanRevision,
		PlanChanged:  n.PlanChanged,
		IssueChanged: n.IssueChanged,
		LabelRemoved: n.LabelRemoved,
		TriggerLabel: oneLine(n.TriggerLabel),
		Replan:       command("replan"),
	})
}

// IntentPRBody is the body of a pull request an intent opens. It refers to
// the intent issue with "Part of", never a closing keyword: patchy closes the
// intent issue itself once every pull request has merged, and nothing in the
// body may close an issue on merge — least of all a Finding's tracking issue
// in the same repository.
type IntentPRBody struct {
	// IntentRepository ("owner/name") and IssueNumber are the intent issue.
	IntentRepository string
	IssueNumber      int64
	// Summary is the approved plan's summary as the planner wrote it.
	Summary string
	// PlanRevision and PlanDigest are the approved plan's.
	PlanRevision int32
	PlanDigest   string
	// ApprovedBy is the approver's login.
	ApprovedBy string
}

// RenderIntentPRBody renders an intent pull request's body.
func RenderIntentPRBody(b IntentPRBody) (string, error) {
	return render("intent_pr_body.md.tmpl", struct {
		IntentRepository string
		IssueNumber      int64
		Summary          string
		PlanRevision     int32
		Digest           string
		ApprovedBy       string
	}{
		IntentRepository: oneLine(b.IntentRepository),
		IssueNumber:      b.IssueNumber,
		Summary:          SanitizeInline(b.Summary),
		PlanRevision:     b.PlanRevision,
		Digest:           shortDigest(oneLine(b.PlanDigest)),
		ApprovedBy:       oneLine(b.ApprovedBy),
	})
}

// IntentCommit is the commit patchy composes for an intent's changeset; the
// agent's own commit messages are dropped.
type IntentCommit struct {
	// Project is the Project's name, the subject's prefix.
	Project string
	// Summary is the approved plan's summary as the planner wrote it.
	Summary string
	// IntentRepository ("owner/name") and IssueNumber are the intent issue.
	IntentRepository string
	IssueNumber      int64
	// Round is the run's round (the IntentRun's spec.round).
	Round int32
	// Namespace and Intent name the Intent (the Patchy-Intent trailer), and
	// Run the IntentRun that produced the changeset (Patchy-Run).
	Namespace string
	Intent    string
	Run       string
}

// MaxCommitSummaryRunes bounds the plan summary in a commit subject, the
// plan contract's own bound on it.
const MaxCommitSummaryRunes = 200

// IntentCommitMessage composes the commit message for an intent changeset:
//
//	<project>: <summary> (<intent repository>#<issue>, round <k>)
//
//	Patchy-Intent: <namespace>/<intent>
//	Patchy-Run: <run>
//
// A commit message is plain text to GitHub, so inline code cannot neutralise
// anything in it: the summary is put on one line and bounded, and each
// mention or issue reference in it is broken apart with a space after its
// "@" or "#" (or before an issue URL's number), which GitHub does not read
// as one. Merged to the default branch, the message therefore closes no
// issue, and the one reference it makes is to the intent issue, after "(",
// where no closing keyword can precede it.
func IntentCommitMessage(c IntentCommit) string {
	summary := strings.TrimSpace(strings.ReplaceAll(plainText(c.Summary), "\n", " "))
	if utf8.RuneCountInString(summary) > MaxCommitSummaryRunes {
		summary = string([]rune(summary)[:MaxCommitSummaryRunes-1]) + "…"
	}
	return fmt.Sprintf("%s: %s (%s#%d, round %d)\n\nPatchy-Intent: %s/%s\nPatchy-Run: %s\n",
		oneLine(c.Project), defang(summary), oneLine(c.IntentRepository), c.IssueNumber, c.Round,
		oneLine(c.Namespace), oneLine(c.Intent), oneLine(c.Run))
}

// defang breaks every mention and issue reference in plain text s so GitHub
// reads none: a space after a mention's "@", and before a reference's
// number. Plain text has no code to hide a token in, so each shape is broken
// wherever it occurs — inside a URL, a word or a character reference
// ("&#64;" holds "#64") alike.
func defang(s string) string {
	for _, d := range defangs {
		s = d.re.ReplaceAllString(s, d.repl)
	}
	return s
}

// defangs are the shapes GitHub reads as a reference or a mention, each with
// the replacement that breaks it. None creates another's shape.
var defangs = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`#([0-9])`), "# $1"},
	{regexp.MustCompile(`(?i)(github\.com/[^\s/]+/[^\s/]+/(?:issues|pulls?|discussions)/)([0-9])`), "$1 $2"},
	{regexp.MustCompile(`(^|[^A-Za-z0-9])((?i:gh)-)([0-9])`), "$1$2 $3"},
	{regexp.MustCompile(`(^|[^A-Za-z0-9])@([A-Za-z0-9])`), "$1@ $2"},
}

// oneLine keeps a controller value on one line of its markdown or message.
func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(plainText(s), "\n", " "))
}

// command renders verb as its /patchy command, in code.
func command(verb string) string {
	if verb = oneLine(verb); verb == "" {
		return ""
	}
	return code("/patchy " + verb)
}

func commands(verbs []string) []string {
	out := make([]string, 0, len(verbs))
	for _, v := range verbs {
		if c := command(v); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// usd renders micro-USD as dollars and cents, rounded down; "" for zero or
// less.
func usd(micro int64) string {
	if micro <= 0 {
		return ""
	}
	cents := micro / 10_000
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
