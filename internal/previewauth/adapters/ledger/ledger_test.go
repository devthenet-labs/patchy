// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func lease(annotations map[string]string) *coordinationv1.Lease {
	return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: "codes", Namespace: "patchy", Annotations: annotations,
	}}
}

func newLedger(c client.Client, now *time.Time, results *[]string) *Ledger {
	return &Ledger{Client: c, Namespace: "patchy", Name: "codes", Now: func() time.Time { return *now },
		Record: func(_ context.Context, r string) { *results = append(*results, r) }}
}

func key(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func TestConsumeOnceThenReplay(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(lease(nil)).Build()
	now := t0
	var results []string
	l := newLedger(c, &now, &results)
	ctx := context.Background()
	if err := l.Consume(ctx, key("a"), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := l.Consume(ctx, key("a"), now.Add(time.Minute)); !errors.Is(err, previewauth.ErrReplayed) {
		t.Fatalf("second consume: %v", err)
	}
	if err := l.Consume(ctx, key("b"), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Once the code would have expired, its entry is forgotten.
	now = now.Add(2 * time.Minute)
	if err := l.Consume(ctx, key("c"), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var got coordinationv1.Lease
	if err := c.Get(ctx, client.ObjectKey{Namespace: "patchy", Name: "codes"}, &got); err != nil {
		t.Fatal(err)
	}
	entries, err := decode(got.Annotations[Annotation])
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %v, %v", entries, err)
	}
	want := []string{ResultOK, ResultReplay, ResultOK, ResultOK}
	if fmt.Sprint(results) != fmt.Sprint(want) {
		t.Errorf("results %v, want %v", results, want)
	}
}

func TestConsumeFull(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(lease(nil)).Build()
	now := t0
	var results []string
	l := newLedger(c, &now, &results)
	for i := range MaxEntries {
		if err := l.Consume(context.Background(), key(fmt.Sprint(i)), now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Consume(context.Background(), key("one more"), now.Add(time.Minute)); !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v", err)
	}
	// A replay is still a replay when the ledger is full.
	if err := l.Consume(context.Background(), key("0"), now.Add(time.Minute)); !errors.Is(err, previewauth.ErrReplayed) {
		t.Fatalf("err = %v", err)
	}
}

func TestConsumeRetriesConflicts(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "codes",
		errors.New("changed"))
	for _, tt := range []struct {
		conflicts int
		ok        bool
	}{{0, true}, {2, true}, {maxAttempts - 1, true}, {maxAttempts, false}} {
		t.Run(fmt.Sprint(tt.conflicts), func(t *testing.T) {
			n := 0
			c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(lease(nil)).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
						if n < tt.conflicts {
							n++
							return conflict
						}
						return c.Update(ctx, o, opts...)
					},
				}).Build()
			now := t0
			var results []string
			err := newLedger(c, &now, &results).Consume(context.Background(), key("a"), now.Add(time.Minute))
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
			if !tt.ok && (errors.Is(err, previewauth.ErrReplayed) || results[0] != ResultConflict) {
				t.Fatalf("a conflict storm must be transient: %v %v", err, results)
			}
		})
	}
}

// TestConcurrentWritersAgree: a real optimistic-concurrency race. A second
// writer redeems the same code between this writer's read and its update;
// the retry sees it and answers replay.
func TestConcurrentWritersAgree(t *testing.T) {
	var other *Ledger
	raced := false
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(lease(nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
				if !raced {
					raced = true
					if err := other.Consume(ctx, key("a"), t0.Add(time.Minute)); err != nil {
						return err
					}
				}
				return c.Update(ctx, o, opts...)
			},
		}).Build()
	now := t0
	var results []string
	l := newLedger(c, &now, &results)
	other = &Ledger{Client: c, Namespace: "patchy", Name: "codes", Now: l.Now}
	if err := l.Consume(context.Background(), key("a"), now.Add(time.Minute)); !errors.Is(err, previewauth.ErrReplayed) {
		t.Fatalf("err = %v", err)
	}
}

func TestConsumeTransientFailures(t *testing.T) {
	ctx := context.Background()
	now := t0
	var results []string
	missing := fake.NewClientBuilder().WithScheme(kube.Scheme()).Build()
	if err := newLedger(missing, &now, &results).Consume(ctx, key("a"), now); err == nil ||
		errors.Is(err, previewauth.ErrReplayed) {
		t.Fatalf("missing lease: %v", err)
	}
	// A corrupt annotation is reset, not a wedge.
	corrupt := fake.NewClientBuilder().WithScheme(kube.Scheme()).
		WithObjects(lease(map[string]string{Annotation: "{not json"})).Build()
	if err := newLedger(corrupt, &now, &results).Consume(ctx, key("a"), now.Add(time.Minute)); err != nil {
		t.Fatalf("corrupt ledger: %v", err)
	}
}
