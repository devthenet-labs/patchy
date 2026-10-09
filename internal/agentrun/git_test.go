// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"strings"
	"testing"
)

// TestVerifyCommitted: commit.sh's work counts only when it left a clean
// tree and at least one commit over the base.
func TestVerifyCommitted(t *testing.T) {
	ctx := context.Background()
	t.Run("committed and clean", func(t *testing.T) {
		f := newRepoFixture(t)
		f.write("change.txt", "after\n")
		f.git("commit", "-qam", "fix")
		if err := verifyCommitted(ctx, f.dir, f.base, fixtureBranch); err != nil {
			t.Errorf("verifyCommitted = %v, want nil", err)
		}
	})
	t.Run("dirty tree", func(t *testing.T) {
		f := newRepoFixture(t)
		f.write("change.txt", "after\n")
		f.git("commit", "-qam", "fix")
		f.write("stray.txt", "left behind\n")
		err := verifyCommitted(ctx, f.dir, f.base, fixtureBranch)
		if err == nil || !strings.Contains(err.Error(), "working tree not clean") ||
			!strings.Contains(err.Error(), "stray.txt") {
			t.Errorf("verifyCommitted = %v, want a not-clean error naming stray.txt", err)
		}
	})
	t.Run("no commits", func(t *testing.T) {
		f := newRepoFixture(t)
		err := verifyCommitted(ctx, f.dir, f.base, fixtureBranch)
		if err == nil || !strings.Contains(err.Error(), "carries no commits") {
			t.Errorf("verifyCommitted = %v, want a no-commits error", err)
		}
	})
	t.Run("unknown base", func(t *testing.T) {
		f := newRepoFixture(t)
		if err := verifyCommitted(ctx, f.dir, "0000000000000000000000000000000000000000", fixtureBranch); err == nil {
			t.Error("verifyCommitted with an unknown base = nil, want an error")
		}
	})
	t.Run("not a clone", func(t *testing.T) {
		if err := verifyCommitted(ctx, t.TempDir(), "HEAD", fixtureBranch); err == nil {
			t.Error("verifyCommitted outside a clone = nil, want an error")
		}
	})
}

func TestBuildChangesetUnknownBase(t *testing.T) {
	f := newRepoFixture(t)
	f.write("change.txt", "after\n")
	f.git("commit", "-qam", "fix")
	if cs, err := buildChangeset(context.Background(), f.dir, "deadbeef", fixtureBranch, 1<<20); err == nil {
		t.Errorf("buildChangeset with an unknown base = %+v, want an error", cs)
	}
}
