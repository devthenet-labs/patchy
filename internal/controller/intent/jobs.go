// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"maps"
	"strconv"

	"k8s.io/client-go/kubernetes"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// JobRunner is the jobs surface the run scheduler needs. Create takes the
// run's stage configuration: each intent stage reads its Finding
// counterpart's PATCHY_* keys, and only turns and tokens have a per-Job
// channel, so the stage's wall clock and ceilings reach the pod through the
// Env of a jobs Client built for that one launch.
type JobRunner interface {
	Create(ctx context.Context, spec jobs.Spec, env map[string]string) (string, v1alpha1.RunnerImageRef, error)
	Result(ctx context.Context, jobName string) (jobs.RunOutput, error)
	Status(ctx context.Context, jobName string) (jobs.Status, error)
	Delete(ctx context.Context, jobName string) error
}

// jobLauncher is the production JobRunner: one base jobs Config, and for
// each launch a Client built from it plus that launch's stage env (jobs.New
// only wraps the clientset). Observing and deleting Jobs needs no stage env.
type jobLauncher struct {
	cs   kubernetes.Interface
	base jobs.Config
	log  *slog.Logger
	*jobs.Client
}

// NewJobRunner builds the production JobRunner over cs and the base Config.
func NewJobRunner(cs kubernetes.Interface, base jobs.Config, log *slog.Logger) JobRunner {
	return &jobLauncher{cs: cs, base: base, log: log, Client: jobs.New(cs, base, log)}
}

// Create builds a Client with env over the base Config's and creates the Job.
func (l *jobLauncher) Create(ctx context.Context, spec jobs.Spec, env map[string]string) (
	string, v1alpha1.RunnerImageRef, error) {
	cfg := l.base
	cfg.Env = maps.Clone(l.base.Env)
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	maps.Copy(cfg.Env, env)
	return jobs.New(l.cs, cfg, l.log).Create(ctx, spec)
}

// stageEnv is a launch's stage configuration. A plan run reads the
// investigate stage's keys: its wall clock and its ceilings (the grant rides
// the per-Job channel and may only lower them). It also states the most its
// build can be granted, which agent-runner reads from the build stage's own
// ceiling, PATCHY_REMEDIATE_MANUAL_*: set to the grant the Project's build
// receives, with the automated budget no higher, as agent-runner requires. A
// build run reads the remediate stage's keys: its wall clock, and its manual
// ceiling (the automated budget, which applies only to a Job with no grant,
// no higher); every intent run is granted.
func stageEnv(stage v1alpha1.IntentStage, s Settings, build v1alpha1.IntentRunGrant) map[string]string {
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	if stage == v1alpha1.IntentStagePlan {
		return map[string]string{
			"PATCHY_INVESTIGATE_TIMEOUT":           s.Plan.Timeout.String(),
			"PATCHY_INVESTIGATE_MAX_TURNS":         n(int64(s.Plan.MaxTurns)),
			"PATCHY_INVESTIGATE_TOKEN_BUDGET":      n(s.Plan.TokenBudget),
			"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    n(int64(build.MaxTurns)),
			"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": n(build.TokenBudget),
			"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      n(int64(build.MaxTurns)),
			"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   n(build.TokenBudget),
		}
	}
	return map[string]string{
		"PATCHY_REMEDIATE_TIMEOUT":             s.Build.Timeout.String(),
		"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    n(int64(s.Build.MaxTurns)),
		"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": n(s.Build.TokenBudget),
		"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      n(int64(s.Build.MaxTurns)),
		"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   n(s.Build.TokenBudget),
	}
}
