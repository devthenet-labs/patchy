// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/devharness"
)

const devPayload = `{"version":"v1","event":"findings","findings":[` +
	`{"repo":{"owner":"acme","name":"app"},"alertId":"A-1","title":"SQL injection","severity":"high"}]}`

func writePayload(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// signedEndpoint is an enhancer/resolver that verifies the patchy signature
// and records the bodies it accepted.
type signedEndpoint struct {
	secret string
	reply  string
	mu     sync.Mutex
	bodies []string
	bad    int
}

func (e *signedEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	mac := hmac.New(sha256.New, []byte(e.secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	e.mu.Lock()
	defer e.mu.Unlock()
	if !hmac.Equal([]byte(r.Header.Get("X-Patchy-Signature-256")), []byte(want)) {
		e.bad++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	e.bodies = append(e.bodies, string(body))
	if e.reply == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(e.reply))
}

// ndjson decodes one event per stdout line.
func ndjson(t *testing.T, out string) []devharness.Event {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	events := make([]devharness.Event, 0, len(lines))
	for _, line := range lines {
		var e devharness.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("stdout line %q is not an event: %v", line, err)
		}
		events = append(events, e)
	}
	return events
}

func TestDevEnhanceExchange(t *testing.T) {
	t.Chdir(t.TempDir())
	ep := &signedEndpoint{secret: "dev-secret", reply: `{"owners":["team-a"],"attributes":{"tier":"1"}}`}
	srv := httptest.NewServer(ep)
	defer srv.Close()

	out, err := execDev(t, "dev", "enhance", "--url", srv.URL+"/enhance", "--secret", "dev-secret",
		"--name", "scanner", "-o", "json", writePayload(t, devPayload))
	if err != nil {
		t.Fatalf("dev enhance: %v", err)
	}
	events := ndjson(t, out)
	if len(events) != 1 || events[0].Kind != "enhance" || events[0].Err != "" {
		t.Fatalf("events = %+v", events)
	}
	if resp := events[0].EnhanceResponse; resp == nil || len(resp.Owners) != 1 || resp.Owners[0] != "team-a" {
		t.Errorf("enhance response = %+v", events[0].EnhanceResponse)
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if ep.bad != 0 || len(ep.bodies) != 1 {
		t.Fatalf("endpoint saw %d bad signatures, %d good requests", ep.bad, len(ep.bodies))
	}
	for _, want := range []string{`"integration":"scanner"`, "SQL injection"} {
		if !strings.Contains(ep.bodies[0], want) {
			t.Errorf("request body missing %s: %s", want, ep.bodies[0])
		}
	}
}

func TestDevResolveExchangeFromSecretFile(t *testing.T) {
	t.Chdir(t.TempDir())
	ep := &signedEndpoint{secret: "file-secret"}
	srv := httptest.NewServer(ep)
	defer srv.Close()
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := execDevCapture(context.Background(), "dev", "resolve", "--url", srv.URL,
		"--secret-file", secretFile, writePayload(t, devPayload))
	if err != nil {
		t.Fatalf("dev resolve: %v\n%s", err, errOut)
	}
	if out != "" {
		t.Errorf("human rendering wrote to stdout: %q", out)
	}
	for _, want := range []string{"Resolve", "A-1"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if ep.bad != 0 || len(ep.bodies) != 1 || !strings.Contains(ep.bodies[0], `"alerts"`) {
		t.Errorf("endpoint: bad=%d bodies=%v", ep.bad, ep.bodies)
	}
}

func TestDevOneShotPayloadErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := []struct {
		name    string
		path    string
		want    string
		isUsage bool
	}{
		{"missing file", filepath.Join(t.TempDir(), "absent.json"), "read payload", false},
		{"malformed envelope", writePayload(t, `{"version":"v2","event":"findings"}`), "unsupported contract version", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execDev(t, "dev", "enhance", "--url", "http://127.0.0.1:1/e", "--secret", "s", tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if isUsage(err) != tc.isUsage {
				t.Errorf("isUsage = %v, want %v", isUsage(err), tc.isUsage)
			}
		})
	}
}

func TestDevOneShotBadOutputFormat(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := execDev(t, "dev", "enhance", "--url", "http://127.0.0.1:1/e", "--secret", "s",
		"-o", "xml", writePayload(t, devPayload))
	if err == nil || !isUsage(err) {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

// execDevCapture runs the CLI under ctx, returning stdout and stderr.
func execDevCapture(ctx context.Context, args ...string) (string, string, error) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	opts := &Options{Out: out, ErrOut: errOut}
	root := NewRoot(opts)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

// TestDevGenericGeneratesSecret: a first run needs no setup. With no secret
// configured the harness mints one, prints it once for the author to sign
// with, and a cancelled run (Ctrl-C) is a clean exit with a summary.
func TestDevGenericGeneratesSecret(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errOut, err := execDevCapture(ctx, "dev", "generic", "--addr", "127.0.0.1:0", "--no-auto-resolve")
	if err != nil {
		t.Fatalf("dev generic: %v\n%s", err, errOut)
	}
	m := regexp.MustCompile(`patchy: generated webhook secret: ([0-9a-f]+)\n`).FindStringSubmatch(errOut)
	if m == nil || len(m[1]) != 64 {
		t.Fatalf("no 32-byte hex secret announced:\n%s", errOut)
	}
	for _, want := range []string{
		"X-Patchy-Signature-256",
		"patchy: 0 deliveries, 0 findings, 0 enhance calls (0 failed), 0 resolve calls (0 failed)",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	// Nothing was retained, so there is nothing to replay.
	if strings.Contains(errOut, "retained without a write-back") {
		t.Errorf("replay hint shown with no findings:\n%s", errOut)
	}
}

func TestDevGenericConfiguredSecretIsNotPrinted(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errOut, err := execDevCapture(ctx, "dev", "generic", "--addr", "127.0.0.1:0", "--secret", "hunter2", "-v")
	if err != nil {
		t.Fatalf("dev generic: %v\n%s", err, errOut)
	}
	if strings.Contains(errOut, "hunter2") || strings.Contains(errOut, "generated webhook secret") {
		t.Errorf("a configured secret was echoed or regenerated:\n%s", errOut)
	}
}

func TestDevGenericRejectsBadOutputFormat(t *testing.T) {
	t.Chdir(t.TempDir())
	_, _, err := execDevCapture(context.Background(), "dev", "generic", "--addr", "127.0.0.1:0",
		"--secret", "s", "-o", "xml")
	if err == nil || !isUsage(err) {
		t.Fatalf("err = %v, want a usage error", err)
	}
}
