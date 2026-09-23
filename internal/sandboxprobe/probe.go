// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sandboxprobe

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// ExitUnenforced is the exit status the probe ends with when a target is
// still reachable at the end of its window. internal/jobs re-exports it as
// ExitSandboxUnenforced and the collectors map that init exit code to a
// SandboxUnenforced failure.
const ExitUnenforced = 78

// DefaultTimeout is how long the probe keeps retrying while egress is open
// before concluding that NetworkPolicy is not enforced.
const DefaultTimeout = 20 * time.Second

// TimeoutEnv names the environment variable the prepare init container
// reads the retry window from, as a Go duration; unset means DefaultTimeout.
const TimeoutEnv = "PATCHY_SANDBOX_PROBE_TIMEOUT"

// Per-attempt bounds: one TCP connect is given dialTimeout, and rounds that
// still see an open target are retried every retryInterval.
const (
	dialTimeout   = 3 * time.Second
	retryInterval = time.Second
)

// Target is one address the sandbox must not be able to reach.
type Target struct {
	Name string // what the address is, for the verdict line
	Addr string // host:port
}

// String renders the target for a verdict line.
func (t Target) String() string { return t.Name + " (" + t.Addr + ")" }

// DefaultTargets are the addresses every repository-image Job must find
// blocked: a public IP, the cluster's API server (from the environment the
// kubelet injects into every container) and the cloud metadata service.
func DefaultTargets(getenv func(string) string) []Target {
	targets := []Target{{Name: "public internet", Addr: "1.1.1.1:443"}}
	if host := getenv("KUBERNETES_SERVICE_HOST"); host != "" {
		port := getenv("KUBERNETES_SERVICE_PORT")
		if port == "" {
			port = "443"
		}
		targets = append(targets, Target{Name: "kubernetes API", Addr: net.JoinHostPort(host, port)})
	}
	return append(targets, Target{Name: "cloud metadata", Addr: "169.254.169.254:80"})
}

// Prober runs the check. The zero value of every optional field selects the
// production behaviour: a real TCP dialer, the wall clock and time.After.
type Prober struct {
	Targets []Target
	// Timeout is the retry window; zero means DefaultTimeout.
	Timeout time.Duration
	// Dial attempts one connection to addr and returns nil when the target
	// answered. Nil uses a net.Dialer bounded by dialTimeout.
	Dial func(ctx context.Context, addr string) error
	// Now and Sleep are the clock; nil uses time.Now and a context-aware
	// time.After.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Result is the probe's conclusion.
type Result struct {
	// Enforced is true when no target answered in the last round.
	Enforced bool
	// Open lists the targets still reachable when the window closed; nil
	// when Enforced.
	Open []Target
	// Rounds is how many attempts were made; Elapsed how long they took.
	Rounds  int
	Elapsed time.Duration
}

// Verdict is the one line the init container prints to stderr.
func (r Result) Verdict() string {
	if r.Enforced {
		return fmt.Sprintf("sandbox probe: egress blocked after %d round(s) in %s; NetworkPolicy is enforced",
			r.Rounds, r.Elapsed.Round(time.Millisecond))
	}
	names := make([]string, 0, len(r.Open))
	for _, t := range r.Open {
		names = append(names, t.String())
	}
	return fmt.Sprintf("sandbox probe: egress still open after %d round(s) in %s to %s; "+
		"NetworkPolicy is not enforced, refusing to run untrusted code",
		r.Rounds, r.Elapsed.Round(time.Millisecond), strings.Join(names, ", "))
}

// FromEnv builds the production Prober: the default targets and the window
// from TimeoutEnv. An unparseable window is an error rather than a silent
// default, because the value is controller-written and a bad one means a
// bug, not an operator choice.
func FromEnv(getenv func(string) string) (Prober, error) {
	p := Prober{Targets: DefaultTargets(getenv), Timeout: DefaultTimeout}
	if raw := getenv(TimeoutEnv); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Prober{}, fmt.Errorf("%s=%q is not a duration", TimeoutEnv, raw)
		}
		p.Timeout = d
	}
	return p, nil
}

// Run probes every target each round until either none answers (enforced)
// or the window has elapsed with at least one still answering (unenforced).
// It returns an error only when ctx ends first; a verdict is never inferred
// from a cancelled round, since a dial that failed because the context died
// looks exactly like a blocked one.
func (p Prober) Run(ctx context.Context) (Result, error) {
	now, sleep, dial := p.Now, p.Sleep, p.Dial
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepContext
	}
	if dial == nil {
		dial = dialTCP
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	start := now()
	for round := 1; ; round++ {
		open := reachable(ctx, p.Targets, dial)
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		elapsed := now().Sub(start)
		if len(open) == 0 {
			return Result{Enforced: true, Rounds: round, Elapsed: elapsed}, nil
		}
		if elapsed >= timeout {
			return Result{Open: open, Rounds: round, Elapsed: elapsed}, nil
		}
		if err := sleep(ctx, retryInterval); err != nil {
			return Result{}, err
		}
	}
}

// reachable dials every target concurrently, so a round costs one dial
// timeout rather than one per target, and returns the ones that answered in
// their declared order.
func reachable(ctx context.Context, targets []Target, dial func(context.Context, string) error) []Target {
	answered := make([]bool, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() {
			dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			answered[i] = dial(dialCtx, t.Addr) == nil
		})
	}
	wg.Wait()
	var open []Target
	for i, t := range targets {
		if answered[i] {
			open = append(open, t)
		}
	}
	return open
}

// dialTCP is the production dialer: a TCP connect bounded by the context,
// closed immediately, because reaching the port at all is the finding.
func dialTCP(ctx context.Context, addr string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// sleepContext waits d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
