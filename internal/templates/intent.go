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

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/command"
)

// This file renders what intent-controller writes to GitHub. The renderers
// take plain values, never the intent resource types. The plan an approver
// approves is shown verbatim, in a code block (RenderPlanComment); every
// other value an agent wrote (a plan's summary and dependencies, a pull
// request's summary) goes through Sanitize, SanitizeInline or code here,
// before any template sees it. Controller values that quote agent output (a
// refusal naming a changeset path) are fenced. Logins are rendered as code,
// never as mentions: patchy notifies nobody. Commands are named from
// internal/command and internal/action, so a reply here and the parser that
// reads the command cannot disagree on a verb.

// MaxCommentBytes is the most a rendered comment may be. GitHub refuses a
// body of more than 65,536 characters, and a character is at least a byte,
// so a body within this many bytes is always accepted.
const MaxCommentBytes = 65536

// ErrPlanRefused reports a plan no comment can show an approver in full,
// exactly as the build agent reads it: rendered for approval it is over
// MaxCommentBytes; it is not UTF-8, which a comment — text, sent to GitHub
// as JSON — cannot carry byte for byte; or it holds characters that render
// as nothing even in a code block and can carry text a model reads (tag
// characters, bidi controls, stray variation selectors: unshowable).
// Cutting, repairing or merely counting them would leave the approver
// approving something other than what the build agent reads, so the plan is
// refused instead: RenderPlanComment returns this error, wrapped, together
// with the notice to post in the plan's place, and intent-controller treats
// the plan as invalid, never offering it for approval.
var ErrPlanRefused = errors.New("plan refused: no comment can show it to an approver exactly as written")

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
	// is shown fenced, any character in it that renders as nothing shown by
	// its code point.
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
var phaseSentences = map[v1alpha1.IntentPhase]string{
	v1alpha1.IntentPending:          "patchy has seen this issue and is checking who asked for the work.",
	v1alpha1.IntentPlanning:         "patchy is writing a plan, which it will post here for approval.",
	v1alpha1.IntentAwaitingApproval: "the plan is posted and waits for an approver.",
	v1alpha1.IntentBuilding:         "patchy is building the approved plan.",
	v1alpha1.IntentInReview:         "the pull requests are open for review.",
	v1alpha1.IntentRevising:         "patchy is revising the pull requests from review feedback.",
	v1alpha1.IntentBlocked:          "patchy has stopped until the reason below is dealt with.",
	v1alpha1.IntentMerged:           "every pull request was merged, and the work is complete.",
	v1alpha1.IntentClosed:           "work on this intent has stopped.",
	v1alpha1.IntentFailed:           "patchy could not complete the work.",
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
		Sentence:         phaseSentences[v1alpha1.IntentPhase(c.Phase)],
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
		Reason:           strings.TrimRight(visibleText(c.Reason), "\n"),
		Commands:         slashCommands(c.Commands),
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
	// digest binds and the build agent reads. The comment shows all of it,
	// verbatim, in a code block no line of it can close, last in the
	// comment; it renders none of it as markdown, so no markup in it can
	// hide from the approver or act on GitHub. What a code block cannot
	// show the approver at a glance, the header points to (planView):
	// characters that render as nothing, lines that run past the block's
	// right edge, long runs of blank lines. A report holding characters
	// that render as nothing and can carry text (unshowable) is refused.
	Report []byte
	// Summary and NewDependencies are the report's parsed frontmatter
	// fields, called out, sanitised, above the plan; Questions are counted
	// there, and read in the plan itself.
	Summary         string
	NewDependencies []string
	Questions       []string
	// ApproveLabel and TriggerLabel are the Project's labels.
	ApproveLabel string
	TriggerLabel string
}

// RenderPlanComment renders a plan for approval: headed by PlanMarker over
// the report's digest, then patchy's own words (the summary and new
// dependencies sanitised, how to approve), then the report verbatim in a
// fenced code block (```markdown, the fence longer than any run of
// backticks in the report), with nothing after it. Between the block's
// opening line and its closing fence is the report byte for byte, and a
// line break before the fence when the report does not end with one.
//
// A plan no comment can show in full, exactly as written, is refused: it
// returns the notice to post in the plan's place (headed by NoticeMarker,
// keyed plan-r<revision>, so nothing finds it as a plan to approve) and an
// error wrapping ErrPlanRefused. Any other error returns no body.
func RenderPlanComment(p PlanComment) (string, error) {
	digest := PlanDigest(p.Report)
	if !utf8.Valid(p.Report) {
		return refusePlan(p, digest, planRefusal{})
	}
	view := viewPlan(string(p.Report))
	if view.unshowable > 0 {
		return refusePlan(p, digest, planRefusal{unshowable: view.unshowable})
	}
	out, err := planComment(p, digest, view)
	if err != nil {
		return "", err
	}
	if len(out) > MaxCommentBytes {
		return refusePlan(p, digest, planRefusal{size: len(out)})
	}
	return out, nil
}

// planComment renders the plan comment whatever its size, with view the
// report's planView.
func planComment(p PlanComment, digest string, view planView) (string, error) {
	deps := make([]string, 0, len(p.NewDependencies))
	for _, d := range p.NewDependencies {
		if d = code(strings.TrimSpace(visibleText(d))); d != "" {
			deps = append(deps, d)
		}
	}
	questions := 0
	for _, q := range p.Questions {
		if strings.TrimSpace(q) != "" {
			questions++
		}
	}
	return render("intent_plan.md.tmpl", struct {
		Marker          string
		Revision        int32
		Digest          string
		ShortDigest     string
		Summary         string
		Invisible       string
		Wide            string
		ViewColumns     int
		BlankRun        int
		NewDependencies []string
		Questions       string
		ApproveLabel    string
		TriggerLabel    string
		Approve         string
		Replan          string
		Cancel          string
		Plan            string
	}{
		Marker:          PlanMarker(p.Namespace, p.Intent, p.Revision, digest),
		Revision:        p.Revision,
		Digest:          digest,
		ShortDigest:     shortDigest(digest),
		Summary:         SanitizeInline(p.Summary),
		Invisible:       count(view.invisible, "1 character", "characters"),
		Wide:            count(view.wide, "1 line", "lines"),
		ViewColumns:     planViewColumns,
		BlankRun:        view.blankRun,
		NewDependencies: deps,
		Questions:       count(questions, "a question", "questions"),
		ApproveLabel:    oneLine(p.ApproveLabel),
		TriggerLabel:    oneLine(p.TriggerLabel),
		Approve:         slashCommand(action.VerbApprove),
		Replan:          slashCommand(action.VerbReplan),
		Cancel:          slashCommand(action.VerbCancel),
		Plan:            verbatim("markdown", string(p.Report)),
	})
}

// planRefusal says why RenderPlanComment refuses a plan: size is what its
// comment came to, over MaxCommentBytes; unshowable is how many characters
// no comment can show it holds; neither set means it is not UTF-8.
type planRefusal struct {
	size, unshowable int
}

// refusePlan renders the notice posted in place of a plan RenderPlanComment
// refuses, with the error wrapping ErrPlanRefused.
func refusePlan(p PlanComment, digest string, why planRefusal) (string, error) {
	notice, err := render("intent_plan_refused.md.tmpl", struct {
		Marker       string
		Revision     int32
		Digest       string
		Size         int
		Limit        int
		Unshowable   string
		TriggerLabel string
		Replan       string
		Cancel       string
	}{
		Marker:       NoticeMarker(p.Namespace, p.Intent, fmt.Sprintf("plan-r%d", p.Revision)),
		Revision:     p.Revision,
		Digest:       shortDigest(digest),
		Size:         why.size,
		Limit:        MaxCommentBytes,
		Unshowable:   count(why.unshowable, "1 character", "characters"),
		TriggerLabel: oneLine(p.TriggerLabel),
		Replan:       slashCommand(action.VerbReplan),
		Cancel:       slashCommand(action.VerbCancel),
	})
	if err != nil {
		return "", err
	}
	switch {
	case why.size > 0:
		return notice, fmt.Errorf("plan r%d renders to %d bytes, over %d: %w",
			p.Revision, why.size, MaxCommentBytes, ErrPlanRefused)
	case why.unshowable > 0:
		return notice, fmt.Errorf("plan r%d holds %d characters no comment can show: %w",
			p.Revision, why.unshowable, ErrPlanRefused)
	}
	return notice, fmt.Errorf("plan r%d is not UTF-8: %w", p.Revision, ErrPlanRefused)
}

// verbatim renders content as a fenced code block holding it byte for byte:
// the fence is backticks, at least three and one more than the longest run
// of backticks in content, so no line of content can close it, however a
// reader splits the lines; a line break is added before the closing fence
// only when non-empty content does not end with one.
func verbatim(info, content string) string {
	f := strings.Repeat("`", max(3, longestBacktickRun(content)+1))
	var b strings.Builder
	b.Grow(2*len(f) + len(info) + len(content) + 2)
	b.WriteString(f + info + "\n" + content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(f)
	return b.String()
}

// count renders n of something: "" for none, one for one ("a question"),
// and n with many otherwise ("3 questions").
func count(n int, one, many string) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return one
	}
	return fmt.Sprintf("%d %s", n, many)
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
		Command:      slashCommand(n.Verb),
		Label:        oneLine(n.Label),
		LabelRemoved: n.LabelRemoved,
		Closed:       n.Closed,
	})
}

// NotAvailableNotice answers a command, or the label standing for one, that
// means nothing in the intent's current phase — a trigger label re-applied
// to an intent that has ended among them.
type NotAvailableNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Surface is where the command was made; zero means the intent issue.
	Surface command.Surface
	// Verb is the command's verb, or Label the label it arrived as.
	Verb  string
	Label string
	// LabelRemoved reports that patchy removed the label again.
	LabelRemoved bool
	// Phase is the Intent's phase. A Merged or Closed intent has ended, and
	// the notice says to open a new issue.
	Phase string
	// Available are the verbs the phase does admit, if any; the notice lists
	// those the surface offers, with command.HelpFor's usage lines.
	Available []string
}

// RenderNotAvailableNotice renders a NotAvailableNotice.
func RenderNotAvailableNotice(n NotAvailableNotice) (string, error) {
	surface := n.Surface
	if surface == "" {
		surface = command.IntentIssue
	}
	phase := v1alpha1.IntentPhase(n.Phase)
	return render("intent_notice_not_available.md.tmpl", struct {
		Marker       string
		Command      string
		Label        string
		LabelRemoved bool
		Phase        string
		Ended        bool
		Help         string
	}{
		Marker:       NoticeMarker(n.Namespace, n.Intent, n.Key),
		Command:      slashCommand(n.Verb),
		Label:        oneLine(n.Label),
		LabelRemoved: n.LabelRemoved,
		Phase:        oneLine(n.Phase),
		Ended:        phase == v1alpha1.IntentMerged || phase == v1alpha1.IntentClosed,
		Help:         command.HelpFor(surface, n.Available),
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
		Replan:       slashCommand(action.VerbReplan),
	})
}

// IntentPRBody is the body of a pull request an intent opens. It refers to
// the intent issue with "Part of", never a closing keyword: patchy closes the
// intent issue itself once every pull request has merged, and nothing in the
// body may close an issue on merge — least of all a Finding's tracking issue
// in the same repository.
//
// The body is read two ways. GitHub renders it as markdown on the pull
// request, and a repository may have GitHub copy it, as written, into the
// merge or squash commit on the default branch ("Pull request title and
// description"), where it is plain text and inline code neutralises
// nothing: a kept code span holding "closes #12" would close issue 12 there.
// So the summary is sanitised for the markdown reading and then defanged
// for the plain one, and read either way the body references only the
// intent issue, after "Part of", and mentions nobody.
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
		// defang only inserts a space between punctuation ("@", "#",
		// "GH-", an issue URL's path segment) and the letter or digit
		// after it, which no markdown construct turns on, so the result
		// stays inert markdown.
		Summary:      defang(SanitizeInline(b.Summary)),
		PlanRevision: b.PlanRevision,
		Digest:       shortDigest(oneLine(b.PlanDigest)),
		ApprovedBy:   oneLine(b.ApprovedBy),
	})
}

// IntentPRTitle composes the title of a pull request an intent opens:
//
//	<project>: <summary>
//
// GitHub links references and mentions in a title, and uses the title as
// the subject of a squash commit, plain text on the default branch; so,
// as in IntentCommitMessage, the summary is put on one line, bounded and
// defanged, and the title references and mentions nothing.
func IntentPRTitle(project, summary string) string {
	return defang(oneLine(project) + ": " + commitSummary(summary))
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
	return fmt.Sprintf("%s: %s (%s#%d, round %d)\n\nPatchy-Intent: %s/%s\nPatchy-Run: %s\n",
		oneLine(c.Project), commitSummary(c.Summary), oneLine(c.IntentRepository), c.IssueNumber, c.Round,
		oneLine(c.Namespace), oneLine(c.Intent), oneLine(c.Run))
}

// commitSummary is a plan summary as a commit subject or a pull request
// title carries it: on one line, at most MaxCommitSummaryRunes runes, and
// defanged.
func commitSummary(summary string) string {
	s := oneLine(summary)
	if utf8.RuneCountInString(s) > MaxCommitSummaryRunes {
		s = string([]rune(s)[:MaxCommitSummaryRunes-1]) + "…"
	}
	return defang(s)
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
	// An issue URL's number, on any host (a Forge may be GitHub Enterprise):
	// broken after the path segment alone, which any URL of the shape holds.
	{regexp.MustCompile(`(?i)(/(?:issues|pulls?|discussions)/)([0-9])`), "$1 $2"},
	{regexp.MustCompile(`(^|[^A-Za-z0-9])((?i:gh)-)([0-9])`), "$1$2 $3"},
	{regexp.MustCompile(`(^|[^A-Za-z0-9])@([A-Za-z0-9])`), "$1@ $2"},
}

// oneLine keeps a controller value on one line of its markdown or message.
func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(plainText(s), "\n", " "))
}

// slashCommand renders verb as its command ("/patchy <verb>"), in code.
func slashCommand(verb string) string {
	if verb = oneLine(verb); verb == "" {
		return ""
	}
	return code(command.Prefix + " " + verb)
}

func slashCommands(verbs []string) []string {
	out := make([]string, 0, len(verbs))
	for _, v := range verbs {
		if c := slashCommand(v); c != "" {
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
