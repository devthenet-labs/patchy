// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/provider"
	"github.com/bitwise-media-group/patchy/internal/templates"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// Phase selects which stage runs.
type Phase string

// PhaseInvestigate runs the analysis stage only; PhaseRemediate remediates
// from a controller-provided analysis file.
//
// PhasePlan and PhaseBuild are an intent's stages. Plan reads the request
// and the tree and writes a plan, read-only, on the investigate stage's
// harness, model, limits and timeout; build builds the approved plan — its
// first build and every revise round — on the remediate stage's, down to
// its grant. Both run on brokered claude only (see intentHarness). The Job
// seams keep their Finding names on an intent run: PATCHY_FINDING carries
// the IntentRun's name, and input/investigation.md the approved plan.
const (
	PhaseInvestigate Phase = "investigate"
	PhaseRemediate   Phase = "remediate"
	PhasePlan        Phase = "plan"
	PhaseBuild       Phase = "build"
)

// Config is the runner's configuration; in the pod every field arrives as
// PATCHY_* environment (see FromEnv).
type Config struct {
	Workspace string
	Repo      string
	// Finding names the owning Finding resource; events echo it so the
	// controller can key them.
	Finding string
	// BaseSHA is the remote commit the workspace tree corresponds to. The
	// local git base is a synthetic commit over the artifact tarball, so the
	// changeset's push base must come from here instead.
	BaseSHA string
	Phase   Phase

	// InvestigateHarness/RemediateHarness are the harness ids the stages run
	// on, resolved controller-side from the stage model and passed per-Job.
	InvestigateHarness string
	RemediateHarness   string
	// InvestigateModel/RemediateModel are canonical provider-qualified model
	// ids (e.g. "anthropic/claude-sonnet-5"). The controller resolves the
	// harness and clamps the remediation model to the allowlist before the
	// Job is created, so these already match the runner image the pod is in.
	InvestigateModel string
	RemediateModel   string
	// ModelAllowlist is the canonical model ids the investigation may suggest
	// for remediation; rendered into the analysis prompt.
	ModelAllowlist []string
	// BrokerTokenFile is the projected ServiceAccount token file the pod
	// authenticates to the egress broker with; empty on a non-brokered run.
	// It is read fresh per stage — the kubelet rotates the projection.
	BrokerTokenFile string
	// ModelMap translates canonical model ids to the provider-specific CLI
	// ids a brokered provider expects (Bedrock inference profiles, Foundry
	// deployment names); consulted before the registry in cliModel.
	ModelMap map[string]string
	// BinDir is the read-only directory the trusted prepare init copied
	// patchy's own binaries into when the pod runs a repository-declared
	// image (BinDirEnv, /patchy/bin); empty on the default runner
	// image. When set, the stage's harness CLI is run from it by absolute
	// path, after a preflight that proves the image can execute it.
	BinDir string

	// The investigation stage's limits are absolute: it runs on exactly
	// these.
	InvestigateMaxTurns    int
	InvestigateTokenBudget int

	// The remediation budget, split by who authorized the run.
	//
	// RemediateAutoMaxTurns/RemediateAutoTokenBudget is what patchy will spend
	// UNATTENDED: every remediation gets at least this, whatever the
	// investigation predicted (including when it predicted nothing). It is
	// also the line past which a fix stops being automatic — an estimate above
	// it holds the finding for a human instead of queueing it.
	RemediateAutoMaxTurns    int
	RemediateAutoTokenBudget int
	// RemediateManualMaxTurns/RemediateManualTokenBudget is the most a
	// human-approved run may spend. Approving an over-auto estimate grants
	// that estimate, so this bounds what one approval can authorize.
	RemediateManualMaxTurns    int
	RemediateManualTokenBudget int
	// GrantedMaxTurns/GrantedTokenBudget are what the controller granted THIS
	// run, already resolved against the estimate and the manual bound. Zero
	// means "no per-run grant" and falls back to the automated budget.
	GrantedMaxTurns    int
	GrantedTokenBudget int

	// Calibration is how previous remediations in this repository actually
	// compared to their estimates, rendered into the investigation prompt so
	// its next estimate can correct for the observed skew. Nil on a cold
	// start, and the prompt then omits the section entirely.
	Calibration *templates.Calibration

	// PreviousAttempt is the failed run of this stage that this Job retries,
	// rendered into the stage prompt so the agent does not repeat the
	// failure. Nil on a first attempt, and the prompt then omits the section.
	// Its text is untrusted; the prompt bounds and fences it.
	PreviousAttempt *templates.PreviousAttempt

	InvestigateTimeout time.Duration
	RemediateTimeout   time.Duration

	// ChangesetMaxBytes caps the cumulative raw content of the changeset the
	// remediation stage may emit.
	ChangesetMaxBytes int

	// Transcript bounds one run's captured conversation. The defaults keep it
	// inside the ConfigMap the controller persists it into; zero takes the
	// transcript package's default and negative disables that bound.
	TranscriptMaxTurnBytes  int
	TranscriptMaxTurns      int
	TranscriptMaxTotalBytes int

	// Out receives the envelope events and transcript turns (stdout in the pod).
	Out io.Writer
	Log *slog.Logger
}

// transcriptLimits is the recorder's bounds for this run.
func (c Config) transcriptLimits() transcript.Limits {
	return transcript.Limits{
		MaxTurnBytes:  c.TranscriptMaxTurnBytes,
		MaxTurns:      c.TranscriptMaxTurns,
		MaxTotalBytes: c.TranscriptMaxTotalBytes,
	}
}

// Workspace layout, derived from Config.Workspace.
func (c Config) repoDir() string   { return filepath.Join(c.Workspace, "repo") }
func (c Config) issuePath() string { return filepath.Join(c.Workspace, "input", "issue.md") }

func (c Config) inputInvestigation() string {
	return filepath.Join(c.Workspace, "input", "investigation.md")
}

func (c Config) investigationPath() string {
	return filepath.Join(c.Workspace, "reports", "investigation.md")
}

func (c Config) remediationPath() string {
	return filepath.Join(c.Workspace, "reports", "remediation.md")
}

func (c Config) commitScript() string { return filepath.Join(c.Workspace, "commit.sh") }

// planPath and buildPath are the intent stages' reports.
func (c Config) planPath() string  { return filepath.Join(c.Workspace, "reports", "plan.md") }
func (c Config) buildPath() string { return filepath.Join(c.Workspace, "reports", "build.md") }

// branch is the remediation branch, keyed by finding name (pull-request
// webhooks resolve the Finding from the head ref).
func (c Config) branch() string { return "patchy/" + c.Finding }

// BinDirEnv names the directory internal/jobs injects patchy's binaries into
// on a repository-declared image (/patchy/bin). It is the one definition of
// the name both sides of the pod boundary use: jobs sets it, FromEnv reads
// it into Config.BinDir, and preflight resolves the harness CLI there alone.
const BinDirEnv = "PATCHY_BIN_DIR"

// ConfigEnvKeys returns every PATCHY_* variable FromEnv reads, sorted. It is
// derived by running the parser against a recording getenv rather than kept
// by hand, so a new configuration key is covered the day it is added:
// internal/jobs blanks each one a Job does not set on a repository-declared
// image, and that backstop can never lag the config surface.
func ConfigEnvKeys() []string {
	seen := map[string]bool{}
	_, _ = FromEnv(func(key string) string {
		seen[key] = true
		return ""
	})
	return slices.Sorted(maps.Keys(seen))
}

// FromEnv builds the pod configuration from PATCHY_* environment variables,
// applying defaults. Every key is read unconditionally, which is what lets
// ConfigEnvKeys enumerate them.
func FromEnv(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := getenv("PATCHY_" + key); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		Workspace:          get("WORKSPACE", "/workspace"),
		Repo:               get("REPO", ""),
		Finding:            get("FINDING", ""),
		BaseSHA:            get("BASE_SHA", ""),
		Phase:              Phase(get("PHASE", string(PhaseInvestigate))),
		InvestigateHarness: get("INVESTIGATE_HARNESS", "claude"),
		RemediateHarness:   get("REMEDIATE_HARNESS", "claude"),
		InvestigateModel:   get("INVESTIGATE_MODEL", "anthropic/claude-sonnet-5"),
		RemediateModel:     get("REMEDIATE_MODEL", "anthropic/claude-sonnet-5"),
	}
	if list := get("MODEL_ALLOWLIST", cfg.RemediateModel); list != "" {
		for m := range strings.SplitSeq(list, ",") {
			if m = strings.TrimSpace(m); m != "" {
				cfg.ModelAllowlist = append(cfg.ModelAllowlist, m)
			}
		}
	}
	cfg.BrokerTokenFile = get("BROKER_TOKEN_FILE", "")
	cfg.BinDir = get(strings.TrimPrefix(BinDirEnv, "PATCHY_"), "")

	var errs []string
	if raw := get("MODEL_MAP", ""); raw != "" {
		mm, err := provider.ParseModelMap(raw)
		if err != nil {
			// A silently dropped translation would send canonical ids to a
			// provider that cannot resolve them; fail loud instead.
			errs = append(errs, fmt.Sprintf("PATCHY_MODEL_MAP: %v", err))
		}
		cfg.ModelMap = mm
	}
	number := func(key, def string) int {
		v := get(key, def)
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("PATCHY_%s=%q is not an integer", key, v))
		}
		return n
	}
	duration := func(key, def string) time.Duration {
		v := get(key, def)
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("PATCHY_%s=%q is not a duration", key, v))
		}
		return d
	}

	cfg.InvestigateMaxTurns = number("INVESTIGATE_MAX_TURNS", "25")
	cfg.InvestigateTokenBudget = number("INVESTIGATE_TOKEN_BUDGET", "150000")
	cfg.RemediateAutoMaxTurns = number("REMEDIATE_AUTO_MAX_TURNS", "80")
	cfg.RemediateAutoTokenBudget = number("REMEDIATE_AUTO_TOKEN_BUDGET", "400000")
	cfg.RemediateManualMaxTurns = number("REMEDIATE_MANUAL_MAX_TURNS", "240")
	cfg.RemediateManualTokenBudget = number("REMEDIATE_MANUAL_TOKEN_BUDGET", "1200000")
	cfg.GrantedMaxTurns = number("GRANTED_MAX_TURNS", "0")
	cfg.GrantedTokenBudget = number("GRANTED_TOKEN_BUDGET", "0")
	cfg.ChangesetMaxBytes = number("CHANGESET_MAX_BYTES", strconv.Itoa(5<<20))
	cfg.TranscriptMaxTurnBytes = number("TRANSCRIPT_MAX_TURN_BYTES", strconv.Itoa(transcript.DefaultMaxTurnBytes))
	cfg.TranscriptMaxTurns = number("TRANSCRIPT_MAX_TURNS", strconv.Itoa(transcript.DefaultMaxTurns))
	cfg.TranscriptMaxTotalBytes = number("TRANSCRIPT_MAX_TOTAL_BYTES", strconv.Itoa(transcript.DefaultMaxTotalBytes))
	cfg.InvestigateTimeout = duration("INVESTIGATE_TIMEOUT", "15m")
	cfg.RemediateTimeout = duration("REMEDIATE_TIMEOUT", "45m")

	if raw := get("CALIBRATION", ""); raw != "" {
		var c templates.Calibration
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			// Advisory prompt garnish: a malformed blob must not cost a run.
			errs = append(errs, fmt.Sprintf("PATCHY_CALIBRATION is not valid JSON: %v", err))
		} else {
			cfg.Calibration = &c
		}
	}
	if raw := get("PREVIOUS_ATTEMPT", ""); raw != "" {
		var p templates.PreviousAttempt
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			errs = append(errs, fmt.Sprintf("PATCHY_PREVIOUS_ATTEMPT is not valid JSON: %v", err))
		} else {
			cfg.PreviousAttempt = &p
		}
	}
	// Approving a fix must never buy less than leaving it alone would: a
	// manual budget below the automated one inverts the whole model.
	if cfg.RemediateManualMaxTurns < cfg.RemediateAutoMaxTurns {
		errs = append(errs, fmt.Sprintf(
			"PATCHY_REMEDIATE_MANUAL_MAX_TURNS=%d is below the automated budget %d",
			cfg.RemediateManualMaxTurns, cfg.RemediateAutoMaxTurns))
	}
	if cfg.RemediateManualTokenBudget < cfg.RemediateAutoTokenBudget {
		errs = append(errs, fmt.Sprintf(
			"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET=%d is below the automated budget %d",
			cfg.RemediateManualTokenBudget, cfg.RemediateAutoTokenBudget))
	}

	if cfg.Repo == "" {
		errs = append(errs, "PATCHY_REPO is required")
	}
	if cfg.Finding == "" {
		errs = append(errs, "PATCHY_FINDING is required")
	}
	switch cfg.Phase {
	case PhaseInvestigate, PhaseRemediate, PhasePlan, PhaseBuild:
	default:
		errs = append(errs, fmt.Sprintf("PATCHY_PHASE=%q is not %q, %q, %q or %q",
			cfg.Phase, PhaseInvestigate, PhaseRemediate, PhasePlan, PhaseBuild))
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("agentrun config: %s", strings.Join(errs, "; "))
	}

	cfg.Out = os.Stdout
	return cfg, nil
}
