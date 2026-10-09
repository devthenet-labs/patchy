// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package context

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/pkg/enhance"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

var findingGR = schema.GroupResource{Group: "patchy.bitwisemedia.uk", Resource: "findings"}

// interceptedReconciler builds a reconciler over a fake client whose writes
// go through funcs, so write failures can be injected.
func interceptedReconciler(
	chain []enhance.Enhancer, funcs interceptor.Funcs, objs ...client.Object,
) (*FindingReconciler, client.Client) {
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithInterceptorFuncs(funcs).
		Build()
	return &FindingReconciler{
		Client:    c,
		Enhancers: chain,
		Now:       func() time.Time { return crdClock },
	}, c
}

func conflict() error { return kerrors.NewConflict(findingGR, "finding-aa-1", errors.New("stale")) }

func statusUpdateFails(err error) interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(
			context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption,
		) error {
			return err
		},
	}
}

func specUpdateFails(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return err
		},
	}
}

// Every write the reconciler makes treats a conflict as "re-read and decide
// again" (a requeue, no error) and anything else as a real error. The
// finding must stay unadvanced either way.
func TestReconcileWriteFailures(t *testing.T) {
	boom := errors.New("apiserver down")
	resolving := func() []enhance.Enhancer {
		return []enhance.Enhancer{&resolver{id: "gcp", ref: &source.RepositoryRef{
			Provider: "github", Owner: "acme", Name: "infra",
		}}}
	}
	failing := func() []enhance.Enhancer {
		return []enhance.Enhancer{&resolver{id: "gcp", err: errors.New("lookup failed")}}
	}

	tests := []struct {
		name        string
		finding     func() *v1alpha1.Finding
		chain       func() []enhance.Enhancer
		funcs       interceptor.Funcs
		wantRequeue bool
		wantErr     bool
	}{
		{
			name: "status update conflict requeues", finding: openedFinding,
			chain: func() []enhance.Enhancer { return nil }, funcs: statusUpdateFails(conflict()), wantRequeue: true,
		},
		{
			name: "status update error is returned", finding: openedFinding,
			chain: func() []enhance.Enhancer { return nil }, funcs: statusUpdateFails(boom), wantErr: true,
		},
		{
			name: "repository write conflict requeues", finding: cloudFinding,
			chain: resolving, funcs: specUpdateFails(conflict()), wantRequeue: true,
		},
		{
			name: "repository write error is returned", finding: cloudFinding,
			chain: resolving, funcs: specUpdateFails(boom), wantErr: true,
		},
		{
			name: "hold write conflict requeues", finding: cloudFinding,
			chain: failing, funcs: statusUpdateFails(conflict()), wantRequeue: true,
		},
		{
			name: "hold write error is returned", finding: cloudFinding,
			chain: failing, funcs: statusUpdateFails(boom), wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, c := interceptedReconciler(tt.chain(), tt.funcs, tt.finding())
			res, err := r.Reconcile(t.Context(), request())
			if tt.wantErr != (err != nil) {
				t.Fatalf("Reconcile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && kerrors.IsConflict(err) {
				t.Errorf("Reconcile() surfaced a conflict %v; conflicts must requeue", err)
			}
			if res.Requeue != tt.wantRequeue { //nolint:staticcheck // the reconciler's contract is Requeue.
				t.Errorf("Requeue = %v, want %v", res.Requeue, tt.wantRequeue) //nolint:staticcheck // as above.
			}
			got := getFinding(t, c)
			if got.Status.Phase != v1alpha1.PhaseOpened {
				t.Errorf("phase = %s, want Opened after a failed write", got.Status.Phase)
			}
		})
	}
}

func TestReconcileMissingFindingIsNoop(t *testing.T) {
	r, _ := newCRDReconciler(t, nil)
	res, err := r.Reconcile(t.Context(), request())
	if err != nil || !reflect.ValueOf(res).IsZero() {
		t.Errorf("Reconcile() = %+v, %v; want zero result and nil for a deleted finding", res, err)
	}
}

func TestReconcileGetErrorIsReturned(t *testing.T) {
	boom := errors.New("cache not synced")
	r, _ := interceptedReconciler(nil, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}, openedFinding())
	if _, err := r.Reconcile(t.Context(), request()); !errors.Is(err, boom) {
		t.Errorf("Reconcile() = %v, want the read error", err)
	}
}

func TestReconcileSkipsDeletingFinding(t *testing.T) {
	fnd := openedFinding()
	now := metav1.NewTime(crdClock)
	fnd.DeletionTimestamp = &now
	fnd.Finalizers = []string{"patchy.bitwisemedia.uk/test"}
	called := false
	r, c := newCRDReconciler(t, []enhance.Enhancer{enhancerFunc(func() { called = true })}, fnd)
	run(t, r)
	if called {
		t.Error("enhancer ran for a finding being deleted")
	}
	if got := getFinding(t, c).Status.Phase; got != v1alpha1.PhaseOpened {
		t.Errorf("phase = %s, want Opened", got)
	}
}

// enhancerFunc records that the chain ran.
type enhancerFunc func()

func (enhancerFunc) ID() string { return "probe" }

func (f enhancerFunc) Enhance(context.Context, enhance.Issue) (*enhance.Enrichment, error) {
	f()
	return nil, nil
}

// The defaults: a zero RetryAfter paces holds at a minute, a nil clock uses
// wall time, and a configured logger receives the outcome.
func TestReconcilerDefaults(t *testing.T) {
	t.Run("retry after defaults to a minute", func(t *testing.T) {
		e := &resolver{id: "gcp", err: errors.New("unavailable")}
		r, _ := newCRDReconciler(t, []enhance.Enhancer{e}, cloudFinding())
		res, err := r.Reconcile(t.Context(), request())
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if res.RequeueAfter != defaultRetryAfter {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, defaultRetryAfter)
		}
	})

	t.Run("nil clock stamps wall time and logger is used", func(t *testing.T) {
		var buf bytes.Buffer
		r, c := newCRDReconciler(t, nil, openedFinding())
		r.Now = nil
		r.Log = slog.New(slog.NewTextHandler(&buf, nil))
		before := time.Now().Add(-time.Second)
		run(t, r)
		f := getFinding(t, c)
		if f.Status.Phase != v1alpha1.PhaseEnhanced {
			t.Fatalf("phase = %s, want Enhanced", f.Status.Phase)
		}
		pt := f.Status.PhaseTimes
		if len(pt) == 0 || pt[len(pt)-1].At.Time.Before(before) {
			t.Errorf("PhaseTimes = %v, want the Enhanced edge stamped with wall-clock time", pt)
		}
		if !strings.Contains(buf.String(), "finding enhanced") {
			t.Errorf("log = %q, want the enhanced line", buf.String())
		}
	})
}

// The enhancer receives the finding's identity: owner/name split from the
// repository and the tracking issue number.
func TestEnhanceInput(t *testing.T) {
	tests := []struct {
		name     string
		repoName string
		issue    int64
		want     source.Repo
	}{
		{
			name: "well-formed repository", repoName: "acme/orders", issue: 42,
			want: source.Repo{Owner: "acme", Name: "orders"},
		},
		{name: "no slash leaves repo empty", repoName: "orders", issue: 0},
		{name: "empty owner leaves repo empty", repoName: "/orders"},
		{name: "empty name leaves repo empty", repoName: "acme/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fnd := openedFinding()
			fnd.Spec.Repository.Name = tt.repoName
			if tt.issue != 0 {
				fnd.Status.Tracking = &v1alpha1.TrackingStatus{IssueNumber: tt.issue}
			}
			got := enhanceInput(fnd)
			if got.Repo != tt.want {
				t.Errorf("Repo = %+v, want %+v", got.Repo, tt.want)
			}
			if got.Number != int(tt.issue) {
				t.Errorf("Number = %d, want %d", got.Number, tt.issue)
			}
		})
	}
}

func TestRepoNameFromURL(t *testing.T) {
	tests := map[string]string{
		"https://github.com/acme/orders":      "acme/orders",
		"https://github.com/acme/orders.git":  "acme/orders",
		"https://github.com/acme/orders/":     "acme/orders",
		"https://github.com/acme":             "",
		"https://github.com/acme/orders/tree": "",
		"://not a url":                        "",
	}
	for in, want := range tests {
		if got := repoNameFromURL(in); got != want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in    string
		limit int
		want  string
	}{
		{in: "short", limit: 10, want: "short"},
		{in: "exactly", limit: 7, want: "exactly"},
		{in: "abcdef", limit: 3, want: "abc"},
		// "é" is two bytes; cutting through it backs off to the boundary.
		{in: "aé", limit: 2, want: "a"},
		// "€" is three bytes.
		{in: "€€", limit: 5, want: "€"},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.limit); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
		}
	}
}

// Property: truncate returns a valid-UTF-8 prefix of valid input within the
// limit, losing less than one rune's worth of bytes.
func TestTruncateProperty(t *testing.T) {
	prop := func(runes []rune, limit uint8) bool {
		s := string(runes)
		got := truncate(s, int(limit))
		return strings.HasPrefix(s, got) &&
			len(got) <= int(limit) &&
			utf8.ValidString(got) &&
			((len(s) <= int(limit) && got == s) || (len(s) > int(limit) && int(limit)-len(got) < utf8.UTFMax))
	}
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(1))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestOpenedOnlyPredicate(t *testing.T) {
	p := openedOnly()
	opened := openedFinding()
	enhanced := openedFinding()
	enhanced.Status.Phase = v1alpha1.PhaseEnhanced
	notAFinding := &corev1.ConfigMap{}

	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"create opened", p.Create(event.CreateEvent{Object: opened}), true},
		{"create enhanced", p.Create(event.CreateEvent{Object: enhanced}), false},
		{"create other kind", p.Create(event.CreateEvent{Object: notAFinding}), false},
		{"update to opened", p.Update(event.UpdateEvent{ObjectOld: enhanced, ObjectNew: opened}), true},
		{"update away from opened", p.Update(event.UpdateEvent{ObjectOld: opened, ObjectNew: enhanced}), false},
		{"delete never", p.Delete(event.DeleteEvent{Object: opened}), false},
		{"generic opened", p.Generic(event.GenericEvent{Object: opened}), true},
		{"generic enhanced", p.Generic(event.GenericEvent{Object: enhanced}), false},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestConfigSourcesSurfaceAmbiguity(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(
		awsIntegration("aws-a", &v1alpha1.AWSResourceTags{Enabled: true}),
		awsIntegration("aws-b", &v1alpha1.AWSResourceTags{Enabled: true}),
		azureIntegration("az-a", &v1alpha1.AzureResourceTags{Enabled: true, ManagementGroup: "a"}),
		azureIntegration("az-b", &v1alpha1.AzureResourceTags{Enabled: true, ManagementGroup: "b"}),
	).Build()
	if cfg, err := AWSTagsConfigSource(c, "patchy")(t.Context()); err == nil {
		t.Errorf("AWSTagsConfigSource() = %+v, nil; want the ambiguity surfaced", cfg)
	}
	if cfg, err := AzureTagsConfigSource(c, "patchy")(t.Context()); err == nil {
		t.Errorf("AzureTagsConfigSource() = %+v, nil; want the ambiguity surfaced", cfg)
	}
}

func TestGenericEnhancerConfigSourceListError(t *testing.T) {
	boom := errors.New("list failed")
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
	}).Build()
	if _, err := GenericEnhancerConfigSource(c, c, "patchy", nil)(t.Context()); !errors.Is(err, boom) {
		t.Errorf("GenericEnhancerConfigSource() = %v, want the list error wrapped", err)
	}
}

// An integration with no secretRef is listed secret-less without a read; a
// missing Secret is logged, attributed to its integration.
func TestGenericEnhancerConfigSourceSecrets(t *testing.T) {
	noRef := genericIntegration("noref", "https://noref.internal/enhance")
	noRef.Spec.SecretRef = nil
	missing := genericIntegration("missing", "https://missing.internal/enhance")
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(noRef, missing).Build()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfgs, err := GenericEnhancerConfigSource(c, c, "patchy", log)(t.Context())
	if err != nil {
		t.Fatalf("GenericEnhancerConfigSource() = %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("configs = %+v, want both integrations listed", cfgs)
	}
	for _, cfg := range cfgs {
		if cfg.Secret != nil {
			t.Errorf("%s secret = %q, want nil", cfg.Name, cfg.Secret)
		}
	}
	out := buf.String()
	if !strings.Contains(out, "generic enhancer secret unavailable") || !strings.Contains(out, "integration=missing") {
		t.Errorf("log = %q, want a warning attributed to the missing integration", out)
	}
	if strings.Contains(out, "integration=noref") {
		t.Errorf("log = %q, want no warning for an integration with no secretRef", out)
	}
}
