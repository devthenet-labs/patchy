// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"sigs.k8s.io/yaml"
)

// The Secret keys the Forge and Integration resources read. They repeat
// internal/ghsecret's constants, which this package cannot import without
// linking a GitHub client into the CLI; a test pins them equal.
const (
	KeyAppID         = "appID"
	KeyPrivateKey    = "privateKey"
	KeyWebhookSecret = "webhookSecret"
)

// DefaultSecretName is the Secret the docs and the chart's examples name.
const DefaultSecretName = "patchy-github"

// secretManifest is a v1 Secret, without the empty fields a corev1.Secret
// would add.
type secretManifest struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   secretMetadata    `json:"metadata"`
	Type       string            `json:"type"`
	Data       map[string][]byte `json:"data"`
}

type secretMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// SecretManifest renders the Secret name in namespace holding app's
// credentials under the ghsecret keys: appID and privateKey, and
// webhookSecret when GitHub issued one (an App with a webhook). The values
// are base64 under data, as `kubectl create secret` writes them.
func SecretManifest(app *App, name, namespace string) ([]byte, error) {
	data := map[string][]byte{
		KeyAppID:      []byte(strconv.FormatInt(app.ID, 10)),
		KeyPrivateKey: app.Credentials.privateKey,
	}
	if app.Credentials.HasWebhookSecret() {
		data[KeyWebhookSecret] = []byte(app.Credentials.webhookSecret)
	}
	out, err := yaml.Marshal(secretManifest{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata:   secretMetadata{Name: name, Namespace: namespace},
		Type:       "Opaque",
		Data:       data,
	})
	if err != nil {
		return nil, fmt.Errorf("render the Secret: %w", err)
	}
	return out, nil
}

// ErrExists reports an output file that is already there.
var ErrExists = errors.New("the file exists")

// CheckWritable reports, before anything is created, a path WriteFile would
// refuse: one that exists, unless force, or whose directory is missing. The
// flow checks it first, so a GitHub App is never created only for its
// credentials to have nowhere to go.
func CheckWritable(path string, force bool) error {
	if _, err := os.Lstat(path); err == nil && !force {
		return fmt.Errorf("%s: %w; pass --force to replace it", path, ErrExists)
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("the directory for %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the directory for %s: %s is not a directory", path, dir)
	}
	return nil
}

// WriteFile writes data to path with mode 0600 (owner read and write; on
// Windows the mode is not enforced). It refuses an existing path unless
// force, and then replaces it through a new file renamed over it, so the
// old file's mode never carries over and a symlink is replaced, not
// followed.
func WriteFile(path string, data []byte, force bool) error {
	if !force {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w; pass --force to replace it", path, ErrExists)
		}
		if err != nil {
			return err
		}
		return finish(f, path, data, "")
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	return finish(f, path, data, f.Name())
}

// finish writes data to f, flushes and closes it, and renames tmp over
// path when tmp is set; on any failure the file it made is removed.
func finish(f *os.File, path string, data []byte, tmp string) error {
	made := f.Name()
	err := f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && tmp != "" {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(made)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
