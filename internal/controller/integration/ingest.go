// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

// maxAlerts caps spec.alerts; later alerts only bump overflowAlerts.
const maxAlerts = 64

// DefaultWindow is the accumulation window when unconfigured.
const DefaultWindow = time.Hour

// KeyHashIndex is the field index over the key-hash label. Every ingested
// alert selects its finding family by this value; without the index that
// List is a full informer-store scan per alert — O(backlog), ruinous on a
// brownfield estate — where the index lookup is O(1).
const KeyHashIndex = "labels.key-hash"

// KeyHashIndexer extracts the index value: the finding's key-hash label.
// Exported so fake-client tests register the same recipe the manager does.
func KeyHashIndexer(obj client.Object) []string {
	hash := obj.GetLabels()[v1alpha1.LabelKeyHash]
	if hash == "" {
		return nil
	}
	return []string{hash}
}

// Ingestor folds scanner findings into Finding resources. The deterministic
// name plus AlreadyExists-tolerant create is the idempotency mechanism — no
// in-process mutex, the API server serializes.
type Ingestor struct {
	client.Client
	// Namespace the Findings live in.
	Namespace string
	// Window is the accumulation window (default DefaultWindow).
	Window time.Duration
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
	// Log receives ingest diagnostics; nil discards.
	Log *slog.Logger
	// Commits answers commit ancestry for the stale-observation check
	// (supersedingFix); nil disables the check, so every observation
	// ingests.
	Commits CommitGraph
}

// CommitGraph answers the questions about repository history ingest asks:
// does one commit strictly precede another, and is a commit still on a
// branch.
type CommitGraph interface {
	// Precedes reports whether commit is a strict ancestor of descendant
	// in repo — reachable from it, and not the same commit.
	Precedes(ctx context.Context, integ *v1alpha1.Integration, repo source.Repo, commit, descendant string) (bool, error)
	// Contains reports whether commit is in branch's history in repo — the
	// branch head or one of its ancestors.
	Contains(ctx context.Context, integ *v1alpha1.Integration, repo source.Repo, branch, commit string) (bool, error)
}

// keyHash is the hex form of the accumulation key's hash — the label value
// selecting a finding family across generations.
//
// scope is what the finding is raised against: the repository URL for a code
// finding, the cloud resource name for an infrastructure one. It occupies the
// position the repository URL always has, and MUST keep doing so: the hash is
// persisted in a label on every live Finding, so changing the string for a
// repo-bearing finding orphans every existing family. Accumulation would then
// find nothing, open generation 1 alongside the live one, and project a
// second tracking issue for every open finding in the estate. There is no
// migration — the label is frozen by the admission policy.
func keyHash(integration, sourceID, scope, advisory string) string {
	sum := sha256.Sum256([]byte(integration + "|" + sourceID + "|" + scope + "|" + advisory))
	return hex.EncodeToString(sum[:5])
}

// SetupWithManager registers the key-hash field index on the manager's
// cache. The Ingestor is webhook-driven — it runs no reconciler — so this is
// its only manager hook; without it every family List errors, which is the
// loud failure mode we want over a silent full scan.
func (in *Ingestor) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &v1alpha1.Finding{}, KeyHashIndex, KeyHashIndexer); err != nil {
		return fmt.Errorf("index %s: %w", KeyHashIndex, err)
	}
	return nil
}

// Ingest folds one scanner finding into the cluster: append its alert to the
// live pre-investigation Finding of its family, or create the next
// generation.
func (in *Ingestor) Ingest(ctx context.Context, integ *v1alpha1.Integration, f source.Finding) error {
	_, err := in.ingest(ctx, integ, f, false)
	return err
}

// ingest is Ingest, reporting whether f was set aside as stale — recorded on
// the remediated generation whose merged fix supersedes it — rather than
// folded or created.
//
// recheck is the stale re-check's mode (recheckStale): f re-reads an alert
// whose stale observation is already recorded and will be read again, so a
// failed ancestry lookup or recording is returned as an error instead of
// failing open — a transient error must not turn an alert that is still
// stale into the duplicate finding the check exists to prevent.
func (in *Ingestor) ingest(
	ctx context.Context, integ *v1alpha1.Integration, f source.Finding, recheck bool,
) (stale bool, err error) {
	repoURL := repositoryURL(integ, f)
	scope := accumulationScope(repoURL, f)
	if scope == "" {
		return false, fmt.Errorf("ingest %s finding: names neither a repository nor a cloud resource", f.Source)
	}
	primary := ""
	if len(f.Advisories) > 0 {
		primary = f.Advisories[0]
	}
	hash := keyHash(integ.Name, f.Source, scope, primary)

	var family v1alpha1.FindingList
	if err := in.List(ctx, &family, client.InNamespace(in.Namespace),
		client.MatchingFields{KeyHashIndex: hash}); err != nil {
		return false, fmt.Errorf("list finding family %s: %w", hash, err)
	}

	fix, err := in.supersedingFix(ctx, integ, f, family.Items)
	switch {
	case err != nil && recheck:
		return false, err
	case err != nil:
		in.log().LogAttrs(ctx, slog.LevelWarn, "commit ancestry lookup failed; ingesting", append([]slog.Attr{
			slog.String("alert", alertID(f)), slog.String("commit", f.Commit), slog.Any("error", err),
		}, deliveryAttrs(ctx)...)...)
	case fix != nil:
		attrs := append([]slog.Attr{
			slog.String("finding", fix.Name), slog.String("alert", alertID(f)),
			slog.String("commit", f.Commit),
			slog.String("merge_commit", fix.Status.PullRequest.MergeCommitSHA),
		}, deliveryAttrs(ctx)...)
		// Recorded, or not skipped at all: the scanner will not report the
		// alert again while it stays open, so an unrecorded skip could never
		// be revisited when a later analysis regresses it.
		err := in.recordStale(ctx, fix, f)
		if err == nil {
			in.log().LogAttrs(ctx, slog.LevelInfo, "stale alert observation skipped", attrs...)
			return true, nil
		}
		if recheck {
			return false, fmt.Errorf("record stale observation: %w", err)
		}
		in.log().LogAttrs(ctx, slog.LevelWarn, "stale alert observation not recorded; ingesting",
			append(attrs, slog.Any("error", err))...)
	}

	// Fold into a live pre-investigation generation when one exists.
	maxGen := 0
	for i := range family.Items {
		cur := &family.Items[i]
		if gen := generationOf(cur.Name); gen > maxGen {
			maxGen = gen
		}
		if cur.DeletionTimestamp.IsZero() && foldable(cur.Status.Phase) {
			err := in.fold(ctx, cur.Name, f)
			if err == errRaced {
				// The live generation advanced mid-fold; open its successor.
				gen := generationOf(cur.Name)
				return false, in.create(ctx, integ, f, repoURL, hash, gen+1, cur.Name)
			}
			return false, err
		}
	}

	return false, in.create(ctx, integ, f, repoURL, hash, maxGen+1, prevName(family.Items, maxGen))
}

// supersedingFix returns the generation whose merged pull request already
// fixed f's alert in code newer than the code f was observed in — f is then
// stale and must not open a successor — or nil when f ingests.
//
// Code scanning moves an alert's state with whichever analysis uploads last,
// not with the newest commit. When pushes land seconds apart and an older
// commit's analysis finishes after a newer one's, GitHub reopens every alert
// the newer commit fixed, observed at the older commit (patchy-target alerts
// 7 and 9: fixed by the squash merges of their remediation PRs, then reopened
// at 45b1bec, the commit both merges descend from, when its analysis landed
// last). That observation describes code the merged fix has already
// replaced; a successor generation for it would investigate and remediate a
// vulnerability no longer on the branch.
//
// The rule is deliberately narrow. The alert's latest generation must be
// Remediated through a merged pull request with a recorded merge commit, f's
// commit must strictly precede that merge commit, and the merge commit must
// still be on the branch f was observed on — a default branch reset to
// before the fix keeps the orphaned merge commit resolvable, but carries the
// vulnerable code again. An observation at or after the merge — a regression
// or revert that brings the code back, a fix the scanner still flags —
// ingests, as does anything unproven (no commit or branch on f, a PR merged
// before merge commits were recorded, a failed lookup): a duplicate finding
// is noise, a dropped regression is a missed vulnerability.
//
// A skipped observation is not forgotten: the caller records it on the
// returned generation, and the projection reconciler re-reads the alert
// (recheckStale), because the scanner reports an alert only when its state
// changes — a later analysis that regresses an alert already open sends
// nothing.
//
// err reports a failed lookup (already reflected on the Integration's
// CommitAncestry condition); what failing means is the caller's call.
func (in *Ingestor) supersedingFix(
	ctx context.Context, integ *v1alpha1.Integration, f source.Finding, family []v1alpha1.Finding,
) (*v1alpha1.Finding, error) {
	if in.Commits == nil || f.Commit == "" {
		return nil, nil
	}
	branch, ok := branchOf(f.Ref)
	if !ok {
		return nil, nil
	}
	latest := latestWithAlert(family, alertID(f))
	if latest == nil || latest.Status.Phase != v1alpha1.PhaseRemediated {
		return nil, nil
	}
	pr := latest.Status.PullRequest
	if pr == nil || pr.State != "merged" || pr.MergeCommitSHA == "" || pr.MergeCommitSHA == f.Commit {
		return nil, nil
	}
	lookupFailed := func(err error) (*v1alpha1.Finding, error) {
		in.noteAncestry(ctx, integ, err)
		return nil, fmt.Errorf("finding %s, merge commit %s, branch %s: %w",
			latest.Name, pr.MergeCommitSHA, branch, err)
	}
	older, err := in.Commits.Precedes(ctx, integ, f.Repo, f.Commit, pr.MergeCommitSHA)
	if err != nil {
		return lookupFailed(err)
	}
	if !older {
		in.noteAncestry(ctx, integ, nil)
		return nil, nil
	}
	fixed, err := in.Commits.Contains(ctx, integ, f.Repo, branch, pr.MergeCommitSHA)
	if err != nil {
		return lookupFailed(err)
	}
	in.noteAncestry(ctx, integ, nil)
	if !fixed {
		return nil, nil
	}
	return latest, nil
}

// latestWithAlert is the newest generation in family carrying alert id, or
// nil when none does.
func latestWithAlert(family []v1alpha1.Finding, id string) *v1alpha1.Finding {
	var latest *v1alpha1.Finding
	for i := range family {
		cur := &family[i]
		if !slices.ContainsFunc(cur.Spec.Alerts, func(a v1alpha1.Alert) bool { return a.ID == id }) {
			continue
		}
		if latest == nil || generationOf(cur.Name) > generationOf(latest.Name) {
			latest = cur
		}
	}
	return latest
}

// branchOf is the branch a ref names — refs/heads/<branch>, or a bare
// branch name — and false for anything else: a tag, a pull-request ref, no
// ref at all.
func branchOf(ref string) (string, bool) {
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b, b != ""
	}
	if ref == "" || strings.HasPrefix(ref, "refs/") {
		return "", false
	}
	return ref, true
}

// repositoryURL is the finding's repository, or empty when it names none. A
// cloud finding starts repo-less; whether it ever gets one is the enhancer
// chain's question, answered from the resource's ownership labels.
func repositoryURL(integ *v1alpha1.Integration, f source.Finding) string {
	if f.Repo.Owner == "" || f.Repo.Name == "" {
		return ""
	}
	return "https://" + githubHost(integ) + "/" + f.Repo.Owner + "/" + f.Repo.Name
}

// accumulationScope is what the finding is raised against — the thing two
// alerts must share, alongside their advisory, to be the same finding.
//
// For a cloud finding that is the resource, not its project: repository
// resolution is per-resource, so a family spanning resources could resolve to
// two different repositories and there would be no right answer. Scoping per
// resource means accumulation folds only SCC's re-notifications of the same
// (resource, category), which is exactly what it re-sends on every update.
func accumulationScope(repoURL string, f source.Finding) string {
	if repoURL != "" {
		return repoURL
	}
	if f.CloudResource != nil {
		return f.CloudResource.Name
	}
	return ""
}

// toCloudResource maps the seam's cloud resource onto the CR shape.
func toCloudResource(cr *source.CloudResource) *v1alpha1.FindingCloudResource {
	if cr == nil {
		return nil
	}
	return &v1alpha1.FindingCloudResource{
		Provider:    v1alpha1.CloudProvider(cr.Provider),
		Name:        cr.Name,
		Type:        cr.Type,
		Project:     cr.Project,
		Location:    cr.Location,
		DisplayName: cr.DisplayName,
	}
}

// errRaced reports a fold target that left the foldable phases mid-fold.
var errRaced = fmt.Errorf("finding advanced past accumulation")

// foldable phases still accept new alerts: the accumulation window overlaps
// enhancement, and an aged window only closes via the AccumulationComplete
// condition, not the phase. An empty phase is the creation window — the
// status write that stamps Opened races the next alert of the family (a
// backfill ingests them back to back), and treating it as closed would
// splinter the family into one generation per alert.
func foldable(p v1alpha1.Phase) bool {
	return p == "" || p == v1alpha1.PhaseOpened || p == v1alpha1.PhaseEnhanced
}

// generationOf parses the trailing generation ordinal of a Finding name.
func generationOf(name string) int {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '-' {
			n, err := strconv.Atoi(name[i+1:])
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

// prevName returns the name of the generation maxGen, for the successor
// edge; empty when none.
func prevName(items []v1alpha1.Finding, maxGen int) string {
	for i := range items {
		if generationOf(items[i].Name) == maxGen {
			return items[i].Name
		}
	}
	return ""
}

// fold appends the finding's alert to an existing Finding, idempotent on
// alert ID, under conflict retry. NotFound retries too, on the slower
// backoff: the fold target may be a finding another ingest created moments
// ago that the cached client has not observed yet — losing the alert to
// informer lag would silently thin a backfill.
func (in *Ingestor) fold(ctx context.Context, name string, f source.Finding) error {
	alert := toAlert(f)
	retriable := func(err error) bool { return kerrors.IsConflict(err) || kerrors.IsNotFound(err) }
	return retry.OnError(retry.DefaultBackoff, retriable, func() error {
		var cur v1alpha1.Finding
		if err := in.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: name}, &cur); err != nil {
			return err
		}
		if !foldable(cur.Status.Phase) {
			return errRaced
		}
		if slices.ContainsFunc(cur.Spec.Alerts, func(a v1alpha1.Alert) bool { return a.ID == alert.ID }) {
			return nil
		}
		if len(cur.Spec.Alerts) >= maxAlerts {
			cur.Spec.OverflowAlerts++
		} else {
			cur.Spec.Alerts = append(cur.Spec.Alerts, alert)
		}
		// New advisories fold in too (same primary, richer identifiers).
		for _, adv := range f.Advisories {
			if !slices.Contains(cur.Spec.Advisories, adv) {
				cur.Spec.Advisories = append(cur.Spec.Advisories, adv)
			}
		}
		if err := in.Update(ctx, &cur); err != nil {
			return err
		}
		in.log().LogAttrs(ctx, slog.LevelInfo, "alert folded into finding", append([]slog.Attr{
			slog.String("finding", cur.Name), slog.String("alert", alert.ID),
		}, deliveryAttrs(ctx)...)...)
		return nil
	})
}

// create makes generation gen of the family, records the successor edge, and
// opens the accumulation window.
func (in *Ingestor) create(
	ctx context.Context, integ *v1alpha1.Integration, f source.Finding,
	repoURL, hash string, gen int, prev string,
) error {
	name := fmt.Sprintf("finding-%s-%d", hash, gen)
	labels := map[string]string{
		v1alpha1.LabelKeyHash:     hash,
		v1alpha1.LabelSource:      f.Source,
		v1alpha1.LabelIntegration: integ.Name,
		v1alpha1.LabelSeverity:    string(levelOf(f.Severity)),
	}
	// Omitted rather than hashed empty: a repo-less finding with a
	// real-looking repo-hash would read as belonging to some repository, and
	// the value would never be corrected once an enhancer resolved one.
	if repoURL != "" {
		labels[v1alpha1.LabelRepoHash] = hashOf(repoURL)
	}
	fnd := &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: in.Namespace,
			Labels:    labels,
		},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: integ.Name},
			TrackingRef:    in.trackingRef(ctx, integ),
			Source:         f.Source,
			CloudResource:  toCloudResource(f.CloudResource),
			Advisories:     f.Advisories,
			RuleID:         f.RuleID,
			Title:          f.Title,
			Description:    truncate(f.Description, 65536),
			Severity:       levelOf(f.Severity),
			Alerts:         []v1alpha1.Alert{toAlert(f)},
		},
	}
	if repoURL != "" {
		fnd.Spec.Repository = &v1alpha1.FindingRepository{
			Type: v1alpha1.RepositoryTypeGitHub,
			URL:  repoURL,
			Name: f.Repo.String(),
		}
	}
	if prev != "" {
		fnd.Spec.Related = []v1alpha1.RelatedFinding{{
			From: name, To: prev, Relationship: v1alpha1.RelationshipSuccessorOf,
		}}
	}
	if err := in.Create(ctx, fnd); err != nil {
		if kerrors.IsAlreadyExists(err) {
			// Two deliveries raced; the winner's object is the family live
			// generation — fold into it.
			return in.fold(ctx, name, f)
		}
		return fmt.Errorf("create finding %s: %w", name, err)
	}

	now := in.now()
	t := metav1.NewTime(now)
	until := metav1.NewTime(now.Add(in.window()))
	if err := v1alpha1.SetPhase(fnd, v1alpha1.PhaseOpened, now); err != nil {
		return err
	}
	fnd.Status.FirstObservedAt = &t
	fnd.Status.AccumulateUntil = &until
	if err := in.Status().Update(ctx, fnd); err != nil {
		// The projection reconciler backfills window fields for a bare
		// Opened-less Finding; log and let it.
		in.log().LogAttrs(ctx, slog.LevelWarn, "finding status init failed",
			slog.String("finding", name), slog.Any("error", err))
	}

	// Mirror the successor edge onto the elder, best-effort.
	if prev != "" {
		in.mirrorEdge(ctx, prev, fnd.Spec.Related[0])
	}
	in.log().LogAttrs(ctx, slog.LevelInfo, "finding created", append([]slog.Attr{
		slog.String("finding", name), slog.String("scope", accumulationScope(repoURL, f)),
		slog.String("alert", alertID(f)),
	}, deliveryAttrs(ctx)...)...)
	return nil
}

// mirrorEdge appends the successor edge to the elder generation's spec,
// best-effort under conflict retry.
func (in *Ingestor) mirrorEdge(ctx context.Context, elder string, edge v1alpha1.RelatedFinding) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := in.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: elder}, &cur); err != nil {
			return err
		}
		if slices.Contains(cur.Spec.Related, edge) {
			return nil
		}
		if len(cur.Spec.Related) >= 32 {
			return nil
		}
		cur.Spec.Related = append(cur.Spec.Related, edge)
		return in.Update(ctx, &cur)
	})
	if err != nil {
		in.log().LogAttrs(ctx, slog.LevelWarn, "successor edge mirror failed",
			slog.String("elder", elder), slog.Any("error", err))
	}
}

// toAlert maps a scanner finding's alert fields. The id is the source's own
// string identifier where it has one; only sources that number their alerts
// fall back to the decimal form.
func toAlert(f source.Finding) v1alpha1.Alert {
	a := v1alpha1.Alert{ID: alertID(f), Source: f.Source, URL: f.HTMLURL}
	for i, loc := range f.Locations {
		if i == 8 {
			break
		}
		a.Locations = append(a.Locations, v1alpha1.Location{
			Path:      loc.Path,
			StartLine: int32(loc.StartLine),
			EndLine:   int32(loc.EndLine),
			Snippet:   truncate(loc.Snippet, 1024),
		})
	}
	return a
}

// alertID is the finding's alert identifier as recorded on the CR: the
// source's own string identifier where it has one, else the decimal alert
// number.
func alertID(f source.Finding) string {
	if f.AlertID != "" {
		return f.AlertID
	}
	return strconv.Itoa(f.AlertNumber)
}

// trackingRef denormalizes the projecting integration at creation: the
// ingesting integration itself when issues-enabled, else the namespace's
// issues-enabled one — a cloud or generic source has no issues capability of
// its own, but its findings still deserve tracking issues. Nil when none (or
// on a transient lookup failure): the finding is still tracked in-cluster.
func (in *Ingestor) trackingRef(ctx context.Context, integ *v1alpha1.Integration) *v1alpha1.LocalObjectReference {
	if issuesEnabled(integ) {
		return &v1alpha1.LocalObjectReference{Name: integ.Name}
	}
	tracker, err := selectIntegration(ctx, in.Client, in.Namespace, issuesEnabled)
	if err != nil {
		if !errors.Is(err, ErrNoIntegration) {
			in.log().LogAttrs(ctx, slog.LevelWarn, "tracking integration lookup failed",
				slog.String("integration", integ.Name), slog.Any("error", err))
		}
		return nil
	}
	return &v1alpha1.LocalObjectReference{Name: tracker.Name}
}

func (in *Ingestor) window() time.Duration {
	if in.Window <= 0 {
		return DefaultWindow
	}
	return in.Window
}

func (in *Ingestor) now() time.Time {
	if in.Now == nil {
		return time.Now()
	}
	return in.Now()
}

func (in *Ingestor) log() *slog.Logger {
	if in.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return in.Log
}

// hashOf is the label-value hash of an arbitrary string (repo URLs don't fit
// label values).
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:5])
}

// truncate caps s at limit bytes without splitting a rune (the API server
// rejects invalid UTF-8).
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// levelOf maps a scanner severity onto the Level enum; unknown values are
// dropped (the field is optional and enum-validated).
func levelOf(s string) v1alpha1.Level {
	switch l := v1alpha1.Level(s); l {
	case v1alpha1.LevelLow, v1alpha1.LevelMedium, v1alpha1.LevelHigh, v1alpha1.LevelCritical:
		return l
	default:
		return ""
	}
}
