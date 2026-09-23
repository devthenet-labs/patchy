// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sandboxprobe

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"
)

var testTargets = []Target{
	{Name: "public internet", Addr: "1.1.1.1:443"},
	{Name: "kubernetes API", Addr: "10.0.0.1:443"},
	{Name: "cloud metadata", Addr: "169.254.169.254:80"},
}

// fakeClock advances only when the prober sleeps, so a test's rounds are
// counted rather than waited for.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

// fakeNet answers dials by round: every round dials each target exactly
// once (concurrently), so the round is the dial count divided by the target
// count.
type fakeNet struct {
	mu      sync.Mutex
	calls   int
	targets int
	open    func(round int, addr string) bool
}

func (n *fakeNet) dial(_ context.Context, addr string) error {
	n.mu.Lock()
	round := n.calls/n.targets + 1
	n.calls++
	n.mu.Unlock()
	if n.open(round, addr) {
		return nil
	}
	return errors.New("connection refused")
}

func newProber(clock *fakeClock, open func(round int, addr string) bool, timeout time.Duration) Prober {
	n := &fakeNet{targets: len(testTargets), open: open}
	return Prober{Targets: testTargets, Timeout: timeout, Dial: n.dial, Now: clock.Now, Sleep: clock.Sleep}
}

// TestRunEnforcedAfterConfirmingRounds: egress blocked from the start is
// concluded after confirmRounds blocked rounds, one retry interval apart,
// and no sooner.
func TestRunEnforcedAfterConfirmingRounds(t *testing.T) {
	clock := newClock()
	p := newProber(clock, func(int, string) bool { return false }, 20*time.Second)
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Enforced || res.Rounds != confirmRounds || res.Open != nil {
		t.Errorf("Result = %+v, want enforced after %d rounds", res, confirmRounds)
	}
	if len(clock.sleeps) != confirmRounds-1 {
		t.Errorf("sleeps = %v, want one between each of the %d confirming rounds", clock.sleeps, confirmRounds)
	}
	if want := time.Duration(confirmRounds-1) * retryInterval; res.Elapsed != want {
		t.Errorf("Elapsed = %s, want %s", res.Elapsed, want)
	}
}

// TestRunRetriesWhileEgressAttaches: the CNI attaches the policy a few
// seconds after the pod starts, so early successful connections must not
// be the verdict; the blocked rounds after it are, once confirmRounds of
// them have run — even when that run of rounds began at the very end of
// the window and finishes past it.
func TestRunRetriesWhileEgressAttaches(t *testing.T) {
	tests := []struct {
		name       string
		openRounds int // rounds during which every target answers
		timeout    time.Duration
		wantRounds int
	}{
		{"closes on the second round", 1, 20 * time.Second, 1 + confirmRounds},
		{"closes after five seconds", 5, 20 * time.Second, 5 + confirmRounds},
		{"closes on the last round inside the window", 19, 20 * time.Second, 19 + confirmRounds},
		{"closes on the round the window elapses", 20, 20 * time.Second, 20 + confirmRounds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newClock()
			p := newProber(clock, func(round int, _ string) bool { return round <= tt.openRounds }, tt.timeout)
			res, err := p.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !res.Enforced {
				t.Fatalf("Result = %+v, want enforced once the policy attached", res)
			}
			if res.Rounds != tt.wantRounds {
				t.Errorf("Rounds = %d, want %d", res.Rounds, tt.wantRounds)
			}
			if len(clock.sleeps) != tt.wantRounds-1 {
				t.Errorf("sleeps = %d, want one between each of the %d rounds", len(clock.sleeps), tt.wantRounds)
			}
			for _, d := range clock.sleeps {
				if d != retryInterval {
					t.Errorf("sleep = %s, want the %s retry interval", d, retryInterval)
				}
			}
			if want := time.Duration(tt.wantRounds-1) * retryInterval; res.Elapsed != want {
				t.Errorf("Elapsed = %s, want %s", res.Elapsed, want)
			}
		})
	}
}

// TestRunUnenforcedAfterWindow: a target still answering when the window
// closes is the SandboxUnenforced verdict, and only then.
func TestRunUnenforcedAfterWindow(t *testing.T) {
	all := []string{"public internet", "kubernetes API", "cloud metadata"}
	tests := []struct {
		name       string
		open       func(round int, addr string) bool
		timeout    time.Duration
		wantRounds int
		wantOpen   []string
	}{
		{
			"everything open for the whole window",
			func(int, string) bool { return true },
			20 * time.Second, 21, all,
		},
		{
			"one target open is enough",
			func(_ int, addr string) bool { return addr == "169.254.169.254:80" },
			20 * time.Second, 21, []string{"cloud metadata"},
		},
		{
			"a zero window means the default",
			func(int, string) bool { return true },
			0, int(DefaultTimeout/retryInterval) + 1, all,
		},
		{
			"a short window bounds the retries",
			func(int, string) bool { return true },
			3 * time.Second, 4, all,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newClock()
			p := newProber(clock, tt.open, tt.timeout)
			res, err := p.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Enforced {
				t.Fatalf("Result = %+v, want unenforced", res)
			}
			if res.Rounds != tt.wantRounds {
				t.Errorf("Rounds = %d, want %d", res.Rounds, tt.wantRounds)
			}
			var got []string
			for _, o := range res.Open {
				got = append(got, o.Name)
			}
			if strings.Join(got, ",") != strings.Join(tt.wantOpen, ",") {
				t.Errorf("Open = %v, want %v", got, tt.wantOpen)
			}
		})
	}
}

// TestRunPropertyVerdictFollowsFirstConfirmedRun pins the invariant behind
// the tables above, stated declaratively rather than as the loop: for any
// pattern of open and blocked rounds and any window, let E be the first
// round that completes confirmRounds consecutive blocked rounds and U the
// first round at or past the end of the window in which a target answers.
// The probe is enforced at E when E comes before U, and otherwise
// unenforced at U with the answering target listed. Seeded so the gate is
// deterministic; both verdicts must occur so the property is never
// vacuously true.
func TestRunPropertyVerdictFollowsFirstConfirmedRun(t *testing.T) {
	const maxSteps = 21 // rounds inside a 20s window at the 1s interval
	const rounds = maxSteps + confirmRounds

	var enforced, unenforced int
	property := func(pattern [rounds]uint8, windowSeed uint8) bool {
		// The window's last round is steps; steps is at least 2 so the
		// window is never zero (which would mean the default).
		steps := int(windowSeed%(maxSteps-1)) + 2
		timeout := time.Duration(steps-1) * retryInterval
		isOpen := func(round int) bool { return round > rounds || pattern[round-1]&1 == 1 }
		open := func(round int, addr string) bool {
			if !isOpen(round) {
				return false
			}
			// Exactly one target answers in an open round, chosen by the
			// pattern, so Open is never trivially the whole list.
			bits := uint8(0)
			if round <= rounds {
				bits = pattern[round-1]
			}
			return addr == testTargets[int(bits>>1)%len(testTargets)].Addr
		}
		res, err := newProber(newClock(), open, timeout).Run(context.Background())
		if err != nil {
			return false
		}
		e, u := verdictRounds(isOpen, steps, rounds)
		if e > 0 && e < u {
			enforced++
			return res.Enforced && res.Rounds == e && res.Open == nil &&
				res.Elapsed == time.Duration(e-1)*retryInterval
		}
		unenforced++
		return !res.Enforced && res.Rounds == u && len(res.Open) == 1 &&
			res.Elapsed == time.Duration(u-1)*retryInterval
	}
	cfg := &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(20260922))}
	if err := quick.Check(property, cfg); err != nil {
		t.Fatal(err)
	}
	if enforced == 0 || unenforced == 0 {
		t.Errorf("generator reached enforced=%d unenforced=%d, want both", enforced, unenforced)
	}
}

// verdictRounds is the property's oracle: e is the first round completing
// confirmRounds consecutive blocked rounds (0 if none within rounds), u the
// first round at or past the window's last round, steps, in which a target
// answers (rounds past the pattern always answer, so u exists).
func verdictRounds(isOpen func(round int) bool, steps, rounds int) (e, u int) {
	for r := confirmRounds; r <= rounds && e == 0; r++ {
		run := true
		for k := r - confirmRounds + 1; k <= r; k++ {
			run = run && !isOpen(k)
		}
		if run {
			e = r
		}
	}
	for u = steps; !isOpen(u); u++ {
	}
	return e, u
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := newProber(newClock(), func(int, string) bool { return false }, 20*time.Second)
	if _, err := p.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run on a cancelled context = %v, want context.Canceled (never a verdict)", err)
	}
}

func TestRunRealDialerRefused(t *testing.T) {
	// A listener closed before the probe runs gives a deterministic
	// refusal on the loopback port it held.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	clock := newClock()
	p := Prober{Targets: []Target{{Name: "closed port", Addr: addr}}, Now: clock.Now, Sleep: clock.Sleep}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Enforced {
		t.Errorf("Result = %+v, want enforced against a closed port", res)
	}
}

func TestRunRealDialerOpen(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	clock := newClock()
	p := Prober{
		Targets: []Target{{Name: "open port", Addr: l.Addr().String()}},
		Timeout: 2 * time.Second, Now: clock.Now, Sleep: clock.Sleep,
	}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Enforced || len(res.Open) != 1 || res.Rounds != 3 {
		t.Errorf("Result = %+v, want unenforced after three rounds against a listening port", res)
	}
}

func TestDefaultTargets(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"no api server env", nil, []string{"1.1.1.1:443", "169.254.169.254:80"}},
		{"api server host and port",
			map[string]string{"KUBERNETES_SERVICE_HOST": "10.96.0.1", "KUBERNETES_SERVICE_PORT": "6443"},
			[]string{"1.1.1.1:443", "10.96.0.1:6443", "169.254.169.254:80"}},
		{"api server host only", map[string]string{"KUBERNETES_SERVICE_HOST": "10.96.0.1"},
			[]string{"1.1.1.1:443", "10.96.0.1:443", "169.254.169.254:80"}},
		{"ipv6 api server", map[string]string{"KUBERNETES_SERVICE_HOST": "fd00::1"},
			[]string{"1.1.1.1:443", "[fd00::1]:443", "169.254.169.254:80"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, target := range DefaultTargets(func(k string) string { return tt.env[k] }) {
				got = append(got, target.Addr)
			}
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("DefaultTargets = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"unset keeps the default", "", DefaultTimeout, false},
		{"a duration is honoured", "45s", 45 * time.Second, false},
		{"garbage is refused", "twenty", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := FromEnv(func(k string) string {
				switch k {
				case TimeoutEnv:
					return tt.raw
				case "KUBERNETES_SERVICE_HOST":
					return "10.96.0.1"
				}
				return ""
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("FromEnv error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && p.Timeout != tt.want {
				t.Errorf("Timeout = %s, want %s", p.Timeout, tt.want)
			}
		})
	}
}

func TestVerdict(t *testing.T) {
	enforced := Result{Enforced: true, Rounds: 4, Elapsed: 3 * time.Second}
	if got := enforced.Verdict(); !strings.Contains(got, "egress blocked after 4 round(s) in 3s") ||
		!strings.Contains(got, "enforced") {
		t.Errorf("Verdict = %q", got)
	}
	open := Result{Rounds: 21, Elapsed: 20 * time.Second, Open: testTargets[2:]}
	got := open.Verdict()
	for _, want := range []string{"still open after 21 round(s) in 20s", "cloud metadata (169.254.169.254:80)",
		"not enforced", "refusing to run untrusted code"} {
		if !strings.Contains(got, want) {
			t.Errorf("Verdict = %q, want it to mention %q", got, want)
		}
	}
}

// closeFailingConn is a connection that was established but whose Close
// reports an error, as a reset arriving before close can on some stacks.
type closeFailingConn struct{ net.Conn }

func (closeFailingConn) Close() error { return errors.New("connection reset by peer") }

// TestConnectCountsAnEstablishedConnection: once the TCP connect succeeded
// the target is reachable, whatever closing the socket reports. Counting a
// failed Close as blocked would be the unsafe direction — a round in which
// every target connected could conclude "enforced".
func TestConnectCountsAnEstablishedConnection(t *testing.T) {
	established := func(context.Context, string, string) (net.Conn, error) { return closeFailingConn{}, nil }
	if err := connect(context.Background(), "1.1.1.1:443", established); err != nil {
		t.Errorf("connect = %v on an established connection whose Close failed, want nil (reachable)", err)
	}
	refused := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	if err := connect(context.Background(), "1.1.1.1:443", refused); err == nil {
		t.Error("connect = nil on a refused dial, want the error (blocked)")
	}
}

// TestRunOneBlockedRoundIsNotEnforcement: a single round in which nothing
// answered (a lost SYN on each target, the CNI still wiring the pod) is not
// evidence that policy is enforced when every later round connects. Only a
// run of blocked rounds is; a target still open when the window closes is
// the unenforced verdict.
func TestRunOneBlockedRoundIsNotEnforcement(t *testing.T) {
	tests := []struct {
		name    string
		blocked func(round int) bool
	}{
		{"only the first round blocked", func(round int) bool { return round == 1 }},
		{"every other round blocked", func(round int) bool { return round%2 == 1 }},
		{"one short of confirming, then open", func(round int) bool { return round <= 2 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProber(newClock(), func(round int, _ string) bool { return !tt.blocked(round) }, 20*time.Second)
			res, err := p.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Enforced {
				t.Errorf("Result = %+v, want unenforced: egress was open in the rounds after the blocked one", res)
			}
		})
	}
}

// TestFromEnvRequiresAPIServer: the API-server target comes from the
// environment the kubelet injects into every container. Without it the
// probe would check two targets that a hardened network blocks regardless
// of NetworkPolicy, so a missing KUBERNETES_SERVICE_HOST is a configuration
// error, not a smaller target set.
func TestFromEnvRequiresAPIServer(t *testing.T) {
	if _, err := FromEnv(func(string) string { return "" }); err == nil ||
		!strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
		t.Errorf("FromEnv without KUBERNETES_SERVICE_HOST = %v, want an error naming it", err)
	}
	p, err := FromEnv(func(k string) string {
		return map[string]string{"KUBERNETES_SERVICE_HOST": "10.96.0.1"}[k]
	})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	addrs := make([]string, 0, len(p.Targets))
	for _, target := range p.Targets {
		addrs = append(addrs, target.Addr)
	}
	if !slices.Contains(addrs, "10.96.0.1:443") {
		t.Errorf("targets = %v, want the API server among them", addrs)
	}
}
