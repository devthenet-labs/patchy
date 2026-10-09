// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package gcpasset

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	asset "cloud.google.com/go/asset/apiv1"
	"cloud.google.com/go/asset/apiv1/assetpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeInventory is an in-memory Asset Inventory: it answers each query from a
// table keyed by the query string, or with a fixed error.
type fakeInventory struct {
	assetpb.UnimplementedAssetServiceServer

	mu       sync.Mutex
	results  map[string][]*assetpb.ResourceSearchResult
	errs     map[string]error
	requests []*assetpb.SearchAllResourcesRequest
}

func (f *fakeInventory) SearchAllResources(
	_ context.Context, req *assetpb.SearchAllResourcesRequest,
) (*assetpb.SearchAllResourcesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if err := f.errs[req.GetQuery()]; err != nil {
		return nil, err
	}
	return &assetpb.SearchAllResourcesResponse{Results: f.results[req.GetQuery()]}, nil
}

func (f *fakeInventory) queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.requests))
	for _, r := range f.requests {
		out = append(out, r.GetQuery())
	}
	return out
}

// newTestClient serves fake over an in-memory listener and returns a Client
// wired to it, scoped to projects/demo.
func newTestClient(t *testing.T, fake *fakeInventory) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	assetpb.RegisterAssetServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	api, err := asset.NewClient(context.Background(), option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{api: api, scope: "projects/demo"}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestValidateScope(t *testing.T) {
	cases := []struct {
		scope string
		ok    bool
	}{
		{"organizations/123", true},
		{"folders/456", true},
		{"projects/foo", true},
		{"projects/", false},
		{"organizations", false},
		{"", false},
		{"buckets/x", false},
	}
	for _, c := range cases {
		err := ValidateScope(c.scope)
		if (err == nil) != c.ok {
			t.Errorf("ValidateScope(%q) = %v, want ok %v", c.scope, err, c.ok)
		}
	}
}

func TestNewRejectsBadScope(t *testing.T) {
	c, err := New(context.Background(), "nope")
	if err == nil || c != nil {
		t.Fatalf("New(bad scope) = %v, %v; want error", c, err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q does not name the scope", err)
	}
}

func TestCloseWithoutAPI(t *testing.T) {
	if err := (&Client{}).Close(); err != nil {
		t.Errorf("Close() on an unbuilt client = %v, want nil", err)
	}
}

func TestLabelsFor(t *testing.T) {
	const name = "//compute.googleapis.com/projects/demo/zones/a/instances/web-1"
	byName := `name="` + name + `"`
	byDisplay := `displayName="web-1"`
	hit := func(n string, labels map[string]string) *assetpb.ResourceSearchResult {
		return &assetpb.ResourceSearchResult{Name: n, Labels: labels}
	}

	cases := []struct {
		name        string
		resource    string
		display     string
		results     map[string][]*assetpb.ResourceSearchResult
		errs        map[string]error
		wantLabels  map[string]string
		wantName    string
		wantErr     bool
		wantMissing bool // ErrNotFound
		wantQueries []string
	}{
		{
			name:        "empty resource name is not found without a call",
			wantErr:     true,
			wantMissing: true,
		},
		{
			name:        "exact name hit",
			resource:    name,
			display:     "web-1",
			results:     map[string][]*assetpb.ResourceSearchResult{byName: {hit(name, map[string]string{"team": "orders"})}},
			wantLabels:  map[string]string{"team": "orders"},
			wantName:    name,
			wantQueries: []string{byName},
		},
		{
			name:        "exact miss without display name is final",
			resource:    name,
			wantErr:     true,
			wantMissing: true,
			wantQueries: []string{byName},
		},
		{
			name:     "exact miss falls back to a unique display-name hit",
			resource: name,
			display:  "web-1",
			results: map[string][]*assetpb.ResourceSearchResult{
				byDisplay: {hit("//other/web-1", map[string]string{"team": "payments"})},
			},
			wantLabels:  map[string]string{"team": "payments"},
			wantName:    "//other/web-1",
			wantQueries: []string{byName, byDisplay},
		},
		{
			name:     "ambiguous display name is not found",
			resource: name,
			display:  "web-1",
			results: map[string][]*assetpb.ResourceSearchResult{
				byDisplay: {hit("//a/web-1", nil), hit("//b/web-1", nil)},
			},
			wantErr:     true,
			wantMissing: true,
			wantQueries: []string{byName, byDisplay},
		},
		{
			name:        "NotFound status is permanent and skips the fallback",
			resource:    name,
			errs:        map[string]error{byName: status.Error(codes.NotFound, "gone")},
			wantErr:     true,
			wantMissing: true,
			wantQueries: []string{byName},
		},
		{
			name:        "InvalidArgument status is permanent",
			resource:    name,
			errs:        map[string]error{byName: status.Error(codes.InvalidArgument, "bad query")},
			wantErr:     true,
			wantMissing: true,
			wantQueries: []string{byName},
		},
		{
			name:        "PermissionDenied stays retryable and skips the fallback",
			resource:    name,
			display:     "web-1",
			errs:        map[string]error{byName: status.Error(codes.PermissionDenied, "not yet")},
			wantErr:     true,
			wantQueries: []string{byName},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeInventory{results: c.results, errs: c.errs}
			cl := newTestClient(t, fake)
			got, err := cl.LabelsFor(context.Background(), c.resource, c.display)
			if (err != nil) != c.wantErr {
				t.Fatalf("LabelsFor err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				if errors.Is(err, ErrNotFound) != c.wantMissing {
					t.Errorf("errors.Is(%v, ErrNotFound) = %v, want %v", err, !c.wantMissing, c.wantMissing)
				}
				if got != nil {
					t.Errorf("labels = %+v alongside an error", got)
				}
			} else {
				if got.Name != c.wantName || len(got.Labels) != len(c.wantLabels) {
					t.Fatalf("labels = %+v, want name %q labels %v", got, c.wantName, c.wantLabels)
				}
				for k, v := range c.wantLabels {
					if got.Labels[k] != v {
						t.Errorf("label %s = %q, want %q", k, got.Labels[k], v)
					}
				}
			}
			if q := fake.queries(); strings.Join(q, "|") != strings.Join(c.wantQueries, "|") {
				t.Errorf("queries = %q, want %q", q, c.wantQueries)
			}
		})
	}
}

// TestSearchAsksForLabelsOnly: the request is scoped and asks only for the
// name and labels, keeping the permission and the response narrow.
func TestSearchAsksForLabelsOnly(t *testing.T) {
	fake := &fakeInventory{}
	cl := newTestClient(t, fake)
	_, _ = cl.LabelsFor(context.Background(), "//x", "")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	req := fake.requests[0]
	if req.GetScope() != "projects/demo" {
		t.Errorf("scope = %q", req.GetScope())
	}
	if paths := strings.Join(req.GetReadMask().GetPaths(), ","); paths != "name,labels" {
		t.Errorf("read mask = %q, want name,labels", paths)
	}
}
