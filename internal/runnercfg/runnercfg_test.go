// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnercfg

import (
	"encoding/json"
	"math/rand"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/model"
)

// newOpts builds the flag surface a controller binary presents and resolves
// args through it, so these tests exercise the real flag/viper path Runners
// reads rather than a hand-built value.
func newOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	o := cli.NewOptions()
	cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
	o.Bind(cmd)
	RegisterFlags(cmd.Flags())
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := o.Load(cmd); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return o
}

// TestRunnersSecretEnvValidation covers the startup gate on the credential
// channel. It is the only thing standing between a typo'd --<harness>-secret-env
// and a controller that runs happily until an agent pod comes up with its
// credential in a variable the CLI never reads. The channels are per-harness,
// so naming the other harness's variable must fail just as a nonsense one does.
func TestRunnersSecretEnvValidation(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantEnv  map[string]string // harness -> resolved SecretEnv
		wantErrs []string          // substrings; non-empty means Runners must fail
	}{
		{
			// Claude is brokered: no credential channel at all, and the runner
			// only configures with a broker URL.
			name:    "claude is brokered with no secret env",
			args:    []string{"--claude-agent-image", "claude:1", "--broker-url", "http://broker:8080"},
			wantEnv: map[string]string{"claude": ""},
		},
		{
			name:     "claude without a broker url is not a configuration",
			args:     []string{"--claude-agent-image", "claude:1"},
			wantErrs: []string{"--broker-url"},
		},
		{
			// A stale --claude-secret-env survives startup: the flag is ignored
			// (with a migration notice), never validated against channels.
			name: "claude tolerates a stale secret env",
			args: []string{"--claude-agent-image", "claude:1", "--broker-url", "http://broker:8080",
				"--claude-secret-env", "COPILOT_GITHUB_TOKEN"},
			wantEnv: map[string]string{"claude": ""},
		},
		{
			name:    "codex defaults to the api key",
			args:    []string{"--codex-agent-image", "codex:1"},
			wantEnv: map[string]string{"codex": "OPENAI_API_KEY"},
		},
		{
			name: "codex accepts the chatgpt-plan access token",
			args: []string{"--codex-agent-image", "codex:1",
				"--codex-secret-env", "CODEX_ACCESS_TOKEN"},
			wantEnv: map[string]string{"codex": "CODEX_ACCESS_TOKEN"},
		},
		{
			name: "codex accepts the codex api key",
			args: []string{"--codex-agent-image", "codex:1",
				"--codex-secret-env", "CODEX_API_KEY"},
			wantEnv: map[string]string{"codex": "CODEX_API_KEY"},
		},
		{
			name:    "copilot defaults to the copilot github token",
			args:    []string{"--copilot-agent-image", "copilot:1"},
			wantEnv: map[string]string{"copilot": "COPILOT_GITHUB_TOKEN"},
		},
		{
			// copilot authenticates with a GitHub token, so the gh CLI's own
			// variable names are legitimate channels for it — and only for it.
			name: "copilot accepts the gh token",
			args: []string{"--copilot-agent-image", "copilot:1",
				"--copilot-secret-env", "GH_TOKEN"},
			wantEnv: map[string]string{"copilot": "GH_TOKEN"},
		},
		{
			name: "copilot accepts the github token",
			args: []string{"--copilot-agent-image", "copilot:1",
				"--copilot-secret-env", "GITHUB_TOKEN"},
			wantEnv: map[string]string{"copilot": "GITHUB_TOKEN"},
		},
		{
			name: "copilot rejects a model api key",
			args: []string{"--copilot-agent-image", "copilot:1",
				"--copilot-secret-env", "ANTHROPIC_API_KEY"},
			wantErrs: []string{"--copilot-secret-env", "ANTHROPIC_API_KEY", "COPILOT_GITHUB_TOKEN"},
		},
		{
			name: "codex rejects a claude channel",
			args: []string{"--codex-agent-image", "codex:1",
				"--codex-secret-env", "ANTHROPIC_API_KEY"},
			wantErrs: []string{"--codex-secret-env", "ANTHROPIC_API_KEY", "CODEX_ACCESS_TOKEN"},
		},
		{
			// The error must enumerate the accepted channels; a bare rejection
			// leaves the operator guessing at the spelling.
			name: "codex rejects a typo and lists the alternatives",
			args: []string{"--codex-agent-image", "codex:1",
				"--codex-secret-env", "CODEX_TOKEN"},
			wantErrs: []string{"--codex-secret-env", "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"},
		},
		{
			// The fake runner carries no credential, so there is no channel to
			// validate and a stale --codex-secret-env must not fail startup.
			name:    "fake runner needs no credential",
			args:    []string{"--fake-agent-image", "fake:1", "--codex-secret-env", "CODEX_TOKEN"},
			wantEnv: map[string]string{"fake": ""},
		},
		{
			name:     "no image configures no runner",
			args:     nil,
			wantErrs: []string{"no agent runner configured"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runners, err := Runners(newOpts(t, tt.args...))

			if len(tt.wantErrs) > 0 {
				if err == nil {
					t.Fatalf("Runners = %+v, want an error", runners)
				}
				for _, want := range tt.wantErrs {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("Runners: %v", err)
			}
			if len(runners) != len(tt.wantEnv) {
				t.Errorf("configured runners = %v, want %v", keys(runners), keys(tt.wantEnv))
			}
			for id, wantEnv := range tt.wantEnv {
				r, ok := runners[id]
				if !ok {
					t.Fatalf("runner %q not configured", id)
				}
				if r.SecretEnv != wantEnv {
					t.Errorf("%s SecretEnv = %q, want %q", id, r.SecretEnv, wantEnv)
				}
			}
		})
	}
}

// TestRunnersCredentialWiring: the Secret name and key ride the same flags as
// the env var, so a validated channel is only half the credential. Claude is
// brokered — no Secret reference of any kind — while codex keeps its channel.
func TestRunnersCredentialWiring(t *testing.T) {
	runners, err := Runners(newOpts(t,
		"--claude-agent-image", "claude:1",
		"--broker-url", "http://broker:8080",
		"--codex-agent-image", "codex:1",
		"--codex-secret", "chatgpt-workspace",
		"--codex-secret-key", "token",
		"--codex-secret-env", "CODEX_ACCESS_TOKEN",
	))
	if err != nil {
		t.Fatalf("Runners: %v", err)
	}

	claude := runners["claude"]
	if !claude.Brokered || claude.Secret != "" || claude.SecretEnv != "" {
		t.Errorf("claude runner = %+v, want brokered with no Secret channel", claude)
	}
	// Only the brokered claude runner may run a repository-declared image;
	// codex holds a real credential in-pod and never injects.
	if want := []string{"agent-runner", "claude"}; !slices.Equal(claude.Inject, want) {
		t.Errorf("claude Inject = %v, want %v", claude.Inject, want)
	}
	if got := claude.Env["ANTHROPIC_BASE_URL"]; got != "http://broker:8080/anthropic" {
		t.Errorf("claude ANTHROPIC_BASE_URL = %q, want the broker's anthropic route", got)
	}

	codex := runners["codex"]
	if codex.Inject != nil {
		t.Errorf("codex Inject = %v, want nil (a credential-holding runner never injects)", codex.Inject)
	}
	want := map[string]string{
		"image":     "codex:1",
		"secret":    "chatgpt-workspace",
		"secretKey": "token",
		"secretEnv": "CODEX_ACCESS_TOKEN",
	}
	got := map[string]string{
		"image":     codex.Image,
		"secret":    codex.Secret,
		"secretKey": codex.SecretKey,
		"secretEnv": codex.SecretEnv,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("codex %s = %q, want %q", k, got[k], w)
		}
	}
}

// chartSecretEnvEnum is where the Helm chart constrains
// agent.runners.<harness>.secretEnv.
const chartSecretEnvEnum = "../../charts/patchy/values.schema.json"

// TestChartSecretEnvEnumMatchesHarnesses guards the one copy of the credential
// vocabulary that Go cannot reach: helm validates secretEnv against a hardcoded
// enum in values.schema.json, while Runners validates the same field against
// harness.EnvKeys. Nothing connected the two, and they drifted — the enum named
// only OPENAI_API_KEY for codex, so a chart install using the CODEX_API_KEY or
// CODEX_ACCESS_TOKEN channel that Runners accepts failed lint before it ever
// reached a controller. Adding a channel to a harness must add it here.
func TestChartSecretEnvEnumMatchesHarnesses(t *testing.T) {
	raw, err := os.ReadFile(chartSecretEnvEnum)
	if err != nil {
		t.Fatalf("read chart schema: %v", err)
	}
	var doc struct {
		Definitions struct {
			AgentRunner struct {
				Properties struct {
					SecretEnv struct {
						Enum []string `json:"enum"`
					} `json:"secretEnv"`
				} `json:"properties"`
			} `json:"agentRunner"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse chart schema: %v", err)
	}
	got := doc.Definitions.AgentRunner.Properties.SecretEnv.Enum
	if len(got) == 0 {
		t.Fatalf("%s: no secretEnv enum found — did the schema shape change?", chartSecretEnvEnum)
	}

	// Every channel every harness accepts, in registry order. The fake harness
	// carries no credential and contributes none.
	var want []string
	for _, id := range model.KnownHarnessIDs {
		if id == model.HarnessFake {
			continue
		}
		want = append(want, envKeys(id)...)
	}

	inEnum := make(map[string]bool, len(got))
	for _, e := range got {
		inEnum[e] = true
	}
	for _, env := range want {
		if !inEnum[env] {
			t.Errorf("%s: secretEnv enum is missing %q, a channel a harness accepts — "+
				"a chart install using it fails lint even though Runners allows it",
				chartSecretEnvEnum, env)
		}
	}
	// The reverse direction: an enum entry no harness accepts would pass helm
	// lint and then fail at controller startup.
	accepted := make(map[string]bool, len(want))
	for _, env := range want {
		accepted[env] = true
	}
	for _, e := range got {
		if !accepted[e] {
			t.Errorf("%s: secretEnv enum offers %q, which no harness accepts — "+
				"helm would accept a value the controller rejects at startup", chartSecretEnvEnum, e)
		}
	}
}

// TestRunnersCopilotCredentialDefaults pins the copilot runner's defaults. They
// deliberately differ from the vendor-native runners: the Secret is named for
// the harness rather than a vendor because the credential is a GitHub token,
// and its key is "token" for the same reason.
func TestRunnersCopilotCredentialDefaults(t *testing.T) {
	runners, err := Runners(newOpts(t, "--copilot-agent-image", "copilot:1"))
	if err != nil {
		t.Fatalf("Runners: %v", err)
	}
	copilot := runners["copilot"]
	want := jobs.Runner{
		Image:     "copilot:1",
		Secret:    "patchy-copilot",
		SecretKey: "token",
		SecretEnv: "COPILOT_GITHUB_TOKEN",
	}
	if copilot.Image != want.Image || copilot.Secret != want.Secret ||
		copilot.SecretKey != want.SecretKey || copilot.SecretEnv != want.SecretEnv ||
		copilot.Brokered || copilot.Env != nil {
		t.Errorf("copilot runner = %+v, want %+v", copilot, want)
	}
}

// TestResolveFoundryCoverage: foundry has no derivable model ids, so a model
// the allowlist (or a stage default) lets claude run without a map entry must
// fail at startup — the alternative is a mid-run failure with a deployment
// name Foundry never heard of.
func TestResolveFoundryCoverage(t *testing.T) {
	args := []string{
		"--claude-agent-image", "claude:1",
		// An unroutable broker URL: the readiness probe must be non-fatal.
		"--broker-url", "http://127.0.0.1:1",
		"--claude-provider", "foundry",
		"--claude-model-map", "anthropic/claude-sonnet-5=sonnet-deploy",
	}
	opts := newOpts(t, args...)
	runners, err := Runners(opts)
	if err != nil {
		t.Fatalf("Runners: %v", err)
	}

	ctx := t.Context()
	cs := fake.NewClientset()
	_, err = Resolve(ctx, opts, cs, "patchy-agents", runners, nil,
		[]string{"anthropic/claude-sonnet-5", "anthropic/claude-opus-5"}, "anthropic/claude-sonnet-5")
	if err == nil || !strings.Contains(err.Error(), "anthropic/claude-opus-5") {
		t.Errorf("Resolve error = %v, want the uncovered model named", err)
	}

	enabled, err := Resolve(ctx, opts, cs, "patchy-agents", runners, nil,
		[]string{"anthropic/claude-sonnet-5"}, "anthropic/claude-sonnet-5")
	if err != nil {
		t.Fatalf("Resolve with full coverage: %v", err)
	}
	// The brokered runner is enabled with no credential Secret in the agent
	// namespace — enablement is configuration, the broker holds the key.
	if !slices.Contains(enabled, "claude") {
		t.Errorf("enabled = %v, want claude (brokered, no Secret probe)", enabled)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestSecretEnvFlagsMatchAcceptedChannels: the --<harness>-secret-env help is
// generated from the harness's accepted channels, so it cannot advertise a set
// the startup validation rejects, or omit one an operator is allowed to pick.
// The default must itself be an accepted channel, or the flag would fail
// validation when left alone.
func TestSecretEnvFlagsMatchAcceptedChannels(t *testing.T) {
	f := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(f)

	// claude-secret-env is absent on purpose: the claude runner is brokered,
	// the flag is ignored, and its usage says so instead of naming channels.
	for _, tt := range []struct{ flag, harness string }{
		{"codex-secret-env", model.HarnessCodex},
	} {
		t.Run(tt.flag, func(t *testing.T) {
			flag := f.Lookup(tt.flag)
			if flag == nil {
				t.Fatalf("--%s is not registered", tt.flag)
			}
			for _, env := range envKeys(tt.harness) {
				if !strings.Contains(flag.Usage, env) {
					t.Errorf("usage %q omits accepted channel %q", flag.Usage, env)
				}
			}
			if !accepts(tt.harness, flag.DefValue) {
				t.Errorf("default %q is not an accepted channel %v", flag.DefValue, envKeys(tt.harness))
			}
		})
	}
}

// TestClaudeProviderEnvRefusesPerJobNames: the operator's provider env
// reaches the agent pod through the claude runner's Env, so a name every
// Job sets itself (a retry's PATCHY_PREVIOUS_ATTEMPT, the Spec's
// PATCHY_REPO, HOME) must fail startup, naming the flag and the variable,
// rather than be dropped from every Job without a word. Any other name
// still passes through: HTTPS_PROXY is provider.Validate's own benign
// example.
func TestClaudeProviderEnvRefusesPerJobNames(t *testing.T) {
	base := []string{"--claude-agent-image", "claude:1", "--broker-url", "http://broker:8080"}
	for _, name := range jobs.PerJobEnvNames() {
		t.Run(name, func(t *testing.T) {
			args := append(slices.Clone(base), "--claude-provider-env", name+"=operator")
			runners, err := Runners(newOpts(t, args...))
			if err == nil {
				t.Fatalf("Runners succeeded with claude Env %v, want %s refused", runners["claude"].Env, name)
			}
			for _, want := range []string{"--claude-provider-env", name} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
	t.Run("other names pass through", func(t *testing.T) {
		args := append(slices.Clone(base), "--claude-provider-env", "HTTPS_PROXY=http://proxy:3128")
		runners, err := Runners(newOpts(t, args...))
		if err != nil {
			t.Fatalf("Runners: %v", err)
		}
		if got := runners["claude"].Env["HTTPS_PROXY"]; got != "http://proxy:3128" {
			t.Errorf("claude Env HTTPS_PROXY = %q, want the operator's value", got)
		}
	})
}

// TestEvolveRunnersNeverInject: evaluation Jobs have no pinned tree to read a
// declaration from and always run their harness's image, so the evolve
// fleet's brokered claude runner, unlike the finding one, injects nothing.
func TestEvolveRunnersNeverInject(t *testing.T) {
	o := cli.NewOptions()
	cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
	o.Bind(cmd)
	RegisterEvolveFlags(cmd.Flags())
	cmd.SetArgs([]string{"--evolve-claude-image", "evolve-claude:1", "--broker-url", "http://broker:8080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := o.Load(cmd); err != nil {
		t.Fatalf("Load: %v", err)
	}
	runners, err := EvolveRunners(o)
	if err != nil {
		t.Fatalf("EvolveRunners: %v", err)
	}
	if claude := runners["claude"]; !claude.Brokered || claude.Inject != nil {
		t.Errorf("evolve claude runner = %+v, want brokered with no Inject", claude)
	}
}

// TestRepositoryImages: the kill switch defaults off with no ephemeral
// storage (every Job exactly as before), and turning it on without the
// wall on disk, or with a quantity that does not parse, fails startup.
func TestRepositoryImages(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantEnabled bool
		wantStorage string
		wantErr     string
	}{
		{"defaults", nil, false, "", ""},
		{"storage alone", []string{"--agent-ephemeral-storage", "8Gi"}, false, "8Gi", ""},
		{"enabled with storage", []string{"--repository-images", "--agent-ephemeral-storage", "8Gi"}, true, "8Gi", ""},
		{"enabled without storage", []string{"--repository-images"}, false, "", "--agent-ephemeral-storage is required"},
		{"bad quantity", []string{"--agent-ephemeral-storage", "lots"}, false, "", "--agent-ephemeral-storage \"lots\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := cli.NewOptions()
			cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
			o.Bind(cmd)
			RegisterRepositoryImageFlags(cmd.Flags())
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if err := o.Load(cmd); err != nil {
				t.Fatalf("Load: %v", err)
			}
			enabled, storage, err := RepositoryImages(o)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("RepositoryImages err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RepositoryImages: %v", err)
			}
			if enabled != tt.wantEnabled || storage != tt.wantStorage {
				t.Errorf("RepositoryImages = (%v, %q), want (%v, %q)", enabled, storage, tt.wantEnabled, tt.wantStorage)
			}
		})
	}
}

// chartEphemeralStoragePattern reads the pattern the chart's values schema
// puts on agent.repositoryImages.ephemeralStorage.
func chartEphemeralStoragePattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(chartSecretEnvEnum)
	if err != nil {
		t.Fatalf("read chart schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Agent struct {
				Properties struct {
					RepositoryImages struct {
						Properties struct {
							EphemeralStorage struct {
								Pattern string `json:"pattern"`
							} `json:"ephemeralStorage"`
						} `json:"properties"`
					} `json:"repositoryImages"`
				} `json:"properties"`
			} `json:"agent"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse chart schema: %v", err)
	}
	pattern := doc.Properties.Agent.Properties.RepositoryImages.Properties.EphemeralStorage.Pattern
	if pattern == "" {
		t.Fatalf("%s: no ephemeralStorage pattern found — did the schema shape change?", chartSecretEnvEnum)
	}
	// helm validates schema patterns with Go's regexp (santhosh-tekuri
	// jsonschema's default engine), so this is the same dialect.
	return regexp.MustCompile(pattern)
}

// genQuantity returns a candidate ephemeral-storage value: a number form
// and a suffix, each drawn from valid and near-miss spellings, or a short
// run of quantity characters.
func genQuantity(r *rand.Rand) string {
	if r.Intn(3) == 0 {
		const alphabet = "0123456789.+-eEinumkKMGTPi xB"
		b := make([]byte, 1+r.Intn(6))
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return string(b)
	}
	numbers := []string{"8", "20", "1.5", ".5", "5.", "08", "0", "+8", "-8", "1.5.5", ".", "", " 8"}
	suffixes := []string{"", "Ki", "Mi", "Gi", "Ti", "Pi", "Ei", "k", "M", "G", "T", "P", "E", "m", "n", "u",
		"K", "GB", "gi", "e3", "E3", "e", "i", " ", "Gi "}
	return numbers[r.Intn(len(numbers))] + suffixes[r.Intn(len(suffixes))]
}

// TestChartEphemeralStoragePatternIsSound: the chart renders
// agent.repositoryImages.ephemeralStorage into PATCHY_AGENT_EPHEMERAL_STORAGE
// for both job controllers, and RepositoryImages refuses a quantity that does
// not parse — at startup, where under the chart's Recreate strategy it
// replaces a running controller with a crash-looping one. So every value the
// schema's pattern admits must be one RepositoryImages accepts, read from the
// environment exactly as the chart delivers it; and the sizes operators write
// must be admitted, or the pattern is merely strict.
func TestChartEphemeralStoragePatternIsSound(t *testing.T) {
	re := chartEphemeralStoragePattern(t)
	accept := func(q string) error {
		t.Setenv("PATCHY_AGENT_EPHEMERAL_STORAGE", q)
		o := cli.NewOptions()
		cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
		o.Bind(cmd)
		RegisterRepositoryImageFlags(cmd.Flags())
		cmd.SetArgs([]string{"--repository-images"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if err := o.Load(cmd); err != nil {
			t.Fatalf("Load: %v", err)
		}
		_, _, err := RepositoryImages(o)
		return err
	}

	for _, q := range []string{"8Gi", "20Gi", "512Mi", "1.5Gi", "1Ti", "8G", "100M", "500000k"} {
		if !re.MatchString(q) {
			t.Errorf("pattern %s refuses %q, a size operators write", re, q)
		}
		if err := accept(q); err != nil {
			t.Errorf("RepositoryImages refuses %q: %v", q, err)
		}
	}

	admitted := 0
	cfg := &quick.Config{
		MaxCount: 1000,
		Rand:     rand.New(rand.NewSource(20260923)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(genQuantity(r))
		},
	}
	sound := func(q string) bool {
		// The empty string is the schema's "unset"; the render-time guard,
		// not the pattern, refuses it when the block is enabled.
		if q == "" || !re.MatchString(q) {
			return true
		}
		admitted++
		if err := accept(q); err != nil {
			t.Logf("pattern admits %q, RepositoryImages refuses it: %v", q, err)
			return false
		}
		return true
	}
	if err := quick.Check(sound, cfg); err != nil {
		t.Error(err)
	} else if admitted < 100 {
		t.Errorf("only %d generated values matched the pattern; the property is near-vacuous", admitted)
	}
}
