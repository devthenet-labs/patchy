// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// netprobe is a one-shot cold-start network-isolation test. It has no listener.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

type target struct {
	name string
	addr string
	imds bool
}

type result struct {
	Name      string `json:"name"`
	Worker    int    `json:"worker,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Outcome   string `json:"outcome"`
	Status    int    `json:"status,omitempty"`
}

type envTarget struct{ name, key, port string }

// requiredTargets are always probed; siblingTargets are probed together or
// not at all. A multi-component Preview runs several components in one slot,
// so the slot policy must also keep a component from its sibling's Pod and
// Service, and from a Pod in another slot: a page reaches a sibling only from
// the browser, through the load balancer.
var (
	requiredTargets = []envTarget{
		{"kubernetes-api", "PROBE_KUBERNETES_API", "443"},
		{"patchy-egress-broker", "PROBE_EGRESS_BROKER", "8080"},
		{"patchy-integration-controller", "PROBE_INTEGRATION_CONTROLLER", "8080"},
		{"patchy-source-controller", "PROBE_SOURCE_CONTROLLER", "9790"},
		{"patchy-status-server", "PROBE_STATUS_SERVER", "8080"},
	}
	siblingTargets = []envTarget{
		{"same-slot-sibling-pod", "PROBE_SIBLING_POD", "8080"},
		{"same-slot-sibling-service", "PROBE_SIBLING_SERVICE", "80"},
		{"other-slot-pod", "PROBE_OTHER_SLOT_POD", "8080"},
	}
)

// The script discovers these service IPs immediately before deployment and
// passes them as ordinary, non-secret environment variables. Missing values
// make the test inconclusive, not a misleading pass. Link-local and internet
// targets are constant. No target is ever accessed by hostname.
func targets() ([]target, bool) { return targetsFrom(os.Getenv) }

func targetsFrom(getenv func(string) string) ([]target, bool) {
	out := []target{
		{name: "imds-v2-token-put", addr: "169.254.169.254:80", imds: true},
		{name: "pod-identity-agent", addr: "169.254.170.23:80"},
		{name: "internet", addr: "1.1.1.1:443"},
	}
	ipv4 := func(item envTarget) (target, bool) {
		ip := net.ParseIP(getenv(item.key))
		if ip == nil || ip.To4() == nil {
			return target{}, false
		}
		return target{name: item.name, addr: net.JoinHostPort(ip.String(), item.port)}, true
	}
	for _, item := range requiredTargets {
		t, ok := ipv4(item)
		if !ok {
			return nil, false
		}
		out = append(out, t)
	}
	// The sibling set is all or nothing: one silently missing would drop a
	// forbidden target from a run that then reports a pass.
	given := 0
	for _, item := range siblingTargets {
		if getenv(item.key) != "" {
			given++
		}
	}
	if given == 0 {
		return out, true
	}
	if given != len(siblingTargets) {
		return nil, false
	}
	for _, item := range siblingTargets {
		t, ok := ipv4(item)
		if !ok {
			return nil, false
		}
		out = append(out, t)
	}
	return out, true
}

func classify(err error) string {
	if err == nil {
		return "REACHABLE"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "blocked"
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return "blocked"
	}
	// ECONNREFUSED is not evidence of policy: the listener might be down.
	return "inconclusive"
}

func probe(ctx context.Context, dialer *net.Dialer, t target, worker, attempt int, start time.Time) result {
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	r := result{Name: t.name, Worker: worker, Attempt: attempt}
	if t.imds {
		status, connected, err := imdsTokenPUT(ctx, dialer, t.addr)
		r.Status = status
		if connected {
			r.Outcome = "REACHABLE"
		} else {
			r.Outcome = classify(err)
		}
	} else {
		conn, err := dialer.DialContext(ctx, "tcp", t.addr)
		if conn != nil {
			_ = conn.Close()
		}
		r.Outcome = classify(err)
	}
	r.ElapsedMS = time.Since(start).Milliseconds()
	return r
}

func burst(start time.Time, targets []target, emit func(result)) (reachable, inconclusive bool) {
	const workers, attempts = 4, 4
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin := make(chan struct{})
	results := make(chan result, len(targets)*workers*attempts)
	var wg sync.WaitGroup
	for _, t := range targets {
		for worker := 1; worker <= workers; worker++ {
			wg.Add(1)
			go func(t target, worker int) {
				defer wg.Done()
				<-begin
				for attempt := 1; attempt <= attempts; attempt++ {
					if ctx.Err() != nil {
						return
					}
					r := probe(ctx, &net.Dialer{}, t, worker, attempt, start)
					results <- r
					if r.Outcome == "REACHABLE" {
						return
					}
				}
			}(t, worker)
		}
	}
	// The first network-related operation of main happens at this release.
	close(begin)
	go func() { wg.Wait(); close(results) }()
	for r := range results {
		emit(r)
		switch r.Outcome {
		case "REACHABLE":
			reachable = true
			cancel()
		case "inconclusive":
			inconclusive = true
		}
	}
	return reachable, inconclusive
}

func emit(r result) { _ = json.NewEncoder(os.Stdout).Encode(r) }

func main() {
	start := time.Now()
	all, ok := targets()
	if !ok {
		emit(result{Name: "summary", Outcome: "INCONCLUSIVE", ElapsedMS: time.Since(start).Milliseconds()})
		os.Exit(3)
	}
	reachable, inconclusive := burst(start, all, emit)
	if reachable || inconclusive {
		outcome, code := "SECURITY_FAILURE", 2
		if !reachable {
			outcome, code = "INCONCLUSIVE", 3
		}
		emit(result{Name: "summary", Outcome: outcome, ElapsedMS: time.Since(start).Milliseconds()})
		time.Sleep(time.Minute) // observer has time to delete the Deployment
		os.Exit(code)
	}
	// DNS may become available after policy programming. Forbidden targets
	// are never retried after the initial burst.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := net.DefaultResolver.LookupHost(ctx, "kubernetes.default.svc.cluster.local")
		cancel()
		if err == nil {
			emit(result{Name: "dns", Outcome: "ok", ElapsedMS: time.Since(start).Milliseconds()})
			emit(result{Name: "summary", Outcome: "PASS", ElapsedMS: time.Since(start).Milliseconds()})
			time.Sleep(time.Minute)
			return
		}
		time.Sleep(time.Second)
	}
	emit(result{Name: "summary", Outcome: "DNS_FAILURE", ElapsedMS: time.Since(start).Milliseconds()})
	time.Sleep(time.Minute)
	os.Exit(4)
}
