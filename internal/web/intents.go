// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/intentview"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

// ProjectGranter resolves a caller's read tier per Project;
// authz.ProjectReviewer is the SubjectAccessReview implementation and
// authz.FullProjects the development bypass.
type ProjectGranter interface {
	Tiers(ctx context.Context, id auth.Identity, projects []string) (map[string]authz.Tier, error)
}

// JobFacts reads an agent Job's clock; *jobs.Tailer implements it.
type JobFacts interface {
	Facts(ctx context.Context, jobName string) (jobs.Facts, error)
}

// IntentsOptions turn on the intents views (statusServer.intents.enabled).
// Turning them on also hardens the browser envelope of the whole server:
// Sec-Fetch-Site on every API and stream request, a missing one refused on
// writes, no-store on streams, open streams re-authorised, stream caps, CSP
// and HSTS.
type IntentsOptions struct {
	// Projects resolves the per-Project tiers. Required.
	Projects ProjectGranter
	// Facts reads a live run's Job clock; nil leaves it out of the run
	// panel.
	Facts JobFacts
	// ReauthPeriod is how often an open stream re-checks the grant it was
	// opened under (default: the access-review cache's 20 s).
	ReauthPeriod time.Duration
	// MaxStreamAge ends a stream so the browser reconnects through its
	// current session, which a signed-out or expired one fails (default 10m).
	MaxStreamAge time.Duration
}

// Defaults of IntentsOptions.
const (
	defaultReauthPeriod = 20 * time.Second
	defaultMaxStreamAge = 10 * time.Minute
	defaultRunPoll      = 2 * time.Second
)

// intentsState is the server's intents side; nil while the views are off.
type intentsState struct {
	projects ProjectGranter
	facts    *factsCache
	signals  *projectBroker
	limiter  *streamLimiter
	reauth   time.Duration
	maxAge   time.Duration
	// runPoll is how often the stream of a run that has not launched yet
	// re-reads it (from the cache) for its Job.
	runPoll time.Duration
}

// WithIntents enables the intents views.
func (s *Server) WithIntents(o IntentsOptions) *Server {
	if o.ReauthPeriod <= 0 {
		o.ReauthPeriod = defaultReauthPeriod
	}
	if o.MaxStreamAge <= 0 {
		o.MaxStreamAge = defaultMaxStreamAge
	}
	st := &intentsState{
		projects: o.Projects,
		signals:  newProjectBroker(maxIntentStreams),
		limiter:  newStreamLimiter(maxIdentityStreams, maxIntentStreams),
		reauth:   o.ReauthPeriod,
		maxAge:   o.MaxStreamAge,
		runPoll:  defaultRunPoll,
	}
	if o.Facts != nil {
		st.facts = &factsCache{src: o.Facts, now: s.now, entries: map[string]factsEntry{}}
	}
	s.intents = st
	return s
}

// registerIntents mounts the intents routes. Every one is a GET: this slice
// has no write path, and approval stays a GitHub fact.
func (s *Server) registerIntents(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/intents", s.handleIntentBoard)
	mux.HandleFunc("GET /api/intents/events", s.handleIntentEvents)
	mux.HandleFunc("GET /api/intents/{name}", s.handleIntentDetail)
	mux.HandleFunc("GET /api/intents/{name}/plans/{revision}", s.handleIntentPlan)
	mux.HandleFunc("GET /api/intents/{name}/runs/{run}", s.handleIntentRun)
	mux.HandleFunc("GET /api/intents/{name}/runs/{run}/stream", s.handleRunStream)
}

// Cache field indexes the intents views look objects up by.
const (
	// IntentRunIntentIndex indexes IntentRuns by spec.intentRef.name.
	IntentRunIntentIndex = "patchy.web/intent"
	// IntentProjectIndex indexes Intents by spec.project.
	IntentProjectIndex = "patchy.web/project"
)

// RegisterIntentIndexes installs the intents views' indexes on the manager's
// cache; call it before the manager starts, and only with the views on (the
// informers it implies need the intents RBAC).
func RegisterIntentIndexes(ctx context.Context, idx client.FieldIndexer) error {
	if err := idx.IndexField(ctx, &v1alpha1.IntentRun{}, IntentRunIntentIndex, IntentRunIntentIndexer); err != nil {
		return fmt.Errorf("index intent runs by intent: %w", err)
	}
	if err := idx.IndexField(ctx, &v1alpha1.Intent{}, IntentProjectIndex, IntentProjectIndexer); err != nil {
		return fmt.Errorf("index intents by project: %w", err)
	}
	return nil
}

// IntentRunIntentIndexer extracts a run's Intent name.
func IntentRunIntentIndexer(obj client.Object) []string {
	if run, ok := obj.(*v1alpha1.IntentRun); ok && run.Spec.IntentRef.Name != "" {
		return []string{run.Spec.IntentRef.Name}
	}
	return nil
}

// IntentProjectIndexer extracts an Intent's Project name.
func IntentProjectIndexer(obj client.Object) []string {
	if in, ok := obj.(*v1alpha1.Intent); ok && in.Spec.Project != "" {
		return []string{in.Spec.Project}
	}
	return nil
}

// tierName is a tier's wire name.
func tierName(t authz.Tier) string {
	switch t {
	case authz.TierIntents:
		return "intents"
	case authz.TierTranscripts:
		return "transcripts"
	}
	return ""
}

// errNotVisible is the one answer for an intent that does not exist and one
// in a Project the caller may not see: the two must be indistinguishable.
var errNotVisible = errors.New("not found")

// notFound writes the 404 every invisible intent, run and plan shares.
func notFound(w http.ResponseWriter) {
	http.Error(w, "not found", http.StatusNotFound)
}

// identify resolves the caller for an intents route, answering 401 or 500
// itself; ok is false when the request has been answered.
func (s *Server) identify(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id, err := s.auth.Identify(w, r)
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "identify failed", slog.Any("error", err))
		http.Error(w, "authentication failed", http.StatusInternalServerError)
		return auth.Identity{}, false
	}
	if id == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return auth.Identity{}, false
	}
	return *id, true
}

// projectTiers lists every Project and resolves the caller's tier for each.
func (s *Server) projectTiers(ctx context.Context, id auth.Identity) ([]v1alpha1.Project, map[string]authz.Tier,
	error) {
	var list v1alpha1.ProjectList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return nil, nil, fmt.Errorf("list projects: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.Project) int { return cmp.Compare(a.Name, b.Name) })
	names := make([]string, len(list.Items))
	for i := range list.Items {
		names[i] = list.Items[i].Name
	}
	tiers, err := s.intents.projects.Tiers(ctx, id, names)
	if err != nil {
		return nil, nil, err
	}
	return list.Items, tiers, nil
}

// visibleIntent loads the named Intent and the caller's tier for its
// Project. A missing Intent and an invisible one both return errNotVisible.
func (s *Server) visibleIntent(ctx context.Context, id auth.Identity, name string) (*v1alpha1.Intent,
	authz.Tier, error) {
	var in v1alpha1.Intent
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: name}, &in); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, authz.TierNone, errNotVisible
		}
		return nil, authz.TierNone, err
	}
	tiers, err := s.intents.projects.Tiers(ctx, id, []string{in.Spec.Project})
	if err != nil {
		return nil, authz.TierNone, err
	}
	tier := tiers[in.Spec.Project]
	if tier == authz.TierNone {
		return nil, authz.TierNone, errNotVisible
	}
	return &in, tier, nil
}

// intentRequest identifies the caller and loads the path's Intent at their
// tier, answering the request itself on failure.
func (s *Server) intentRequest(w http.ResponseWriter, r *http.Request) (auth.Identity, *v1alpha1.Intent,
	authz.Tier, bool) {
	id, ok := s.identify(w, r)
	if !ok {
		return id, nil, authz.TierNone, false
	}
	in, tier, err := s.visibleIntent(r.Context(), id, r.PathValue("name"))
	switch {
	case errors.Is(err, errNotVisible):
		notFound(w)
		return id, nil, authz.TierNone, false
	case err != nil:
		s.log.LogAttrs(r.Context(), slog.LevelError, "load intent", slog.String("intent", r.PathValue("name")),
			slog.Any("error", err))
		http.Error(w, "failed to load the intent", http.StatusInternalServerError)
		return id, nil, authz.TierNone, false
	}
	return id, in, tier, true
}

// intentRuns lists the Intent's runs, oldest first. A run whose
// spec.intentRef names this Intent by name but not by UID belongs to an
// earlier Intent of the same name and is left out.
func (s *Server) intentRuns(ctx context.Context, in *v1alpha1.Intent) ([]*v1alpha1.IntentRun, error) {
	var list v1alpha1.IntentRunList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace),
		client.MatchingFields{IntentRunIntentIndex: in.Name}); err != nil {
		return nil, fmt.Errorf("list runs of %s: %w", in.Name, err)
	}
	var out []*v1alpha1.IntentRun
	for i := range list.Items {
		run := &list.Items[i]
		if run.Spec.IntentRef.Name == in.Name && run.Spec.IntentRef.UID == in.UID {
			out = append(out, run)
		}
	}
	slices.SortFunc(out, func(a, b *v1alpha1.IntentRun) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out, nil
}

// intentPreview is the Intent's Preview, when one exists for this very
// Intent (its intentRef UID matches).
func (s *Server) intentPreview(ctx context.Context, in *v1alpha1.Intent) *v1alpha1.Preview {
	var pv v1alpha1.Preview
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: in.Name}, &pv); err != nil {
		return nil
	}
	if pv.Spec.IntentRef.UID != in.UID {
		return nil
	}
	return &pv
}

// handleMe serves who the caller is and which Projects they may see.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	projects, tiers, err := s.projectTiers(r.Context(), id)
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "project tiers", slog.Any("error", err))
		http.Error(w, "authorization failed", http.StatusInternalServerError)
		return
	}
	me := Me{Name: id.Display(), LoggedIn: id.Session, Projects: []ProjectAccess{}}
	for i := range projects {
		if t := tiers[projects[i].Name]; t != authz.TierNone {
			me.Projects = append(me.Projects, ProjectAccess{Name: projects[i].Name, Tier: tierName(t)})
		}
	}
	writeJSON(w, me)
}

// handleIntentBoard serves the board: the granted Projects and their
// intents. Projects the caller may not see are left out entirely.
func (s *Server) handleIntentBoard(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	board, err := s.buildBoard(r.Context(), id)
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "build intent board", slog.Any("error", err))
		http.Error(w, "failed to load intents", http.StatusInternalServerError)
		return
	}
	writeJSONGzip(w, r, board)
}

func (s *Server) buildBoard(ctx context.Context, id auth.Identity) (*IntentBoard, error) {
	projects, tiers, err := s.projectTiers(ctx, id)
	if err != nil {
		return nil, err
	}
	board := &IntentBoard{
		GeneratedAt: s.now().UTC().Format(time.RFC3339),
		Projects:    []BoardProject{},
		Intents:     []IntentCard{},
	}
	for i := range projects {
		p := &projects[i]
		tier := tiers[p.Name]
		if tier == authz.TierNone {
			continue
		}
		board.Projects = append(board.Projects, projectHeader(p, tier))
		var intents v1alpha1.IntentList
		if err := s.client.List(ctx, &intents, client.InNamespace(s.namespace),
			client.MatchingFields{IntentProjectIndex: p.Name}); err != nil {
			return nil, fmt.Errorf("list intents of %s: %w", p.Name, err)
		}
		slices.SortFunc(intents.Items, func(a, b v1alpha1.Intent) int {
			if c := b.Spec.RequestedBy.At.Compare(a.Spec.RequestedBy.At.Time); c != 0 {
				return c
			}
			return cmp.Compare(a.Name, b.Name)
		})
		limits := intentview.LimitsOf(p)
		for j := range intents.Items {
			in := &intents.Items[j]
			if in.Spec.Project != p.Name {
				continue
			}
			runs, err := s.intentRuns(ctx, in)
			if err != nil {
				return nil, err
			}
			board.Intents = append(board.Intents, projectCard(in, runs, s.intentPreview(ctx, in), limits))
		}
	}
	return board, nil
}

// projectHeader is one granted Project's board header.
func projectHeader(p *v1alpha1.Project, tier authz.Tier) BoardProject {
	bp := BoardProject{
		Name: p.Name, Tier: tierName(tier), Suspended: p.Spec.Suspend,
		Limits: wireLimits(intentview.LimitsOf(p)),
	}
	for _, repo := range p.Spec.Repositories {
		bp.Repositories = append(bp.Repositories, ProjectRepo{
			Key: intentview.Text(repo.Name, 16), Slug: intentview.RepoSlug(repo.URL),
		})
	}
	return bp
}

func wireLimits(l intentview.Limits) IntentLimits {
	return IntentLimits{
		MaxRevisions: l.MaxRevisions, MaxCheckFixes: l.MaxCheckFixes,
		MaxCostMicroUSD: l.MaxCostMicroUSD, MaxAttempts: intentview.MaxAttempts,
	}
}

// projectCard is one intent's board card, built from its own status, its
// runs and its Preview only. Its revision and check-fix counts are the
// rounds the runs count against the limits, as intent-controller counts
// them, so the card agrees with what blocks the intent.
func projectCard(in *v1alpha1.Intent, runs []*v1alpha1.IntentRun, pv *v1alpha1.Preview,
	limits intentview.Limits) IntentCard {
	st := in.Status
	c := IntentCard{
		Name:         in.Name,
		Project:      in.Spec.Project,
		Issue:        in.Spec.Issue.Number,
		IssueURL:     intentview.SafeURL(in.Spec.Issue.URL),
		Phase:        string(st.Phase),
		Column:       string(intentview.ColumnFor(in)),
		Suspended:    in.Spec.Suspend,
		Revisions:    intentview.RevisionRounds(runs),
		CheckFixes:   intentview.CheckFixRounds(runs),
		CostMicroUSD: st.Usage.CostMicroUSD,
		RequestedBy:  intentview.Text(in.Spec.RequestedBy.Login, 64),
		RequestedAt:  stamp(in.Spec.RequestedBy.At),
		CompletedAt:  stampPtr(st.CompletedAt),
	}
	if c.Phase == "" {
		c.Phase = string(v1alpha1.IntentPending)
	}
	if st.Phase == v1alpha1.IntentBlocked {
		c.BlockedFrom = string(v1alpha1.IntentBlockedFrom(in))
		c.BlockedReasons = intentview.BlockedReasons(in, limits, runs)
	}
	if n := len(st.PhaseTimes); n > 0 {
		c.PhaseSince = stamp(st.PhaseTimes[n-1].At)
	}
	if pl := st.Plan; pl != nil {
		c.Summary = intentview.Text(pl.Summary, 200)
		for _, repo := range pl.Repositories {
			if slug := intentview.RepoSlug(repo); slug != "" {
				c.Repositories = append(c.Repositories, slug)
			}
		}
	}
	for _, pr := range st.PullRequests {
		c.PullRequests = append(c.PullRequests, IntentPR{
			Repository: intentview.RepoSlug(pr.Repository), Number: pr.Number,
			URL: intentview.SafeURL(pr.URL), State: pr.State, MergedAt: stampPtr(pr.MergedAt),
		})
	}
	c.Preview = wirePreview(pv)
	for _, run := range runs {
		if run.Status.Phase == v1alpha1.RunRunning || run.Status.Phase == v1alpha1.RunPending {
			c.RunningRuns = append(c.RunningRuns, RunningRun{
				Name: run.Name, Stage: string(run.Spec.Stage),
				Repository: intentview.RepoSlug(run.Spec.Repository.URL),
				Round:      run.Spec.Round, Attempt: run.Spec.Attempt, Phase: string(run.Status.Phase),
				StartedAt: stampPtr(run.Status.StartedAt),
			})
		}
	}
	if n := len(runs); n > 0 && !v1alpha1.IntentTerminal(st.Phase) {
		last := runs[n-1]
		c.Attempt = &AttemptCount{Stage: string(last.Spec.Stage), Current: intentview.CountedAttempt(last, runs),
			Max: intentview.MaxAttempts}
	}
	return c
}

// wirePreview projects a Preview: its phase and served revision always, its
// URL only while it is Ready and only as an https link.
func wirePreview(pv *v1alpha1.Preview) *PreviewLink {
	if pv == nil {
		return nil
	}
	out := &PreviewLink{Phase: string(pv.Status.Phase), LastDeployedAt: stampPtr(pv.Status.LastDeployedAt)}
	if rev := pv.Status.ObservedRevision; len(rev) >= 12 {
		out.Revision = rev[:12]
	}
	if pv.Status.Phase == v1alpha1.PreviewReady {
		out.URL = intentview.SafeURL(pv.Status.URL)
	}
	return out
}

// handleIntentDetail serves one intent's timeline.
func (s *Server) handleIntentDetail(w http.ResponseWriter, r *http.Request) {
	_, in, tier, ok := s.intentRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	runs, err := s.intentRuns(ctx, in)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelError, "list intent runs", slog.String("intent", in.Name),
			slog.Any("error", err))
		http.Error(w, "failed to load the intent", http.StatusInternalServerError)
		return
	}
	var proj v1alpha1.Project
	limits := intentview.LimitsOf(nil)
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: in.Spec.Project}, &proj); err == nil {
		limits = intentview.LimitsOf(&proj)
	}
	writeJSONGzip(w, r, buildIntentDetail(in, runs, s.intentPreview(ctx, in), limits, tier))
}

func buildIntentDetail(in *v1alpha1.Intent, runs []*v1alpha1.IntentRun, pv *v1alpha1.Preview,
	limits intentview.Limits, tier authz.Tier) IntentDetail {
	st := in.Status
	d := IntentDetail{
		IntentCard: projectCard(in, runs, pv, limits),
		Tier:       tierName(tier),
		Limits:     wireLimits(limits),
		Rounds:     st.Rounds,
		Runs:       []IntentRunRow{},
	}
	for _, pt := range st.PhaseTimes {
		d.PhaseTimes = append(d.PhaseTimes, PhaseTime{Phase: string(pt.Phase), At: stamp(pt.At)})
	}
	if inp := st.Input; inp != nil {
		d.Input = &IntentInputView{Revision: inp.Revision, Digest: inp.Digest}
	}
	if pl := st.Plan; pl != nil {
		d.Plan = &IntentPlanView{
			Revision: pl.Revision, Digest: pl.Digest, Summary: intentview.Text(pl.Summary, 200),
			PostedAt: stampPtr(pl.PostedAt), Repositories: d.Repositories,
		}
		if pl.CommentID != 0 && d.IssueURL != "" {
			d.Plan.CommentURL = d.IssueURL + "#issuecomment-" + strconv.FormatInt(pl.CommentID, 10)
		}
	}
	if ap := st.Approval; ap != nil {
		d.Approval = &IntentApproval{
			By: intentview.Text(ap.By, 64), Source: string(ap.Source), At: stamp(ap.At),
			PlanRevision: ap.PlanRevision, PlanDigest: ap.PlanDigest, InputDigest: ap.InputDigest,
		}
	}
	plans := map[int32]bool{}
	if pl := st.Plan; pl != nil {
		plans[pl.Revision] = true
	}
	for _, run := range runs {
		d.Runs = append(d.Runs, runRow(run))
		if run.Spec.Stage == v1alpha1.IntentStagePlan && run.Status.Outcome == "ok" {
			plans[run.Spec.Round] = true
		}
	}
	if tier == authz.TierTranscripts {
		for rev := range plans {
			d.PlanTexts = append(d.PlanTexts, rev)
		}
		slices.Sort(d.PlanTexts)
	}
	return d
}

// runRow is one run's timeline row: its numbers and its outcome in public
// wording. Never its detail, its image reference or its report.
func runRow(run *v1alpha1.IntentRun) IntentRunRow {
	st := run.Status
	row := IntentRunRow{
		Name: run.Name, Stage: string(run.Spec.Stage), Trigger: string(run.Spec.Trigger),
		Repository: intentview.RepoSlug(run.Spec.Repository.URL),
		Round:      run.Spec.Round, Attempt: run.Spec.Attempt,
		Phase: string(st.Phase), Outcome: intentview.Text(st.Outcome, 64), Reason: intentview.RunReason(run),
		CreatedAt: stamp(run.CreationTimestamp), StartedAt: stampPtr(st.StartedAt),
		FinishedAt:   stampPtr(st.FinishedAt),
		CostMicroUSD: intentview.MicroUSD(st.Usage.CostUSD),
		BaseSHA:      st.BaseSHA, PushedCommit: st.PushedCommit,
		Transcript: wireTranscript(st.Transcript),
		Running:    st.Phase == v1alpha1.RunRunning || st.Phase == v1alpha1.RunPending,
	}
	if u := (Usage{
		InputTokens: st.Usage.InputTokens, OutputTokens: st.Usage.OutputTokens,
		CacheReadTokens: st.Usage.CacheReadTokens, CacheCreationTokens: st.Usage.CacheCreationTokens,
		CostMicroUSD: row.CostMicroUSD,
	}); u != (Usage{}) {
		row.Usage = &u
	}
	if img := st.RunnerImage; img != nil {
		row.ImageSource = img.Source
		row.ImageDigest = intentview.DigestHex(img.Image)
	}
	if g := run.Spec.Grant; g != (v1alpha1.IntentRunGrant{}) {
		row.Grant = &RunGrant{MaxTurns: g.MaxTurns, TokenBudget: g.TokenBudget,
			TimeoutMilliseconds: g.TimeoutMilliseconds}
	}
	return row
}

// intentRunFor loads the path's run and checks it belongs to the Intent by
// name and UID: run names are derivable, so naming another intent's run
// under this intent's path must find nothing.
func (s *Server) intentRunFor(ctx context.Context, in *v1alpha1.Intent, name string) (*v1alpha1.IntentRun, error) {
	var run v1alpha1.IntentRun
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: name}, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNotVisible
		}
		return nil, err
	}
	if run.Spec.IntentRef.Name != in.Name || run.Spec.IntentRef.UID != in.UID {
		return nil, errNotVisible
	}
	return &run, nil
}

// handleIntentRun serves one run's panel. The report is tier 2 only.
func (s *Server) handleIntentRun(w http.ResponseWriter, r *http.Request) {
	id, in, tier, ok := s.intentRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	run, err := s.intentRunFor(ctx, in, r.PathValue("run"))
	switch {
	case errors.Is(err, errNotVisible):
		notFound(w)
		return
	case err != nil:
		s.log.LogAttrs(ctx, slog.LevelError, "load intent run", slog.String("intent", in.Name), slog.Any("error", err))
		http.Error(w, "failed to load the run", http.StatusInternalServerError)
		return
	}
	runs, err := s.intentRuns(ctx, in)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelError, "list intent runs", slog.String("intent", in.Name), slog.Any("error", err))
		http.Error(w, "failed to load the run", http.StatusInternalServerError)
		return
	}
	limits := intentview.LimitsOf(nil)
	var proj v1alpha1.Project
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: in.Spec.Project}, &proj); err == nil {
		limits = intentview.LimitsOf(&proj)
	}
	counted := intentview.CountedAttempt(run, runs)
	out := IntentRunDetail{
		IntentRunRow: runRow(run), Intent: in.Name, Project: in.Spec.Project, Tier: tierName(tier),
		CountedAttempt: counted, LastAttempt: counted >= intentview.MaxAttempts, Limits: wireLimits(limits),
		IntentCostMicroUSD: in.Status.Usage.CostMicroUSD,
	}
	if out.Running && run.Status.JobRef != nil {
		out.Job = s.jobClock(ctx, run)
	}
	if tier == authz.TierTranscripts && run.Status.Report != "" {
		out.Report = intentview.Text(report.StripFrontmatter(run.Status.Report), 0)
		s.auditRead(ctx, id, in, run.Name, "report")
	}
	writeJSON(w, out)
}

// jobClock is a live run's Job clock and limits, from the facts cache.
func (s *Server) jobClock(ctx context.Context, run *v1alpha1.IntentRun) *RunJobClock {
	if s.intents.facts == nil {
		return nil
	}
	f, err := s.intents.facts.get(ctx, run.Status.JobRef.Name)
	if err != nil {
		return nil
	}
	clock := &RunJobClock{DeadlineSeconds: f.DeadlineSeconds}
	if !f.Created.IsZero() {
		clock.CreatedAt = f.Created.UTC().Format(time.RFC3339)
	}
	if !f.Started.IsZero() {
		clock.StartedAt = f.Started.UTC().Format(time.RFC3339)
	}
	idle := f.RemediateIdle
	if run.Spec.Stage == v1alpha1.IntentStagePlan {
		idle = f.InvestigateIdle
	}
	clock.IdleTimeoutSeconds = int64(idle / time.Second)
	return clock
}

// handleIntentPlan serves one plan revision's text, tier 2 only. The
// ConfigMap name is derived from the revision in the path, so it is the
// one place a derived name meets user input: what is read must carry the
// Intent's label and be controlled by this very Intent, or it is not its
// plan.
func (s *Server) handleIntentPlan(w http.ResponseWriter, r *http.Request) {
	id, in, tier, ok := s.intentRequest(w, r)
	if !ok {
		return
	}
	if tier != authz.TierTranscripts {
		s.denyContent(w, id, in)
		return
	}
	rev, err := strconv.ParseInt(r.PathValue("revision"), 10, 32)
	if err != nil || rev < 1 || rev > v1alpha1.MaxIntentRound {
		http.Error(w, "invalid revision", http.StatusBadRequest)
		return
	}
	name := fmt.Sprintf("%s-plan-r%d", in.Name, rev)
	current := in.Status.Plan != nil && in.Status.Plan.Revision == int32(rev)
	if current {
		name = in.Status.Plan.ConfigMap
	}
	ctx := r.Context()
	cm, err := s.guardedConfigMap(ctx, name, in.Name, in.UID)
	switch {
	case errors.Is(err, errNotVisible):
		notFound(w)
		return
	case err != nil:
		s.log.LogAttrs(ctx, slog.LevelError, "read plan", slog.String("intent", in.Name), slog.Any("error", err))
		http.Error(w, "failed to load the plan", http.StatusInternalServerError)
		return
	}
	out := IntentPlanText{Intent: in.Name, Revision: int32(rev), Current: current,
		Text: intentview.Text(cm.Data["plan.md"], 0)}
	if current {
		out.Digest = in.Status.Plan.Digest
	}
	s.auditRead(ctx, id, in, "", "plan r"+strconv.FormatInt(rev, 10))
	writeJSON(w, out)
}

// denyContent answers a tier 1 caller asking for tier 2 content. The intent
// is visible to them, so a 403 tells them nothing new.
func (s *Server) denyContent(w http.ResponseWriter, id auth.Identity, in *v1alpha1.Intent) {
	http.Error(w, fmt.Sprintf("Permission denied. User %q may not read plans and transcripts in project %q.",
		id.Display(), in.Spec.Project), http.StatusForbidden)
}

// guardedConfigMap reads a ConfigMap an Intent or IntentRun owns through the
// uncached reader, and refuses it unless it carries the Intent's label and a
// controller reference to owner. status-server may get any ConfigMap in its
// namespace (the Finding transcripts beside these included), so the name
// alone, derived from status or from the path, is never trusted.
func (s *Server) guardedConfigMap(ctx context.Context, name, intent string, owner types.UID) (*corev1.ConfigMap,
	error) {
	var cm corev1.ConfigMap
	if err := s.reader.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNotVisible
		}
		return nil, err
	}
	if cm.Labels[v1alpha1.LabelIntent] != intent || !controlledBy(cm.OwnerReferences, owner) {
		s.log.LogAttrs(ctx, slog.LevelWarn, "configmap refused: not the intent's own",
			slog.String("configmap", name), slog.String("intent", intent))
		return nil, errNotVisible
	}
	return &cm, nil
}

// controlledBy reports a controller owner reference to uid.
func controlledBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.UID == uid && uid != "" {
			return true
		}
	}
	return false
}

// auditRead logs one structured line per tier 2 read: ids only, never the
// content or any agent-written string.
func (s *Server) auditRead(ctx context.Context, id auth.Identity, in *v1alpha1.Intent, run, what string) {
	s.log.LogAttrs(ctx, slog.LevelInfo, "intent content read",
		slog.String("user", id.Username), slog.String("project", in.Spec.Project),
		slog.String("intent", in.Name), slog.String("run", run), slog.String("what", what))
}

// factsCache keeps each live Job's facts briefly, so viewers refreshing a
// run panel cost one Job read per window, not one each.
type factsCache struct {
	src JobFacts
	now func() time.Time

	mu      sync.Mutex
	entries map[string]factsEntry
}

type factsEntry struct {
	facts   jobs.Facts
	err     error
	fetched time.Time
}

// factsTTL bounds how stale a cached Job clock may be.
const factsTTL = 30 * time.Second

// maxFactsEntries bounds the cache; at the limit it resets.
const maxFactsEntries = 256

func (c *factsCache) get(ctx context.Context, job string) (jobs.Facts, error) {
	c.mu.Lock()
	if e, ok := c.entries[job]; ok && c.now().Sub(e.fetched) < factsTTL {
		c.mu.Unlock()
		return e.facts, e.err
	}
	c.mu.Unlock()
	f, err := c.src.Facts(ctx, job)
	c.mu.Lock()
	if len(c.entries) >= maxFactsEntries {
		c.entries = map[string]factsEntry{}
	}
	c.entries[job] = factsEntry{facts: f, err: err, fetched: c.now()}
	c.mu.Unlock()
	return f, err
}
