// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scan

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/bitwise-media-group/patchy/internal/mirror/ocireg"
	"github.com/bitwise-media-group/patchy/internal/mirror/spec"
)

// fakeBin writes an executable shell script named name into a fresh dir
// and points PATH at that dir alone, so no real scanner can ever run.
func fakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if name != "" {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestExecRunnerRun(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		env        []string
		wantStdout string
		wantErr    string
	}{
		{
			name:       "success echoes args and env",
			script:     `echo "args=$*"; echo "flavour=$FLAVOUR"`,
			env:        []string{"FLAVOUR=mint"},
			wantStdout: "args=-o json\nflavour=mint\n",
		},
		{
			// grype gating: non-zero exit with a report keeps both.
			name:       "failure with stdout returns both",
			script:     `echo '{"matches":[]}'; exit 2`,
			wantStdout: "{\"matches\":[]}\n",
			wantErr:    "exit status 2",
		},
		{
			name:    "failure without stdout carries stderr",
			script:  `echo "database stale" >&2; exit 3`,
			wantErr: "database stale",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin(t, "faketool", tc.script)
			out, err := ExecRunner{}.Run(context.Background(), "faketool", []string{"-o", "json"}, tc.env)
			if string(out) != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", out, tc.wantStdout)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %v does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestExecRunnerLook(t *testing.T) {
	fakeBin(t, "present-tool", "exit 0\n")
	r := ExecRunner{}
	if !r.Look("present-tool") {
		t.Error("Look(present-tool) = false, want true")
	}
	if r.Look("absent-tool") {
		t.Error("Look(absent-tool) = true, want false")
	}
}

func TestScannerNames(t *testing.T) {
	scanners := []ImageScanner{&Grype{}, &OSV{}}
	want := []string{"grype", "osv"}
	for i, s := range scanners {
		if s.Name() != want[i] {
			t.Errorf("Name() = %q, want %q", s.Name(), want[i])
		}
	}
}

func TestSeverityIn(t *testing.T) {
	tests := []struct {
		severity string
		list     []string
		want     bool
	}{
		{"HIGH", []string{"critical", "High"}, true},
		{"CRITICAL", []string{" critical "}, true},
		{"LOW", []string{"CRITICAL", "HIGH"}, false},
		{"UNKNOWN", []string{"bogus"}, true}, // unknown spellings normalise to UNKNOWN
		{"HIGH", nil, false},
	}
	for _, tc := range tests {
		if got := SeverityIn(tc.severity, tc.list); got != tc.want {
			t.Errorf("SeverityIn(%q, %q) = %v, want %v", tc.severity, tc.list, got, tc.want)
		}
	}
}

func TestGrypeErrors(t *testing.T) {
	t.Run("run failure without report", func(t *testing.T) {
		g := &Grype{Runner: &cannedRunner{have: true, err: context.DeadlineExceeded}}
		_, err := g.ScanImage(context.Background(), "example.test/app:1")
		if err == nil || !strings.Contains(err.Error(), "grype scan of example.test/app:1") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unparseable report", func(t *testing.T) {
		g := &Grype{Runner: &cannedRunner{have: true, stdout: []byte("not json")}}
		_, err := g.ScanImage(context.Background(), "example.test/app:1")
		if err == nil || !strings.Contains(err.Error(), "parse grype output") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("gating exit with report is not an error", func(t *testing.T) {
		r := &cannedRunner{have: true, stdout: []byte(grypeJSON), err: context.Canceled}
		findings, err := (&Grype{Runner: r}).ScanImage(context.Background(), "example.test/app:1")
		if err != nil || len(findings) != 2 {
			t.Errorf("findings = %v, err = %v", findings, err)
		}
		if strings.Join(r.args, " ") != "--quiet -o json registry:example.test/app:1" {
			t.Errorf("args = %q", r.args)
		}
	})
	t.Run("nil runner uses PATH", func(t *testing.T) {
		fakeBin(t, "", "")
		_, err := (&Grype{}).ScanImage(context.Background(), "example.test/app:1")
		if err == nil || !strings.Contains(err.Error(), "grype is enabled but not on PATH") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("nil runner runs fake grype on PATH", func(t *testing.T) {
		fakeBin(t, "grype", "printf '%s\\n' '"+grypeJSON+"'\n")
		findings, err := (&Grype{}).ScanImage(context.Background(), "example.test/app:1")
		if err != nil || len(findings) != 2 || findings[0].Scanner != "grype" {
			t.Errorf("findings = %+v, err = %v", findings, err)
		}
	})
	t.Run("related ID equal to primary is not an alias", func(t *testing.T) {
		const out = `{"matches":[{"vulnerability":{"id":"CVE-1","severity":"high"},
			"relatedVulnerabilities":[{"id":"CVE-1"},{"id":""},{"id":"GHSA-x"}],
			"artifact":{"name":"p","version":"1"}}]}`
		findings, err := (&Grype{Runner: &cannedRunner{have: true, stdout: []byte(out)}}).
			ScanImage(context.Background(), "r")
		if err != nil || len(findings) != 1 {
			t.Fatalf("findings = %v, err = %v", findings, err)
		}
		if !reflect.DeepEqual(findings[0].Aliases, []string{"GHSA-x"}) || findings[0].Fixed() {
			t.Errorf("finding = %+v", findings[0])
		}
	})
}

func TestOSVErrors(t *testing.T) {
	t.Run("image cannot be pulled", func(t *testing.T) {
		host := strings.SplitN(pushTestImage(t), "/", 2)[0]
		r := &cannedRunner{have: true}
		o := &OSV{Runner: r, Registry: ocireg.New(authn.NewMultiKeychain())}
		_, err := o.ScanImage(context.Background(), host+"/absent/app:1")
		if err == nil || !strings.Contains(err.Error(), "pull") {
			t.Errorf("err = %v", err)
		}
		if r.args != nil {
			t.Errorf("scanner must not run without an archive, got args %q", r.args)
		}
	})
	t.Run("unparseable report", func(t *testing.T) {
		ref := pushTestImage(t)
		o := &OSV{Runner: &cannedRunner{have: true, stdout: []byte("{broken")}}
		_, err := o.ScanImage(context.Background(), ref)
		if err == nil || !strings.Contains(err.Error(), "parse osv-scanner output") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("nil runner uses PATH", func(t *testing.T) {
		fakeBin(t, "", "")
		_, err := (&OSV{}).ScanImage(context.Background(), "example.test/app:1")
		if err == nil || !strings.Contains(err.Error(), "osv-scanner is enabled but not on PATH") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("fake osv-scanner exit 1 keeps findings", func(t *testing.T) {
		ref := pushTestImage(t)
		// The fake checks it received a real archive before reporting.
		fakeBin(t, "osv-scanner", `for a; do last=$a; done
[ -s "$last" ] || { echo "no archive" >&2; exit 127; }
printf '%s\n' '`+osvVulnJSON+`'
exit 1
`)
		findings, err := (&OSV{Registry: ocireg.New(authn.NewMultiKeychain())}).ScanImage(context.Background(), ref)
		if err != nil || len(findings) != 1 || findings[0].ID != "CVE-2026-1" {
			t.Errorf("findings = %+v, err = %v", findings, err)
		}
	})
}

func TestKubescapeFailModeArgs(t *testing.T) {
	r := &cannedRunner{have: true, stdout: []byte("ok")}
	res, err := Kubescape(context.Background(), r, "rendered.yaml", "fail")
	if err != nil || res.Failed || string(res.Output) != "ok" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	want := "scan rendered.yaml --logger warning --severity-threshold low"
	if strings.Join(r.args, " ") != want {
		t.Errorf("args = %q, want %q", strings.Join(r.args, " "), want)
	}
	if len(r.envs) != 1 || !strings.HasPrefix(r.envs[0], "KUBECONFIG=") ||
		!strings.HasSuffix(r.envs[0], "patchy-mirror-empty-kubeconfig") {
		t.Errorf("env = %q, want an isolated empty KUBECONFIG", r.envs)
	}

	fakeBin(t, "", "")
	res, err = Kubescape(context.Background(), nil, "rendered.yaml", "fail")
	if err != nil || res.Skipped == "" {
		t.Errorf("nil runner without kubescape on PATH: res = %+v, err = %v", res, err)
	}
}

// findingSet generates findings and an allowlist drawn from a small shared
// ID pool, so matches (by ID, by alias, by case) actually occur.
type findingSet struct {
	findings []Finding
	allow    *spec.Allowlist
}

func (findingSet) Generate(r *rand.Rand, size int) reflect.Value {
	pool := []string{"CVE-2026-1", "cve-2026-2", "GHSA-aaaa", "ghsa-BBBB", "GO-2026-3", ""}
	pick := func() string { return pool[r.Intn(len(pool))] }
	var fs findingSet
	for i := range r.Intn(size + 1) {
		f := Finding{ID: pick(), Package: "pkg" + strconv.Itoa(i)}
		for range r.Intn(3) {
			f.Aliases = append(f.Aliases, pick())
		}
		fs.findings = append(fs.findings, f)
	}
	if r.Intn(5) > 0 {
		fs.allow = &spec.Allowlist{}
		for range r.Intn(4) {
			id := pick()
			if r.Intn(2) == 0 {
				id = strings.ToLower(id)
			} else {
				id = strings.ToUpper(id)
			}
			fs.allow.Vulnerabilities = append(fs.allow.Vulnerabilities, spec.AllowlistEntry{ID: id})
		}
	}
	return reflect.ValueOf(fs)
}

// TestSuppressPartitionProperty: Suppress is a stable partition — every
// finding lands in exactly one side, in input order, and a finding is
// suppressed exactly when its ID or an alias case-insensitively equals a
// non-empty allowlist ID.
func TestSuppressPartitionProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 1000, Rand: rand.New(rand.NewSource(20261009))}
	prop := func(fs findingSet) bool {
		kept, suppressed := Suppress(fs.findings, fs.allow)
		if len(kept)+len(suppressed) != len(fs.findings) {
			return false
		}
		allowed := map[string]bool{}
		if fs.allow != nil {
			for _, e := range fs.allow.Vulnerabilities {
				if e.ID != "" {
					allowed[strings.ToUpper(e.ID)] = true
				}
			}
		}
		isAllowed := func(f Finding) bool {
			return slices.ContainsFunc(append([]string{f.ID}, f.Aliases...), func(id string) bool {
				return allowed[strings.ToUpper(id)]
			})
		}
		var k, s int
		for _, f := range fs.findings {
			if isAllowed(f) {
				if s >= len(suppressed) || suppressed[s].Package != f.Package {
					return false
				}
				s++
			} else {
				if k >= len(kept) || kept[k].Package != f.Package {
					return false
				}
				k++
			}
		}
		return k == len(kept) && s == len(suppressed)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestSeverityProperties: NormalizeSeverity is idempotent and always lands
// on the ladder; SeverityFromScore is monotone in the score.
func TestSeverityProperties(t *testing.T) {
	cfg := &quick.Config{MaxCount: 1000, Rand: rand.New(rand.NewSource(20261009))}
	rank := func(s string) int { return slices.Index(severityOrder, s) }

	idempotent := func(s string) bool {
		n := NormalizeSeverity(s)
		return rank(n) >= 0 && NormalizeSeverity(n) == n
	}
	if err := quick.Check(idempotent, cfg); err != nil {
		t.Error(err)
	}

	monotone := func(a, b uint16) bool {
		x, y := float64(a%101)/10, float64(b%101)/10
		if x > y {
			x, y = y, x
		}
		sx := SeverityFromScore(strconv.FormatFloat(x, 'f', 1, 64))
		sy := SeverityFromScore(strconv.FormatFloat(y, 'f', 1, 64))
		// Higher score never maps to a less severe rung (lower index =
		// more severe).
		return rank(sy) <= rank(sx)
	}
	if err := quick.Check(monotone, cfg); err != nil {
		t.Error(err)
	}
}
