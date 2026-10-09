// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package awsinv

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/configservice"
	cstypes "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"
)

// fakeAWS is an in-process AWS endpoint for the two inventory services: it
// answers each operation (by X-Amz-Target for Config, by path for Resource
// Explorer) with a canned status and body, and records what it was asked.
type fakeAWS struct {
	mu      sync.Mutex
	answers map[string]struct {
		status int
		body   string
	}
	seen []string
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := r.Header.Get("X-Amz-Target")
	if op == "" {
		op = r.URL.Path
	}
	if i := strings.LastIndex(op, "."); i >= 0 {
		op = op[i+1:]
	}
	op = strings.TrimPrefix(op, "/")
	f.mu.Lock()
	f.seen = append(f.seen, op)
	a, ok := f.answers[op]
	f.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"UnknownOperationException","message":"`+op+`"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if a.status != 0 {
		w.WriteHeader(a.status)
	}
	_, _ = io.WriteString(w, a.body)
}

func (f *fakeAWS) answer(op string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.answers == nil {
		f.answers = map[string]struct {
			status int
			body   string
		}{}
	}
	f.answers[op] = struct {
		status int
		body   string
	}{status, body}
}

// isolate points both services at srv, with static environment credentials,
// empty shared files and no instance metadata service.
func isolate(t *testing.T, srv *httptest.Server) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": empty, "AWS_SHARED_CREDENTIALS_FILE": empty, "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_PROFILE": "", "AWS_ACCESS_KEY_ID": "AKIATEST", "AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "", "AWS_ENDPOINT_URL": srv.URL,
		"AWS_MAX_ATTEMPTS": "1",
	} {
		t.Setenv(k, v)
	}
}

const viewARN = "arn:aws:resource-explorer-2:eu-west-2:123456789012:view/main/abc"

// TestNewAggregatorEndToEnd: New verifies the aggregator through the SDK,
// and lookups then run as aggregator queries.
func TestNewAggregatorEndToEnd(t *testing.T) {
	f := &fakeAWS{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	isolate(t, srv)

	cfg := Config{ConfigAggregator: &ConfigAggregator{Name: "org", Region: "eu-west-2"}}
	f.answer("DescribeConfigurationAggregators", 0, `{"ConfigurationAggregators":[]}`)
	if _, err := New(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("New with no aggregator = %v", err)
	}

	f.answer("DescribeConfigurationAggregators", 0,
		`{"ConfigurationAggregators":[{"ConfigurationAggregatorName":"org"}]}`)
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer func() { _ = c.Close() }()
	f.answer("SelectAggregateResourceConfig", 0,
		`{"Results":["{\"resourceName\":\"legacy\",\"tags\":[{\"key\":\"team\",\"value\":\"orders\"}]}"]}`)
	tags, err := c.TagsFor(context.Background(), bucketARN)
	if err != nil {
		t.Fatalf("TagsFor = %v", err)
	}
	if tags.Name != "legacy" || tags.Tags["team"] != "orders" {
		t.Errorf("tags = %+v", tags)
	}
}

// TestNewViewEndToEnd: New verifies the view includes tags, and lookups then
// search it, decoding the tags document the SDK hands back.
func TestNewViewEndToEnd(t *testing.T) {
	f := &fakeAWS{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	isolate(t, srv)

	cfg := Config{ResourceExplorer: &ResourceExplorer{ViewARN: viewARN}}
	f.answer("GetView", 0, `{"View":{"ViewArn":"`+viewARN+`","IncludedProperties":[]}}`)
	if _, err := New(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "tags property") {
		t.Fatalf("New with a tagless view = %v", err)
	}

	f.answer("GetView", 0, `{"View":{"ViewArn":"`+viewARN+`","IncludedProperties":[{"Name":"tags"}]}}`)
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	f.answer("Search", 0, `{"Resources":[{"Arn":"`+bucketARN+`","Properties":[`+
		`{"Name":"tags","Data":[{"Key":"team","Value":"orders"}]}]}]}`)
	tags, err := c.TagsFor(context.Background(), bucketARN)
	if err != nil {
		t.Fatalf("TagsFor = %v", err)
	}
	if tags.Name != bucketARN || tags.Tags["team"] != "orders" {
		t.Errorf("tags = %+v", tags)
	}

	f.answer("GetView", http.StatusForbidden, `{"message":"denied"}`)
	if _, err := New(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "get view") {
		t.Errorf("New with a refused view = %v", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Error("New accepted an empty config")
	}
	bad := Config{ResourceExplorer: &ResourceExplorer{ViewARN: "arn:aws:s3:::nope"}}
	if _, err := New(context.Background(), bad); err == nil {
		t.Error("New accepted a non-view ARN")
	}
}

func TestRegion(t *testing.T) {
	agg := Config{ConfigAggregator: &ConfigAggregator{Name: "org", Region: "us-east-2"}}
	if r, err := agg.region(); err != nil || r != "us-east-2" {
		t.Errorf("aggregator region = %q, %v", r, err)
	}
	view := Config{ResourceExplorer: &ResourceExplorer{ViewARN: viewARN}}
	if r, err := view.region(); err != nil || r != "eu-west-2" {
		t.Errorf("view region = %q, %v", r, err)
	}
}

// describeFails is a Config API whose aggregator lookup fails.
type describeFails struct{ fakeConfig }

func (describeFails) DescribeConfigurationAggregators(context.Context,
	*configservice.DescribeConfigurationAggregatorsInput, ...func(*configservice.Options),
) (*configservice.DescribeConfigurationAggregatorsOutput, error) {
	return nil, errors.New("throttled")
}

// describeFound is a Config API that knows the aggregator.
type describeFound struct{ fakeConfig }

func (describeFound) DescribeConfigurationAggregators(context.Context,
	*configservice.DescribeConfigurationAggregatorsInput, ...func(*configservice.Options),
) (*configservice.DescribeConfigurationAggregatorsOutput, error) {
	return &configservice.DescribeConfigurationAggregatorsOutput{
		ConfigurationAggregators: []cstypes.ConfigurationAggregator{{}},
	}, nil
}

// nilView is an explorer whose view answer carries no view.
type nilView struct{ fakeExplorer }

func (nilView) GetView(context.Context, *resourceexplorer2.GetViewInput,
	...func(*resourceexplorer2.Options)) (*resourceexplorer2.GetViewOutput, error) {
	return &resourceexplorer2.GetViewOutput{}, nil
}

func TestVerifyAggregator(t *testing.T) {
	if err := (&Client{aggregator: "org", config: &describeFails{}}).verify(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "describe aggregator org") {
		t.Errorf("verify with a failing describe = %v", err)
	}
	if err := (&Client{aggregator: "org", config: &describeFound{}}).verify(context.Background()); err != nil {
		t.Errorf("verify with the aggregator present = %v", err)
	}
	if err := (&Client{aggregator: "org", config: &fakeConfig{}}).verify(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("verify with no aggregator = %v", err)
	}
	if err := (&Client{viewARN: viewARN, explorer: &nilView{}}).verify(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "tags property") {
		t.Errorf("verify with no view = %v", err)
	}
}

// TestConfigTagsDecodeAndNameFallback: an unparseable row is an error, and a
// row without a resource name is named by its ARN.
func TestConfigTagsDecodeAndNameFallback(t *testing.T) {
	c := &Client{aggregator: "org", config: &fakeConfig{results: []string{"{not json"}}}
	if _, err := c.TagsFor(context.Background(), bucketARN); err == nil ||
		!strings.Contains(err.Error(), "decode result") {
		t.Errorf("unparseable row = %v", err)
	}
	c = &Client{aggregator: "org", config: &fakeConfig{results: []string{`{"tags":{"team":"x","n":1}}`}}}
	tags, err := c.TagsFor(context.Background(), bucketARN)
	if err != nil || tags.Name != bucketARN || tags.Tags["team"] != "x" || len(tags.Tags) != 1 {
		t.Errorf("nameless row = %+v, %v", tags, err)
	}
}

func TestNormalizeTagsShapes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want map[string]string
	}{
		{"nil", nil, nil},
		{"string", "team=x", nil},
		{"empty map", map[string]any{}, nil},
		{"map drops non-strings", map[string]any{"a": "1", "b": 2}, map[string]string{"a": "1"}},
		{"list in either casing, junk skipped", []any{
			map[string]any{"key": "a", "value": "1"},
			map[string]any{"Key": "b", "Value": "2"},
			map[string]any{"Key": "c"},
			map[string]any{"value": "orphan"},
			"not a pair",
		}, map[string]string{"a": "1", "b": "2", "c": ""}},
	}
	for _, c := range cases {
		got := normalizeTags(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%s: normalizeTags = %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: tag %s = %q, want %q", c.name, k, got[k], v)
			}
		}
	}
}
