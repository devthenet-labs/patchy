// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// nudgeBuffer bounds the nudges waiting for the intent reconciler. A nudge
// dropped because it is full is repeated by discovery's next full listing;
// one whose hand-off fails is kept for the reconcile's retry (restore).
const nudgeBuffer = 256

// Nudger hands an Intent to the intent reconciler out of band: discovery saw
// the issue of an Intent that has ended carry the trigger label, which the
// Intent answers (a revival, or the notice that it has ended). An ended
// Intent does not poll its issue on its own, so this is the only way such a
// trigger reaches it. The zero value is not usable; use NewNudger.
type Nudger struct {
	mu      sync.Mutex
	pending map[string]bool
	events  chan event.GenericEvent
}

// NewNudger returns an empty Nudger.
func NewNudger() *Nudger {
	return &Nudger{pending: map[string]bool{}, events: make(chan event.GenericEvent, nudgeBuffer)}
}

// Nudge asks the intent reconciler to look at the named Intent's issue
// events. It never blocks.
func (n *Nudger) Nudge(namespace, name string) {
	n.mu.Lock()
	already := n.pending[name]
	n.pending[name] = true
	n.mu.Unlock()
	if already {
		return
	}
	obj := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	select {
	case n.events <- event.GenericEvent{Object: obj}:
	default:
		n.mu.Lock()
		delete(n.pending, name)
		n.mu.Unlock()
	}
}

// restore puts back a nudge take cleared, for a hand-off that failed: the
// reconcile's retry answers it. Discovery's listing of an unchanged issue is
// a 304, which never nudges again, so a nudge dropped on a transient failure
// would lose the trigger it was for.
func (n *Nudger) restore(name string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pending[name] = true
}

// take reports whether the named Intent was nudged, and clears it.
func (n *Nudger) take(name string) bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	nudged := n.pending[name]
	delete(n.pending, name)
	return nudged
}

// source is the controller-runtime source the intent reconciler watches.
func (n *Nudger) source() source.Source {
	return source.Channel(n.events, &handler.TypedEnqueueRequestForObject[client.Object]{})
}
