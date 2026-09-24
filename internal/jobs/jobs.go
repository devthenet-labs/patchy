// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentrun"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/provider"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
	"github.com/bitwise-media-group/patchy/internal/sandboxprobe"
)

// Label keys and values identifying the Jobs patchy owns. The repo
// annotation carries the true owner/name (a slash is illegal in label
// values).
const (
	labelApp       = "app.kubernetes.io/name"
	labelManagedBy = "app.kubernetes.io/managed-by"
	annotationRepo = "patchy.bitwisemedia.uk/repo"

	appName   = "patchy-agent"
	managedBy = "patchy"
)

// Pod layout: container, volume, and mount names.
const (
	initContainerName  = "prepare"
	agentContainerName = "agent"

	volWorkspace = "workspace"
	volTmp       = "tmp"
	volInput     = "input"

	workspaceDir = "/workspace"
	inputMount   = "/patchy/input"
)

// Per-Job Secret keys. No forge credential ever appears here — the init
// container fetches a digest-verified tarball from the artifact server.
const (
	secretKeyIssue         = "issue.md"
	secretKeyInvestigation = "investigation.md"
)

// Broker caller-token projection: the volume a brokered runner's agent
// container mounts and the path agent-runner reads the projected
// ServiceAccount token from. The init container never mounts it — the
// prepare step is identity-free.
const (
	volBrokerToken  = "broker-token"
	brokerTokenDir  = "/var/run/patchy/broker"
	brokerTokenPath = brokerTokenDir + "/token"
)

// DefaultBrokerAudience is the audience brokered runners' projected tokens
// are bound to; must match the broker's --token-audience.
const DefaultBrokerAudience = "patchy-egress-broker"

// Repository-declared image injection. The trusted prepare init copies
// patchy's binaries out of the runner image (toolsBinDir, where the
// Dockerfiles put them) into the patchy-bin emptyDir, which the agent
// container mounts read-only at patchyBinDir and runs agent-runner from by
// absolute path. A Job that ran such an image carries the audit annotations
// (the digest reference that ran, its source, the trusted donor image) and
// the selectable label; a Job that did not carries none of them.
const (
	volPatchyBin = "patchy-bin"
	patchyBinDir = "/patchy/bin"
	toolsBinDir  = "/usr/local/bin"

	annotationRunnerImage       = "patchy.bitwisemedia.uk/runner-image"
	annotationRunnerImageSource = "patchy.bitwisemedia.uk/runner-image-source"
	annotationToolsImage        = "patchy.bitwisemedia.uk/tools-image"
	labelRunnerImageSource      = "patchy.bitwisemedia.uk/runner-image-source"
)

// ExitSandboxUnenforced is the prepare init container's exit status when
// the sandbox probe found egress still open at the end of its window. The
// collectors map it to a SandboxUnenforced failure that consumes no attempt.
const ExitSandboxUnenforced = sandboxprobe.ExitUnenforced

// DefaultSandboxProbeTimeout is how long the probe keeps retrying while
// egress is open before it declares NetworkPolicy unenforced: long enough
// for a CNI that attaches a new pod's policy a few seconds late (EKS Auto
// Mode), short enough not to be the Job's cost.
const DefaultSandboxProbeTimeout = sandboxprobe.DefaultTimeout

// runAsUser is the fixed non-root UID (distroless "nonroot").
const runAsUser = 65532

// prepareScript is the init container's shell: a credential-less fetch of
// the SHA-pinned tree tarball from source-controller's artifact server
// (digest-verified end to end), followed by a synthetic git base commit —
// the agent's commit/diff flow needs a local base, and diffs against the
// synthetic commit are identical to diffs against the real remote SHA the
// controller pushes on. No forge credential exists anywhere in the pod.
const prepareScript = `set -eu
mkdir -p /workspace/repo
cd /workspace/repo
curl -fsSL --retry 5 --retry-all-errors "$PATCHY_ARTIFACT_URL" -o /tmp/src.tar.gz
echo "$PATCHY_ARTIFACT_DIGEST  /tmp/src.tar.gz" | sha256sum -c - >/dev/null
tar -xzf /tmp/src.tar.gz --strip-components=1
rm -f /tmp/src.tar.gz
git init -q
git add -A
git -c user.name=patchy -c user.email=patchy@invalid commit -qm "base $PATCHY_BASE_SHA"
git checkout -q --detach HEAD
mkdir -p /workspace/input
cp /patchy/input/issue.md /workspace/input/issue.md
if [ -f /patchy/input/investigation.md ]; then
  cp /patchy/input/investigation.md /workspace/input/investigation.md
fi
`

// injectScript is appended to prepareScript on a repository-image Job, and
// only then, so the default Job stays byte-identical. It still runs in the
// trusted runner image: each binary named in $PATCHY_INJECT is copied from
// where the Dockerfile put it into the patchy-bin emptyDir (read-only in the
// agent container), then the sandbox probe runs from the trusted binary,
// selected by the one subcommand name agent-runner dispatches on. Under
// set -e a non-zero probe exit ends the script with that status —
// ExitSandboxUnenforced — before the agent container ever starts.
const injectScript = `for bin in $PATCHY_INJECT; do
  cp "` + toolsBinDir + `/$bin" "` + patchyBinDir + `/$bin"
done
` + toolsBinDir + `/` + agentRunnerBin + ` ` + sandboxprobe.Command + `
`

// Runner is one harness's agent-runner deployment surface: the container image
// bundling that harness's CLI and the Secret its model credential is injected
// from. A Job picks its Runner by the harness resolved for its model, so a
// claude Job runs the claude image with the Anthropic credential, a codex Job
// the codex image with the OpenAI credential, and a copilot Job the copilot
// image with the GitHub token that CLI authenticates with.
type Runner struct {
	Image     string // the agent-runner image bundling this harness's CLI
	Secret    string // name of the Secret holding the model credential
	SecretKey string // key within it (default "api-key")
	// SecretEnv is the env var the credential is injected into the agent
	// container as: ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN for claude,
	// OPENAI_API_KEY for codex, COPILOT_GITHUB_TOKEN (or GH_TOKEN /
	// GITHUB_TOKEN) for copilot. The fake runner needs no credential and may
	// leave Secret empty.
	SecretEnv string
	// Brokered routes this runner's model traffic through the egress
	// credential broker: the pod gets no model credential at all — only an
	// audience-bound projected ServiceAccount token — plus the gateway Env
	// below. The Secret* fields are ignored when set.
	Brokered bool
	// Env is per-runner gateway/provider environment (base-URL overrides,
	// skip-auth switches, the model map, the operator's provider env),
	// values only, controller-built. It wins over Config.Env but can never
	// name a credential channel, nor a name the Job sets itself
	// (PerJobEnvNames on a finding Job, EvalJobEnvNames on an evaluation one).
	Env map[string]string
	// Inject names the binaries this runner image contributes to a Job that
	// runs a repository-declared image instead: the prepare init copies each
	// one from toolsBinDir into the patchy-bin volume, agent-runner always
	// among them since the agent container runs it from there. Nil means this
	// harness never runs a repository image (codex and copilot hold a real
	// credential in-pod; the fake harness replays fixtures), whatever the
	// Repository declares. runnercfg sets {"agent-runner", "claude"} for the
	// claude runner, so "claude only" is configuration, not code. A runner
	// that injects a Secret credential (not Brokered, Secret set) may not
	// Inject: Create refuses such a Job rather than hand the credential to
	// the repository's image.
	Inject []string
}

// Config configures Job creation.
type Config struct {
	Namespace      string        // where Jobs run, e.g. "patchy-agents"
	ServiceAccount string        // pod service account
	Deadline       time.Duration // activeDeadlineSeconds
	TTL            time.Duration // ttlSecondsAfterFinished
	// Runners is the per-harness runner fleet, keyed by harness id
	// ("claude"/"codex"/"copilot"/"fake"). A Job whose Spec.Harness is not a key here
	// fails to build — the controller resolves and enables harnesses before a
	// Job is ever created.
	Runners map[string]Runner
	// Env is extra PATCHY_* configuration passed through to every runner
	// (models, timeouts, budgets, thresholds). Per-Job harness and model are
	// carried on the Spec, not here.
	Env map[string]string
	// BrokerAudience is the audience brokered runners' projected caller
	// tokens are bound to (default DefaultBrokerAudience).
	BrokerAudience string
	// Resource strings (Kubernetes quantities), optional.
	CPURequest, MemoryRequest, CPULimit, MemoryLimit string
	// EphemeralStorage, when set, is the ephemeral-storage request AND
	// limit on both containers, so a pod that fills its emptyDirs is
	// evicted by the kubelet rather than filling the node. Optional for a
	// default Job; required for a repository-image one, which Create
	// refuses without it.
	EphemeralStorage string
	// AllowRepositoryImages is the kill switch for repository-declared
	// runner images: while false (the default) a Spec.RunnerImage is
	// ignored and every Job runs its harness's runner image, so a flip
	// takes effect at the next Job without touching any CR.
	AllowRepositoryImages bool
	// SandboxProbeTimeout is how long a repository-image Job's prepare init
	// keeps re-probing while egress is open before concluding that
	// NetworkPolicy is not enforced (default DefaultSandboxProbeTimeout).
	SandboxProbeTimeout time.Duration
}

// Spec is one agent Job to create.
type Spec struct {
	Repo    string // "owner/name"
	Attempt int
	Phase   string // agentrun phase: "investigate" | "remediate"
	// Harness runs this Job ("claude"/"codex"/"copilot"/"fake"); selects the runner
	// image, credential, and egress network policy. Model is the canonical
	// provider-qualified model id the harness runs. Both are resolved
	// controller-side before the Job is created.
	Harness string
	Model   string
	// BaseSHA is the pinned commit the artifact tree corresponds to; the
	// agent's changeset parents it.
	BaseSHA       string
	IssueMarkdown string // the issue handoff file content
	// Kind discriminates the two job controllers sharing one namespace:
	// "investigation" | "remediation".
	Kind    string
	Owner   string // owning Investigation/Remediation name
	Finding string // owning Finding name
	// ArtifactURL/ArtifactDigest locate and pin the repo tarball.
	ArtifactURL    string
	ArtifactDigest string
	// InvestigationMarkdown is the analysis handed to a remediation run.
	InvestigationMarkdown string
	// MaxTurns/TokenBudget are the budget granted to a remediation run,
	// already resolved controller-side against the automated budget, the
	// estimate and the manual budget. Zero leaves the pod on its own
	// configured automated budget.
	MaxTurns    int32
	TokenBudget int64
	// Calibration is pre-serialized JSON describing how earlier estimates
	// compared to reality, passed opaquely to the investigation prompt. Empty
	// omits it. This package deliberately does not know its shape — it is
	// prompt garnish, not job configuration.
	Calibration string
	// PreviousAttempt is pre-serialized JSON describing the failed run this
	// one retries, passed opaquely to the stage prompt. Empty omits it. Like
	// Calibration its shape is not this package's business, and the text
	// inside it is untrusted — the prompt fences it.
	PreviousAttempt string
	// RunnerImage is the digest-pinned repository-declared image from the
	// Repository's status (name@sha256:...), copied by the launching
	// controller; empty runs the harness's runner image. It is honoured only
	// when Config.AllowRepositoryImages is on and the runner has binaries to
	// Inject — otherwise the Job is exactly what it would be without it.
	// When it is honoured it must carry a sha256 digest; Create refuses a
	// tag or bare name rather than run and record an unchecked image.
	RunnerImage string
	// RunnerSearchPath is that image's sanitized PATH from the Repository's
	// status (colon-joined absolute entries); the Job prepends its own
	// binary directory and owns the resulting PATH.
	RunnerSearchPath string
	// RunnerImageManifest is the repository-relative path of the file that
	// declared RunnerImage, echoed on the returned RunnerImageRef so the
	// tracking issue can name it.
	RunnerImageManifest string
}

// Client creates and observes agent Jobs in one namespace. It embeds the
// read-only Tailer, so the pod-discovery and log-reading logic has exactly one
// implementation whether a controller collects a finished run or the status
// server follows a live one.
type Client struct {
	*Tailer
	cfg Config
	log *slog.Logger
}

// New builds a Client, applying Config defaults.
func New(cs kubernetes.Interface, cfg Config, log *slog.Logger) *Client {
	runners := make(map[string]Runner, len(cfg.Runners))
	for id, r := range cfg.Runners {
		if r.SecretKey == "" {
			r.SecretKey = "api-key"
		}
		runners[id] = r
	}
	cfg.Runners = runners
	if cfg.BrokerAudience == "" {
		cfg.BrokerAudience = DefaultBrokerAudience
	}
	if cfg.SandboxProbeTimeout <= 0 {
		cfg.SandboxProbeTimeout = DefaultSandboxProbeTimeout
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{Tailer: NewTailer(cs, cfg.Namespace), cfg: cfg, log: log}
}

// runnerFor returns the runner configured for a Job's harness, or an error
// naming the harness when none is configured (a controller bug — harnesses are
// enabled at startup, before any Job is built).
func (c *Client) runnerFor(harnessID string) (Runner, error) {
	r, ok := c.cfg.Runners[harnessID]
	if !ok {
		return Runner{}, fmt.Errorf("jobs: no runner configured for harness %q", harnessID)
	}
	return r, nil
}

// NameFor is the deterministic Job (and per-Job Secret) name for one
// attempt: patchy-<findinghash>-{inv|rem}-a<attempt>. Always DNS-1123 safe
// and <=63 chars; the kind discriminator keeps the two job controllers
// sharing one namespace out of each other's way.
func NameFor(finding, kind string, attempt int32) string {
	sum := sha256.Sum256([]byte(finding))
	short := map[string]string{"investigation": "inv", "remediation": "rem"}[kind]
	return fmt.Sprintf("patchy-%x-%s-a%d", sum[:5], short, attempt)
}

// Create builds and creates the per-Job Secret (the handoff markdown files),
// then the Job itself, then owner-references the Secret to the Job so it is
// garbage collected with it. It returns the Job name and the runner image
// the Job actually runs, read back from the Job's own annotations: Source
// is repository only when injection happened, and default whenever it did
// not, for any reason (a non-claude harness, the fake harness, the kill
// switch, nothing declared) — so what the caller records can never say
// repository for a pod that ran the default image.
func (c *Client) Create(ctx context.Context, spec Spec) (string, v1alpha1.RunnerImageRef, error) {
	if spec.Kind == "" || spec.Finding == "" {
		return "", v1alpha1.RunnerImageRef{}, fmt.Errorf("jobs: spec requires Kind and Finding")
	}
	name := NameFor(spec.Finding, spec.Kind, int32(spec.Attempt))
	job, err := c.buildJob(name, spec)
	if err != nil {
		return "", v1alpha1.RunnerImageRef{}, err
	}

	// Create is idempotent: the Secret and Job contents are deterministic
	// per (finding, kind, attempt), so a duplicate reconcile — or a retry
	// after a partial launch — adopts what already exists instead of
	// failing on AlreadyExists.
	secrets := c.cs.CoreV1().Secrets(c.cfg.Namespace)
	secret, err := secrets.Create(ctx, buildSecret(name, c.cfg.Namespace, spec), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		secret, err = secrets.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return "", v1alpha1.RunnerImageRef{}, fmt.Errorf("jobs: create secret %s: %w", name, err)
	}
	created, err := c.cs.BatchV1().Jobs(c.cfg.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = c.cs.BatchV1().Jobs(c.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		_ = secrets.Delete(ctx, name, metav1.DeleteOptions{})
		return "", v1alpha1.RunnerImageRef{}, fmt.Errorf("jobs: create job %s: %w", name, err)
	}
	owner := metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       created.Name,
		UID:        created.UID,
		Controller: new(true),
	}
	if !slices.ContainsFunc(secret.OwnerReferences, func(r metav1.OwnerReference) bool { return r.UID == owner.UID }) {
		secret.OwnerReferences = append(secret.OwnerReferences, owner)
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return "", v1alpha1.RunnerImageRef{}, fmt.Errorf("jobs: own secret %s: %w", name, err)
		}
	}

	// Read the image back from the Job that exists — adopted or created —
	// rather than from what this call built, so a retry that adopts an
	// earlier launch reports the image that launch actually runs.
	ref := runnerImageRef(created, spec.RunnerImageManifest)
	c.log.LogAttrs(ctx, slog.LevelInfo, "created agent job",
		slog.String("job", name),
		slog.String("repo", spec.Repo),
		slog.String("finding", spec.Finding),
		slog.Int("attempt", spec.Attempt),
		slog.String("runner_image", ref.Image),
		slog.String("runner_image_source", ref.Source))
	return name, ref, nil
}

// runnerImageRef reads the effective runner image off a Job: the audit
// annotations when it ran a repository-declared image, else the agent
// container's image with Source default. manifest is echoed only for a
// repository image.
func runnerImageRef(job *batchv1.Job, manifest string) v1alpha1.RunnerImageRef {
	if job.Annotations[annotationRunnerImageSource] == v1alpha1.RunnerImageSourceRepository {
		return v1alpha1.RunnerImageRef{
			Image:    job.Annotations[annotationRunnerImage],
			Source:   v1alpha1.RunnerImageSourceRepository,
			Manifest: manifest,
		}
	}
	ref := v1alpha1.RunnerImageRef{Source: v1alpha1.RunnerImageSourceDefault}
	for _, ct := range job.Spec.Template.Spec.Containers {
		if ct.Name == agentContainerName {
			ref.Image = ct.Image
		}
	}
	return ref
}

// injects reports whether a Job runs the repository-declared image: only
// when the kill switch is off, the Spec carries a pinned image and the
// runner has binaries to inject. Any one of the three missing means the
// default Job, unchanged.
func (c *Client) injects(runner Runner, spec Spec) bool {
	return c.cfg.AllowRepositoryImages && spec.RunnerImage != "" && len(runner.Inject) > 0
}

// injectionRefusal reports why a Job that would run a repository-declared
// image must not be built at all, or nil when it may. It is a refusal, not
// a quiet fall back to the default image: each case is a contradiction in
// controller configuration, and a Job built anyway would either break the
// isolation model or hide the misconfiguration behind an audit trail that
// just says "default".
//
// A runner that injects its model credential from a Secret is the first:
// Inject is meant only for a credential-free (brokered) runner, and the
// threat model's promise that no credential exists in a repository-image
// pod is kept here, where the pod is built, not left to whoever assembles
// the runner fleet.
//
// An image reference that is not pinned to a sha256 digest is the second:
// the kubelet would pull whatever a tag points to at pull time, an image no
// resolution check ever saw, while the runner-image annotation and the
// RunnerImageRef Create returns recorded the tag as the reference that ran.
// The resolver only ever writes a pinned reference, so anything else in
// Spec.RunnerImage is a bug or a tampered status, not an operator choice.
//
// A missing ephemeral-storage limit is the third: it is the threat model's
// wall on disk, and without it a hostile image can fill the node's disk
// through the emptyDirs until the kubelet starts evicting other workloads.
func (c *Client) injectionRefusal(harnessID string, runner Runner, spec Spec) error {
	if !runner.Brokered && runner.Secret != "" {
		return fmt.Errorf("jobs: runner %q injects a model credential (%s from Secret %s) and cannot run a "+
			"repository-declared image; only a brokered or credential-free runner may Inject",
			harnessID, runner.SecretEnv, runner.Secret)
	}
	if ref, err := runnerimage.ParseDeclared(spec.RunnerImage); err != nil || ref.Digest == "" {
		return fmt.Errorf("jobs: repository-declared image %q is not pinned to a sha256 digest; "+
			"only a digest reference the resolver pinned may run", spec.RunnerImage)
	}
	if c.cfg.EphemeralStorage == "" {
		return fmt.Errorf("jobs: a repository-declared image needs an ephemeral-storage limit " +
			"(Config.EphemeralStorage), the wall on the disk its emptyDirs can fill")
	}
	return nil
}

// agentRunnerBin is the binary the agent container runs; on a
// repository-image Job it is the injected copy, by absolute path.
const agentRunnerBin = "agent-runner"

// injectedBinaries is the prepare step's copy list: agent-runner first,
// always — the agent container's command is the injected agent-runner
// whatever the runner lists — then the runner's other binaries, in order,
// each once.
func injectedBinaries(runner Runner) []string {
	bins := []string{agentRunnerBin}
	for _, b := range runner.Inject {
		if !slices.Contains(bins, b) {
			bins = append(bins, b)
		}
	}
	return bins
}

// buildSecret holds everything the init container needs: the handoff
// markdown files.
func buildSecret(name, namespace string, spec Spec) *corev1.Secret {
	data := map[string][]byte{
		secretKeyIssue: []byte(spec.IssueMarkdown),
	}
	if spec.InvestigationMarkdown != "" {
		data[secretKeyInvestigation] = []byte(spec.InvestigationMarkdown)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: jobLabels(spec)},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func (c *Client) buildJob(name string, spec Spec) (*batchv1.Job, error) {
	res, err := c.cfg.resources()
	if err != nil {
		return nil, err
	}
	runner, err := c.runnerFor(spec.Harness)
	if err != nil {
		return nil, err
	}
	lbls := jobLabels(spec)
	ann := map[string]string{annotationRepo: spec.Repo}
	inject := c.injects(runner, spec)
	if inject {
		if err := c.injectionRefusal(spec.Harness, runner, spec); err != nil {
			return nil, err
		}
		ann[annotationRunnerImage] = spec.RunnerImage
		ann[annotationRunnerImageSource] = v1alpha1.RunnerImageSourceRepository
		ann[annotationToolsImage] = runner.Image
		lbls[labelRunnerImageSource] = v1alpha1.RunnerImageSourceRepository
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.cfg.Namespace,
			Labels:      lbls,
			Annotations: maps.Clone(ann),
		},
		Spec: batchv1.JobSpec{
			// Retries are the issue state machine's job, not the Job
			// controller's.
			BackoffLimit: new(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(lbls), Annotations: maps.Clone(ann)},
				Spec: corev1.PodSpec{
					ServiceAccountName: c.cfg.ServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						FSGroup:        new(int64(runAsUser)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes:        c.podVolumes(name, runner, findingTokenTTL(c.cfg.Deadline), inject),
					InitContainers: []corev1.Container{c.prepareContainer(runner, spec, res, inject)},
					Containers:     []corev1.Container{c.agentContainer(runner, spec, res, inject)},
				},
			},
		},
	}
	if inject {
		// A repository image is untrusted: refuse the kube-api-access token
		// in the pod's own spec instead of relying on the ServiceAccount's
		// automount setting, which an operator overlay could change. The
		// broker's projected caller token is an explicit volume and is
		// unaffected. Default Jobs are left byte-identical.
		job.Spec.Template.Spec.AutomountServiceAccountToken = new(false)
	}
	if c.cfg.Deadline > 0 {
		job.Spec.ActiveDeadlineSeconds = new(int64(c.cfg.Deadline.Seconds()))
	}
	if c.cfg.TTL > 0 {
		job.Spec.TTLSecondsAfterFinished = new(int32(c.cfg.TTL.Seconds()))
	}
	return job, nil
}

// volumes: the shared workspace, a writable /tmp (the root filesystem is
// read-only), and the per-Job Secret for the init container.
func volumes(secretName string) []corev1.Volume {
	return []corev1.Volume{
		{Name: volWorkspace, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: volTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: volInput, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secretName},
		}},
	}
}

// podVolumes is volumes plus, for a brokered runner, the projected broker
// caller token, plus, when injecting, the patchy-bin emptyDir. The
// projection works despite automountServiceAccountToken being off on the
// agent ServiceAccount — that suppresses only the default API token — and
// the pod's FSGroup keeps the file readable at uid 65532.
func (c *Client) podVolumes(secretName string, runner Runner, tokenTTL int64, inject bool) []corev1.Volume {
	vols := volumes(secretName)
	if runner.Brokered {
		vols = append(vols, corev1.Volume{
			Name: volBrokerToken,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          c.cfg.BrokerAudience,
							ExpirationSeconds: new(tokenTTL),
							Path:              "token",
						},
					}},
				},
			},
		})
	}
	if inject {
		vols = append(vols, corev1.Volume{
			Name:         volPatchyBin,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	return vols
}

// findingTokenTTL sizes a finding Job's caller-token expiry. agent-runner
// re-reads the projected file each stage (the kubelet rotates it), so the
// TTL only needs headroom over one rotation window — but it must still
// comfortably outlive the Job deadline so a token minted at pod start works
// even if rotation lags.
func findingTokenTTL(deadline time.Duration) int64 {
	ttl := int64((deadline + 15*time.Minute).Seconds())
	return max(ttl, 3600)
}

// prepareContainer fetches the artifact tarball and stages the handoff
// files. No credential of any kind reaches it. It always runs the harness's
// own runner image — the init only needs /bin/sh, curl, and git, which every
// runner image carries — so on a repository-image Job the digest-verified
// fetch, the synthetic base commit, the binary injection and the sandbox
// probe all happen in trusted code before the declared image runs anything.
func (c *Client) prepareContainer(runner Runner, spec Spec, res corev1.ResourceRequirements,
	inject bool) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "HOME", Value: workspaceDir},
		{Name: "PATCHY_BASE_SHA", Value: spec.BaseSHA},
		{Name: "PATCHY_ARTIFACT_URL", Value: spec.ArtifactURL},
		{Name: "PATCHY_ARTIFACT_DIGEST", Value: spec.ArtifactDigest},
	}
	script := prepareScript
	mounts := []corev1.VolumeMount{
		{Name: volWorkspace, MountPath: workspaceDir},
		{Name: volTmp, MountPath: "/tmp"},
		{Name: volInput, MountPath: inputMount, ReadOnly: true},
	}
	if inject {
		env = append(env,
			corev1.EnvVar{Name: "PATCHY_INJECT", Value: strings.Join(injectedBinaries(runner), " ")},
			corev1.EnvVar{Name: sandboxprobe.TimeoutEnv, Value: c.cfg.SandboxProbeTimeout.String()})
		script += injectScript
		mounts = append(mounts, corev1.VolumeMount{Name: volPatchyBin, MountPath: patchyBinDir})
	}
	return corev1.Container{
		Name:            initContainerName,
		Image:           runner.Image,
		Command:         []string{"/bin/sh", "-c", script},
		Env:             env,
		VolumeMounts:    mounts,
		SecurityContext: containerSecurity(),
		Resources:       res,
	}
}

// agentContainer runs agent-runner. No GitHub credential reaches it — that
// is the isolation model. A brokered runner additionally mounts the
// projected caller token (agent container only; the init stays
// identity-free). When injecting, the container runs the repository's
// image instead, with agent-runner started by absolute path from the
// read-only patchy-bin mount — neither the image's ENTRYPOINT nor its PATH
// can redirect it — and the injection env on top of the usual one.
func (c *Client) agentContainer(runner Runner, spec Spec, res corev1.ResourceRequirements,
	inject bool) corev1.Container {
	mounts := []corev1.VolumeMount{
		{Name: volWorkspace, MountPath: workspaceDir},
		{Name: volTmp, MountPath: "/tmp"},
	}
	if runner.Brokered {
		mounts = append(mounts, corev1.VolumeMount{Name: volBrokerToken, MountPath: brokerTokenDir, ReadOnly: true})
	}
	image, command, env := runner.Image, []string{agentRunnerBin}, c.agentEnv(runner, spec)
	if inject {
		image, command = spec.RunnerImage, []string{patchyBinDir + "/" + agentRunnerBin}
		env = injectEnv(env, spec)
		mounts = append(mounts, corev1.VolumeMount{Name: volPatchyBin, MountPath: patchyBinDir, ReadOnly: true})
	}
	return corev1.Container{
		Name:            agentContainerName,
		Image:           image,
		Command:         command,
		Env:             env,
		VolumeMounts:    mounts,
		SecurityContext: containerSecurity(),
		Resources:       res,
	}
}

// reservedEnv are the names Create owns; Config.Env entries with these names
// are ignored so per-Job values (and the no-GitHub-token invariant) always
// win. Every model credential channel (claude's, codex's and copilot's alike)
// is reserved regardless of which one a runner injects — credentials reach the
// pod only via the secretKeyRef, never as a plaintext value in the Job spec.
// The copilot channels are why GH_TOKEN and COPILOT_GITHUB_TOKEN appear beside
// GITHUB_TOKEN: copilot authenticates with a GitHub token, so the name that
// carries its model credential is also a name the no-GitHub-token invariant
// has to keep out of the controller-global Env.
// The per-Job names (perJobEnv) are folded in below, and so are the gateway
// names a brokered runner's Env owns (base-URL overrides, skip-auth
// switches, the caller-token channel) from provider.GatewayEnvNames —
// Config.Env can never shadow those either. The proxy variables are
// reserved because a proxy would redirect the broker traffic: nothing but
// the gateway env decides where a pod's model calls go.
var reservedEnv = map[string]bool{
	"ANTHROPIC_API_KEY":       true,
	"CLAUDE_CODE_OAUTH_TOKEN": true,
	"ANTHROPIC_AUTH_TOKEN":    true,
	"OPENAI_API_KEY":          true,
	"CODEX_API_KEY":           true,
	"CODEX_ACCESS_TOKEN":      true,
	"COPILOT_GITHUB_TOKEN":    true,
	"GH_TOKEN":                true,
	"GITHUB_TOKEN":            true,
}

// perJobEnv are the names a finding Job sets itself: HOME, the workspace,
// and what agentEnv reads off the Spec (repository, phase, finding, base
// SHA, the stage's harness and model, the budget grant, the estimate
// calibration and the previous attempt). The injected-binary directory is
// one too, set only by a Job that injects: on the default image it would
// send agent-runner looking for an injected CLI that was never copied.
// Unlike the gateway names, these are refused on Runner.Env as well as on
// Config.Env, since Runner.Env carries the operator's provider env: a copy
// there would be a second, conflicting entry on a Job that sets the name
// (Kubernetes leaves the winner undefined), a value the Spec never carried
// on one that does not, and on a repository-image Job the stand-in for the
// blank that keeps an image's ENV from reaching agent-runner.
var perJobEnv = map[string]bool{
	"HOME":                        true,
	"PATCHY_WORKSPACE":            true,
	"PATCHY_REPO":                 true,
	"PATCHY_PHASE":                true,
	"PATCHY_FINDING":              true,
	"PATCHY_BASE_SHA":             true,
	"PATCHY_INVESTIGATE_HARNESS":  true,
	"PATCHY_INVESTIGATE_MODEL":    true,
	"PATCHY_REMEDIATE_HARNESS":    true,
	"PATCHY_REMEDIATE_MODEL":      true,
	"PATCHY_GRANTED_MAX_TURNS":    true,
	"PATCHY_GRANTED_TOKEN_BUDGET": true,
	"PATCHY_CALIBRATION":          true,
	"PATCHY_PREVIOUS_ATTEMPT":     true,
	agentrun.BinDirEnv:            true,
}

func init() {
	maps.Copy(reservedEnv, perJobEnv)
	for _, name := range provider.GatewayEnvNames {
		reservedEnv[name] = true
	}
	for _, name := range proxyEnv {
		reservedEnv[name] = true
	}
	for _, name := range gitRedirectEnv {
		reservedEnv[name] = true
	}
}

// gitRedirectEnv are the variables that point git at a different
// repository, work tree, index or object store. They cannot join scrubEnv:
// Kubernetes can set a variable but never unset it, and git reads an empty
// value of any of these as a broken path, not as absent, so blanking them
// would fail every git call agent-runner makes. They are reserved instead,
// which keeps them out of Config.Env and, through ReservedEnvNames, gets an
// image whose ENV sets one refused at resolution with a readable reason.
var gitRedirectEnv = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR"}

// proxyEnv are the proxy variables in both cases (curl and Go honour the
// lowercase forms): reserved against Config.Env and blanked on a
// repository image, so no image ENV and no operator passthrough can point
// the pod's traffic anywhere but the broker.
var proxyEnv = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
}

// ReservedEnvNames returns every name Create owns, sorted: the credential
// channels, the per-Job PATCHY_* vars, the gateway names, the proxy
// variables and git's repository redirections. source-controller passes it to runnerimage.CheckEnv as the
// set an image's ENV may not name, so resolve-time rejection and the
// Job's own reservations are one list.
func ReservedEnvNames() []string {
	return slices.Sorted(maps.Keys(reservedEnv))
}

// PerJobEnvNames returns the names a finding Job sets itself (perJobEnv),
// sorted: a subset of ReservedEnvNames that Runner.Env may not name either.
// runnercfg refuses them in the operator's provider env at startup, so a
// clash is an error the operator sees rather than an entry dropped here.
func PerJobEnvNames() []string {
	return slices.Sorted(maps.Keys(perJobEnv))
}

// scrubEnv are the names blanked, with an explicit empty value, in the
// agent container of a repository-image Job: an explicit container env
// overrides image ENV, so this is the backstop for whatever the resolver's
// reserved-ENV check did not know. It is a list of known redirection
// points, not an enumeration of everything an image's ENV can influence,
// and only names whose empty value means "unset" may join it. Shell startup hooks (the runner execs the
// CLI without a shell, but the CLI's own shell tool does not), the dynamic
// loader's injection points (agent-runner is static; claude is not), the
// interpreters' startup files, git's config and helper redirections, and
// the proxies. The gateway names and every PATCHY_* key agent-runner reads
// join them at build time (injectEnv), minus whatever the Job sets itself.
var scrubEnv = append([]string{
	"BASH_ENV", "ENV", "SHELLOPTS", "PROMPT_COMMAND",
	"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT",
	"NODE_OPTIONS", "PYTHONSTARTUP",
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_EXEC_PATH", "GIT_SSH_COMMAND",
}, proxyEnv...)

// injectEnv is the agent env of a repository-image Job: the usual env, then
// where the injected binaries are, a controller-owned PATH (patchy's
// directory first, then the image's sanitized entries — never an empty or
// trailing component a shell would read as the working tree, yet the
// image's own toolchain directories survive), git told to ignore the
// image's system config, the injected CLI's self-updater off (as the
// trusted image's ENV has it), and finally an explicit empty value for every
// scrubbed name, gateway name and agent-runner key the Job did not set. The
// blanks are sorted so the Job is deterministic.
//
// Those four names and the scrubbed ones are owned outright: an entry of
// the same name in the usual env (an operator's Config.Env passthrough,
// which a default Job keeps) is dropped first, so the pod carries patchy's
// value exactly once rather than a duplicate whose winner Kubernetes leaves
// undefined. They are not in reservedEnv, because that list is also what an
// image's ENV may not name, and every image sets PATH.
func injectEnv(env []corev1.EnvVar, spec Spec) []corev1.EnvVar {
	own := []corev1.EnvVar{
		{Name: agentrun.BinDirEnv, Value: patchyBinDir},
		{Name: "PATH", Value: podPath(spec.RunnerSearchPath)},
		{Name: "GIT_CONFIG_NOSYSTEM", Value: "1"},
		{Name: "DISABLE_AUTOUPDATER", Value: "1"},
	}
	owned := make(map[string]bool, len(own)+len(scrubEnv))
	for _, e := range own {
		owned[e.Name] = true
	}
	for _, name := range scrubEnv {
		owned[name] = true
	}
	env = append(slices.DeleteFunc(env, func(e corev1.EnvVar) bool { return owned[e.Name] }), own...)
	present := make(map[string]bool, len(env))
	for _, e := range env {
		present[e.Name] = true
	}
	blank := map[string]bool{}
	for _, name := range slices.Concat(scrubEnv, provider.GatewayEnvNames, agentrun.ConfigEnvKeys()) {
		if !present[name] {
			blank[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(blank)) {
		env = append(env, corev1.EnvVar{Name: name, Value: ""})
	}
	return env
}

// InjectedEnv is what injectEnv adds to a repository-image Job's agent
// container over its usual env, for an image whose sanitized search path
// is searchPath (":"-joined, as Spec.RunnerSearchPath): where the injected
// binaries are, the controller-owned PATH, git's system config and the
// CLI's self-updater off, and an explicit empty value for every scrubbed
// name, gateway name and agent-runner key. The workstation check
// (`patchy check image --run`) replays it, so a local run of an image sees
// what the pod's harness would; names the usual env sets are blanked here
// because a local run has no usual env.
func InjectedEnv(searchPath string) []corev1.EnvVar {
	return injectEnv(nil, Spec{RunnerSearchPath: searchPath})
}

// podPath joins patchy's binary directory with the image's sanitized search
// path. The Repository status never carries an empty or relative entry
// (runnerimage.SanitizePath refuses them), but the join drops any anyway;
// an empty search path — a Repository written before the field existed —
// falls back to the runc default rather than leaving git and bash
// unreachable.
func podPath(searchPath string) string {
	entries := []string{patchyBinDir}
	if searchPath == "" {
		searchPath = runnerimage.DefaultPath
	}
	for p := range strings.SplitSeq(searchPath, ":") {
		if strings.HasPrefix(p, "/") && p != patchyBinDir {
			entries = append(entries, p)
		}
	}
	return strings.Join(entries, ":")
}

// credentialChannelEnv is every env var name any harness accepts a model
// credential on. Runner.Env is controller-built and provider-validated, but
// a credential must still never ride the per-runner env — the broker holds
// credentials, the pod holds none. Defense in depth, matching the
// reservedEnv posture for Config.Env.
var credentialChannelEnv = func() map[string]bool {
	out := map[string]bool{}
	for _, h := range harness.All() {
		for _, k := range h.EnvKeys() {
			out[k] = true
		}
	}
	return out
}()

// stageEnvNames returns the harness and model env var names agent-runner reads
// for a given phase; the controller resolves both per Job, so they are carried
// on the Spec and injected here rather than in the controller-global Env.
func stageEnvNames(phase string) (harnessEnv, modelEnv string) {
	if phase == "remediate" {
		return "PATCHY_REMEDIATE_HARNESS", "PATCHY_REMEDIATE_MODEL"
	}
	return "PATCHY_INVESTIGATE_HARNESS", "PATCHY_INVESTIGATE_MODEL"
}

func (c *Client) agentEnv(runner Runner, spec Spec) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, len(c.cfg.Env)+10)
	env = append(env,
		// HOME must be writable under readOnlyRootFilesystem.
		corev1.EnvVar{Name: "HOME", Value: workspaceDir},
		corev1.EnvVar{Name: "PATCHY_WORKSPACE", Value: workspaceDir},
		corev1.EnvVar{Name: "PATCHY_REPO", Value: spec.Repo},
		corev1.EnvVar{Name: "PATCHY_PHASE", Value: spec.Phase},
		corev1.EnvVar{Name: "PATCHY_FINDING", Value: spec.Finding},
		// The remote base SHA stamps the changeset (the local base is a
		// synthetic commit over the artifact tree).
		corev1.EnvVar{Name: "PATCHY_BASE_SHA", Value: spec.BaseSHA})

	// The harness and model resolved for this Job's stage. They match the
	// runner image the pod runs in, so the agent runs the harness it was built
	// for on the model the controller chose.
	harnessEnv, modelEnv := stageEnvNames(spec.Phase)
	if spec.Harness != "" {
		env = append(env, corev1.EnvVar{Name: harnessEnv, Value: spec.Harness})
	}
	if spec.Model != "" {
		env = append(env, corev1.EnvVar{Name: modelEnv, Value: spec.Model})
	}

	// The per-run budget grant, the estimate calibration and the previous
	// attempt. All are decided per Job, so like harness and model they are
	// injected from the Spec rather than the controller-global Env.
	if spec.MaxTurns > 0 {
		env = append(env, corev1.EnvVar{
			Name: "PATCHY_GRANTED_MAX_TURNS", Value: strconv.FormatInt(int64(spec.MaxTurns), 10)})
	}
	if spec.TokenBudget > 0 {
		env = append(env, corev1.EnvVar{
			Name: "PATCHY_GRANTED_TOKEN_BUDGET", Value: strconv.FormatInt(spec.TokenBudget, 10)})
	}
	if spec.Calibration != "" {
		env = append(env, corev1.EnvVar{Name: "PATCHY_CALIBRATION", Value: spec.Calibration})
	}
	if spec.PreviousAttempt != "" {
		env = append(env, corev1.EnvVar{Name: "PATCHY_PREVIOUS_ATTEMPT", Value: spec.PreviousAttempt})
	}

	// The controller-global Env under the full reserved filter, then the
	// per-runner gateway env on top (runner wins). Runner.Env is
	// controller-built, so it may name the reserved gateway vars — that is
	// its purpose — but never a credential channel, nor a name the Job sets
	// above: it carries the operator's provider env too.
	extra := make(map[string]string, len(c.cfg.Env)+len(runner.Env))
	for k, v := range c.cfg.Env {
		if !reservedEnv[k] {
			extra[k] = v
		}
	}
	for k, v := range runner.Env {
		if !credentialChannelEnv[k] && !perJobEnv[k] {
			extra[k] = v
		}
	}
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		env = append(env, corev1.EnvVar{Name: k, Value: extra[k]})
	}

	// A brokered runner authenticates to the egress broker with the
	// projected caller token; no model credential enters the pod at all —
	// only the fixed placeholder the CLI's login gate demands, which the
	// broker strips.
	if runner.Brokered {
		return append(env,
			corev1.EnvVar{Name: "PATCHY_BROKER_TOKEN_FILE", Value: brokerTokenPath},
			brokeredPlaceholderEnv())
	}
	// The fake runner needs no credential; the fixture replay authenticates
	// nothing.
	if runner.Secret == "" {
		return env
	}
	return append(env, corev1.EnvVar{Name: runner.SecretEnv, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: runner.Secret},
			Key:                  runner.SecretKey,
		},
	}})
}

// brokeredPlaceholderEnv is the non-secret auth token every brokered claude
// container carries so the CLI starts (see provider.PlaceholderAuthToken).
// It is set here and only here: the name is a reserved credential channel,
// so neither Config.Env nor Runner.Env can set or shadow it.
func brokeredPlaceholderEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: provider.PlaceholderAuthEnv, Value: provider.PlaceholderAuthToken}
}

func containerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             new(true),
		RunAsUser:                new(int64(runAsUser)),
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func jobLabels(spec Spec) map[string]string {
	lbls := map[string]string{
		labelApp:              appName,
		labelManagedBy:        managedBy,
		v1alpha1.LabelAttempt: strconv.Itoa(spec.Attempt),
		v1alpha1.LabelRunKind: spec.Kind,
		v1alpha1.LabelOwner:   sanitizeLabelValue(spec.Owner),
		v1alpha1.LabelFinding: sanitizeLabelValue(spec.Finding),
	}
	// The per-harness egress network policies select agent pods by this label,
	// so each runner reaches only its own model API.
	if spec.Harness != "" {
		lbls[v1alpha1.LabelHarness] = spec.Harness
	}
	return lbls
}

// sanitizeLabelValue coerces owner/name into a legal label value: lowercase
// [a-z0-9-._], <=63 chars, alphanumeric at both ends.
func sanitizeLabelValue(s string) string {
	b := []byte(strings.ToLower(s))
	for i, ch := range b {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-', ch == '.', ch == '_':
		default:
			b[i] = '-'
		}
	}
	out := string(b)
	if len(out) > 63 {
		out = out[:63]
	}
	out = strings.Trim(out, "-._")
	if out == "" {
		return "unknown"
	}
	return out
}

// resources renders the per-container requests and limits. Ephemeral
// storage, when configured, is both a request and a limit of the same
// quantity: the limit is the wall on disk (the kubelet evicts a pod that
// fills its emptyDirs), and requesting it keeps the scheduler honest.
func (c Config) resources() (corev1.ResourceRequirements, error) {
	var rr corev1.ResourceRequirements
	var err error
	if rr.Requests, err = resourceList(c.CPURequest, c.MemoryRequest, c.EphemeralStorage); err != nil {
		return rr, err
	}
	rr.Limits, err = resourceList(c.CPULimit, c.MemoryLimit, c.EphemeralStorage)
	return rr, err
}

func resourceList(cpu, memory, ephemeral string) (corev1.ResourceList, error) {
	if cpu == "" && memory == "" && ephemeral == "" {
		return nil, nil
	}
	rl := corev1.ResourceList{}
	if cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return nil, fmt.Errorf("jobs: cpu quantity %q: %w", cpu, err)
		}
		rl[corev1.ResourceCPU] = q
	}
	if memory != "" {
		q, err := resource.ParseQuantity(memory)
		if err != nil {
			return nil, fmt.Errorf("jobs: memory quantity %q: %w", memory, err)
		}
		rl[corev1.ResourceMemory] = q
	}
	if ephemeral != "" {
		q, err := resource.ParseQuantity(ephemeral)
		if err != nil {
			return nil, fmt.Errorf("jobs: ephemeral-storage quantity %q: %w", ephemeral, err)
		}
		rl[corev1.ResourceEphemeralStorage] = q
	}
	return rl, nil
}
