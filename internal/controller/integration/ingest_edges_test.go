// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

func edgeIngestor(funcs *interceptor.Funcs, objs ...client.Object) (*Ingestor, client.Client, *bytes.Buffer) {
	c := receiverClient(funcs, objs...)
	logs := &bytes.Buffer{}
	return &Ingestor{
		Client: c, Namespace: "patchy", Window: time.Hour,
		Now: func() time.Time { return testClock },
		Log: slog.New(slog.NewTextHandler(logs, nil)),
	}, c, logs
}

// familyName is the deterministic name of generation gen of f's family.
func familyName(integ *v1alpha1.Integration, f source.Finding, gen int) string {
	repoURL := repositoryURL(integ, f)
	hash := keyHash(integ.Name, f.Source, accumulationScope(repoURL, f), f.Advisories[0])
	return fmt.Sprintf("finding-%s-%d", hash, gen)
}

// Two deliveries racing to create one generation: the loser's create finds
// the winner's object and folds its alert into it.
func TestIngestCreateRaceFolds(t *testing.T) {
	integ := testIntegration()
	// The winner's object, not yet visible to the family listing (no
	// key-hash label: the cache has not caught up).
	winner := &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: familyName(integ, testSourceFinding(42), 1), Namespace: "patchy"},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "gh"}, Source: "ghas",
			Advisories: []string{"CVE-2026-0001"},
			Alerts:     []v1alpha1.Alert{{ID: "41", Source: "ghas"}},
		},
	}
	in, c, _ := edgeIngestor(nil, winner)
	if err := in.Ingest(t.Context(), integ, testSourceFinding(42)); err != nil {
		t.Fatalf("Ingest() = %v", err)
	}
	items := listFindings(t, c)
	if len(items) != 1 {
		t.Fatalf("findings = %d, want the winner alone", len(items))
	}
	ids := make([]string, 0, len(items[0].Spec.Alerts))
	for _, a := range items[0].Spec.Alerts {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "41,42" {
		t.Errorf("alerts = %v, want the loser's alert folded into the winner", ids)
	}
	if got := items[0].Spec.Advisories; len(got) != 2 || got[1] != "CWE-79" {
		t.Errorf("advisories = %v, want the new identifier folded in", got)
	}
}

// A live generation that moves past pre-investigation mid-fold gets a
// successor instead, and the successor edge is mirrored onto it.
func TestIngestFoldRacedOpensSuccessor(t *testing.T) {
	integ := testIntegration()
	first := familyName(integ, testSourceFinding(42), 1)
	in, c, _ := edgeIngestor(nil)
	if err := in.Ingest(t.Context(), integ, testSourceFinding(42)); err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}
	// The cache still lists the generation as Opened, but by the time the
	// fold reads it the gate has admitted it.
	racing := receiverClient(&interceptor.Funcs{
		Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if f, ok := obj.(*v1alpha1.Finding); ok && f.Name == first {
				f.Status.Phase = v1alpha1.PhaseInvestigating
			}
			return nil
		},
	})
	// Copy the existing generation into the racing client.
	elder := get(t, c, first)
	elder.ResourceVersion = ""
	if err := racing.Create(t.Context(), elder); err != nil {
		t.Fatalf("seed elder: %v", err)
	}
	elder.Status.Phase = v1alpha1.PhaseOpened
	if err := racing.Status().Update(t.Context(), elder); err != nil {
		t.Fatalf("seed elder status: %v", err)
	}
	in.Client = racing

	if err := in.Ingest(t.Context(), integ, testSourceFinding(43)); err != nil {
		t.Fatalf("second Ingest() = %v", err)
	}
	second := familyName(integ, testSourceFinding(43), 2)
	succ := get(t, racing, second)
	if len(succ.Spec.Related) != 1 || succ.Spec.Related[0].To != first ||
		succ.Spec.Related[0].Relationship != v1alpha1.RelationshipSuccessorOf {
		t.Errorf("successor related = %+v, want successor-of %s", succ.Spec.Related, first)
	}
	if len(succ.Spec.Alerts) != 1 || succ.Spec.Alerts[0].ID != "43" {
		t.Errorf("successor alerts = %+v, want alert 43", succ.Spec.Alerts)
	}
	if e := get(t, racing, first); len(e.Spec.Related) != 1 || e.Spec.Related[0].From != second {
		t.Errorf("elder related = %+v, want the mirrored edge from %s", e.Spec.Related, second)
	}
	if e := get(t, racing, first); len(e.Spec.Alerts) != 1 {
		t.Errorf("elder alerts = %+v, want alert 43 not folded into an admitted generation", e.Spec.Alerts)
	}
}

func TestIngestWriteFailures(t *testing.T) {
	boom := errors.New("apiserver down")

	t.Run("a family listing failure is an error", func(t *testing.T) {
		in, _, _ := edgeIngestor(&interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
		})
		err := in.Ingest(t.Context(), testIntegration(), testSourceFinding(42))
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "list finding family") {
			t.Errorf("Ingest() = %v, want the listing failure", err)
		}
	})

	t.Run("a create failure is an error naming the finding", func(t *testing.T) {
		in, c, _ := edgeIngestor(&interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return boom },
		})
		err := in.Ingest(t.Context(), testIntegration(), testSourceFinding(42))
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "create finding finding-") {
			t.Errorf("Ingest() = %v, want the create failure", err)
		}
		if got := listFindings(t, c); len(got) != 0 {
			t.Errorf("findings = %d, want none", len(got))
		}
	})

	t.Run("a status init failure keeps the finding and logs", func(t *testing.T) {
		in, c, logs := edgeIngestor(&interceptor.Funcs{
			SubResourceUpdate: func(
				context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption,
			) error {
				return boom
			},
		})
		if err := in.Ingest(t.Context(), testIntegration(), testSourceFinding(42)); err != nil {
			t.Fatalf("Ingest() = %v, want nil: the projection backfills the window", err)
		}
		items := listFindings(t, c)
		if len(items) != 1 || items[0].Status.Phase != "" {
			t.Fatalf("findings = %+v, want one phaseless finding", items)
		}
		if !strings.Contains(logs.String(), "finding status init failed") {
			t.Errorf("logs = %q, want the status failure logged", logs.String())
		}
	})

	t.Run("a fold write failure is an error", func(t *testing.T) {
		in, c, _ := edgeIngestor(nil)
		if err := in.Ingest(t.Context(), testIntegration(), testSourceFinding(42)); err != nil {
			t.Fatalf("seed Ingest() = %v", err)
		}
		failing := receiverClient(&interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return boom },
		})
		seed := listFindings(t, c)[0]
		seed.ResourceVersion = ""
		if err := failing.Create(t.Context(), &seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		in.Client = failing
		if err := in.Ingest(t.Context(), testIntegration(), testSourceFinding(43)); !errors.Is(err, boom) {
			t.Errorf("Ingest() = %v, want the fold write failure", err)
		}
	})
}

// The successor edge mirror is best effort: a full Related list is left as
// is, and a failure is only logged.
func TestMirrorEdgeBounds(t *testing.T) {
	edge := v1alpha1.RelatedFinding{From: "finding-x-2", To: "finding-x-1", Relationship: v1alpha1.RelationshipSuccessorOf}

	t.Run("a full related list is not extended", func(t *testing.T) {
		elder := &v1alpha1.Finding{ObjectMeta: metav1.ObjectMeta{Name: "finding-x-1", Namespace: "patchy"}}
		for i := range 32 {
			elder.Spec.Related = append(elder.Spec.Related, v1alpha1.RelatedFinding{
				From: fmt.Sprintf("finding-y-%d", i), To: "finding-x-1", Relationship: v1alpha1.RelationshipSuccessorOf,
			})
		}
		in, c, _ := edgeIngestor(nil, elder)
		in.mirrorEdge(t.Context(), "finding-x-1", edge)
		if got := get(t, c, "finding-x-1").Spec.Related; len(got) != 32 {
			t.Errorf("related = %d entries, want the cap of 32 kept", len(got))
		}
	})

	t.Run("an existing edge is not duplicated", func(t *testing.T) {
		elder := &v1alpha1.Finding{
			ObjectMeta: metav1.ObjectMeta{Name: "finding-x-1", Namespace: "patchy"},
			Spec:       v1alpha1.FindingSpec{Related: []v1alpha1.RelatedFinding{edge}},
		}
		in, c, _ := edgeIngestor(nil, elder)
		in.mirrorEdge(t.Context(), "finding-x-1", edge)
		if got := get(t, c, "finding-x-1").Spec.Related; len(got) != 1 {
			t.Errorf("related = %+v, want the edge once", got)
		}
	})

	t.Run("a gone elder is logged", func(t *testing.T) {
		in, _, logs := edgeIngestor(nil)
		in.mirrorEdge(t.Context(), "finding-x-1", edge)
		if !strings.Contains(logs.String(), "successor edge mirror failed") {
			t.Errorf("logs = %q, want the failure logged", logs.String())
		}
	})
}

// An alert carries at most eight locations, each snippet capped.
func TestToAlertBounds(t *testing.T) {
	f := testSourceFinding(42)
	f.Locations = nil
	for i := range 10 {
		f.Locations = append(f.Locations, source.Location{
			Path: fmt.Sprintf("f%d.go", i), StartLine: i, Snippet: strings.Repeat("x", 2000),
		})
	}
	a := toAlert(f)
	if len(a.Locations) != 8 {
		t.Fatalf("locations = %d, want 8", len(a.Locations))
	}
	for _, l := range a.Locations {
		if len(l.Snippet) != 1024 {
			t.Errorf("snippet length = %d, want 1024", len(l.Snippet))
		}
	}
	if a.Locations[7].Path != "f7.go" {
		t.Errorf("last location = %s, want the first eight kept in order", a.Locations[7].Path)
	}
}
