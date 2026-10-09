// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var errAPI = errors.New("api server unavailable")

// failOn makes the fake clientset answer verb on resource with errAPI.
func failOn(cs *fake.Clientset, verb, resource string) {
	cs.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errAPI
	})
}

// TestPodLogsStream: the real logReader opens the agent container's log
// through the clientset (whose fake serves a fixed body).
func TestPodLogsStream(t *testing.T) {
	cs := fake.NewClientset(jobPod("patchy-x-inv-a1"))
	logs := podLogs{cs: cs, namespace: "patchy-agents"}
	rc, err := logs.Stream(context.Background(), "patchy-x-inv-a1-x7k2p", "agent", false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil || len(body) == 0 {
		t.Errorf("Stream body = %q, %v; want the clientset's log body", body, err)
	}
}

func TestResultLines(t *testing.T) {
	const jobName = "patchy-lines-evl"
	body := "first line\n" + eventLine(t, investigationEvent("f-1")) + "\nEVOLVE-EVENT: {\"x\":1}\nlast"

	t.Run("every raw line in order", func(t *testing.T) {
		c := New(fake.NewClientset(jobPod(jobName)), testConfig(), nil)
		logs := &fakeLogs{body: body}
		c.logs = logs
		var got []string
		err := c.ResultLines(context.Background(), jobName, func(line []byte) error {
			got = append(got, string(line))
			return nil
		})
		if err != nil {
			t.Fatalf("ResultLines: %v", err)
		}
		want := strings.Split(body, "\n")
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("lines = %q, want %q", got, want)
		}
		if logs.pod != jobName+"-x7k2p" || logs.container != "agent" || logs.follow {
			t.Errorf("read %s/%s follow=%v, want the agent container of the job's pod, no follow",
				logs.pod, logs.container, logs.follow)
		}
	})

	t.Run("handler error stops the read", func(t *testing.T) {
		c := New(fake.NewClientset(jobPod(jobName)), testConfig(), nil)
		c.logs = &fakeLogs{body: body}
		stop := errors.New("stop")
		calls := 0
		err := c.ResultLines(context.Background(), jobName, func([]byte) error {
			calls++
			return stop
		})
		if !errors.Is(err, stop) || calls != 1 {
			t.Errorf("ResultLines = %v after %d calls, want the handler's error after 1", err, calls)
		}
	})

	t.Run("stream error", func(t *testing.T) {
		c := New(fake.NewClientset(jobPod(jobName)), testConfig(), nil)
		c.logs = &fakeLogs{err: errAPI}
		err := c.ResultLines(context.Background(), jobName, func([]byte) error { return nil })
		if !errors.Is(err, errAPI) || !strings.Contains(err.Error(), "jobs: read logs of "+jobName) {
			t.Errorf("ResultLines = %v, want a wrapped stream error", err)
		}
	})

	t.Run("no pod and the job is gone", func(t *testing.T) {
		c := New(fake.NewClientset(), testConfig(), nil)
		c.logs = &fakeLogs{}
		called := false
		err := c.ResultLines(context.Background(), jobName, func([]byte) error { called = true; return nil })
		if err == nil || !strings.Contains(err.Error(), "no pods for job") || called {
			t.Errorf("ResultLines = %v (handler called %v), want a no-pods error", err, called)
		}
	})
}

// TestWaitForAgentFailures: a terminal Job with no pod is unrecoverable, and
// API errors listing pods or reading the Job surface rather than spin.
func TestWaitForAgentFailures(t *testing.T) {
	const jobName = "patchy-wait-inv-a1"
	complete := bareJob(jobName)
	complete.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}

	tests := []struct {
		name    string
		objects []runtime.Object
		fail    [2]string // verb, resource
		wantMsg string
		wantErr error
	}{
		{name: "terminal job without pods", objects: []runtime.Object{complete}, wantMsg: "no pods for job " + jobName},
		{name: "pod list fails", fail: [2]string{"list", "pods"}, wantMsg: "jobs: list pods of " + jobName, wantErr: errAPI},
		{name: "job read fails", objects: []runtime.Object{bareJob(jobName)}, fail: [2]string{"get", "jobs"},
			wantMsg: "jobs: status of " + jobName, wantErr: errAPI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset(tt.objects...)
			if tt.fail[0] != "" {
				failOn(cs, tt.fail[0], tt.fail[1])
			}
			c := New(cs, testConfig(), nil)
			c.logs = &fakeLogs{}
			_, err := c.Result(context.Background(), jobName)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Result = %v, want error containing %q", err, tt.wantMsg)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Result = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// TestFindPodPicksNewest: with several pods for one Job (a retried pod), the
// newest one's log is read.
func TestFindPodPicksNewest(t *testing.T) {
	const jobName = "patchy-retry-inv-a1"
	old := jobPod(jobName)
	old.Name = jobName + "-old"
	old.CreationTimestamp = metav1.Unix(100, 0)
	newer := jobPod(jobName)
	newer.Name = jobName + "-new"
	newer.CreationTimestamp = metav1.Unix(200, 0)
	c := New(fake.NewClientset(old, newer), testConfig(), nil)
	logs := &fakeLogs{}
	c.logs = logs
	if _, err := c.Result(context.Background(), jobName); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if logs.pod != jobName+"-new" {
		t.Errorf("read logs of %q, want the newest pod", logs.pod)
	}
}

func TestResolveRunnersSecretReadError(t *testing.T) {
	cs := fake.NewClientset()
	failOn(cs, "get", "secrets")
	runners := map[string]Runner{"claude": {Image: "img", Secret: "anthropic", SecretKey: "api-key"}}
	enabled, err := ResolveRunners(context.Background(), cs, "patchy-agents", runners, nil)
	if err == nil || !errors.Is(err, errAPI) || !strings.Contains(err.Error(), `harness "claude"`) {
		t.Errorf("ResolveRunners = (%v, %v), want a wrapped secret read error naming the harness", enabled, err)
	}
	if len(enabled) != 0 {
		t.Errorf("enabled = %v, want none on error", enabled)
	}
}

func TestCreateUnknownHarness(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	spec := testSpec()
	spec.Harness = "copilot"
	if _, _, err := c.Create(context.Background(), spec); err == nil ||
		!strings.Contains(err.Error(), `no runner configured for harness "copilot"`) {
		t.Errorf("Create = %v, want a no-runner error", err)
	}
	eval := testEvalSpec()
	eval.Harness = "copilot"
	if _, err := c.CreateEval(context.Background(), eval); err == nil ||
		!strings.Contains(err.Error(), `no runner configured for harness "copilot"`) {
		t.Errorf("CreateEval = %v, want a no-runner error", err)
	}
	if acts := cs.Actions(); len(acts) != 0 {
		t.Errorf("an unbuildable spec reached the API: %v", acts)
	}
}

// TestCreateAPIFailures: each API step of a launch surfaces its failure; a
// failed Job create removes the handoff Secret it would have owned, so no
// orphan credential-adjacent object is left behind.
func TestCreateAPIFailures(t *testing.T) {
	tests := []struct {
		name           string
		verb, res      string
		wantMsg        string
		wantSecretGone bool
	}{
		{name: "secret create", verb: "create", res: "secrets", wantMsg: "jobs: create secret"},
		{name: "job create", verb: "create", res: "jobs", wantMsg: "jobs: create job", wantSecretGone: true},
		{name: "secret owner update", verb: "update", res: "secrets", wantMsg: "jobs: own secret"},
	}
	launches := []struct {
		name   string
		launch func(c *Client) error
	}{
		{"finding", func(c *Client) error { _, _, err := c.Create(context.Background(), testSpec()); return err }},
		{"eval", func(c *Client) error { _, err := c.CreateEval(context.Background(), testEvalSpec()); return err }},
	}
	for _, l := range launches {
		for _, tt := range tests {
			t.Run(l.name+"/"+tt.name, func(t *testing.T) {
				cs := fake.NewClientset()
				failOn(cs, tt.verb, tt.res)
				err := l.launch(New(cs, testConfig(), nil))
				if !errors.Is(err, errAPI) || !strings.Contains(err.Error(), tt.wantMsg) {
					t.Fatalf("launch = %v, want %q wrapping the API error", err, tt.wantMsg)
				}
				secrets, _ := cs.CoreV1().Secrets("patchy-agents").List(context.Background(), metav1.ListOptions{})
				if tt.wantSecretGone && len(secrets.Items) != 0 {
					t.Errorf("handoff Secret left behind after a failed Job create: %d", len(secrets.Items))
				}
			})
		}
	}
}
