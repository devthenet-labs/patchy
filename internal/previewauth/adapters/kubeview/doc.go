// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package kubeview is the preview sign-in relay's previewauth.PreviewLookup
// over a controller-runtime reader (the relay's cache of Previews in the
// release namespace). It reads Previews only: the Project comes from the
// Preview's own spec.project, which intent-controller stamps, so the relay
// needs no access to Intents (critique F8). The Preview at a host label is
// the one named by it; previewauth.ViewOf also requires its spec.hostLabel to
// match, so a Preview whose name and label differ is never live.
package kubeview
