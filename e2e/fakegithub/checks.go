// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"cmp"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
)

// CheckAnnotation is one check-run annotation a test seeds.
type CheckAnnotation struct {
	Path    string
	Line    int
	Message string
}

// CheckRun is the fake's check-run state on one commit. Seeding the same ID
// again updates its state, as a GitHub check moves from queued to completed.
// RunID is the Actions workflow run it is a job of; its check suite is
// CheckSuiteID, or the run's own (WorkflowRun.CheckSuiteID's default) when
// that is 0.
type CheckRun struct {
	ID           int64
	Name         string
	Status       string
	Conclusion   string
	Title        string
	Summary      string
	Text         string
	RunID        int64
	CheckSuiteID int64
	AppSlug      string
	Annotations  []CheckAnnotation
}

// WorkflowRun is one Actions workflow run. Its check suite is CheckSuiteID,
// or, when that is 0, its own ID: the fake's one suite per run.
type WorkflowRun struct {
	ID           int64
	CheckSuiteID int64
	HeadSHA      string
	Status       string
	Conclusion   string
}

func (r WorkflowRun) suite() int64 {
	if r.CheckSuiteID != 0 {
		return r.CheckSuiteID
	}
	return r.ID
}

// CommitStatus is one commit-status record. Repeating a context creates a
// newer status; GitHub's raw list returns newest first.
type CommitStatus struct {
	ID          int64
	Context     string
	State       string
	Description string
}

// WorkflowJob relates one Actions job to a check run and holds its log.
type WorkflowJob struct {
	ID         int64
	CheckRunID int64
	HeadSHA    string
	Name       string
	Conclusion string
	Log        string
}

// SetCheckRun creates or updates a check run on sha.
func (s *Server) SetCheckRun(sha string, run CheckRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.checkRuns[sha], func(v CheckRun) bool { return v.ID == run.ID }); i >= 0 {
		s.checkRuns[sha][i] = run
	} else {
		s.checkRuns[sha] = append(s.checkRuns[sha], run)
	}
}

// SetWorkflowRun creates or updates an Actions workflow run.
func (s *Server) SetWorkflowRun(run WorkflowRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workflowRuns[run.ID] = run
}

// Reruns are the workflow runs whose failed jobs were re-run, in the order
// they were asked for.
func (s *Server) Reruns() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reruns)
}

// SetCommitStatus adds a raw status to sha, newest first.
func (s *Server) SetCommitStatus(sha string, status CommitStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[sha] = append([]CommitStatus{status}, s.statuses[sha]...)
}

// SetWorkflowJob creates or updates a workflow job on runID.
func (s *Server) SetWorkflowJob(runID int64, job WorkflowJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.workflowJobs[runID], func(v WorkflowJob) bool { return v.ID == job.ID }); i >= 0 {
		s.workflowJobs[runID][i] = job
	} else {
		s.workflowJobs[runID] = append(s.workflowJobs[runID], job)
	}
	s.jobLogs[job.ID] = job.Log
}

func (s *Server) listCheckRuns(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	s.mu.Lock()
	runs := slices.Clone(s.checkRuns[sha])
	s.mu.Unlock()
	out := make([]map[string]any, 0, len(runs))
	for _, v := range runs {
		appSlug := v.AppSlug
		if appSlug == "" && v.RunID != 0 {
			appSlug = "github-actions"
		}
		suite := v.CheckSuiteID
		if suite == 0 {
			suite = WorkflowRun{ID: v.RunID}.suite()
		}
		out = append(out, map[string]any{
			"id": v.ID, "name": v.Name, "head_sha": sha, "status": v.Status,
			"conclusion":  v.Conclusion,
			"app":         map[string]any{"slug": appSlug},
			"check_suite": map[string]any{"id": suite},
			"details_url": fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d/job/%d",
				r.PathValue("owner"), r.PathValue("repo"), v.RunID, v.ID),
			"output": map[string]any{"title": v.Title, "summary": v.Summary, "text": v.Text,
				"annotations_count": len(v.Annotations)},
		})
	}
	writeJSON(w, map[string]any{"total_count": len(out), "check_runs": out})
}

func (s *Server) listCheckAnnotations(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	var annotations []CheckAnnotation
	found := false
	for _, runs := range s.checkRuns {
		for _, run := range runs {
			if run.ID == id {
				annotations = slices.Clone(run.Annotations)
				found = true
				break
			}
		}
	}
	s.mu.Unlock()
	if !found {
		notFound(w)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if limit > 0 && len(annotations) > limit {
		annotations = annotations[:limit]
	}
	out := make([]map[string]any, 0, len(annotations))
	for _, a := range annotations {
		out = append(out, map[string]any{"path": a.Path, "start_line": a.Line,
			"end_line": a.Line, "message": a.Message})
	}
	writeJSON(w, out)
}

func (s *Server) listCommitStatuses(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	statuses := slices.Clone(s.statuses[r.PathValue("sha")])
	s.mu.Unlock()
	out := make([]map[string]any, 0, len(statuses))
	for _, v := range statuses {
		out = append(out, map[string]any{"id": v.ID, "context": v.Context, "state": v.State,
			"description": v.Description})
	}
	writeJSON(w, out)
}

func (s *Server) listWorkflowJobs(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	jobs := slices.Clone(s.workflowJobs[runID])
	s.mu.Unlock()
	out := make([]map[string]any, 0, len(jobs))
	for _, v := range jobs {
		out = append(out, map[string]any{
			"id": v.ID, "name": v.Name, "head_sha": v.HeadSHA, "conclusion": v.Conclusion,
			"check_run_url": fmt.Sprintf("https://api.github.com/repos/%s/%s/check-runs/%d",
				r.PathValue("owner"), r.PathValue("repo"), v.CheckRunID),
		})
	}
	writeJSON(w, map[string]any{"total_count": len(out), "jobs": out})
}

// listWorkflowRuns answers GET /repos/{o}/{r}/actions/runs, filtered by
// check_suite_id as GitHub filters it.
func (s *Server) listWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	suite, _ := strconv.ParseInt(r.URL.Query().Get("check_suite_id"), 10, 64)
	s.mu.Lock()
	var runs []WorkflowRun
	for _, run := range s.workflowRuns {
		if suite == 0 || run.suite() == suite {
			runs = append(runs, run)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(runs, func(a, b WorkflowRun) int { return cmp.Compare(b.ID, a.ID) })
	out := make([]map[string]any, 0, len(runs))
	for _, v := range runs {
		out = append(out, map[string]any{"id": v.ID, "check_suite_id": v.suite(), "head_sha": v.HeadSHA,
			"status": v.Status, "conclusion": v.Conclusion})
	}
	writeJSON(w, map[string]any{"total_count": len(out), "workflow_runs": out})
}

// rerunFailedJobs answers POST /repos/{o}/{r}/actions/runs/{id}/rerun-failed-jobs
// as GitHub does: 404 for a run it does not know, 403 for one still running,
// and otherwise 201, the run queued again as its next attempt.
func (s *Server) rerunFailedJobs(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	run, ok := s.workflowRuns[id]
	running := ok && run.Status != "completed"
	if ok && !running {
		s.reruns = append(s.reruns, id)
		run.Status, run.Conclusion = "queued", ""
		s.workflowRuns[id] = run
	}
	s.mu.Unlock()
	switch {
	case !ok:
		notFound(w)
	case running:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"This workflow is already running"}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}
}

func (s *Server) jobLogRedirect(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	_, ok := s.jobLogs[id]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v3/_logs/%d", id))
	w.WriteHeader(http.StatusFound)
}

func (s *Server) jobLogDownload(w http.ResponseWriter, r *http.Request) {
	// The redirect destination is a credential-less signed object-store URL
	// in GitHub. Catch a client that accidentally forwards its App token.
	if r.Header.Get("Authorization") != "" {
		forbidden(w)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	log, ok := s.jobLogs[id]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, log)
}
