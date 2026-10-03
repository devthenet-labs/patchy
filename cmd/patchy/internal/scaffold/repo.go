// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// ownerPattern is GitHub's account name grammar.
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	// namePattern is GitHub's repository name grammar.
	namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	// branchPattern keeps a branch name to characters every generated file
	// (YAML, JavaScript, a bash regex) can carry literally.
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
)

// Repo is a GitHub repository by name.
type Repo struct {
	Owner string
	Name  string
}

// String is owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// ParseRepo parses owner/name.
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || !ownerPattern.MatchString(owner) || !namePattern.MatchString(name) || name == "." || name == ".." {
		return Repo{}, fmt.Errorf("repository %q must be owner/name, as GitHub names it", s)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// ValidateBranch checks a default branch name.
func ValidateBranch(s string) error {
	if !branchPattern.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".lock") {
		return fmt.Errorf("branch %q must be letters, digits, '.', '_', '-' and '/' (%s)", s, branchPattern)
	}
	return nil
}

// errNoGitDir reports a directory with no .git directory to read.
var errNoGitDir = errors.New("no .git directory")

// OriginRepo reads the repository the origin remote of the git checkout at
// dir points at, from .git/config, so no git binary is needed. Only a plain
// .git directory is read (not a worktree's .git file).
func OriginRepo(dir string) (Repo, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Repo{}, errNoGitDir
		}
		return Repo{}, err
	}
	raw := originURL(data)
	if raw == "" {
		return Repo{}, errors.New("the git checkout has no origin remote")
	}
	return repoFromURL(raw)
}

// originURL is the url of the [remote "origin"] section of a git config.
func originURL(config []byte) string {
	in := false
	sc := bufio.NewScanner(bytes.NewReader(config))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section := strings.TrimSpace(strings.Trim(line, "[]"))
			kind, sub, _ := strings.Cut(section, " ")
			in = strings.EqualFold(kind, "remote") && strings.TrimSpace(sub) == `"origin"`
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if in && ok && strings.EqualFold(strings.TrimSpace(key), "url") {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

// repoFromURL takes owner/name from a remote URL: https://host/o/n(.git),
// ssh://git@host/o/n(.git) or the scp-like git@host:o/n(.git).
func repoFromURL(raw string) (Repo, error) {
	path := raw
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		path = u.Path
	} else if _, rest, ok := strings.Cut(raw, ":"); ok {
		path = rest
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return Repo{}, fmt.Errorf("origin %q names no owner/name", raw)
	}
	repo, err := ParseRepo(parts[len(parts)-2] + "/" + parts[len(parts)-1])
	if err != nil {
		return Repo{}, fmt.Errorf("origin %q: %w", raw, err)
	}
	return repo, nil
}

// OriginBranch reads the default branch the origin remote's HEAD points at
// (.git/refs/remotes/origin/HEAD, which a clone writes), and whether there
// is one to read.
func OriginBranch(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, ".git", "refs", "remotes", "origin", "HEAD"))
	if err != nil {
		return "", false
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: refs/remotes/origin/")
	if !ok || ValidateBranch(ref) != nil {
		return "", false
	}
	return ref, true
}
