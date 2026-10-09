// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package spec

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

const header = "apiVersion: " + APIVersion + "\n"

// writeFile writes content at dir/rel, creating parents.
func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEntryAccessorsByKind(t *testing.T) {
	chart := Entry{
		Name: "c", Kind: KindChart, Dir: "/store/charts/c",
		Chart: &ChartManifest{Chart: ChartSpec{Lockstep: "otel", Version: "1.0.0", VersionConstraint: ">=1"}},
	}
	artifact := Entry{
		Name: "a", Kind: KindArtifact, Dir: "/store/artifacts/a",
		Artifact: &ArtifactManifest{Artifact: ArtifactSpec{Lockstep: "bundle", Version: "0.5.0", VersionConstraint: "<1"}},
	}
	tests := []struct {
		e                                      Entry
		lockstep, version, constraint, mf, lck string
	}{
		{chart, "otel", "1.0.0", ">=1", "/store/charts/c/manifest.yaml", "/store/charts/c/images.lock.yaml"},
		{artifact, "bundle", "0.5.0", "<1", "/store/artifacts/a/manifest.yaml", "/store/artifacts/a/lock.yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.e.Kind, func(t *testing.T) {
			if got := tc.e.Lockstep(); got != tc.lockstep {
				t.Errorf("Lockstep = %q, want %q", got, tc.lockstep)
			}
			if got := tc.e.Version(); got != tc.version {
				t.Errorf("Version = %q, want %q", got, tc.version)
			}
			if got := tc.e.VersionConstraint(); got != tc.constraint {
				t.Errorf("VersionConstraint = %q, want %q", got, tc.constraint)
			}
			if got := tc.e.ManifestPath(); got != filepath.FromSlash(tc.mf) {
				t.Errorf("ManifestPath = %q, want %q", got, tc.mf)
			}
			if got := tc.e.LockPath(); got != filepath.FromSlash(tc.lck) {
				t.Errorf("LockPath = %q, want %q", got, tc.lck)
			}
		})
	}
}

func TestEffectiveDefaults(t *testing.T) {
	zero := 0
	five := 5
	if got := (Update{}).EffectiveCooldownDays(); got != defaultCooldownDays {
		t.Errorf("default cooldown = %d", got)
	}
	if got := (Update{CooldownDays: &zero}).EffectiveCooldownDays(); got != 0 {
		t.Errorf("explicit 0 cooldown must be honoured, got %d", got)
	}
	if got := (Update{CooldownDays: &five}).EffectiveCooldownDays(); got != 5 {
		t.Errorf("cooldown = %d", got)
	}
	if got := (Scan{FailOn: []string{"LOW"}}).EffectiveFailOn(); !reflect.DeepEqual(got, []string{"LOW"}) {
		t.Errorf("FailOn override = %v", got)
	}
	if got := (Discovery{}).EffectiveNamespace("entry"); got != "entry" {
		t.Errorf("namespace default = %q", got)
	}
	if got := (Discovery{Namespace: "ns"}).EffectiveNamespace("entry"); got != "ns" {
		t.Errorf("namespace = %q", got)
	}
	if got := (Discovery{}).EffectiveKubeVersion(); got != "1.34.0" {
		t.Errorf("kube default = %q", got)
	}
	if got := (Discovery{KubeVersion: "1.30.0"}).EffectiveKubeVersion(); got != "1.30.0" {
		t.Errorf("kube = %q", got)
	}
	if got := (VerifyRule{}).EffectiveProvider(); got != "none" {
		t.Errorf("provider default = %q", got)
	}
	if got := (VerifyRule{Provider: "cosign-key"}).EffectiveProvider(); got != "cosign-key" {
		t.Errorf("provider = %q", got)
	}
	if got := (ArtifactScan{Enabled: "false"}).EffectiveEnabled(); got != "false" {
		t.Errorf("artifact scan enabled = %q", got)
	}
	if got := (ArtifactScan{}).EffectiveEnabled(); got != "auto" {
		t.Errorf("artifact scan default = %q", got)
	}
}

func TestValidateSigning(t *testing.T) {
	tests := []struct {
		s       Signing
		wantErr string
	}{
		{Signing{Provider: "keyless"}, ""},
		{Signing{Provider: "kms", KMS: &KMSSigning{Key: "awskms:///alias/x"}}, ""},
		{Signing{Provider: "kms", KMS: &KMSSigning{}}, "requires kms.key"},
		{Signing{Provider: "kms"}, "requires kms.key"},
		{Signing{}, "provider is required"},
		{Signing{Provider: "pgp"}, `unknown provider "pgp"`},
	}
	for _, tc := range tests {
		err := validateSigning(&tc.s)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%+v: unexpected %v", tc.s, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%+v: error %v does not contain %q", tc.s, err, tc.wantErr)
		}
	}
}

func TestLoadConfigMoreRejects(t *testing.T) {
	registries := "registries: [{name: r, url: r.example.com/x}]\n"
	tests := []struct {
		name, content, wantErr string
	}{
		{"unknown signing provider", header + "kind: MirrorConfig\n" + registries + "signing: {provider: gpg}\n",
			`unknown provider "gpg"`},
		{"two documents", header + "kind: MirrorConfig\n" + registries + "---\nfoo: bar\n", "single YAML document"},
		{"not yaml", "registries: [unclosed\n", "parse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "mirror.yaml", tc.content)
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %v does not contain %q", err, tc.wantErr)
			}
		})
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml")); err == nil ||
		!strings.Contains(err.Error(), "read") {
		t.Errorf("missing config: %v", err)
	}
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	path := writeFile(t, t.TempDir(), "mirror.yaml",
		header+"kind: MirrorConfig\nregistries: [{name: r, url: r.example.com/x}]\n")
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	r := c.Registries[0]
	if r.ChartNamespace != "charts" || r.ImageNamespace != "images" || r.ArtifactNamespace != "artifacts" {
		t.Errorf("namespace defaults = %+v", r)
	}
	if c.Signing.Provider != "keyless" {
		t.Errorf("signing default = %q", c.Signing.Provider)
	}
}

func TestFindRootErrors(t *testing.T) {
	dir := t.TempDir()
	// A directory named mirror.yaml is not a config file.
	if err := os.Mkdir(filepath.Join(dir, ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// The temp dir's ancestors are not expected to hold a mirror.yaml; if
	// one did, FindRoot would legitimately find it, so assert the
	// directory-named-config is skipped rather than returned.
	root, err := FindRoot(nested)
	if err == nil {
		if root == dir {
			t.Errorf("FindRoot returned a directory named %s as a root", ConfigFile)
		}
		return
	}
	if !strings.Contains(err.Error(), ErrNoRoot.Error()) {
		t.Errorf("error = %v, want ErrNoRoot", err)
	}
}

func TestLoadManifestErrors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		load    func(string) error
		content string
		wantErr string
	}{
		{
			name:    "chart: legacy per-entry signing gets a hint",
			load:    func(p string) error { _, err := LoadChartManifest(p); return err },
			content: header + "kind: Chart\nname: demo\nsigning: {provider: keyless}\n",
			wantErr: "per-entry signing was replaced",
		},
		{
			name:    "artifact: legacy per-entry signing gets a hint",
			load:    func(p string) error { _, err := LoadArtifactManifest(p); return err },
			content: header + "kind: Artifact\nname: b\nsigning: {provider: keyless}\n",
			wantErr: "per-entry signing was replaced",
		},
		{
			name:    "chart: other unknown field has no hint",
			load:    func(p string) error { _, err := LoadChartManifest(p); return err },
			content: header + "kind: Chart\nname: demo\nbogus: 1\n",
			wantErr: "field bogus not found",
		},
		{
			name:    "chart: wrong kind",
			load:    func(p string) error { _, err := LoadChartManifest(p); return err },
			content: header + "kind: Artifact\nname: demo\n",
			wantErr: `kind "Artifact" is not Chart`,
		},
		{
			name:    "artifact: wrong kind",
			load:    func(p string) error { _, err := LoadArtifactManifest(p); return err },
			content: header + "kind: Chart\nname: b\n",
			wantErr: `kind "Chart" is not Artifact`,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, dir, filepath.Join(string(rune('a'+i)), "manifest.yaml"), tc.content)
			err := tc.load(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %v does not contain %q", err, tc.wantErr)
			}
			if strings.Contains(tc.wantErr, "bogus") && err != nil && strings.Contains(err.Error(), "replaced") {
				t.Errorf("unrelated field must not get the signing hint: %v", err)
			}
		})
	}
	for _, load := range []func(string) error{
		func(p string) error { _, err := LoadChartManifest(p); return err },
		func(p string) error { _, err := LoadArtifactManifest(p); return err },
	} {
		if err := load(filepath.Join(dir, "absent.yaml")); err == nil || !strings.Contains(err.Error(), "read") {
			t.Errorf("missing manifest: %v", err)
		}
	}
}

func TestDiscoverErrors(t *testing.T) {
	t.Run("artifact name mismatch", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "artifacts/bundle/manifest.yaml",
			header+"kind: Artifact\nname: other\nartifact: {ref: ghcr.io/x/y, version: \"1\"}\n")
		if _, err := Discover(dir); err == nil || !strings.Contains(err.Error(), "match its directory") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("name claimed by chart and artifact", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "charts/dup/manifest.yaml",
			header+"kind: Chart\nname: dup\nchart: {repo: oci://x/y, name: dup, version: \"1\"}\n")
		writeFile(t, dir, "artifacts/dup/manifest.yaml",
			header+"kind: Artifact\nname: dup\nartifact: {ref: ghcr.io/x/y, version: \"1\"}\n")
		if _, err := Discover(dir); err == nil || !strings.Contains(err.Error(), `entry name "dup" is claimed by both`) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("invalid chart manifest", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "charts/bad/manifest.yaml", header+"kind: Chart\nname: bad\nnope: 1\n")
		if _, err := Discover(dir); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("invalid artifact manifest", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "artifacts/bad/manifest.yaml", "kind: Artifact\n")
		if _, err := Discover(dir); err == nil || !strings.Contains(err.Error(), "apiVersion") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown kind", func(t *testing.T) {
		if _, err := loadEntry("/x/y/manifest.yaml", "Widget"); err == nil ||
			!strings.Contains(err.Error(), `unknown entry kind "Widget"`) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("empty store", func(t *testing.T) {
		entries, err := Discover(t.TempDir())
		if err != nil || len(entries) != 0 {
			t.Errorf("Discover(empty) = %v, %v", entries, err)
		}
	})
}

func TestLoadEntryArtifact(t *testing.T) {
	e, err := LoadEntry("testdata/store", "bundle")
	if err != nil {
		t.Fatalf("LoadEntry: %v", err)
	}
	if e.Kind != KindArtifact || e.Artifact == nil || e.Chart != nil || e.Version() != "0.5.0" {
		t.Errorf("entry = %+v", e)
	}
}

func TestLockLoadErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.yaml")
	if _, err := LoadImagesLock(missing); err == nil || !strings.Contains(err.Error(), "read") {
		t.Errorf("LoadImagesLock missing: %v", err)
	}
	if _, err := LoadArtifactLock(missing); err == nil || !strings.Contains(err.Error(), "read") {
		t.Errorf("LoadArtifactLock missing: %v", err)
	}
	bad := writeFile(t, dir, "bad.yaml", "chart: {name: x}\nextra: 1\n")
	if _, err := LoadImagesLock(bad); err == nil || strings.Contains(err.Error(), "predates") {
		t.Errorf("LoadImagesLock unknown field: %v (no legacy hint expected)", err)
	}
	if _, err := LoadArtifactLock(bad); err == nil {
		t.Error("LoadArtifactLock unknown field: want error")
	}
}

func TestSidecarAndAllowlistErrors(t *testing.T) {
	t.Run("sidecar unknown field", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, SidecarFile, "extra: []\nbogus: 1\n")
		if _, err := LoadSidecar(dir); err == nil || !strings.Contains(err.Error(), "parse") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("sidecar unreadable", func(t *testing.T) {
		dir := t.TempDir()
		// A directory where the file should be is not "missing".
		if err := os.Mkdir(filepath.Join(dir, SidecarFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSidecar(dir); err == nil || !strings.Contains(err.Error(), "read") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("allowlist unknown field", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, AllowlistFile, "vulnerabilities: []\nbogus: 1\n")
		if _, err := LoadAllowlist(dir); err == nil || !strings.Contains(err.Error(), "parse") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("allowlist unreadable", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, AllowlistFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAllowlist(dir); err == nil || !strings.Contains(err.Error(), "read") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing allowlist is empty", func(t *testing.T) {
		a, err := LoadAllowlist(t.TempDir())
		if err != nil || len(a.Vulnerabilities) != 0 {
			t.Errorf("a = %+v, err = %v", a, err)
		}
	})
}

func TestSidecarEncodeEmpty(t *testing.T) {
	got := string((&Sidecar{}).Encode())
	if !strings.HasPrefix(got, "# GENERATED by patchy mirror") || !strings.HasSuffix(got, "extra: []\n") {
		t.Errorf("empty sidecar = %q", got)
	}
	dir := t.TempDir()
	writeFile(t, dir, SidecarFile, got)
	s, err := LoadSidecar(dir)
	if err != nil || len(s.Extra) != 0 {
		t.Errorf("reload = %+v, %v", s, err)
	}
}

// token draws YAML-plain-safe tokens (the shapes lock and sidecar fields
// hold: names, versions, references, digests).
func token(r *rand.Rand) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	parts := []string{"ghcr.io/org/app", "registry.example.com/x", "v1.2.3", "sha256:" + strings.Repeat("0f", 32)}
	if r.Intn(2) == 0 {
		return parts[r.Intn(len(parts))]
	}
	n := 1 + r.Intn(10)
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

type genImagesLock struct{ ImagesLock }

func (genImagesLock) Generate(r *rand.Rand, size int) reflect.Value {
	l := ImagesLock{Chart: LockChart{Name: token(r), Version: token(r), UpstreamTgzSha256: token(r)}}
	for range r.Intn(size%6 + 1) {
		img := LockImage{Source: token(r), Digest: token(r), Targets: map[string]string{}}
		for range 1 + r.Intn(3) {
			img.Targets["reg"+token(r)] = token(r)
		}
		for range r.Intn(3) {
			img.Platforms = append(img.Platforms, "linux/"+token(r))
		}
		l.Images = append(l.Images, img)
	}
	return reflect.ValueOf(genImagesLock{l})
}

type genArtifactLock struct{ ArtifactLock }

func (genArtifactLock) Generate(r *rand.Rand, _ int) reflect.Value {
	l := ArtifactLock{Artifact: LockArtifact{
		Ref: token(r), Version: token(r), Digest: token(r), Targets: map[string]string{},
	}}
	for range 1 + r.Intn(3) {
		l.Artifact.Targets["reg"+token(r)] = token(r)
	}
	for range r.Intn(3) {
		l.Artifact.Platforms = append(l.Artifact.Platforms, "linux/"+token(r))
	}
	return reflect.ValueOf(genArtifactLock{l})
}

type genSidecar struct{ Sidecar }

func (genSidecar) Generate(r *rand.Rand, size int) reflect.Value {
	var s Sidecar
	for range r.Intn(size%5 + 1) {
		s.Extra = append(s.Extra, ExtraImage{Image: token(r), Reason: token(r)})
	}
	return reflect.ValueOf(genSidecar{s})
}

// normalizeLock maps empty slices to nil so decode(encode(x)) compares
// equal to x regardless of how the generator built empty lists.
func normalizeLock(l *ImagesLock) {
	if len(l.Images) == 0 {
		l.Images = nil
	}
	for i := range l.Images {
		if len(l.Images[i].Platforms) == 0 {
			l.Images[i].Platforms = nil
		}
	}
}

// TestEncodeRoundTripProperties: the hand-rolled canonical encoders
// produce YAML that the strict loaders decode back to the same value, and
// re-encoding is byte-stable.
func TestEncodeRoundTripProperties(t *testing.T) {
	cfg := &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261009))}
	dir := t.TempDir()

	imagesProp := func(g genImagesLock) bool {
		want := g.ImagesLock
		path := writeFile(t, dir, "images.lock.yaml", string(want.Encode()))
		got, err := LoadImagesLock(path)
		if err != nil {
			t.Logf("decode: %v\n%s", err, want.Encode())
			return false
		}
		normalizeLock(&want)
		normalizeLock(got)
		return reflect.DeepEqual(*got, want) && string(got.Encode()) == string(want.Encode())
	}
	if err := quick.Check(imagesProp, cfg); err != nil {
		t.Error(err)
	}

	artifactProp := func(g genArtifactLock) bool {
		want := g.ArtifactLock
		path := writeFile(t, dir, "lock.yaml", string(want.Encode()))
		got, err := LoadArtifactLock(path)
		if err != nil {
			t.Logf("decode: %v", err)
			return false
		}
		if len(want.Artifact.Platforms) == 0 {
			want.Artifact.Platforms = nil
		}
		if len(got.Artifact.Platforms) == 0 {
			got.Artifact.Platforms = nil
		}
		return reflect.DeepEqual(*got, want) && string(got.Encode()) == string(want.Encode())
	}
	if err := quick.Check(artifactProp, cfg); err != nil {
		t.Error(err)
	}

	sidecarProp := func(g genSidecar) bool {
		want := g.Sidecar
		sdir := t.TempDir()
		writeFile(t, sdir, SidecarFile, string(want.Encode()))
		got, err := LoadSidecar(sdir)
		if err != nil {
			t.Logf("decode: %v", err)
			return false
		}
		if len(want.Extra) == 0 {
			want.Extra = nil
		}
		if len(got.Extra) == 0 {
			got.Extra = nil
		}
		return reflect.DeepEqual(*got, want)
	}
	if err := quick.Check(sidecarProp, cfg); err != nil {
		t.Error(err)
	}
}
