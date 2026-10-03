// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// AgentDockerfilePath is the agent image's recipe, beside the declaration.
const AgentDockerfilePath = ".patchy/Dockerfile"

// ToolchainPaths are the agent toolchain's files: the image
// .patchy/agent.yaml declares and the recipe it is built from. Once written
// they are the repository's own, since every toolchain change edits the
// recipe and bumps the declared tag. A scaffold rendered again would roll
// the declaration back to ToolchainTag, an image the registry already holds,
// and drop the recipe's changes, so a forced scaffold keeps them
// (KeepToolchain).
var ToolchainPaths = []string{runnerimage.AgentYAMLPath, AgentDockerfilePath}

// toolchainTagPattern is an agent image tag the trusted agent publisher
// accepts, the same pattern its guard holds .patchy/agent.yaml to.
var toolchainTagPattern = regexp.MustCompile(`^toolchain-v[1-9][0-9]{0,5}$`)

// Kept is the part of the agent toolchain KeepToolchain left as it is.
type Kept struct {
	// Paths are the toolchain files kept, in ToolchainPaths order.
	Paths []string
	// Tag is the toolchain-v<N> tag the kept .patchy/agent.yaml declares,
	// empty when no .patchy/agent.yaml was kept.
	Tag string
}

// KeepToolchain takes out of files every toolchain file that already exists
// under dir as a regular file, so a forced Write leaves the repository's
// toolchain as it is, and returns the rest. A kept .patchy/agent.yaml is
// read the way source-controller reads it and must declare a toolchain-v<N>
// tag of exactly the agent repository o publishes to, as the agent
// publisher requires: when o moved the registry, the agent prefix or the
// image name, the declaration would no longer publish, so the scaffold is
// refused rather than half applied. A path that is not a regular file, or
// that a link leads to, stays in files for Write to refuse.
func KeepToolchain(dir string, files []File, o Options) ([]File, Kept, error) {
	var kept Kept
	keep := map[string]bool{}
	for _, p := range ToolchainPaths {
		state, err := inspect(dir, p)
		if err != nil {
			return nil, Kept{}, err
		}
		if state == stateFile {
			keep[p] = true
			kept.Paths = append(kept.Paths, p)
		}
	}
	if keep[runnerimage.AgentYAMLPath] {
		tag, err := keptTag(dir, o)
		if err != nil {
			return nil, Kept{}, err
		}
		kept.Tag = tag
	}
	rest := slices.DeleteFunc(slices.Clone(files), func(f File) bool { return keep[f.Path] })
	return rest, kept, nil
}

// keptTag is the toolchain-v<N> tag the existing .patchy/agent.yaml under
// dir declares for o's agent repository.
func keptTag(dir string, o Options) (string, error) {
	data, size, err := readDeclaration(filepath.Join(dir, filepath.FromSlash(runnerimage.AgentYAMLPath)))
	if err != nil {
		return "", err
	}
	decl, err := runnerimage.Declare(runnerimage.Files{
		AgentYAML: runnerimage.File{Present: true, Size: size, Data: data},
	})
	if err != nil {
		return "", fmt.Errorf("the existing %s, which --force keeps, cannot be read: %w; fix it, or remove it "+
			"and %s to generate both again", runnerimage.AgentYAMLPath, err, AgentDockerfilePath)
	}
	repository := o.Registry + "/" + o.AgentRepository()
	tag, ok := strings.CutPrefix(decl.Image, repository+":")
	if !ok || !toolchainTagPattern.MatchString(tag) {
		return "", fmt.Errorf("the existing %s, which --force keeps, declares %s, not a toolchain-v<N> tag of %s, "+
			"the agent repository these options publish to, so the agent publisher would refuse it; pass the "+
			"options it was generated with, set its image to %s:toolchain-v<N>, or remove it and %s to generate "+
			"both again", runnerimage.AgentYAMLPath, decl.Image, repository, repository, AgentDockerfilePath)
	}
	return tag, nil
}

// readDeclaration reads at most runnerimage.MaxDeclarationBytes of path. A
// longer file comes back as nil data with its size, which Declare refuses
// as source-controller does.
func readDeclaration(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, runnerimage.MaxDeclarationBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > runnerimage.MaxDeclarationBytes {
		info, err := f.Stat()
		if err != nil {
			return nil, 0, err
		}
		return nil, info.Size(), nil
	}
	return data, int64(len(data)), nil
}
