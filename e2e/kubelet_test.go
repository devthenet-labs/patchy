// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// kubeletNode is the Node the fake kubelet registers and the pods it runs
// are bound to.
const kubeletNode = "e2e-kubelet"

// fakeAgentScript is the fake agent-runner the kubelet runs in place of the
// agent container's image: it speaks every stage's event stream.
const fakeAgentScript = "../hack/fake-agent/agent-runner"

// kubelet stands in for the node agent Jobs run on. envtest runs no
// kubelet, so a Job a controller creates would never run, and the leg a
// Job's output drives (collect, transcript, changeset push, pull request)
// could not be driven end to end. It plays the kubelet's part at the
// kubelet's own edge, and nothing inside the binaries changes:
//
//   - It registers a Node whose kubelet endpoint is its own TLS server, so
//     the real API server serves pods/log by proxying to it, as it proxies
//     to a real kubelet: the controllers read logs through the clientset,
//     unchanged.
//   - For each Job carrying its run kind, it does what the Job's pod would.
//     The prepare step fetches the artifact and checks its digest, and
//     copies the handoff from the per-Job Secret into the workspace's
//     input/. The agent container is hack/fake-agent, run with the
//     container's own environment.
//   - It records the pod, bound to the Node, with its containers'
//     terminated statuses, then the Job's terminal status, as the kubelet
//     and the Job controller would.
//
// Jobs of any other run kind are left alone, never run, as without it.
type kubelet struct {
	t       *testing.T
	cl      *cluster
	runKind string
	script  string
	work    string
	srv     *httptest.Server

	mu   sync.Mutex
	logs map[string][]byte // "<namespace>/<pod>/<container>"
	ran  map[string]bool   // Job names already run
	runs []agentRun        // in the order they ran
}

// agentRun is one Job as the kubelet ran it: the Job, the handoff its agent
// was given, and what the agent printed.
type agentRun struct {
	Job batchv1.Job
	// Image is the agent container's image.
	Image string
	// Env is the agent container's environment, literal values only.
	Env map[string]string
	// Issue and Investigation are the handoff files; HasInvestigation
	// says whether the Secret carried investigation.md at all.
	Issue            []byte
	Investigation    []byte
	HasInvestigation bool
	Stdout           []byte
	ExitCode         int
}

// startKubelet registers the Node and runs every Job of runKind the
// controllers create, until the test ends.
func startKubelet(t *testing.T, cl *cluster, runKind string) *kubelet {
	t.Helper()
	script, err := filepath.Abs(fakeAgentScript)
	if err != nil {
		t.Fatal(err)
	}
	k := &kubelet{
		t: t, cl: cl, runKind: runKind, script: script, work: t.TempDir(),
		logs: map[string][]byte{}, ran: map[string]bool{},
	}
	k.srv = httptest.NewTLSServer(http.HandlerFunc(k.serveLogs))
	t.Cleanup(k.srv.Close)
	k.registerNode(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go k.loop(ctx, done)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return k
}

// registerNode creates the Node the API server proxies log reads to: its
// kubelet endpoint is the fake's TLS listener. With no kubelet CA
// configured, the API server does not verify the kubelet's certificate, as
// it would not verify a real one's.
func (k *kubelet) registerNode(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, portText, err := net.SplitHostPort(k.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: kubeletNode}}
	if err := k.cl.client.Create(ctx, node); err != nil {
		t.Fatalf("register node: %v", err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}
	node.Status.DaemonEndpoints.KubeletEndpoint.Port = int32(port)
	if err := k.cl.client.Status().Update(ctx, node); err != nil {
		t.Fatalf("register node status: %v", err)
	}
}

// serveLogs is the kubelet's containerLogs endpoint, which the API server's
// pods/log proxies to.
func (k *kubelet) serveLogs(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 4 || parts[0] != "containerLogs" {
		http.NotFound(w, r)
		return
	}
	ns, pod, container := parts[1], parts[2], parts[3]
	k.mu.Lock()
	body, ok := k.logs[ns+"/"+pod+"/"+container]
	k.mu.Unlock()
	if !ok {
		http.Error(w, "container "+container+" of "+pod+" has no log", http.StatusNotFound)
		return
	}
	_, _ = w.Write(body)
}

// splitPath splits a URL path into its non-empty segments.
func splitPath(p string) []string {
	return slices.DeleteFunc(strings.Split(p, "/"), func(s string) bool { return s == "" })
}

// loop runs each new Job of the kubelet's run kind, one at a time.
func (k *kubelet) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list batchv1.JobList
		if err := k.cl.client.List(ctx, &list, client.InNamespace(agentsNS),
			client.MatchingLabels{v1alpha1.LabelRunKind: k.runKind}); err != nil {
			continue
		}
		for i := range list.Items {
			job := &list.Items[i]
			k.mu.Lock()
			seen := k.ran[job.Name]
			k.mu.Unlock()
			if seen || !job.DeletionTimestamp.IsZero() {
				continue
			}
			if err := k.run(ctx, job); err != nil {
				if ctx.Err() != nil {
					return
				}
				k.t.Errorf("kubelet: run job %s: %v", job.Name, err)
			}
			k.mu.Lock()
			k.ran[job.Name] = true
			k.mu.Unlock()
		}
	}
}

// run does what job's pod would, then records the pod and the Job's end.
func (k *kubelet) run(ctx context.Context, job *batchv1.Job) error {
	started := metav1.Now()
	spec := job.Spec.Template.Spec
	prepare, agent, err := jobContainers(&spec)
	if err != nil {
		return err
	}
	var secret corev1.Secret
	if err := k.cl.client.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &secret); err != nil {
		// A Job deleted since it was listed never gets a pod, and its
		// Secret goes with it.
		var cur batchv1.Job
		if apierrors.IsNotFound(err) &&
			(apierrors.IsNotFound(k.cl.client.Get(ctx, client.ObjectKeyFromObject(job), &cur)) ||
				!cur.DeletionTimestamp.IsZero()) {
			return nil
		}
		return fmt.Errorf("read the handoff secret: %w", err)
	}
	rec := agentRun{Job: *job.DeepCopy(), Image: agent.Image, Env: literalEnv(agent.Env)}
	rec.Issue = secret.Data["issue.md"]
	rec.Investigation, rec.HasInvestigation = secret.Data["investigation.md"]

	// The prepare step: the digest-verified artifact, then the handoff.
	prepareExit := int32(0)
	if err := fetchArtifact(ctx, literalEnv(prepare.Env)); err != nil {
		k.t.Errorf("kubelet: job %s: prepare: %v", job.Name, err)
		prepareExit = 1
	}
	workspace, err := os.MkdirTemp(k.work, job.Name+"-")
	if err != nil {
		return err
	}
	if err := writeHandoff(workspace, &secret); err != nil {
		return err
	}

	// The agent container, when the prepare step let it start.
	if prepareExit == 0 {
		rec.Stdout, rec.ExitCode, err = k.runAgent(ctx, workspace, rec.Env)
		if err != nil {
			return err
		}
	}

	pod, err := k.createPod(ctx, job)
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.logs[pod.Namespace+"/"+pod.Name+"/"+agent.Name] = rec.Stdout
	k.mu.Unlock()
	if err := k.finishPod(ctx, pod, prepare, agent, started, prepareExit, rec.ExitCode); err != nil {
		return err
	}
	if err := k.finishJob(ctx, job, started, prepareExit == 0 && rec.ExitCode == 0); err != nil {
		return err
	}
	k.mu.Lock()
	k.runs = append(k.runs, rec)
	k.mu.Unlock()
	return nil
}

// jobContainers returns the Job's prepare init container and its agent
// container.
func jobContainers(spec *corev1.PodSpec) (prepare, agent *corev1.Container, err error) {
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == "prepare" {
			prepare = &spec.InitContainers[i]
		}
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == "agent" {
			agent = &spec.Containers[i]
		}
	}
	if prepare == nil || agent == nil {
		return nil, nil, errors.New("the pod template has no prepare init container or no agent container")
	}
	return prepare, agent, nil
}

// literalEnv is a container's environment, the literal values only (a
// secretKeyRef never reaches an intent Job; a Finding Job's is not run).
func literalEnv(env []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		if e.ValueFrom == nil {
			out[e.Name] = e.Value
		}
	}
	return out
}

// fetchArtifact is the prepare step's fetch: the artifact the Job pins,
// refused unless its bytes hash to the digest the Job carries.
func fetchArtifact(ctx context.Context, env map[string]string) error {
	url, want := env["PATCHY_ARTIFACT_URL"], env["PATCHY_ARTIFACT_DIGEST"]
	if url == "" || want == "" {
		return errors.New("the Job names no artifact URL or digest")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: status %d, %v", url, resp.StatusCode, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
		return fmt.Errorf("artifact %s hashes to %s, not the pinned %s", url, got, want)
	}
	return nil
}

// writeHandoff copies the per-Job Secret's files into the workspace's
// input/, as the prepare step does.
func writeHandoff(workspace string, secret *corev1.Secret) error {
	input := filepath.Join(workspace, "input")
	if err := os.MkdirAll(input, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"issue.md", "investigation.md"} {
		body, ok := secret.Data[name]
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(input, name), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// runAgent runs the fake agent with the agent container's environment,
// pointed at the workspace, and returns its stdout and exit code.
func (k *kubelet) runAgent(ctx context.Context, workspace string, env map[string]string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "sh", k.script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for name, value := range env {
		switch name {
		case "PATH", "HOME", "PATCHY_WORKSPACE":
			continue // the pod's own; the workspace is this run's directory
		}
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.Env = append(cmd.Env, "HOME="+workspace, "PATCHY_WORKSPACE="+workspace, "PATCHY_FAKE_TURN_DELAY=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return stdout.Bytes(), exit.ExitCode(), nil
	case err != nil:
		return nil, 0, fmt.Errorf("run the fake agent: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), 0, nil
}

// createPod records the Job's pod, bound to the Node, as the Job controller
// and the scheduler would.
func (k *kubelet) createPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	labels := map[string]string{}
	for key, value := range job.Spec.Template.Labels {
		labels[key] = value
	}
	labels["batch.kubernetes.io/job-name"] = job.Name
	labels["batch.kubernetes.io/controller-uid"] = string(job.UID)
	spec := job.Spec.Template.Spec.DeepCopy()
	spec.NodeName = kubeletNode
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            job.Name + "-e2e",
			Namespace:       job.Namespace,
			Labels:          labels,
			Annotations:     job.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
		},
		Spec: *spec,
	}
	if err := k.cl.client.Create(ctx, pod); err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}
	return pod, nil
}

// finishPod records the pod's end: the prepare step terminated with its
// exit code, then the agent container with its own, or still waiting when
// the prepare step failed and it never started.
func (k *kubelet) finishPod(ctx context.Context, pod *corev1.Pod, prepare, agent *corev1.Container,
	started metav1.Time, prepareExit int32, agentExit int) error {
	now := metav1.Now()
	terminated := func(code int32) corev1.ContainerState {
		reason := "Completed"
		if code != 0 {
			reason = "Error"
		}
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, StartedAt: started, FinishedAt: now,
		}}
	}
	pod.Status = corev1.PodStatus{
		Phase:     corev1.PodSucceeded,
		StartTime: &started,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: prepare.Name, Image: prepare.Image, ImageID: prepare.Image, State: terminated(prepareExit),
		}},
	}
	agentStatus := corev1.ContainerStatus{Name: agent.Name, Image: agent.Image, ImageID: agent.Image}
	switch {
	case prepareExit != 0:
		pod.Status.Phase = corev1.PodFailed
		agentStatus.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	default:
		agentStatus.State = terminated(int32(agentExit))
		if agentExit != 0 {
			pod.Status.Phase = corev1.PodFailed
		}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{agentStatus}
	if err := k.cl.client.Status().Update(ctx, pod); err != nil {
		return fmt.Errorf("record pod status: %w", err)
	}
	return nil
}

// finishJob records the Job's terminal status as the Job controller would.
func (k *kubelet) finishJob(ctx context.Context, job *batchv1.Job, started metav1.Time, succeeded bool) error {
	var cur batchv1.Job
	if err := k.cl.client.Get(ctx, client.ObjectKeyFromObject(job), &cur); err != nil {
		return fmt.Errorf("read job: %w", err)
	}
	now := metav1.Now()
	cur.Status.StartTime = &started
	cond := func(typ batchv1.JobConditionType, reason string) batchv1.JobCondition {
		return batchv1.JobCondition{
			Type: typ, Status: corev1.ConditionTrue, Reason: reason, LastProbeTime: now, LastTransitionTime: now,
		}
	}
	if succeeded {
		cur.Status.Succeeded = 1
		cur.Status.CompletionTime = &now
		cur.Status.Conditions = []batchv1.JobCondition{
			cond(batchv1.JobSuccessCriteriaMet, "CompletionsReached"),
			cond(batchv1.JobComplete, "CompletionsReached"),
		}
	} else {
		cur.Status.Failed = 1
		cur.Status.Conditions = []batchv1.JobCondition{
			cond(batchv1.JobFailureTarget, "BackoffLimitExceeded"),
			cond(batchv1.JobFailed, "BackoffLimitExceeded"),
		}
	}
	if err := k.cl.client.Status().Update(ctx, &cur); err != nil {
		return fmt.Errorf("record job status: %w", err)
	}
	return nil
}

// Runs returns every Job the kubelet ran so far, in order.
func (k *kubelet) Runs() []agentRun {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]agentRun(nil), k.runs...)
}

// waitRun waits for a run matching match and returns it.
func (k *kubelet) waitRun(t *testing.T, why string, match func(agentRun) bool) agentRun {
	t.Helper()
	var found agentRun
	eventually(t, why, func() bool {
		for _, r := range k.Runs() {
			if match(r) {
				found = r
				return true
			}
		}
		return false
	})
	return found
}

// phaseIs matches a run whose agent ran phase (its PATCHY_PHASE).
func phaseIs(phase string) func(agentRun) bool {
	return func(r agentRun) bool { return r.Env["PATCHY_PHASE"] == phase }
}

// TestKubelet checks the stand-in itself against the real API server: a Job
// whose agent succeeds ends Complete and one whose agent fails ends Failed,
// each with its pod's terminated statuses, and either way the agent's log is
// what pods/log serves; a Job of another run kind is never run.
func TestKubelet(t *testing.T) {
	const runKind = "intent"
	cl := startCluster(t)
	k := startKubelet(t, cl, runKind)
	ctx := context.Background()
	artifact := []byte("the pinned tree")
	art := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(artifact) }))
	t.Cleanup(art.Close)
	cfg, err := clientcmd.BuildConfigFromFlags("", cl.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, kind, phase string
		handoff           map[string]string
		wantCondition     batchv1.JobConditionType
		wantPod           corev1.PodPhase
		wantLog           string
	}{
		{"an agent that succeeds", runKind, "plan", map[string]string{"issue.md": "# a request\n"},
			batchv1.JobComplete, corev1.PodSucceeded, `"type":"plan"`},
		// agent-runner refuses a build handed a request; so does the fake.
		{"an agent that fails", runKind, "build",
			map[string]string{"issue.md": "# a request\n", "investigation.md": "---\n---\n"},
			batchv1.JobFailed, corev1.PodFailed, `"type":"fatal"`},
		{"another run kind", string(v1alpha1.RunKindInvestigation), "investigate",
			map[string]string{"issue.md": "# a finding\n"}, "", "", ""},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("kubelet-%d", i)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS},
				StringData: tc.handoff}
			if err := cl.client.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS,
					Labels: map[string]string{v1alpha1.LabelRunKind: tc.kind}},
				Spec: batchv1.JobSpec{BackoffLimit: new(int32(0)), Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						InitContainers: []corev1.Container{{Name: "prepare", Image: "tools", Env: []corev1.EnvVar{
							{Name: "PATCHY_ARTIFACT_URL", Value: art.URL},
							{Name: "PATCHY_ARTIFACT_DIGEST", Value: fmt.Sprintf("%x", sha256.Sum256(artifact))},
						}}},
						Containers: []corev1.Container{{Name: "agent", Image: "agent", Env: []corev1.EnvVar{
							{Name: "PATCHY_PHASE", Value: tc.phase},
							{Name: "PATCHY_REPO", Value: "acme/shop"},
						}}},
					},
				}},
			}
			if err := cl.client.Create(ctx, job); err != nil {
				t.Fatal(err)
			}

			if tc.wantCondition == "" {
				consistently(t, "a job of another run kind to be left alone", func() bool {
					var pods corev1.PodList
					return cl.client.List(ctx, &pods, client.InNamespace(agentsNS),
						client.MatchingLabels{"batch.kubernetes.io/job-name": name}) == nil && len(pods.Items) == 0
				})
				return
			}
			k.waitRun(t, "the kubelet to run "+name, func(r agentRun) bool { return r.Job.Name == name })
			var cur batchv1.Job
			if err := cl.client.Get(ctx, client.ObjectKeyFromObject(job), &cur); err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(cur.Status.Conditions, func(c batchv1.JobCondition) bool {
				return c.Type == tc.wantCondition && c.Status == corev1.ConditionTrue
			}) {
				t.Errorf("job conditions = %+v, want %s", cur.Status.Conditions, tc.wantCondition)
			}
			var pods corev1.PodList
			if err := cl.client.List(ctx, &pods, client.InNamespace(agentsNS),
				client.MatchingLabels{"batch.kubernetes.io/job-name": name}); err != nil || len(pods.Items) != 1 {
				t.Fatalf("pods of %s = %d (%v), want 1", name, len(pods.Items), err)
			}
			pod := pods.Items[0]
			if pod.Status.Phase != tc.wantPod || pod.Spec.NodeName != kubeletNode {
				t.Errorf("pod = %s on %q, want %s on %s", pod.Status.Phase, pod.Spec.NodeName, tc.wantPod, kubeletNode)
			}
			stream, err := cs.CoreV1().Pods(agentsNS).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "agent"}).Stream(ctx)
			if err != nil {
				t.Fatalf("pods/log: %v", err)
			}
			defer func() { _ = stream.Close() }()
			logs, err := io.ReadAll(stream)
			if err != nil || !strings.Contains(string(logs), tc.wantLog) {
				t.Errorf("pods/log = %q (%v), want the agent's %s event", logs, err, tc.wantLog)
			}
		})
	}
}
