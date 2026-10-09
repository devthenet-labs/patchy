// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integrationcap

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func integration(ns, name string, mut func(*v1alpha1.IntegrationSpec)) *v1alpha1.Integration {
	i := &v1alpha1.Integration{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if mut != nil {
		mut(&i.Spec)
	}
	return i
}

func withCAI(s *v1alpha1.IntegrationSpec) {
	s.GoogleCloud = &v1alpha1.GoogleCloudIntegration{
		CloudAssetInventory: &v1alpha1.GoogleCloudAssetInventory{Enabled: true},
	}
}

func TestSelect(t *testing.T) {
	cases := []struct {
		name    string
		objs    []client.Object
		want    string
		wantErr error
	}{
		{name: "none", wantErr: ErrNoIntegration},
		{
			name:    "only non-providers",
			objs:    []client.Object{integration("ns", "plain", nil)},
			wantErr: ErrNoIntegration,
		},
		{
			name: "exactly one provider among others",
			objs: []client.Object{integration("ns", "plain", nil), integration("ns", "gcp", withCAI)},
			want: "gcp",
		},
		{
			name:    "provider in another namespace is invisible",
			objs:    []client.Object{integration("other", "gcp", withCAI)},
			wantErr: ErrNoIntegration,
		},
		{
			name:    "two providers are ambiguous",
			objs:    []client.Object{integration("ns", "a", withCAI), integration("ns", "b", withCAI)},
			wantErr: ErrAmbiguousIntegration,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(c.objs...).Build()
			got, err := Select(context.Background(), cl, "ns", CloudAssetInventoryEnabled)
			if !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
				t.Fatalf("Select err = %v, want %v", err, c.wantErr)
			}
			if c.wantErr != nil {
				if got != nil {
					t.Errorf("Select = %s alongside an error", got.Name)
				}
				return
			}
			if got == nil || got.Name != c.want {
				t.Errorf("Select = %v, want %s", got, c.want)
			}
		})
	}
}

func TestSelectListError(t *testing.T) {
	boom := errors.New("apiserver down")
	cl := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
	}).Build()
	got, err := Select(context.Background(), cl, "ns", CloudAssetInventoryEnabled)
	if !errors.Is(err, boom) || got != nil {
		t.Fatalf("Select = %v, %v; want wrapped list error", got, err)
	}
}

func TestCapabilityPredicates(t *testing.T) {
	type pred struct {
		name string
		fn   Capability
	}
	cai := pred{"CloudAssetInventoryEnabled", CloudAssetInventoryEnabled}
	aws := pred{"AWSResourceTagsEnabled", AWSResourceTagsEnabled}
	azure := pred{"AzureResourceTagsEnabled", AzureResourceTagsEnabled}
	generic := pred{"GenericEnhanceEnabled", GenericEnhanceEnabled}

	cases := []struct {
		name string
		spec v1alpha1.IntegrationSpec
		// exactly these predicates hold
		want []pred
	}{
		{name: "empty spec", spec: v1alpha1.IntegrationSpec{}},
		{
			name: "google cloud without inventory block",
			spec: v1alpha1.IntegrationSpec{GoogleCloud: &v1alpha1.GoogleCloudIntegration{}},
		},
		{
			name: "inventory disabled",
			spec: v1alpha1.IntegrationSpec{GoogleCloud: &v1alpha1.GoogleCloudIntegration{
				CloudAssetInventory: &v1alpha1.GoogleCloudAssetInventory{},
			}},
		},
		{
			name: "inventory enabled",
			spec: v1alpha1.IntegrationSpec{GoogleCloud: &v1alpha1.GoogleCloudIntegration{
				CloudAssetInventory: &v1alpha1.GoogleCloudAssetInventory{Enabled: true},
			}},
			want: []pred{cai},
		},
		{
			name: "aws tags enabled",
			spec: v1alpha1.IntegrationSpec{AWS: &v1alpha1.AWSIntegration{
				ResourceTags: &v1alpha1.AWSResourceTags{Enabled: true},
			}},
			want: []pred{aws},
		},
		{
			name: "aws without tags block",
			spec: v1alpha1.IntegrationSpec{AWS: &v1alpha1.AWSIntegration{}},
		},
		{
			name: "azure tags enabled",
			spec: v1alpha1.IntegrationSpec{Azure: &v1alpha1.AzureIntegration{
				ResourceTags: &v1alpha1.AzureResourceTags{Enabled: true},
			}},
			want: []pred{azure},
		},
		{
			name: "azure without tags block",
			spec: v1alpha1.IntegrationSpec{Azure: &v1alpha1.AzureIntegration{}},
		},
		{
			name: "generic enhance enabled",
			spec: v1alpha1.IntegrationSpec{
				Provider: v1alpha1.IntegrationProviderGeneric,
				Generic:  &v1alpha1.GenericIntegration{Enhance: &v1alpha1.GenericEnhancer{Enabled: true}},
			},
			want: []pred{generic},
		},
		{
			name: "generic block on a non-generic provider",
			spec: v1alpha1.IntegrationSpec{
				Generic: &v1alpha1.GenericIntegration{Enhance: &v1alpha1.GenericEnhancer{Enabled: true}},
			},
		},
		{
			name: "generic without enhance block",
			spec: v1alpha1.IntegrationSpec{
				Provider: v1alpha1.IntegrationProviderGeneric,
				Generic:  &v1alpha1.GenericIntegration{},
			},
		},
		{
			name: "suspend disables every capability",
			spec: v1alpha1.IntegrationSpec{
				Suspend:  true,
				Provider: v1alpha1.IntegrationProviderGeneric,
				GoogleCloud: &v1alpha1.GoogleCloudIntegration{
					CloudAssetInventory: &v1alpha1.GoogleCloudAssetInventory{Enabled: true},
				},
				AWS:     &v1alpha1.AWSIntegration{ResourceTags: &v1alpha1.AWSResourceTags{Enabled: true}},
				Azure:   &v1alpha1.AzureIntegration{ResourceTags: &v1alpha1.AzureResourceTags{Enabled: true}},
				Generic: &v1alpha1.GenericIntegration{Enhance: &v1alpha1.GenericEnhancer{Enabled: true}},
			},
		},
	}
	all := []pred{cai, aws, azure, generic}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := &v1alpha1.Integration{Spec: c.spec}
			for _, p := range all {
				want := false
				for _, w := range c.want {
					if w.name == p.name {
						want = true
					}
				}
				if got := p.fn(i); got != want {
					t.Errorf("%s = %v, want %v", p.name, got, want)
				}
			}
		})
	}
}
