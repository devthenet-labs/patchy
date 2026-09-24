// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// testFindingCommandsSchema writes a fully populated status.commands through
// the status subresource and reads it back, so a field the generated CRD
// does not know (and the API server would silently prune, losing a command
// the webhook recorded) fails here; and it pins the bounds the webhook
// handler and the projection rely on.
func testFindingCommandsSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	f := &patchyv1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: "finding-cmd-1", Namespace: "default"},
		Spec: patchyv1.FindingSpec{
			IntegrationRef: patchyv1.LocalObjectReference{Name: "gh"},
			Source:         "github-code-scanning",
			Advisories:     []string{"CVE-2026-0001"},
		},
	}
	if err := c.Create(ctx, f); err != nil {
		t.Fatalf("Create(finding) = %v", err)
	}
	at := metav1.NewTime(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	command := func(id int64) patchyv1.FindingCommand {
		return patchyv1.FindingCommand{
			CommentID: id,
			Actor:     patchyv1.CommandActor{Login: "octo-cat", ID: 583231, Type: "User"},
			Verb:      "approve", Note: "ship it", Legacy: true, ReceivedAt: at,
			Outcome: patchyv1.CommandUnavailable, DecidedAt: &at,
			Available: []string{"expedite", "suspend"}, Applied: false,
		}
	}
	want := &patchyv1.FindingCommands{
		Pending:  []patchyv1.FindingCommand{command(12)},
		Consumed: []int64{3, 7},
	}
	f.Status.Commands = want.DeepCopy()
	if err := c.Status().Update(ctx, f); err != nil {
		t.Fatalf("Status().Update(commands) = %v, want nil", err)
	}
	got := &patchyv1.Finding{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatalf("Get(finding) = %v", err)
	}
	if !equality.Semantic.DeepEqual(got.Status.Commands, want) {
		t.Errorf("status.commands = %+v, want %+v", got.Status.Commands, want)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*patchyv1.FindingCommands)
	}{
		{"more pending than MaxPendingCommands", func(cs *patchyv1.FindingCommands) {
			cs.Pending = nil
			for i := range patchyv1.MaxPendingCommands + 1 {
				cs.Pending = append(cs.Pending, command(int64(100+i)))
			}
		}},
		{"more consumed than MaxConsumedCommands", func(cs *patchyv1.FindingCommands) {
			cs.Consumed = nil
			for i := range patchyv1.MaxConsumedCommands + 1 {
				cs.Consumed = append(cs.Consumed, int64(200+i))
			}
		}},
		{"two pending commands for one comment", func(cs *patchyv1.FindingCommands) {
			cs.Pending = append(cs.Pending, command(12))
		}},
		{"a verb that is not lower-case letters", func(cs *patchyv1.FindingCommands) {
			cs.Pending[0].Verb = "<b>"
		}},
		{"a note over 1 KiB", func(cs *patchyv1.FindingCommands) {
			cs.Pending[0].Note = strings.Repeat("x", 1025)
		}},
		{"an unknown outcome", func(cs *patchyv1.FindingCommands) {
			cs.Pending[0].Outcome = "Maybe"
		}},
		{"a login that is not one", func(cs *patchyv1.FindingCommands) {
			cs.Pending[0].Actor.Login = "not a login"
		}},
		{"a comment id of zero", func(cs *patchyv1.FindingCommands) {
			cs.Pending[0].CommentID = 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := &patchyv1.Finding{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(f), cur); err != nil {
				t.Fatalf("Get(finding) = %v", err)
			}
			tc.mutate(cur.Status.Commands)
			if err := c.Status().Update(ctx, cur); err == nil {
				t.Errorf("Status().Update(%s) = nil, want a schema rejection", tc.name)
			}
		})
	}
}
