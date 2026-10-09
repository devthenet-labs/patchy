// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package kubecfg_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
)

// writeKubeconfig writes a two-context kubeconfig pointing at server and
// returns its path.
func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
users:
- name: u
  user:
    token: test-token
contexts:
- name: ctx-a
  context:
    cluster: c
    user: u
    namespace: from-context
- name: ctx-b
  context:
    cluster: c
    user: u
current-context: ctx-a
`, server)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConnectResolvesNamespace(t *testing.T) {
	path := writeKubeconfig(t, "https://cluster.invalid:6443")
	cases := []struct {
		name string
		opts kubecfg.Options
		want string
	}{
		{"context namespace", kubecfg.Options{Kubeconfig: path}, "from-context"},
		{"flag wins over context", kubecfg.Options{Kubeconfig: path, Namespace: "flag"}, "flag"},
		{"all namespaces wins over both", kubecfg.Options{Kubeconfig: path, Namespace: "flag", AllNamespaces: true}, ""},
		// A context without a namespace falls back to "default", as kubectl does.
		{"other context", kubecfg.Options{Kubeconfig: path, Context: "ctx-b"}, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := kubecfg.Connect(tc.opts)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if env.Namespace != tc.want {
				t.Errorf("Namespace = %q, want %q", env.Namespace, tc.want)
			}
			if env.Client == nil || env.Config == nil {
				t.Fatal("Connect returned no client or config")
			}
			if env.Config.Host != "https://cluster.invalid:6443" {
				t.Errorf("Config.Host = %q", env.Config.Host)
			}
			if env.Config.BearerToken != "test-token" {
				t.Errorf("BearerToken = %q, want the kubeconfig's token", env.Config.BearerToken)
			}
		})
	}
}

func TestConnectErrors(t *testing.T) {
	path := writeKubeconfig(t, "https://cluster.invalid:6443")
	for name, opts := range map[string]kubecfg.Options{
		"missing file":    {Kubeconfig: filepath.Join(t.TempDir(), "absent")},
		"unknown context": {Kubeconfig: path, Context: "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			if env, err := kubecfg.Connect(opts); err == nil {
				t.Fatalf("Connect succeeded: %+v", env)
			}
		})
	}
}

func TestScope(t *testing.T) {
	if got := (&kubecfg.Env{}).Scope(); got != "all namespaces" {
		t.Errorf("Scope() = %q", got)
	}
	if got := (&kubecfg.Env{Namespace: "patchy"}).Scope(); got != "namespace patchy" {
		t.Errorf("Scope() = %q", got)
	}
}

func rowWith(t *testing.T, meta any) metav1.TableRow {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return metav1.TableRow{Object: runtime.RawExtension{Raw: raw}}
}

func TestRowMetaAndName(t *testing.T) {
	good := rowWith(t, map[string]any{"metadata": map[string]any{"name": "fnd-1", "namespace": "patchy"}})
	if m := kubecfg.RowMeta(good); m == nil || m.Name != "fnd-1" || m.Namespace != "patchy" {
		t.Errorf("RowMeta = %+v", m)
	}
	if got := kubecfg.RowName(good); got != "fnd-1" {
		t.Errorf("RowName = %q", got)
	}

	for name, row := range map[string]metav1.TableRow{
		"no object": {Cells: []any{"fnd-1"}},
		"not json":  {Object: runtime.RawExtension{Raw: []byte("{not json")}},
	} {
		t.Run(name, func(t *testing.T) {
			if m := kubecfg.RowMeta(row); m != nil {
				t.Errorf("RowMeta = %+v, want nil", m)
			}
			if got := kubecfg.RowName(row); got != "" {
				t.Errorf("RowName = %q, want empty", got)
			}
		})
	}
}

// fakeAPI records the table requests it served.
type fakeAPI struct {
	mu   sync.Mutex
	reqs []*http.Request
	body string
	code int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.code != 0 {
		w.WriteHeader(f.code)
	}
	_, _ = w.Write([]byte(f.body))
}

func (f *fakeAPI) last(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no request reached the server")
	}
	return f.reqs[len(f.reqs)-1]
}

const tableBody = `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
"columnDefinitions":[{"name":"Name","type":"string"}],
"rows":[{"cells":["fnd-1"],"object":{"metadata":{"name":"fnd-1"}}}]}`

func TestTableRequestShape(t *testing.T) {
	cases := []struct {
		name      string
		namespace string
		names     []string
		selector  string
		wantPath  string
		wantSel   string
	}{
		{"namespaced list with selector", "patchy", nil, "a=b",
			"/apis/patchy.bitwisemedia.uk/v1alpha1/namespaces/patchy/findings", "a=b"},
		{"single name is a get", "patchy", []string{"fnd-1"}, "a=b",
			"/apis/patchy.bitwisemedia.uk/v1alpha1/namespaces/patchy/findings/fnd-1", ""},
		{"several names list everything", "patchy", []string{"x", "y"}, "",
			"/apis/patchy.bitwisemedia.uk/v1alpha1/namespaces/patchy/findings", ""},
		{"all namespaces", "", nil, "",
			"/apis/patchy.bitwisemedia.uk/v1alpha1/findings", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{body: tableBody}
			srv := httptest.NewServer(api)
			defer srv.Close()

			env := &kubecfg.Env{Config: &rest.Config{Host: srv.URL}, Namespace: tc.namespace}
			table, err := env.Table(context.Background(), "findings", tc.names, tc.selector)
			if err != nil {
				t.Fatalf("Table: %v", err)
			}
			if len(table.Rows) != 1 || kubecfg.RowName(table.Rows[0]) != "fnd-1" {
				t.Errorf("rows = %+v", table.Rows)
			}

			req := api.last(t)
			if req.URL.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", req.URL.Path, tc.wantPath)
			}
			q := req.URL.Query()
			if got := q.Get("labelSelector"); got != tc.wantSel {
				t.Errorf("labelSelector = %q, want %q", got, tc.wantSel)
			}
			if got := q.Get("includeObject"); got != "Metadata" {
				t.Errorf("includeObject = %q, want Metadata", got)
			}
			if accept := req.Header.Get("Accept"); !strings.Contains(accept, "as=Table") {
				t.Errorf("Accept = %q, want a Table negotiation", accept)
			}
		})
	}
}

func TestTableErrors(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		api := &fakeAPI{code: http.StatusForbidden,
			body: `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"nope"}`}
		srv := httptest.NewServer(api)
		defer srv.Close()
		env := &kubecfg.Env{Config: &rest.Config{Host: srv.URL}, Namespace: "patchy"}
		if _, err := env.Table(context.Background(), "findings", nil, ""); err == nil {
			t.Fatal("a 403 was not reported")
		}
	})
	t.Run("undecodable body", func(t *testing.T) {
		api := &fakeAPI{body: `[1,2,3]`}
		srv := httptest.NewServer(api)
		defer srv.Close()
		env := &kubecfg.Env{Config: &rest.Config{Host: srv.URL}, Namespace: "patchy"}
		_, err := env.Table(context.Background(), "findings", nil, "")
		if err == nil || !strings.Contains(err.Error(), "decode table for findings") {
			t.Fatalf("err = %v, want a decode failure", err)
		}
	})
	t.Run("bad config", func(t *testing.T) {
		env := &kubecfg.Env{Config: &rest.Config{Host: "http://[::1"}, Namespace: "patchy"}
		_, err := env.Table(context.Background(), "findings", nil, "")
		if err == nil || !strings.Contains(err.Error(), "rest client") {
			t.Fatalf("err = %v, want a rest client failure", err)
		}
	})
}
