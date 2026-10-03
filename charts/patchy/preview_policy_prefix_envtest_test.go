// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestPreviewAdmissionCustomImagePrefix renders the slot policies with an
// operator's own preview.imagePathPrefix and applies them to a real API
// server: a slot Pod or Deployment whose image is one leaf under that prefix
// is admitted, and one outside it is denied, the default patchy/previews/
// path, another registry, a sibling path, a nested path, the agent images and
// a mutable tag included.
func TestPreviewAdmissionCustomImagePrefix(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	_, admin := startPreviewPolicyEnv(t, "--set", "preview.imagePathPrefix=acme/runtime.images")
	ctx := t.Context()
	prefix := previewRegistry + "/acme/runtime.images/"
	custom := prefix + "demo:sha-" + previewSHA

	for _, obj := range []client.Object{
		pod("patchy-preview-0", "custom-prefix", custom),
		pod("patchy-preview-1", "custom-prefix-second-slot", prefix+"web-app-2:sha-"+previewSHA),
		deployment("patchy-preview-0", "custom-prefix-deployment", custom),
	} {
		if err := admin.Create(ctx, obj, client.DryRunAll); err != nil {
			t.Errorf("image under the custom prefix refused for %T %s/%s: %v", obj, obj.GetNamespace(), obj.GetName(), err)
		}
	}

	for name, image := range map[string]string{
		"default-path":   previewImage,
		"other-registry": "123456789012.dkr.ecr.us-east-1.amazonaws.com/acme/runtime.images/demo:sha-" + previewSHA,
		"sibling-path":   previewRegistry + "/acme/runtime.images-evil/demo:sha-" + previewSHA,
		"nested-path":    prefix + "team/demo:sha-" + previewSHA,
		"parent-path":    previewRegistry + "/acme/demo:sha-" + previewSHA,
		"agent-image":    previewRegistry + "/acme/agents/demo:sha-" + previewSHA,
		"mutable-tag":    prefix + "demo:latest",
		"short-sha":      prefix + "demo:sha-aaaa",
	} {
		t.Run(name, func(t *testing.T) {
			for _, obj := range []client.Object{
				pod("patchy-preview-0", "outside-"+name, image),
				deployment("patchy-preview-0", "outside-"+name, image),
			} {
				err := admin.Create(ctx, obj, client.DryRunAll)
				if err == nil || !strings.Contains(err.Error(), "only immutable patchy preview ECR images") {
					t.Errorf("%T with image %s: want the image rule's denial, got %v", obj, image, err)
				}
			}
		})
	}
}
