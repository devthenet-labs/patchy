// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/go-github/v90/github"
)

// CheckOutput is a check run's prose. It is untrusted agent input.
type CheckOutput struct{ Title, Summary, Text string }

// CheckRun is a check run associated with exactly HeadSHA.
type CheckRun struct {
	ID         int64
	Name       string
	HeadSHA    string
	Status     string
	Conclusion string
	DetailsURL string
	AppSlug    string
	Output     CheckOutput
}

// CheckAnnotation is one source annotation attached to a check run.
type CheckAnnotation struct {
	Path, Message string
	Line          int
}

// CommitStatus is one (not a combined) status on a commit. Callers retain
// only the newest status for each context when deciding whether it failed.
type CommitStatus struct {
	ID          int64
	Context     string
	State       string
	Description string
}

// WorkflowJob relates an Actions job to its check run by CheckRunID.
type WorkflowJob struct {
	ID         int64
	CheckRunID int64
	HeadSHA    string
	Name       string
	Conclusion string
}

// ListCheckRuns lists the latest check runs for a commit SHA.
func (c *Client) ListCheckRuns(ctx context.Context, repo Repo, sha string) ([]CheckRun, error) {
	opts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: listPageSize}}
	var out []CheckRun
	for range walkPageCap {
		page, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, repo.Owner, repo.Name, sha, opts)
		if err != nil {
			return nil, fmt.Errorf("ghclient: list check runs on %s@%s: %w", repo, sha, err)
		}
		for _, v := range page.CheckRuns {
			if v == nil {
				continue
			}
			o := v.GetOutput()
			out = append(out, CheckRun{ID: v.GetID(), Name: v.GetName(), HeadSHA: v.GetHeadSHA(),
				Status: v.GetStatus(), Conclusion: v.GetConclusion(), DetailsURL: v.GetDetailsURL(),
				AppSlug: v.GetApp().GetSlug(),
				Output:  CheckOutput{Title: o.GetTitle(), Summary: o.GetSummary(), Text: o.GetText()}})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, fmt.Errorf("ghclient: check runs on %s@%s run past %d pages", repo, sha, walkPageCap)
}

// ListCheckAnnotations reads at most limit annotations; a caller uses 50
// for one failing check. The API can return at most 100 per page.
func (c *Client) ListCheckAnnotations(ctx context.Context, repo Repo, id int64, limit int) ([]CheckAnnotation, error) {
	if limit <= 0 || limit > listPageSize {
		return nil, fmt.Errorf("ghclient: annotation limit %d must be 1..%d", limit, listPageSize)
	}
	page, _, err := c.gh.Checks.ListCheckRunAnnotations(ctx, repo.Owner, repo.Name, id,
		&github.ListOptions{PerPage: limit})
	if err != nil {
		return nil, fmt.Errorf("ghclient: list annotations on %s check %d: %w", repo, id, err)
	}
	out := make([]CheckAnnotation, 0, len(page))
	for _, a := range page {
		if a != nil {
			out = append(out, CheckAnnotation{Path: a.GetPath(), Line: a.GetStartLine(), Message: a.GetMessage()})
		}
	}
	return out, nil
}

// ListCommitStatuses lists raw statuses, newest first, for one commit.
func (c *Client) ListCommitStatuses(ctx context.Context, repo Repo, sha string) ([]CommitStatus, error) {
	opts := &github.ListOptions{PerPage: listPageSize}
	var out []CommitStatus
	for range walkPageCap {
		page, resp, err := c.gh.Repositories.ListStatuses(ctx, repo.Owner, repo.Name, sha, opts)
		if err != nil {
			return nil, fmt.Errorf("ghclient: list statuses on %s@%s: %w", repo, sha, err)
		}
		for _, v := range page {
			if v != nil {
				out = append(out, CommitStatus{ID: v.GetID(), Context: v.GetContext(), State: v.GetState(),
					Description: v.GetDescription()})
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, fmt.Errorf("ghclient: statuses on %s@%s run past %d pages", repo, sha, walkPageCap)
}

// ListWorkflowJobs lists the latest attempt's jobs for one Actions run.
func (c *Client) ListWorkflowJobs(ctx context.Context, repo Repo, runID int64) ([]WorkflowJob, error) {
	opts := &github.ListWorkflowJobsOptions{ListOptions: github.ListOptions{PerPage: listPageSize}}
	var out []WorkflowJob
	for range walkPageCap {
		page, resp, err := c.gh.Actions.ListWorkflowJobs(ctx, repo.Owner, repo.Name, runID, opts)
		if err != nil {
			return nil, fmt.Errorf("ghclient: list jobs for %s Actions run %d: %w", repo, runID, err)
		}
		for _, v := range page.Jobs {
			if v == nil {
				continue
			}
			out = append(out, WorkflowJob{ID: v.GetID(), CheckRunID: checkRunID(v.GetCheckRunURL()),
				HeadSHA: v.GetHeadSHA(), Name: v.GetName(), Conclusion: v.GetConclusion()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, fmt.Errorf("ghclient: jobs for %s Actions run %d run past %d pages", repo, runID, walkPageCap)
}

func checkRunID(raw string) int64 {
	u, err := url.Parse(raw)
	if err != nil || !strings.Contains(u.Path, "/check-runs/") {
		return 0
	}
	id, _ := strconv.ParseInt(u.Path[strings.LastIndex(u.Path, "/")+1:], 10, 64)
	return id
}

// maxJobLogDownload caps a single log body. A giant CI log cannot exhaust
// controller memory or stall a reconciliation indefinitely.
const maxJobLogDownload = 8 << 20

// GetJobLogTail downloads at most maxJobLogDownload bytes from GitHub's
// signed redirect, with NO credential on that request. Only the last
// tailBytes are returned. An oversized log is an error, never silently
// treated as the complete failure evidence.
func (c *Client) GetJobLogTail(ctx context.Context, repo Repo, jobID int64, tailBytes int) (string, error) {
	if tailBytes <= 0 || tailBytes > 32<<10 {
		return "", fmt.Errorf("ghclient: job log tail %d must be 1..32768", tailBytes)
	}
	u, _, err := c.gh.Actions.GetWorkflowJobLogs(ctx, repo.Owner, repo.Name, jobID, 0)
	if err != nil {
		return "", fmt.Errorf("ghclient: get job log URL for %s job %d: %w", repo, jobID, err)
	}
	if u == nil {
		return "", fmt.Errorf("ghclient: no job log URL for %s job %d", repo, jobID)
	}
	base, _ := url.Parse(c.gh.BaseURL())
	u = base.ResolveReference(u)
	allowed := u.Scheme == "https" || (u.Scheme == "http" && u.Host == base.Host && base.Scheme == "http")
	if !allowed {
		return "", fmt.Errorf("ghclient: unsafe job log URL scheme for %s job %d", repo, jobID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("ghclient: job log request for %s job %d: %w", repo, jobID, err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=-%d", tailBytes))
	resp, err := c.logHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("ghclient: download job log for %s job %d: %w", repo, jobID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return "", fmt.Errorf("ghclient: download job log for %s job %d: HTTP %d", repo, jobID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJobLogDownload+1))
	if err != nil {
		return "", fmt.Errorf("ghclient: read job log for %s job %d: %w", repo, jobID, err)
	}
	if len(body) > maxJobLogDownload {
		return "", fmt.Errorf("ghclient: job log for %s job %d exceeds %d bytes", repo, jobID, maxJobLogDownload)
	}
	if len(body) > tailBytes {
		body = body[len(body)-tailBytes:]
	}
	return string(body), nil
}
