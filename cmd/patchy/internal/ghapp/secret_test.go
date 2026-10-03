// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/internal/ghsecret"
)

// TestKeysAreGhsecrets pins this package's copy of the Secret keys to the
// constants the controllers read them by.
func TestKeysAreGhsecrets(t *testing.T) {
	if KeyAppID != ghsecret.KeyAppID || KeyPrivateKey != ghsecret.KeyPrivateKey ||
		KeyWebhookSecret != ghsecret.KeyWebhookSecret {
		t.Error("the Secret keys drifted from internal/ghsecret")
	}
}

// TestSecretManifestReadsAsAForgeCredential: the manifest is a v1 Secret
// with exactly the ghsecret keys, which ghsecret's own validation accepts as
// an App credential, as a Forge or Integration would read it.
func TestSecretManifestReadsAsAForgeCredential(t *testing.T) {
	key := testKeyPEM(t)
	for _, tt := range []struct {
		name     string
		webhook  string
		wantKeys []string
	}{
		{"with a webhook", "whsec", []string{KeyAppID, KeyPrivateKey, KeyWebhookSecret}},
		{"without a webhook", "", []string{KeyAppID, KeyPrivateKey}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{ID: 123456, Slug: "patchy-acme",
				Credentials: newCredentials([]byte(key), tt.webhook)}
			out, err := SecretManifest(app, "patchy-github", "patchy")
			if err != nil {
				t.Fatal(err)
			}
			var s corev1.Secret
			if err := yaml.UnmarshalStrict(out, &s); err != nil {
				t.Fatalf("not a Secret: %v\n%s", err, out)
			}
			if s.APIVersion != "v1" || s.Kind != "Secret" || s.Name != "patchy-github" || s.Namespace != "patchy" ||
				s.Type != corev1.SecretTypeOpaque {
				t.Errorf("secret = %+v", s.ObjectMeta)
			}
			if got := slices.Sorted(maps.Keys(s.Data)); !slices.Equal(got, tt.wantKeys) {
				t.Errorf("keys = %v, want %v", got, tt.wantKeys)
			}
			if string(s.Data[KeyAppID]) != "123456" || string(s.Data[KeyPrivateKey]) != key ||
				string(s.Data[KeyWebhookSecret]) != tt.webhook {
				t.Error("the values did not round-trip")
			}
			s.ResourceVersion = "1"
			if err := ghsecret.NewApps().Validate(&s, "", ""); err != nil {
				t.Errorf("ghsecret refuses the Secret: %v", err)
			}
		})
	}
}

// mode is path's permission bits.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// TestCheckWritable: a new path in an existing directory is writable; an
// existing file only with force; a directory or a missing parent never.
func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patchy-github.secret.yaml")
	if err := CheckWritable(path, false); err != nil {
		t.Fatalf("CheckWritable(new) = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("CheckWritable left %v behind", entries)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckWritable(path, false); !errors.Is(err, ErrExists) {
		t.Errorf("CheckWritable(existing) = %v, want ErrExists", err)
	}
	if err := CheckWritable(path, true); err != nil {
		t.Errorf("CheckWritable(existing, force) = %v", err)
	}
	if err := CheckWritable(filepath.Join(dir, "missing", "x.yaml"), false); err == nil {
		t.Error("CheckWritable accepted a missing directory")
	}
	if err := CheckWritable(dir, true); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("CheckWritable(a directory, force) = %v, want a refusal", err)
	}
}

// TestCheckWritableReadOnlyDirectory: a directory this process cannot
// create a file in is refused, with or without force, as WriteFile would
// refuse it after GitHub had created the App.
func TestCheckWritableReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows ignores a directory's mode bits")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes to a 0555 directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "patchy-github.secret.yaml")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	for _, force := range []bool{false, true} {
		if err := CheckWritable(path, force); err == nil || !strings.Contains(err.Error(), "cannot be written to") {
			t.Errorf("CheckWritable(read-only directory, force=%v) = %v, want a refusal", force, err)
		}
		if err := WriteFile(path, []byte("x"), force); err == nil {
			t.Errorf("WriteFile(read-only directory, force=%v) succeeded: CheckWritable's premise is wrong", force)
		}
	}
}

// TestWriteFile: a new file is 0600; an existing one is refused, untouched,
// without force, and with force replaced by a 0600 file whatever its old
// mode, leaving nothing else behind.
func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patchy-github.secret.yaml")
	if err := WriteFile(path, []byte("one"), false); err != nil {
		t.Fatal(err)
	}
	unix := runtime.GOOS != "windows"
	if unix && mode(t, path) != 0o600 {
		t.Errorf("new file mode %o, want 600", mode(t, path))
	}
	if err := WriteFile(path, []byte("two"), false); !errors.Is(err, ErrExists) {
		t.Errorf("WriteFile(existing) = %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "one" {
		t.Errorf("a refused write changed the file: %q", got)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("three"), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "three" {
		t.Errorf("forced write = %q", got)
	}
	if unix && mode(t, path) != 0o600 {
		t.Errorf("forced file mode %o, want 600", mode(t, path))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only the file", len(entries))
	}
}

// TestWriteFileReplacesSymlink: a symlink at the path is refused without
// force, and with force replaced by a 0600 file rather than followed.
func TestWriteFileReplacesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("x"), false); !errors.Is(err, ErrExists) {
		t.Errorf("WriteFile(symlink) = %v, want ErrExists", err)
	}
	if err := WriteFile(link, []byte("x"), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Errorf("a forced write followed the symlink: target now %q", got)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink != 0 || mode(t, link) != 0o600 {
		t.Errorf("link.yaml is still a symlink or not 0600: %v", info.Mode())
	}
}
