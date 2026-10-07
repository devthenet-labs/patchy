// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
	"github.com/bitwise-media-group/patchy/internal/runnerimage/resolve"
)

// The labels a controller's ConfigMap carries: the chart's
// app.kubernetes.io/name is the controller, and both the chart and kustomize
// set part-of. Kustomize's one shared ConfigMap (patchy-config) is named for
// the whole stack instead, and a controller reads it first and its own after.
const (
	labelName   = "app.kubernetes.io/name"
	labelPartOf = "app.kubernetes.io/part-of"
	partOf      = "patchy"
	sharedName  = "patchy"
)

// The controllers whose settings the check reads.
const (
	sourceController  = "source-controller"
	intentController  = "intent-controller"
	previewController = "preview-controller"
	// previewAuth is the preview sign-in relay, chart-only: it never reads
	// kustomize's shared ConfigMap.
	previewAuth = "preview-auth"
)

// The configuration keys the check reads; each is a PATCHY_* environment
// variable a controller binds to its flag of the same name.
const (
	keyRepositoryImages      = "PATCHY_REPOSITORY_IMAGES"
	keyImageRegistries       = "PATCHY_REPOSITORY_IMAGE_REGISTRIES"
	keyImageDenied           = "PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES"
	keyImageMaxBytes         = "PATCHY_REPOSITORY_IMAGE_MAX_BYTES"
	keyImageAllowUnsigned    = "PATCHY_REPOSITORY_IMAGE_ALLOW_UNSIGNED"
	keyImageCosignKeyFile    = "PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE"
	keyIntentPreviewsEnabled = "PATCHY_INTENT_PREVIEWS_ENABLED"
	keyPreviewImagePrefix    = "PATCHY_PREVIEW_IMAGE_PREFIX"
	keyPreviewHostSuffix     = "PATCHY_PREVIEW_HOST_SUFFIX"
	// cosignKeyData is the key of the chart's repository-image-key
	// ConfigMap, which source-controller mounts as its cosign key file.
	cosignKeyData = "cosign.pub"
)

// controllerConfig is one controller's settings as its ConfigMaps hold them.
type controllerConfig struct {
	// from names the ConfigMaps read, for reasons; empty when none exists.
	from string
	data map[string]string
	// err is why the ConfigMaps could not be listed.
	err error
}

// found reports whether any ConfigMap configures the controller.
func (c controllerConfig) found() bool { return c.err == nil && c.from != "" }

// bool reads a boolean setting; unset or unparsable is false, as a flag's
// default would be.
func (c controllerConfig) bool(key string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(c.data[key]))
	return err == nil && v
}

// list reads a comma-separated setting the way a controller's StringList
// does: entries trimmed, empty ones dropped.
func (c controllerConfig) list(key string) []string {
	var out []string
	for e := range strings.SplitSeq(c.data[key], ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// settings are the controllers' configuration, read once per run. Never a
// Secret: only ConfigMaps.
type settings struct {
	source, intent, preview controllerConfig
	// auth is the preview sign-in relay's own settings, found only when the
	// chart's previewAuth is on.
	auth controllerConfig
	// cosignKey is the PEM of source-controller's cosign key ConfigMap,
	// empty when there is none.
	cosignKey string
}

// loadSettings lists the namespace's patchy ConfigMaps once and assigns
// each controller its own over the shared one. With several of one
// controller's (two releases in a namespace), the first by name is read.
func loadSettings(ctx context.Context, r client.Reader, namespace string) settings {
	var list corev1.ConfigMapList
	err := r.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{labelPartOf: partOf})
	if err != nil {
		err = fmt.Errorf("list the patchy ConfigMaps in namespace %s: %w", namespace, err)
		failed := controllerConfig{err: err}
		return settings{source: failed, intent: failed, preview: failed, auth: failed}
	}
	items := list.Items
	slices.SortFunc(items, func(a, b corev1.ConfigMap) int { return strings.Compare(a.Name, b.Name) })
	shared := pick(items, sharedName)
	s := settings{
		source:  overlay(shared, pick(items, sourceController)),
		intent:  overlay(shared, pick(items, intentController)),
		preview: overlay(shared, pick(items, previewController)),
		auth:    overlay(nil, pick(items, previewAuth)),
	}
	for i := range items {
		if items[i].Labels[labelName] == sourceController && items[i].Data[cosignKeyData] != "" {
			s.cosignKey = items[i].Data[cosignKeyData]
			break
		}
	}
	return s
}

// pick returns the first ConfigMap labelled for name that holds settings.
func pick(items []corev1.ConfigMap, name string) *corev1.ConfigMap {
	for i := range items {
		if items[i].Labels[labelName] != name {
			continue
		}
		for k := range items[i].Data {
			if strings.HasPrefix(k, "PATCHY_") {
				return &items[i]
			}
		}
	}
	return nil
}

// overlay is a controller's settings: the shared ConfigMap's data, then its
// own, which wins, as the Deployment's envFrom order has it.
func overlay(shared, own *corev1.ConfigMap) controllerConfig {
	var c controllerConfig
	var from []string
	c.data = map[string]string{}
	for _, cm := range []*corev1.ConfigMap{shared, own} {
		if cm == nil {
			continue
		}
		maps.Copy(c.data, cm.Data)
		from = append(from, cm.Namespace+"/"+cm.Name)
	}
	c.from = strings.Join(from, " and ")
	return c
}

// imagePolicy is source-controller's policy for repository-declared images,
// as its ConfigMaps configure it.
type imagePolicy struct {
	from     string
	enabled  bool
	policy   *runnerimage.Policy
	maxBytes int64
	// key is the operator's cosign key, set when source-controller
	// requires a signature and its key ConfigMap was readable.
	key *ecdsa.PublicKey
	// signature says how the signature is judged, for the reason.
	signature string
	// unverified is why a signature source-controller requires cannot be
	// verified here (its key was not found); empty when it can be, or when
	// none is required. An image that passes every other check is then a
	// SKIP, never a PASS.
	unverified string
}

// policy parses source-controller's settings. An error is a configuration
// source-controller itself would refuse to start with.
func (s settings) policy() (imagePolicy, error) {
	c := s.source
	p := imagePolicy{from: c.from, enabled: c.bool(keyRepositoryImages), maxBytes: resolve.DefaultMaxBytes}
	if !p.enabled {
		return p, nil
	}
	policy, err := runnerimage.NewPolicy(c.list(keyImageRegistries))
	if err != nil {
		return p, fmt.Errorf("%s: %s: %w", c.from, keyImageRegistries, err)
	}
	// The registry paths source-controller refuses declared images from
	// whatever the allowlist says: the chart's preview image prefix.
	if policy, err = policy.Deny(c.list(keyImageDenied)); err != nil {
		return p, fmt.Errorf("%s: %s: %w", c.from, keyImageDenied, err)
	}
	p.policy = &policy
	if v := strings.TrimSpace(c.data[keyImageMaxBytes]); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return p, fmt.Errorf("%s: %s is %q, not a positive byte count", c.from, keyImageMaxBytes, v)
		}
		p.maxBytes = n
	}
	switch {
	case c.bool(keyImageAllowUnsigned):
		p.signature = "unsigned images allowed"
	case strings.TrimSpace(c.data[keyImageCosignKeyFile]) == "":
		return p, fmt.Errorf("%s: neither %s nor %s is set", c.from, keyImageAllowUnsigned, keyImageCosignKeyFile)
	case s.cosignKey == "":
		p.unverified = fmt.Sprintf("source-controller requires a signature (%s is set), but no ConfigMap "+
			"labelled %s=%s holds its key as %s, so it cannot be verified here", keyImageCosignKeyFile, labelName,
			sourceController, cosignKeyData)
	default:
		key, err := resolve.ParsePublicKey([]byte(s.cosignKey))
		if err != nil {
			return p, fmt.Errorf("source-controller's cosign key ConfigMap: %w", err)
		}
		p.key = key
		p.signature = "signature verified with source-controller's cosign key"
	}
	return p, nil
}
