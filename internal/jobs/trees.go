// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentrun"
)

// Tree is one more repository a multi-repository intent's plan Job reads,
// read-only, beside its own (Spec.Repo, the working tree at
// /workspace/repo): a Project repository other than the planning one.
//
// Its tarball is fetched and digest-verified exactly as the Job's own, by
// the trusted prepare init, and extracted to /workspace/repos/<Key>, with
// no git history: the planner reads it, nothing builds in it.
type Tree struct {
	// Key is the repository's Project key: a DNS label of at most 16
	// characters, and the tree's directory under /workspace/repos.
	Key string
	// URL is the repository's https URL, as the Project spells it: the
	// manifest agent-runner checks a plan's repositories against.
	URL string
	// ArtifactURL and ArtifactDigest locate and pin the tree's tarball
	// (the tree Repository's status.artifact): an http(s) URL and the bare
	// hex sha256 the init verifies.
	ArtifactURL    string
	ArtifactDigest string
	// BaseSHA is the commit the tarball holds (the tree Repository's
	// status.resolvedSHA). The pod needs none — the tree has no git — but
	// it is checked and logged with the launch, beside the digest, as what
	// the planner was handed.
	BaseSHA string
}

// Per-Job Secret keys a Job with Trees carries, and only such a Job.
const (
	// secretKeyTrees lists one "<key> <sha256> <artifact URL>" line per
	// tree: what the prepare init fetches, verifies and extracts.
	secretKeyTrees = "trees"
	// secretKeyRepositories is the repositories manifest, which the init
	// stages under the workspace's input/ for agent-runner: one
	// "<key> <path> <url>" line per repository the Job holds, its own
	// first.
	secretKeyRepositories = agentrun.RepositoriesManifest
)

// Where a Job's trees are in the pod: its own repository is the working
// tree, and each other tree a directory, named by its key, beside it.
const (
	repoDir  = workspaceDir + "/repo"
	treesDir = workspaceDir + "/" + agentrun.TreesDir
)

// ErrTreesRefused wraps every refusal of a Spec's Trees: a contradiction
// in what the controller asked for, never a transient failure, so a caller
// settles the launch instead of retrying it.
var ErrTreesRefused = errors.New("jobs: trees refused")

var (
	// treeKey is the Project repository key's grammar
	// (ProjectRepository.Name, IntentRunTree.Name).
	treeKey = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// treeDigest is a bare hex sha256, as the prepare init verifies one.
	treeDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// treeCommit is a git commit, SHA-1 or SHA-256.
	treeCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// treeKeyMaxLen bounds a key as the Project schema does.
const treeKeyMaxLen = 16

// treesRefusal reports why a Spec's Trees must not be built into a Job, or
// nil when they may (or when there are none). Each refusal keeps a tree off
// a pod it was never meant for, or out of a line the init script reads by
// field:
//
//   - only a plan Job reads trees: a build or revise run works on its one
//     repository, and its image is the repository's own;
//   - never beside a repository-declared image, honoured or not: N trees
//     only ever meet the default runner image, read-only;
//   - the Job's own repository must be named (RepoKey, RepoURL) for the
//     manifest, and at most v1alpha1.MaxIntentRunTrees others;
//   - every key is a Project key, used once (the Job's own included): it is
//     a directory name the init creates;
//   - every digest is 64 hex characters and every commit a commit;
//   - no URL is empty, holds whitespace or a control character, and an
//     artifact URL is http(s), so a line splits into exactly its fields and
//     curl reads the URL as a URL, never an option.
func treesRefusal(spec Spec) error {
	if len(spec.Trees) == 0 {
		return nil
	}
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if spec.Phase != string(agentrun.PhasePlan) {
		refuse("phase %q reads one repository; only a plan Job reads trees", spec.Phase)
	}
	if spec.RunnerImage != "" {
		refuse("a Job with trees runs the default runner image, never a repository-declared one (%q)",
			spec.RunnerImage)
	}
	if len(spec.Trees) > v1alpha1.MaxIntentRunTrees {
		refuse("%d trees, over %d", len(spec.Trees), v1alpha1.MaxIntentRunTrees)
	}
	seen := map[string]bool{}
	checkKey := func(what, key string) {
		switch {
		case len(key) > treeKeyMaxLen || !treeKey.MatchString(key):
			refuse("%s key %q is not a DNS label of at most %d characters", what, key, treeKeyMaxLen)
		case seen[key]:
			refuse("%s key %q is used twice", what, key)
		}
		seen[key] = true
	}
	checkKey("the Job's own repository", spec.RepoKey)
	if !lineSafeURL(spec.RepoURL, "https://") {
		refuse("the Job's own repository URL %q is not an https URL without whitespace", spec.RepoURL)
	}
	for i, tr := range spec.Trees {
		checkKey(fmt.Sprintf("trees[%d]", i), tr.Key)
		if !lineSafeURL(tr.URL, "https://") {
			refuse("trees[%d] URL %q is not an https URL without whitespace", i, tr.URL)
		}
		if !lineSafeURL(tr.ArtifactURL, "http://", "https://") {
			refuse("trees[%d] artifact URL %q is not an http(s) URL without whitespace", i, tr.ArtifactURL)
		}
		if !treeDigest.MatchString(tr.ArtifactDigest) {
			refuse("trees[%d] artifact digest %q is not 64 hex characters", i, tr.ArtifactDigest)
		}
		if !treeCommit.MatchString(tr.BaseSHA) {
			refuse("trees[%d] commit %q is not a commit SHA", i, tr.BaseSHA)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrTreesRefused, errors.Join(errs...))
}

// lineSafeURL reports whether u starts with one of schemes, has more after
// it, and holds no whitespace or control character: one field of a line.
func lineSafeURL(u string, schemes ...string) bool {
	if strings.ContainsFunc(u, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return false
	}
	for _, s := range schemes {
		if rest, ok := strings.CutPrefix(u, s); ok && rest != "" {
			return true
		}
	}
	return false
}

// treesFile renders the init's fetch list, one line per tree, each ending
// in a line break (a POSIX read drops a last line without one).
func treesFile(trees []Tree) string {
	var b strings.Builder
	for _, tr := range trees {
		fmt.Fprintf(&b, "%s %s %s\n", tr.Key, tr.ArtifactDigest, tr.ArtifactURL)
	}
	return b.String()
}

// repositoriesFile renders the manifest agent-runner reads: the Job's own
// repository, at the working tree, then each tree at its directory.
func repositoriesFile(spec Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\n", spec.RepoKey, repoDir, spec.RepoURL)
	for _, tr := range spec.Trees {
		fmt.Fprintf(&b, "%s %s %s\n", tr.Key, treesDir+"/"+tr.Key, tr.URL)
	}
	return b.String()
}

// treesScript is appended to the prepare init's script on a Job with Trees,
// and only then, so every other Job stays byte-identical. It runs in the
// trusted runner image, identity-free, after prepareScript has staged the
// Job's own tree and handoff: each tree is fetched with the same retries,
// verified against its digest before anything of it is unpacked, and
// extracted into its own new directory with no git init — then the
// manifest is staged for agent-runner. Under set -eu a failed fetch, a
// digest mismatch or a directory that already exists ends the init, and the
// agent container never starts.
const treesScript = `mkdir -p ` + treesDir + `
while read -r key digest url; do
  mkdir "` + treesDir + `/$key"
  curl -fsSL --retry 5 --retry-all-errors "$url" -o /tmp/tree.tar.gz
  echo "$digest  /tmp/tree.tar.gz" | sha256sum -c - >/dev/null
  tar -xzf /tmp/tree.tar.gz -C "` + treesDir + `/$key" --strip-components=1
  rm -f /tmp/tree.tar.gz
done < ` + inputMount + `/` + secretKeyTrees + `
cp ` + inputMount + `/` + secretKeyRepositories + ` /workspace/input/` + secretKeyRepositories + `
`
