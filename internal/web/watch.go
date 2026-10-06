// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"fmt"
	"sync"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// defaultDebounce coalesces informer bursts (an ingest window closing, a
// scan delivering hundreds of alerts) into one refetch signal.
const defaultDebounce = 500 * time.Millisecond

// StartWatch registers change handlers on the Finding/FindingRollup and
// Integration/Forge informers and publishes debounced findings-changed /
// config-changed notifications to the SSE broker. It blocks until ctx is
// cancelled, so it slots straight into a manager runnable — running after
// the cache has synced and stopping with the manager.
func (s *Server) StartWatch(ctx context.Context, c cache.Cache) error {
	watch := func(signal chan<- struct{}, objs ...client.Object) error {
		notify := func() {
			select {
			case signal <- struct{}{}:
			default:
			}
		}
		for _, obj := range objs {
			informer, err := c.GetInformer(ctx, obj)
			if err != nil {
				return fmt.Errorf("watch %T: %w", obj, err)
			}
			_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
				AddFunc: func(any) { notify() },
				UpdateFunc: func(oldObj, newObj any) {
					if resourceChanged(oldObj, newObj) {
						notify()
					}
				},
				DeleteFunc: func(any) { notify() },
			})
			if err != nil {
				return fmt.Errorf("watch %T: %w", obj, err)
			}
		}
		return nil
	}

	findings := make(chan struct{}, 1)
	if err := watch(findings, &v1alpha1.Finding{}, &v1alpha1.FindingRollup{}); err != nil {
		return err
	}
	config := make(chan struct{}, 1)
	if err := watch(config, &v1alpha1.Integration{}, &v1alpha1.Forge{}); err != nil {
		return err
	}
	if s.intents != nil {
		changed, err := s.watchIntents(ctx, c)
		if err != nil {
			return err
		}
		go s.intentsDebounceLoop(ctx, changed)
	}
	go s.debounceLoop(ctx, config, eventConfigChanged)
	s.debounceLoop(ctx, findings, eventFindingsChanged)
	return nil
}

// changedProjects collects the Projects whose intents changed within one
// debounce window.
type changedProjects struct {
	signal chan struct{}

	mu  sync.Mutex
	set map[string]bool
}

func (p *changedProjects) add(project string) {
	if project == "" {
		return
	}
	p.mu.Lock()
	p.set[project] = true
	p.mu.Unlock()
	select {
	case p.signal <- struct{}{}:
	default:
	}
}

func (p *changedProjects) take() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.set
	p.set = map[string]bool{}
	return out
}

// watchIntents registers change handlers on the Project, Intent, IntentRun
// and Preview informers, each resolving the Project the change belongs to.
// Registered only with the intents views on: the informers need their RBAC.
func (s *Server) watchIntents(ctx context.Context, c cache.Cache) (*changedProjects, error) {
	changed := &changedProjects{signal: make(chan struct{}, 1), set: map[string]bool{}}
	note := func(obj any) {
		if tomb, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
			obj = tomb.Obj
		}
		changed.add(s.projectOf(ctx, obj))
	}
	for _, obj := range []client.Object{
		&v1alpha1.Project{}, &v1alpha1.Intent{}, &v1alpha1.IntentRun{}, &v1alpha1.Preview{},
	} {
		informer, err := c.GetInformer(ctx, obj)
		if err != nil {
			return nil, fmt.Errorf("watch %T: %w", obj, err)
		}
		_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc: note,
			UpdateFunc: func(oldObj, newObj any) {
				if resourceChanged(oldObj, newObj) {
					note(newObj)
				}
			},
			DeleteFunc: note,
		})
		if err != nil {
			return nil, fmt.Errorf("watch %T: %w", obj, err)
		}
	}
	return changed, nil
}

// projectOf is the Project an intents object belongs to: a Project's own
// name, an Intent's spec.project, and for an IntentRun or a Preview its
// Intent's. "" when it cannot be told (the Intent is already gone, in which
// case its own deletion carries the signal).
func (s *Server) projectOf(ctx context.Context, obj any) string {
	intent := ""
	switch o := obj.(type) {
	case *v1alpha1.Project:
		return o.Name
	case *v1alpha1.Intent:
		return o.Spec.Project
	case *v1alpha1.IntentRun:
		intent = o.Spec.IntentRef.Name
	case *v1alpha1.Preview:
		intent = o.Spec.IntentRef.Name
	}
	if intent == "" {
		return ""
	}
	var in v1alpha1.Intent
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: intent}, &in); err != nil {
		return ""
	}
	return in.Spec.Project
}

// intentsDebounceLoop publishes at most one intents-changed round per
// debounce window, to the subscribers of the Projects that changed in it.
func (s *Server) intentsDebounceLoop(ctx context.Context, changed *changedProjects) {
	debounce := s.debounce
	if debounce <= 0 {
		debounce = defaultDebounce
	}
	timer := time.NewTimer(debounce)
	timer.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed.signal:
			if !pending {
				pending = true
				timer.Reset(debounce)
			}
		case <-timer.C:
			if pending {
				pending = false
				if projects := changed.take(); len(projects) > 0 {
					s.intents.signals.publish(projects)
				}
			}
		}
	}
}

// debounceLoop publishes at most one notification of event per debounce
// window: the first signal arms the timer, further signals are absorbed
// into the pending publish.
func (s *Server) debounceLoop(ctx context.Context, signal <-chan struct{}, event string) {
	debounce := s.debounce
	if debounce <= 0 {
		debounce = defaultDebounce
	}
	timer := time.NewTimer(debounce)
	timer.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-signal:
			if !pending {
				pending = true
				timer.Reset(debounce)
			}
		case <-timer.C:
			if pending {
				pending = false
				s.broker.publish(event)
			}
		}
	}
}

// resourceChanged filters informer resyncs: an update whose resourceVersion
// did not move carries no new state.
func resourceChanged(oldObj, newObj any) bool {
	o, ok1 := oldObj.(client.Object)
	n, ok2 := newObj.(client.Object)
	if !ok1 || !ok2 {
		return true
	}
	return o.GetResourceVersion() != n.GetResourceVersion()
}
