// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// git runs one git command in dir, returning trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// gitRaw runs one git command in dir, returning stdout byte-exact (blob
// contents must not be trimmed).
func gitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitRawEnv(ctx, dir, nil, args...)
}

// gitRawEnv is gitRaw with env appended to the process's environment.
func gitRawEnv(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

// headSHA resolves the clone's current commit. Captured before the
// remediation branch is created, it is the pinned base the changeset is
// diffed against and the pushed commit's parent.
func headSHA(ctx context.Context, dir string) (string, error) {
	return git(ctx, dir, "rev-parse", "HEAD")
}

// ensureIdentity configures a commit identity when the clone has none, so
// commit.sh can commit.
func ensureIdentity(ctx context.Context, dir string) error {
	if _, err := git(ctx, dir, "config", "user.email"); err == nil {
		return nil
	}
	if _, err := git(ctx, dir, "config", "user.email", "patchy[bot]@users.noreply.github.com"); err != nil {
		return err
	}
	_, err := git(ctx, dir, "config", "user.name", "patchy[bot]")
	return err
}

// checkoutBranch creates (or resets to HEAD) the remediation branch.
func checkoutBranch(ctx context.Context, dir, branch string) error {
	_, err := git(ctx, dir, "checkout", "-B", branch)
	return err
}

// verifyCommitted checks the working tree is clean and the branch carries
// at least one commit over base.
func verifyCommitted(ctx context.Context, dir, base, branch string) error {
	status, err := git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("working tree not clean after commit.sh:\n%s", status)
	}
	count, err := git(ctx, dir, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 {
		return fmt.Errorf("branch %s carries no commits over %s", branch, base)
	}
	return nil
}

// fingerprint identifies a clone's state as a report repair must leave it:
// the commit HEAD names, the tree its index stages, and the tree of its
// working files, untracked ones included and ignored ones not.
type fingerprint struct {
	head, staged, worktree string
}

// treeFingerprint fingerprints the clone in dir. Both trees are written
// from a copy of the clone's index, inside its git directory, so the
// clone's own index — what commit.sh will commit — is never touched; the
// copy keeps git's stat cache, so only files changed since it was written
// are hashed again.
func treeFingerprint(ctx context.Context, dir string) (fingerprint, error) {
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return fingerprint{}, err
	}
	gitDir, err := git(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fingerprint{}, err
	}
	index, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fingerprint{}, err
	}
	tmp, err := os.CreateTemp(gitDir, "patchy-fingerprint-index-")
	if err != nil {
		return fingerprint{}, err
	}
	copied := tmp.Name()
	defer func() { _ = os.Remove(copied) }()
	_, err = tmp.Write(index)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fingerprint{}, err
	}
	if len(index) == 0 {
		// No index yet: git reads a missing index as an empty one, but an
		// empty file as a broken one.
		if err := os.Remove(copied); err != nil {
			return fingerprint{}, err
		}
	}
	env := []string{"GIT_INDEX_FILE=" + copied}
	staged, err := gitRawEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return fingerprint{}, err
	}
	if _, err := gitRawEnv(ctx, dir, env, "add", "--all"); err != nil {
		return fingerprint{}, err
	}
	worktree, err := gitRawEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return fingerprint{}, err
	}
	return fingerprint{
		head: head, staged: strings.TrimSpace(string(staged)), worktree: strings.TrimSpace(string(worktree)),
	}, nil
}

// changedPathsShown bounds how many changed paths a refusal names, and
// changedPathBytes how much of each.
const (
	changedPathsShown = 3
	changedPathBytes  = 120
)

// changed describes how the clone in dir moved from before to after, for a
// refused repair's detail: whether it committed, staged, or changed working
// files, naming the first few of those.
func changed(ctx context.Context, dir string, before, after fingerprint) string {
	var parts []string
	if after.head != before.head {
		parts = append(parts, "it moved HEAD (a commit)")
	}
	if after.staged != before.staged {
		parts = append(parts, "it staged changes")
	}
	if after.worktree != before.worktree {
		what := "it changed the working tree"
		out, err := git(ctx, dir, "diff", "--name-only", "--no-renames", before.worktree, after.worktree)
		if err == nil && out != "" {
			paths := strings.Split(out, "\n")
			shown := paths[:min(len(paths), changedPathsShown)]
			for i, p := range shown {
				shown[i] = oneLine(p, changedPathBytes)
			}
			what += ": " + strings.Join(shown, ", ")
			if more := len(paths) - len(shown); more > 0 {
				what += fmt.Sprintf(" and %d more", more)
			}
		}
		parts = append(parts, what)
	}
	return strings.Join(parts, "; ") + ", and a repair may change nothing but the report and commit.sh"
}
