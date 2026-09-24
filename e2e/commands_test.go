// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
)

// commentDelivery is the issue_comment.created delivery of a comment the
// test wrote on the fake (id, by author) on a tracking issue, shaped as the
// recorded fixture is.
func commentDelivery(t *testing.T, issueURL string, number int, id int64, body string,
	author fakegithub.Actor) []byte {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal(fixture(t, "issue_comment.approve.json"), &ev); err != nil {
		t.Fatal(err)
	}
	issue := ev["issue"].(map[string]any)
	issue["number"], issue["html_url"] = number, issueURL
	comment := ev["comment"].(map[string]any)
	comment["id"], comment["body"] = id, body
	comment["user"] = map[string]any{"login": author.Login, "id": author.ID, "type": author.Type}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// commandReplies are the replies patchy posted on issue number to the
// command in comment id.
func commandReplies(gh *fakegithub.Server, number int, id int64) []string {
	var out []string
	for _, body := range gh.Comments(number) {
		if strings.HasPrefix(body, fmt.Sprintf("<!-- patchy:command %d -->\n", id)) {
			out = append(out, body)
		}
	}
	return out
}

// TestFindingCommands drives "/patchy approve" on a held finding's tracking
// issue through the shipped integration-controller and remediation-
// controller. The webhook handler only records each command; the
// projection asks GitHub for the commenter's permission on the repository,
// applies the approval, and answers with an eyes reaction and exactly one
// reply. A drive-by commenter (read, as every account is on a public
// repository) is refused, and refused again with the reaction alone; a
// maintainer with write access releases the hold — even with the delivery
// arriving twice.
func TestFindingCommands(t *testing.T) {
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)
	ctx := context.Background()

	const name = "finding-ffffffffff-1"
	fnd := &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "github"},
			TrackingRef:    &v1alpha1.LocalObjectReference{Name: "github"},
			Source:         "ghas",
			Advisories:     []string{"CWE-79"},
			Title:          "Reflected cross-site scripting",
			Severity:       v1alpha1.LevelHigh,
			Repository: &v1alpha1.FindingRepository{
				Type: v1alpha1.RepositoryTypeGitHub,
				URL:  "https://127.0.0.1/acme/shop", Name: "acme/shop", DefaultBranch: "main",
			},
		},
	}
	if err := cl.client.Create(ctx, fnd); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	fnd.Status.Phase = v1alpha1.PhaseAwaitingApproval
	fnd.Status.PhaseTimes = []v1alpha1.PhaseTime{{Phase: v1alpha1.PhaseAwaitingApproval, At: now}}
	fnd.Status.Investigation = &v1alpha1.InvestigationSummary{
		Name: name + "-inv-1", Attempt: 1, Recommendation: v1alpha1.RecommendationRemediate, AwaitApproval: true,
	}
	if err := cl.client.Status().Update(ctx, fnd); err != nil {
		t.Fatal(err)
	}

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.controller(t, "integration-controller", "--listen-addr", listen)
	cl.controller(t, "remediation-controller", runnerArgs...)
	webhookURL := "http://" + listen + "/github/webhooks"

	get := func() v1alpha1.Finding {
		var cur v1alpha1.Finding
		if err := cl.client.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return cur
	}
	consumed := func(id int64) bool {
		cmds := get().Status.Commands
		return cmds != nil && slices.Contains(cmds.Consumed, id)
	}
	// A refusal leaves pending without joining consumed (commenting must
	// not push a maintainer's command out of it); its author is remembered
	// instead, so a later refusal gets the reaction alone.
	refusedFor := func(id int64, author fakegithub.Actor) bool {
		cmds := get().Status.Commands
		return cmds != nil && !slices.Contains(cmds.Consumed, id) &&
			!slices.ContainsFunc(cmds.Pending, func(c v1alpha1.FindingCommand) bool { return c.CommentID == id }) &&
			slices.Contains(cmds.RefusedActors, author.ID)
	}

	var tracking v1alpha1.TrackingStatus
	eventually(t, "the held finding's tracking issue and its approval notice", func() bool {
		cur := get()
		if cur.Status.Tracking == nil ||
			cur.GetAnnotations()["patchy.bitwisemedia.uk/projected-notice"] != string(v1alpha1.PhaseAwaitingApproval) {
			return false
		}
		tracking = *cur.Status.Tracking
		return true
	})
	number := int(tracking.IssueNumber)
	if notices := strings.Join(gh.Comments(number), "\n"); !strings.Contains(notices, "`/patchy approve`") {
		t.Errorf("the hold's notice does not offer /patchy approve:\n%s", notices)
	}

	// A drive-by commenter reads as "read": refused, and the hold stays.
	driveBy := fakegithub.Actor{Login: "drive-by", ID: 300001, Type: "User"}
	refused := gh.CommentAs(number, "/patchy approve", driveBy)
	deliver(t, webhookURL, "issue_comment",
		commentDelivery(t, tracking.URL, number, refused, "/patchy approve", driveBy))
	eventually(t, "the drive-by's command to be refused and answered", func() bool {
		return slices.Equal(gh.Reactions(refused), []string{"eyes"}) &&
			len(commandReplies(gh, number, refused)) == 1 && refusedFor(refused, driveBy)
	})
	if reply := commandReplies(gh, number, refused)[0]; !strings.Contains(reply,
		"@drive-by you may not use `/patchy approve` here") {
		t.Errorf("refusal reply:\n%s", reply)
	}
	if cur := get(); cur.Spec.Approval != nil || cur.Status.Phase != v1alpha1.PhaseAwaitingApproval {
		t.Fatalf("approval %+v, phase %s: a read-only commenter released the hold",
			cur.Spec.Approval, cur.Status.Phase)
	}

	// The same account refused again gets the reaction alone: commenting
	// cannot make patchy post a reply per comment.
	again := gh.CommentAs(number, "/patchy approve", driveBy)
	deliver(t, webhookURL, "issue_comment",
		commentDelivery(t, tracking.URL, number, again, "/patchy approve", driveBy))
	eventually(t, "the drive-by's second command to be refused with the reaction alone", func() bool {
		cmds := get().Status.Commands
		return slices.Equal(gh.Reactions(again), []string{"eyes"}) && cmds != nil &&
			!slices.ContainsFunc(cmds.Pending, func(c v1alpha1.FindingCommand) bool { return c.CommentID == again })
	})
	if n := len(commandReplies(gh, number, again)); n != 0 {
		t.Errorf("replies to the repeated refusal = %d, want the reaction alone", n)
	}

	// A maintainer with write access approves; GitHub delivers it twice.
	gh.SetRole("maintainer", "write")
	maintainer := fakegithub.Actor{Login: "maintainer", ID: 300002, Type: "User"}
	approved := gh.CommentAs(number, "/patchy approve ship it", maintainer)
	payload := commentDelivery(t, tracking.URL, number, approved, "/patchy approve ship it", maintainer)
	deliver(t, webhookURL, "issue_comment", payload)
	deliver(t, webhookURL, "issue_comment", payload)
	eventually(t, "the maintainer's approval to release the hold", func() bool {
		cur := get()
		return cur.Spec.Approval != nil && cur.Spec.Approval.By == "maintainer" &&
			cur.Spec.Approval.Note == "ship it" &&
			meta.IsStatusConditionTrue(cur.Status.Conditions, v1alpha1.ConditionApproved)
	})
	eventually(t, "the approval to be answered", func() bool {
		return slices.Equal(gh.Reactions(approved), []string{"eyes"}) &&
			len(commandReplies(gh, number, approved)) == 1 && consumed(approved)
	})
	if reply := commandReplies(gh, number, approved)[0]; !strings.Contains(reply,
		"@maintainer `/patchy approve` is done") {
		t.Errorf("approval reply:\n%s", reply)
	}
	consistently(t, "exactly one reply to each command, and none to the repeated refusal", func() bool {
		return len(commandReplies(gh, number, refused)) == 1 && len(commandReplies(gh, number, approved)) == 1 &&
			len(commandReplies(gh, number, again)) == 0
	})
}
