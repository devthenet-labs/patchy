// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// The pod layout of a multi-repository plan, shared with internal/jobs,
// which writes it: the manifest's name under the workspace's input/, and
// the workspace directory the other repositories' trees are extracted
// under, one directory per Project key. Only a plan Job of an intent whose
// Project has more than one repository carries either; every other Job
// has neither, and agent-runner runs it exactly as before.
const (
	RepositoriesManifest = "repositories"
	TreesDir             = "repos"
)

// manifestMaxBytes bounds the manifest agent-runner reads: eight lines of
// a 16-character key, a workspace path and a 256-byte URL fit many times
// over.
const manifestMaxBytes = 16 << 10

// repositoryKey is a Project repository key (ProjectRepository.Name).
var repositoryKey = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// repositoryKeyMaxLen bounds a key as the Project schema does.
const repositoryKeyMaxLen = 16

// manifestRepository is one line of the repositories manifest: a Project
// repository the plan Job holds, where its tree is, and its URL as the
// Project spells it.
type manifestRepository struct {
	Key  string
	Path string
	URL  string
}

// repositoriesManifest is where the prepare init stages the manifest.
func (c Config) repositoriesManifest() string {
	return filepath.Join(c.Workspace, "input", RepositoriesManifest)
}

// treeDir is where the tree of the repository keyed key is.
func (c Config) treeDir(key string) string { return filepath.Join(c.Workspace, TreesDir, key) }

// repositories reads the repositories manifest of a plan Job: nil, with no
// error, when there is none — a one-repository plan, which runs exactly as
// before. A manifest that exists is held to its whole contract, and any
// breach is an error the stage reports as fatal before an agent runs:
//
//   - one "<key> <path> <url>" line per repository, two to
//     report.PlanMaxRepositories of them;
//   - each key a Project key, used once;
//   - the first repository's path the working tree, every other's its own
//     directory under the workspace's repos/ — nowhere else;
//   - each URL of the shape a plan names a repository by, no repository
//     listed twice (report.SameRepository);
//   - each path an existing directory, and the working tree a git
//     repository: a tree the init did not extract is a planner that would
//     plan blind.
func (a *Agent) repositories() ([]manifestRepository, error) {
	path := a.cfg.repositoriesManifest()
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("repositories manifest: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, manifestMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("repositories manifest: %w", err)
	}
	if len(raw) > manifestMaxBytes {
		return nil, fmt.Errorf("repositories manifest: over %d bytes", manifestMaxBytes)
	}
	repos, err := parseRepositories(raw, a.cfg)
	if err != nil {
		return nil, fmt.Errorf("repositories manifest %s: %w", path, err)
	}
	for i, r := range repos {
		info, err := os.Stat(r.Path)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("repositories manifest %s: the tree of %q (%s) is missing at %s; "+
				"the plan would be made without it", path, r.Key, r.URL, r.Path)
		}
		if i == 0 {
			if _, err := os.Stat(filepath.Join(r.Path, ".git")); err != nil {
				return nil, fmt.Errorf("repositories manifest %s: the working tree %s is not a git repository",
					path, r.Path)
			}
		}
	}
	return repos, nil
}

// parseRepositories parses and checks the manifest's lines against cfg's
// workspace (see repositories), leaving the directories to the caller.
func parseRepositories(raw []byte, cfg Config) ([]manifestRepository, error) {
	var repos []manifestRepository
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for line := 1; sc.Scan(); line++ {
		fields := strings.Split(sc.Text(), " ")
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d is not \"<key> <path> <url>\"", line)
		}
		repos = append(repos, manifestRepository{Key: fields[0], Path: fields[1], URL: fields[2]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(repos) < 2:
		return nil, fmt.Errorf("lists %d repositories; a manifest lists every repository of a "+
			"multi-repository plan, at least 2", len(repos))
	case len(repos) > report.PlanMaxRepositories:
		return nil, fmt.Errorf("lists %d repositories, over %d", len(repos), report.PlanMaxRepositories)
	}
	keys := map[string]bool{}
	for i, r := range repos {
		want := cfg.treeDir(r.Key)
		if i == 0 {
			want = cfg.repoDir()
		}
		switch {
		case len(r.Key) > repositoryKeyMaxLen || !repositoryKey.MatchString(r.Key):
			return nil, fmt.Errorf("line %d: key %q is not a Project repository key", i+1, r.Key)
		case keys[r.Key]:
			return nil, fmt.Errorf("line %d: key %q is listed twice", i+1, r.Key)
		case r.Path != want:
			return nil, fmt.Errorf("line %d: %q is at %q, not %q", i+1, r.Key, r.Path, want)
		case report.RepositoryPath(r.URL) == "":
			return nil, fmt.Errorf("line %d: %q is not an https://<host>/<owner>/<name> URL", i+1, r.URL)
		case slices.ContainsFunc(repos[:i], func(o manifestRepository) bool {
			return report.SameRepository(o.URL, r.URL)
		}):
			return nil, fmt.Errorf("line %d: %q is listed twice", i+1, r.URL)
		}
		keys[r.Key] = true
	}
	return repos, nil
}

// planTrees is the plan prompt's view of the manifest: every repository's
// URL and where its tree is, the working tree first. Nil for no manifest.
func planTrees(repos []manifestRepository) []templates.PlanTree {
	if len(repos) == 0 {
		return nil
	}
	trees := make([]templates.PlanTree, len(repos))
	for i, r := range repos {
		trees[i] = templates.PlanTree{URL: r.URL, Path: r.Path}
	}
	return trees
}

// outsideManifest returns why a plan naming urls cannot be built from the
// trees it was made from — one it names is no repository the Job held — or
// "" when every one is: the in-pod half of the controller's own check that
// a plan names only its Project's repositories, made where the retry can
// be told why.
func outsideManifest(urls []string, repos []manifestRepository) string {
	if len(repos) == 0 {
		return ""
	}
	for i, u := range urls {
		if !slices.ContainsFunc(repos, func(r manifestRepository) bool { return report.SameRepository(r.URL, u) }) {
			listed := make([]string, len(repos))
			for j, r := range repos {
				listed[j] = r.URL
			}
			return fmt.Sprintf("report: plan: repositories[%d] %q is not one of the repositories this plan was "+
				"made from (%s); name each repository exactly as the request lists it", i, u,
				strings.Join(listed, ", "))
		}
	}
	return ""
}

// buildScope is what a build of a multi-repository plan is told of where
// it stands: the plan's entry for the repository it builds, and the plan's
// other entries, each built by a run of its own. Both are zero for a
// one-repository plan, which builds exactly as before.
type buildScope struct {
	this     string
	siblings []string
}

// scopeBuild resolves which of an approved plan's repositories this build
// is in: the one entry naming the Job's repository (PATCHY_REPO, the
// "owner/name" the controller launched it in), compared as the plan's own
// entries are (report.SameRepository). A plan naming one repository needs
// none. Naming several, a build that finds its repository in none of them,
// or in more than one (one owner/name on two hosts), cannot know which
// steps are its own, and is refused before anything runs.
func scopeBuild(urls []string, repo string) (buildScope, error) {
	if len(urls) < 2 {
		return buildScope{}, nil
	}
	var scope buildScope
	matches := 0
	for _, u := range urls {
		if report.SameRepository(report.RepositoryPath(u), repo) {
			matches++
			scope.this = u
			continue
		}
		scope.siblings = append(scope.siblings, u)
	}
	if matches != 1 {
		return buildScope{}, fmt.Errorf("the approved plan names %d repositories, and %d of them are this "+
			"build's repository %q; a build of a multi-repository plan builds exactly one of them",
			len(urls), matches, repo)
	}
	return scope, nil
}
