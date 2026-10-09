// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package mirror

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/mirror/scan"
	"github.com/bitwise-media-group/patchy/internal/mirror/spec"
	"github.com/bitwise-media-group/patchy/internal/mirror/verify"
)

// seams carries the injectable collaborators a stage-error test varies.
type seams struct {
	verifyErr  error
	signerErr  error
	signErr    error
	pushErr    error
	tools      scan.ToolRunner
	scanners   []scan.ImageScanner
	signed     []string
	pushed     []string
	signedLock sync.Mutex
}

func (s *seams) Sign(_ context.Context, ref string, _ bool) error {
	s.signedLock.Lock()
	defer s.signedLock.Unlock()
	if s.signErr != nil {
		return s.signErr
	}
	s.signed = append(s.signed, ref)
	return nil
}

// seamEngine rebuilds the fixture engine over the given seams. Nothing in
// it reaches cosign, a scanner binary or the network.
func seamEngine(f *fixture, s *seams) *Engine {
	f.t.Helper()
	global, err := spec.LoadConfig(filepath.Join(f.root, "mirror.yaml"))
	if err != nil {
		f.t.Fatal(err)
	}
	tools := s.tools
	if tools == nil {
		tools = &toolStub{}
	}
	f.eng = New(Config{
		Root:   f.root,
		Global: global,
		Now:    func() time.Time { return testNow },
		Verify: func(context.Context, verify.Subject) error { return s.verifyErr },
		NewSigner: func(*spec.Signing) (ArtifactSigner, error) {
			if s.signerErr != nil {
				return nil, s.signerErr
			}
			return s, nil
		},
		PushChart: func(_ []byte, ref string) (string, error) {
			if s.pushErr != nil {
				return "", s.pushErr
			}
			s.pushed = append(s.pushed, ref)
			return "sha256:" + strings.Repeat("ab", 32), nil
		},
		Tools:         tools,
		ImageScanners: s.scanners,
	})
	return f.eng
}

// toolStub is a ToolRunner whose binaries are absent unless have is set.
type toolStub struct {
	have   bool
	stdout []byte
	err    error
	calls  []string
}

func (t *toolStub) Run(_ context.Context, name string, args, _ []string) ([]byte, error) {
	t.calls = append(t.calls, name+" "+strings.Join(args, " "))
	return t.stdout, t.err
}

func (t *toolStub) Look(string) bool { return t.have }

// erroringScanner fails every scan.
type erroringScanner struct{}

func (erroringScanner) Name() string { return "broken" }
func (erroringScanner) ScanImage(context.Context, string) ([]scan.Finding, error) {
	return nil, errors.New("scanner database unavailable")
}

func TestSyncArtifactErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushImage(f.host+"/bundles/bundle:1.0.0", testNow.AddDate(0, -1, 0))
	f.artifactManifest("example.test/bundles/bundle", "")

	t.Run("no lock", func(t *testing.T) {
		eng := seamEngine(f, &seams{verifyErr: errors.New("unsigned")})
		entry, _ := eng.Entry("bundle")
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "no lock (run upgrade first)")
	})

	s := &seams{verifyErr: errors.New("unsigned")}
	eng := seamEngine(f, s)
	entry, _ := eng.Entry("bundle")
	if _, err := eng.Upgrade(ctx, entry, ""); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	t.Run("unknown registry filter", func(t *testing.T) {
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{Registries: []string{"elsewhere"}})),
			`unknown registry "elsewhere"`)
	})

	t.Run("dry run touches nothing", func(t *testing.T) {
		res, err := eng.Sync(ctx, entry, SyncOptions{DryRun: true})
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if len(res.Records) != 1 || res.Records[0].Action != ActionPushed || res.Records[0].Registry != "primary" {
			t.Errorf("records = %+v", res.Records)
		}
		if _, exists, err := eng.reg.Exists(ctx, res.Records[0].Ref); err != nil || exists {
			t.Errorf("dry run published %s (exists=%v, err=%v)", res.Records[0].Ref, exists, err)
		}
		if len(s.signed) != 0 {
			t.Errorf("dry run signed %v", s.signed)
		}
	})

	t.Run("signer construction fails", func(t *testing.T) {
		eng := seamEngine(f, &seams{verifyErr: errors.New("unsigned"), signerErr: errors.New("no kms access")})
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "bundle: no kms access")
	})

	t.Run("signing fails", func(t *testing.T) {
		eng := seamEngine(f, &seams{verifyErr: errors.New("unsigned"), signErr: errors.New("fulcio down")})
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "bundle: fulcio down")
	})

	t.Run("lock missing the registry", func(t *testing.T) {
		f.setRegistries("  - name: primary\n    url: " + f.host + "/mirror\n" +
			"  - name: second\n    url: " + f.host + "/second\n")
		defer f.setRegistries("  - name: primary\n    url: " + f.host + "/mirror\n")
		eng := seamEngine(f, &seams{verifyErr: errors.New("unsigned")})
		_, err := eng.Sync(ctx, entry, SyncOptions{})
		if err == nil || !strings.Contains(err.Error(), `lock is stale for registry "second"`) {
			t.Errorf("Sync = %v", err)
		}
	})

	t.Run("source vanished upstream", func(t *testing.T) {
		lockPath := "artifacts/bundle/lock.yaml"
		original := f.read(lockPath)
		defer f.write(lockPath, original)
		f.write(lockPath, strings.Replace(original, "example.test/bundles/bundle", "example.test/bundles/gone", 1))
		eng := seamEngine(f, &seams{verifyErr: errors.New("unsigned")})
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "bundle: copy")
	})

	t.Run("unparseable lock source", func(t *testing.T) {
		lockPath := "artifacts/bundle/lock.yaml"
		original := f.read(lockPath)
		defer f.write(lockPath, original)
		// An empty version leaves a source with an empty tag.
		f.write(lockPath, strings.Replace(original, "version: 1.0.0", `version: ""`, 1))
		eng := seamEngine(f, &seams{verifyErr: errors.New("unsigned")})
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "empty tag")
	})
}

func TestSyncChartErrors(t *testing.T) {
	f, entry := scanFixture(t)
	ctx := context.Background()

	t.Run("dry run reports would-push and would-sign", func(t *testing.T) {
		s := &seams{verifyErr: errors.New("unsigned")}
		eng := seamEngine(f, s)
		res, err := eng.Sync(ctx, entry, SyncOptions{DryRun: true})
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if len(res.Records) != 2 {
			t.Fatalf("records = %+v", res.Records)
		}
		chart := res.Records[0]
		if chart.Action != ActionPushed || !chart.Signed || !strings.HasSuffix(chart.Ref, "/mirror/charts/demo:1.0.0") {
			t.Errorf("chart record = %+v", chart)
		}
		if len(s.pushed) != 0 || len(s.signed) != 0 {
			t.Errorf("dry run pushed %v / signed %v", s.pushed, s.signed)
		}
	})

	t.Run("chart push fails", func(t *testing.T) {
		eng := seamEngine(f, &seams{verifyErr: errors.New("unsigned"), pushErr: errors.New("registry read-only")})
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "demo: registry read-only")
	})

	t.Run("chart signature fails", func(t *testing.T) {
		eng := seamEngine(f, &seams{verifyErr: errors.New("unsigned"), signErr: errors.New("no identity token")})
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "demo: no identity token")
	})

	t.Run("upstream tgz mutated", func(t *testing.T) {
		lockPath := "charts/demo/images.lock.yaml"
		original := f.read(lockPath)
		defer f.write(lockPath, original)
		lock, err := spec.LoadImagesLock(filepath.Join(f.root, lockPath))
		if err != nil {
			t.Fatal(err)
		}
		f.write(lockPath, strings.Replace(original, lock.Chart.UpstreamTgzSha256, strings.Repeat("0", 64), 1))
		eng := seamEngine(f, &seams{})
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "upstream tgz digest mismatch")
	})

	t.Run("upstream chart gone", func(t *testing.T) {
		gone := entry
		chart := *entry.Chart
		chart.Chart.Version = "9.9.9"
		gone.Chart = &chart
		eng := seamEngine(f, &seams{})
		wantErrContains(t, errOf(eng.Sync(ctx, gone, SyncOptions{})), "pull chart demo 9.9.9")
	})

	t.Run("no lock", func(t *testing.T) {
		lockPath := "charts/demo/images.lock.yaml"
		original := f.read(lockPath)
		defer f.write(lockPath, original)
		f.write(lockPath, "chart: [broken\n")
		eng := seamEngine(f, &seams{})
		wantErrContains(t, errOf(eng.Sync(ctx, entry, SyncOptions{})), "no lock (run upgrade first)")
	})

	t.Run("image lock missing the registry", func(t *testing.T) {
		lockPath := "charts/demo/images.lock.yaml"
		original := f.read(lockPath)
		defer f.write(lockPath, original)
		f.write(lockPath, strings.Replace(original, "      primary: ", "      retired: ", 1))
		s := &seams{verifyErr: errors.New("unsigned")}
		eng := seamEngine(f, s)
		_, err := eng.Sync(ctx, entry, SyncOptions{})
		if err == nil || !strings.Contains(err.Error(), `lock is stale for registry "primary"`) {
			t.Errorf("Sync = %v", err)
		}
	})
}

func TestScanStageErrors(t *testing.T) {
	f, entry := scanFixture(t)
	ctx := context.Background()

	t.Run("scanner failure aborts the scan", func(t *testing.T) {
		eng := seamEngine(f, &seams{scanners: []scan.ImageScanner{erroringScanner{}}})
		wantErrContains(t, errOf(eng.Scan(ctx, entry)), "demo: scanner database unavailable")
	})

	t.Run("broken allowlist", func(t *testing.T) {
		f.write("charts/demo/security/allowlist.yaml", "vulnerabilities: [oops\n")
		defer f.write("charts/demo/security/allowlist.yaml", "vulnerabilities: []\n")
		eng := seamEngine(f, &seams{scanners: []scan.ImageScanner{&fakeScanner{name: "fake"}}})
		wantErrContains(t, errOf(eng.Scan(ctx, entry)), "parse")
	})

	t.Run("no lock", func(t *testing.T) {
		gone := entry
		gone.Dir = t.TempDir()
		eng := seamEngine(f, &seams{scanners: []scan.ImageScanner{&fakeScanner{name: "fake"}}})
		wantErrContains(t, errOf(eng.Scan(ctx, gone)), "no lock (run upgrade first)")
		if _, err := eng.Validate(ctx, gone, []string{StageScan}); err == nil {
			t.Error("Validate scan stage should surface the scan error")
		}
	})

	tests := []struct {
		name   string
		mode   string
		tools  *toolStub
		want   string
		failed bool
	}{
		{"kubescape absent is a skip", "warn", &toolStub{have: false}, "skipped: kubescape not on PATH", false},
		{"kubescape clean", "warn", &toolStub{have: true, stdout: []byte("ok")}, "clean", false},
		{"kubescape warn mode never fails", "warn", &toolStub{have: true, err: errors.New("exit 1")}, "clean", false},
		{"kubescape fail mode blocks", "fail", &toolStub{have: true, err: errors.New("exit 1")}, "failed", true},
	}
	original := f.read("mirror.yaml")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f.write("mirror.yaml", original+"scan:\n  scanners:\n    kubescape: {enabled: true, mode: "+tc.mode+"}\n")
			defer f.write("mirror.yaml", original)
			eng := seamEngine(f, &seams{tools: tc.tools, scanners: []scan.ImageScanner{&fakeScanner{name: "fake"}}})
			report, err := eng.Scan(ctx, entry)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if report.ConfigScan != tc.want || report.ConfigScanFailed != tc.failed || report.Failed() != tc.failed {
				t.Errorf("report = %+v", report)
			}
			if tc.tools.have && (len(tc.tools.calls) != 1 || !strings.HasPrefix(tc.tools.calls[0], "kubescape scan ")) {
				t.Errorf("kubescape calls = %v", tc.tools.calls)
			}
		})
	}

	t.Run("roster built from config", func(t *testing.T) {
		f.write("mirror.yaml", original+"scan:\n  scanners:\n    osv: {enabled: true}\n"+
			"    grype: {enabled: true}\n    kubescape: {enabled: false}\n")
		defer f.write("mirror.yaml", original)
		eng := seamEngine(f, &seams{tools: &toolStub{have: false}})
		roster := eng.scanners()
		names := make([]string, 0, len(roster))
		for _, s := range roster {
			names = append(names, s.Name())
		}
		if strings.Join(names, ",") != "osv,grype" {
			t.Errorf("roster = %v", names)
		}
		// Both configured scanners are absent from the stub PATH.
		wantErrContains(t, errOf(eng.Scan(ctx, entry)), "not on PATH")
	})
}

func TestDeriveAllowlistGuards(t *testing.T) {
	f, entry := scanFixture(t)
	ctx := context.Background()
	optIn := entry
	chart := *entry.Chart
	chart.Scan = &spec.ScanOverride{Allowlist: spec.AllowlistPolicy{Generate: true}}
	optIn.Chart = &chart

	t.Run("artifact entries never derive", func(t *testing.T) {
		eng := seamEngine(f, &seams{})
		res, err := eng.DeriveAllowlist(ctx, spec.Entry{Kind: spec.KindArtifact})
		if err != nil || res.Generated {
			t.Errorf("res = %+v, err = %v", res, err)
		}
	})

	t.Run("scanning disabled skips derivation", func(t *testing.T) {
		original := f.read("mirror.yaml")
		f.write("mirror.yaml", original+"scan:\n  enabled: false\n")
		defer f.write("mirror.yaml", original)
		eng := seamEngine(f, &seams{scanners: []scan.ImageScanner{erroringScanner{}}})
		res, err := eng.DeriveAllowlist(ctx, optIn)
		if err != nil || res.Generated {
			t.Errorf("res = %+v, err = %v", res, err)
		}
	})

	t.Run("scanner failure", func(t *testing.T) {
		eng := seamEngine(f, &seams{scanners: []scan.ImageScanner{erroringScanner{}}})
		wantErrContains(t, errOf(eng.DeriveAllowlist(ctx, optIn)), "scanner database unavailable")
	})

	t.Run("no lock", func(t *testing.T) {
		gone := optIn
		gone.Dir = t.TempDir()
		eng := seamEngine(f, &seams{scanners: []scan.ImageScanner{&fakeScanner{name: "fake"}}})
		wantErrContains(t, errOf(eng.DeriveAllowlist(ctx, gone)), "no lock")
	})

	t.Run("broken previous allowlist", func(t *testing.T) {
		f.write("charts/demo/security/allowlist.yaml", "vulnerabilities: [oops\n")
		defer f.write("charts/demo/security/allowlist.yaml", "vulnerabilities: []\n")
		eng := seamEngine(f, &seams{scanners: []scan.ImageScanner{&fakeScanner{name: "fake"}}})
		wantErrContains(t, errOf(eng.DeriveAllowlist(ctx, optIn)), "parse")
	})
}

func TestScanArtifactAutoErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushImage(f.host+"/bundles/bundle:1.0.0", testNow.AddDate(0, -1, 0))
	f.artifactManifest("example.test/bundles/bundle", "")
	eng := seamEngine(f, &seams{scanners: []scan.ImageScanner{&fakeScanner{name: "fake"}}})
	entry, _ := eng.Entry("bundle")
	if _, err := eng.Upgrade(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}

	// The lock now points at a digest the registry does not hold, so the
	// auto-mode runnable probe cannot read its config.
	lockPath := "artifacts/bundle/lock.yaml"
	original := f.read(lockPath)
	lock, err := spec.LoadArtifactLock(filepath.Join(f.root, lockPath))
	if err != nil {
		t.Fatal(err)
	}
	f.write(lockPath, strings.Replace(original, lock.Artifact.Digest, "sha256:"+strings.Repeat("0", 64), 1))
	wantErrContains(t, errOf(eng.Scan(ctx, entry)), "bundle:")
	f.write(lockPath, "artifact: [broken\n")
	wantErrContains(t, errOf(eng.Scan(ctx, entry)), "no lock")
}
