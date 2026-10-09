// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package enhancers

import (
	"context"
	"errors"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/awsinv"
	"github.com/bitwise-media-group/patchy/internal/azureinv"
	"github.com/bitwise-media-group/patchy/pkg/enhance"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

// TestEnhancerIDs: the dynamic wrappers attribute their enrichments under
// the same id as the static enhancer they wrap.
func TestEnhancerIDs(t *testing.T) {
	aws, err := NewAWSTags(&fakeInventory{}, TagsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gcl, err := NewGoogleCloudLabels(GoogleCloudOptions{Assets: &fakeAssets{}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		e    enhance.Enhancer
		want string
	}{
		{&DynamicAWS{}, AWSTagsID},
		{aws, AWSTagsID},
		{&DynamicAzure{}, AzureTagsID},
		{&DynamicGoogleCloud{}, GoogleCloudLabelsID},
		{gcl, GoogleCloudLabelsID},
		{&DynamicGeneric{}, "generic"},
		{&StaticFile{}, "static-context"},
	}
	for _, c := range cases {
		if got := c.e.ID(); got != c.want {
			t.Errorf("%T.ID() = %q, want %q", c.e, got, c.want)
		}
	}
}

// TestDynamicGenericEnhanceIsVacuous: the chain runs EnhanceAll, so the
// single-enrichment entry point contributes nothing and never calls out.
func TestDynamicGenericEnhanceIsVacuous(t *testing.T) {
	called := false
	d := &DynamicGeneric{Configs: func(context.Context) ([]GenericConfig, error) {
		called = true
		return nil, nil
	}}
	got, err := d.Enhance(context.Background(), enhance.Issue{})
	if got != nil || err != nil || called {
		t.Errorf("Enhance = %+v, %v (configs read %v); want nil, nil, unread", got, err, called)
	}
}

// TestDynamicBuildFailuresSurfaceAndAreNotMemoized: a client that cannot be
// built is a retryable error, and the next enhancement tries to build again.
func TestDynamicBuildFailuresSurfaceAndAreNotMemoized(t *testing.T) {
	boom := errors.New("cannot verify backend")
	issue := enhance.Issue{CloudResource: s3bucket()}
	t.Run("aws", func(t *testing.T) {
		builds := 0
		d := &DynamicAWS{
			Config: func(context.Context) (*AWSTagsConfig, error) {
				return &AWSTagsConfig{Backend: aggregatorBackend()}, nil
			},
			NewInventory: func(context.Context, awsinv.Config) (AWSInventory, func() error, error) {
				builds++
				return nil, nil, boom
			},
		}
		for range 2 {
			if _, err := d.Enhance(context.Background(), issue); !errors.Is(err, boom) {
				t.Fatalf("Enhance = %v, want the build failure", err)
			}
		}
		if builds != 2 {
			t.Errorf("builds = %d, want 2 (a failure is not memoized)", builds)
		}
		if err := d.Close(); err != nil {
			t.Errorf("Close with nothing built = %v", err)
		}
	})
	t.Run("azure", func(t *testing.T) {
		builds := 0
		d := &DynamicAzure{
			Config: func(context.Context) (*AzureTagsConfig, error) { return &AzureTagsConfig{}, nil },
			NewInventory: func(context.Context, azureinv.Config) (AzureInventory, func() error, error) {
				builds++
				return nil, nil, boom
			},
		}
		for range 2 {
			if _, err := d.Enhance(context.Background(), issue); !errors.Is(err, boom) {
				t.Fatalf("Enhance = %v, want the build failure", err)
			}
		}
		if builds != 2 {
			t.Errorf("builds = %d, want 2", builds)
		}
		if err := d.Close(); err != nil {
			t.Errorf("Close with nothing built = %v", err)
		}
	})
	t.Run("google-cloud", func(t *testing.T) {
		builds := 0
		d := &DynamicGoogleCloud{
			Config: func(context.Context) (*AssetConfig, error) { return &AssetConfig{Scope: "projects/p"}, nil },
			NewAssets: func(context.Context, string) (AssetLabels, func() error, error) {
				builds++
				return nil, nil, boom
			},
		}
		for range 2 {
			if _, err := d.Enhance(context.Background(), enhance.Issue{CloudResource: bucket()}); !errors.Is(err, boom) {
				t.Fatalf("Enhance = %v, want the build failure", err)
			}
		}
		if builds != 2 {
			t.Errorf("builds = %d, want 2", builds)
		}
		if err := d.Close(); err != nil {
			t.Errorf("Close with nothing built = %v", err)
		}
	})
}

// closeCounter is a closer that reports err and counts its calls.
type closeCounter struct {
	n   int
	err error
}

func (c *closeCounter) close() error { c.n++; return c.err }

// TestDynamicCloseReleasesOnce: Close closes the memoized client, returns
// its error, and forgets it, so a second Close is a no-op and the next
// enhancement builds afresh.
func TestDynamicCloseReleasesOnce(t *testing.T) {
	closeErr := errors.New("close failed")
	t.Run("aws", func(t *testing.T) {
		cc := &closeCounter{err: closeErr}
		builds := 0
		d := &DynamicAWS{
			Config: func(context.Context) (*AWSTagsConfig, error) {
				return &AWSTagsConfig{Backend: aggregatorBackend()}, nil
			},
			NewInventory: func(context.Context, awsinv.Config) (AWSInventory, func() error, error) {
				builds++
				return &fakeInventory{}, cc.close, nil
			},
		}
		if _, err := d.Enhance(context.Background(), enhance.Issue{CloudResource: s3bucket()}); err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); !errors.Is(err, closeErr) || cc.n != 1 {
			t.Fatalf("Close = %v (closes %d)", err, cc.n)
		}
		if err := d.Close(); err != nil || cc.n != 1 {
			t.Errorf("second Close = %v (closes %d), want a no-op", err, cc.n)
		}
		if _, err := d.Enhance(context.Background(), enhance.Issue{CloudResource: s3bucket()}); err != nil || builds != 2 {
			t.Errorf("enhance after Close = %v (builds %d), want a fresh build", err, builds)
		}
	})
	t.Run("azure", func(t *testing.T) {
		cc := &closeCounter{err: closeErr}
		d := &DynamicAzure{
			Config: func(context.Context) (*AzureTagsConfig, error) { return &AzureTagsConfig{}, nil },
			NewInventory: func(context.Context, azureinv.Config) (AzureInventory, func() error, error) {
				return &fakeAzureInventory{}, cc.close, nil
			},
		}
		if _, err := d.Enhance(context.Background(), enhance.Issue{CloudResource: azureVM()}); err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); !errors.Is(err, closeErr) || cc.n != 1 {
			t.Fatalf("Close = %v (closes %d)", err, cc.n)
		}
		if err := d.Close(); err != nil || cc.n != 1 {
			t.Errorf("second Close = %v (closes %d)", err, cc.n)
		}
	})
	t.Run("google-cloud", func(t *testing.T) {
		cc := &closeCounter{err: closeErr}
		d := &DynamicGoogleCloud{
			Config: func(context.Context) (*AssetConfig, error) { return &AssetConfig{Scope: "projects/p"}, nil },
			NewAssets: func(context.Context, string) (AssetLabels, func() error, error) {
				return &fakeAssets{}, cc.close, nil
			},
		}
		if _, err := d.Enhance(context.Background(), enhance.Issue{CloudResource: bucket()}); err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); !errors.Is(err, closeErr) || cc.n != 1 {
			t.Fatalf("Close = %v (closes %d)", err, cc.n)
		}
		if err := d.Close(); err != nil || cc.n != 1 {
			t.Errorf("second Close = %v (closes %d)", err, cc.n)
		}
	})
}

func TestBackendKey(t *testing.T) {
	view := awsinv.Config{ResourceExplorer: &awsinv.ResourceExplorer{ViewARN: "arn:aws:resource-explorer-2:v"}}
	keys := map[string]string{
		"aggregator": backendKey(aggregatorBackend()),
		"view":       backendKey(view),
		"none":       backendKey(awsinv.Config{}),
	}
	if keys["none"] != "" {
		t.Errorf("empty backend key = %q", keys["none"])
	}
	if keys["view"] == keys["aggregator"] || keys["view"] == "" {
		t.Errorf("keys collide: %v", keys)
	}
	same := awsinv.Config{ResourceExplorer: &awsinv.ResourceExplorer{ViewARN: "arn:aws:resource-explorer-2:v"}}
	if backendKey(same) != keys["view"] {
		t.Error("equal view configs keyed differently")
	}
}

func TestRepositoryFrom(t *testing.T) {
	keys := defaultKeys(LabelKeys{})
	cases := []struct {
		name   string
		labels map[string]string
		want   *source.RepositoryRef
	}{
		{name: "no labels"},
		{name: "org without name", labels: map[string]string{keys.Org: "acme"}},
		{
			name:   "triple on the default host",
			labels: map[string]string{keys.Org: "acme", keys.Name: "web"},
			want:   &source.RepositoryRef{Provider: "github", Owner: "acme", Name: "web", URL: "https://git.example/acme/web"},
		},
		{
			name:   "provider label overrides the default",
			labels: map[string]string{keys.Org: "acme", keys.Name: "web", keys.Provider: "gitlab"},
			want:   &source.RepositoryRef{Provider: "gitlab", Owner: "acme", Name: "web", URL: "https://git.example/acme/web"},
		},
		{
			name:   "schemeless URL label wins",
			labels: map[string]string{keys.URL: "//ghe.acme/acme/web", keys.Org: "x", keys.Name: "y"},
			want:   &source.RepositoryRef{Provider: "github", URL: "https://ghe.acme/acme/web"},
		},
		{
			name:   "full URL kept",
			labels: map[string]string{keys.URL: "http://forge.local/a/b"},
			want:   &source.RepositoryRef{Provider: "github", URL: "http://forge.local/a/b"},
		},
	}
	for _, c := range cases {
		got := repositoryFrom(c.labels, keys, "github", "git.example")
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: repositoryFrom = %+v, want nil", c.name, got)
		case c.want != nil && (got == nil || *got != *c.want):
			t.Errorf("%s: repositoryFrom = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestAttributesOfABareResource: a resource that names nothing beyond its
// identifier carries no attributes at all, rather than an empty map.
func TestAttributesOfABareResource(t *testing.T) {
	if got := attributes(&source.CloudResource{Name: "//x"}); got != nil {
		t.Errorf("attributes(bare) = %v, want nil", got)
	}
	if got := attributes(&source.CloudResource{Project: "projects/demo"}); got["gcp-project"] != "demo" {
		t.Errorf("attributes(project) = %v", got)
	}
	aws, err := NewAWSTags(&fakeInventory{}, TagsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := aws.attributes(&source.CloudResource{Name: "arn:x"}, nil); got != nil {
		t.Errorf("cloudTags.attributes(bare) = %v, want nil", got)
	}
}
