// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package kubeview

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// Lookup finds Previews in one namespace.
type Lookup struct {
	Reader    client.Reader
	Namespace string
}

var _ previewauth.PreviewLookup = Lookup{}

// none is the View of an address no Preview holds.
var none = previewauth.View{Slot: -1}

// ByLabel returns the View of the Preview named label. No such Preview, or
// a label that cannot be one, is a View that is not live; only a failed read
// is an error.
func (l Lookup) ByLabel(ctx context.Context, label string) (previewauth.View, error) {
	if !previewauth.ValidLabel(label) {
		return none, nil
	}
	var p v1alpha1.Preview
	err := l.Reader.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: label}, &p)
	if apierrors.IsNotFound(err) {
		return none, nil
	}
	if err != nil {
		return previewauth.View{}, fmt.Errorf("get preview %s: %w", label, err)
	}
	return previewauth.ViewOf(&p), nil
}

// Ready lists the Views of every live Preview whose phase is Ready: the
// ones whose host the ALB serves, and so the ones the host probe checks.
func (l Lookup) Ready(ctx context.Context) ([]previewauth.View, error) {
	var list v1alpha1.PreviewList
	if err := l.Reader.List(ctx, &list, client.InNamespace(l.Namespace)); err != nil {
		return nil, fmt.Errorf("list previews: %w", err)
	}
	var out []previewauth.View
	for i := range list.Items {
		p := &list.Items[i]
		if v := previewauth.ViewOf(p); v.Live && p.Status.Phase == v1alpha1.PreviewReady {
			out = append(out, v)
		}
	}
	return out, nil
}
