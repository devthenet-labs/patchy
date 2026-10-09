// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/resource"
)

// tableRow is one row the fake API server renders: its object metadata and
// its cells in column order (Name, Severity, Priority, Phase).
type tableRow struct {
	name                      string
	age                       time.Duration
	severity, priority, phase string
}

// tableAPI is an API server that answers Table requests the way a real one
// does for the patchy CRDs: rows carry the print columns and, because the CLI
// asks for includeObject=Metadata, each row's object metadata.
type tableAPI struct {
	mu     sync.Mutex
	rows   map[string][]tableRow // by plural
	forbid map[string]bool       // plurals answered 403
	paths  []string
}

func (a *tableAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.paths = append(a.paths, r.URL.Path+"?"+r.URL.RawQuery)
	a.mu.Unlock()

	// /apis/<group>/<version>/namespaces/<ns>/<plural>[/<name>]
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/"), "/")
	plural := parts[4]
	w.Header().Set("Content-Type", "application/json")
	if a.forbid[plural] {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
			Message: plural + " is forbidden",
		})
		return
	}
	table := metav1.Table{
		TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1"},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string"},
			{Name: "Severity", Type: "string"},
			{Name: "Priority", Type: "string"},
			{Name: "Phase", Type: "string"},
			{Name: "Issue", Type: "string", Priority: 1},
		},
	}
	for _, row := range a.rows[plural] {
		if len(parts) > 5 && parts[5] != row.name {
			continue
		}
		meta, _ := json.Marshal(metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
			Name: row.name, Namespace: testNamespace,
			CreationTimestamp: metav1.NewTime(testClock.Add(-row.age)),
		}})
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []any{row.name, row.severity, row.priority, row.phase, "#1"},
			Object: runtime.RawExtension{Raw: meta},
		})
	}
	_ = json.NewEncoder(w).Encode(table)
}

// tableHarness is a harness whose Table requests reach api and whose object
// reads reach the fake client holding objs.
func tableHarness(t *testing.T, api *tableAPI, objs ...*v1alpha1.Finding) *harness {
	t.Helper()
	cobjs := make([]clientObject, 0, len(objs))
	for _, o := range objs {
		cobjs = append(cobjs, o)
	}
	h := newHarness(t, cobjs...)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	h.opts.WithEnv(&kubecfg.Env{Client: h.client, Config: &rest.Config{Host: srv.URL}, Namespace: testNamespace})
	return h
}

func findingRows() []tableRow {
	return []tableRow{
		{"fnd-a", 3 * time.Hour, "low", "medium", "Queued"},
		{"fnd-b", 1 * time.Hour, "critical", "low", "AwaitingApproval"},
		{"fnd-c", 2 * time.Hour, "medium", "critical", "Failed"},
		{"fnd-d", 2 * time.Hour, "critical", "", "Dismissed"},
	}
}

// names reads the first column of a plain table, skipping the header.
func names(out string) []string {
	var got []string
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if i == 0 || line == "" {
			continue
		}
		got = append(got, strings.Fields(line)[0])
	}
	return got
}

func TestGetTableSorting(t *testing.T) {
	cases := []struct {
		sortBy string
		want   string
	}{
		// Newest first; the two equally old rows fall back to name.
		{"age", "fnd-b fnd-c fnd-d fnd-a"},
		{"name", "fnd-a fnd-b fnd-c fnd-d"},
		// Ranked by seriousness, not alphabetically; ties by name.
		{"severity", "fnd-b fnd-d fnd-c fnd-a"},
		// An empty priority sorts after every level.
		{"priority", "fnd-c fnd-a fnd-b fnd-d"},
		{"phase", "fnd-b fnd-d fnd-c fnd-a"},
		{"SEVERITY", "fnd-b fnd-d fnd-c fnd-a"},
	}
	for _, tc := range cases {
		t.Run(tc.sortBy, func(t *testing.T) {
			api := &tableAPI{rows: map[string][]tableRow{"findings": findingRows()}}
			h := tableHarness(t, api)
			if err := h.execRoot(t, "get", "findings", "--sort-by", tc.sortBy); err != nil {
				t.Fatalf("get: %v", err)
			}
			if got := strings.Join(names(h.out.String()), " "); got != tc.want {
				t.Errorf("order = %s, want %s\n%s", got, tc.want, h.out.String())
			}
			if strings.Contains(h.out.String(), "ISSUE") {
				t.Errorf("a priority>0 column showed without -o wide:\n%s", h.out.String())
			}
		})
	}
}

func TestGetTableWideAndSelector(t *testing.T) {
	api := &tableAPI{rows: map[string][]tableRow{"findings": findingRows()}}
	h := tableHarness(t, api)
	err := h.execRoot(t, "get", "findings", "-o", "wide", "--severity", "critical,HIGH", "--source", "ghas")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(h.out.String(), "ISSUE") {
		t.Errorf("-o wide dropped the priority>0 column:\n%s", h.out.String())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.paths) != 1 {
		t.Fatalf("requests = %v", api.paths)
	}
	// The server-side filters travel as one label selector.
	for _, want := range []string{"labelSelector=", "severity+in+%28critical%2Chigh%29", "source%3Dghas"} {
		if !strings.Contains(api.paths[0], want) {
			t.Errorf("request %q missing %q", api.paths[0], want)
		}
	}
}

func TestGetTableNames(t *testing.T) {
	t.Run("one name is a get", func(t *testing.T) {
		api := &tableAPI{rows: map[string][]tableRow{"findings": findingRows()}}
		h := tableHarness(t, api)
		if err := h.execRoot(t, "get", "findings", "fnd-c"); err != nil {
			t.Fatalf("get: %v", err)
		}
		if got := strings.Join(names(h.out.String()), " "); got != "fnd-c" {
			t.Errorf("rows = %s", got)
		}
	})
	t.Run("several names narrow a list", func(t *testing.T) {
		api := &tableAPI{rows: map[string][]tableRow{"findings": findingRows()}}
		h := tableHarness(t, api)
		if err := h.execRoot(t, "get", "findings", "fnd-d", "fnd-a", "--sort-by", "name"); err != nil {
			t.Fatalf("get: %v", err)
		}
		if got := strings.Join(names(h.out.String()), " "); got != "fnd-a fnd-d" {
			t.Errorf("rows = %s", got)
		}
	})
}

// TestGetTableObjectFilters: the finding-only filters need the objects, so
// they are read from the cluster and applied to the server-rendered rows.
func TestGetTableObjectFilters(t *testing.T) {
	objs := []*v1alpha1.Finding{
		testFinding("fnd-a", v1alpha1.PhaseQueued),
		testFinding("fnd-b", v1alpha1.PhaseAwaitingApproval),
		testFinding("fnd-c", v1alpha1.PhaseFailed),
		testFinding("fnd-d", v1alpha1.PhaseDismissed),
	}
	api := &tableAPI{rows: map[string][]tableRow{"findings": findingRows()}}
	h := tableHarness(t, api, objs...)
	if err := h.execRoot(t, "get", "findings", "--phase", "failed,awaitingapproval", "--sort-by", "name"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := strings.Join(names(h.out.String()), " "); got != "fnd-b fnd-c" {
		t.Errorf("rows = %s\n%s", got, h.out.String())
	}

	h.out.Reset()
	if err := h.execRoot(t, "get", "findings", "--phase", "Remediated"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if h.out.Len() != 0 || !strings.Contains(h.errOut.String(), "No findings found in namespace patchy.") {
		t.Errorf("stdout %q, stderr %q", h.out.String(), h.errOut.String())
	}
}

func TestGetTableErrors(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		api := &tableAPI{forbid: map[string]bool{"findings": true}}
		h := tableHarness(t, api)
		err := h.execRoot(t, "get", "findings")
		if exitCode(err) != ExitDenied {
			t.Fatalf("err = %v (exit %d), want exit %d", err, exitCode(err), ExitDenied)
		}
	})
	t.Run("object filter list fails", func(t *testing.T) {
		api := &tableAPI{rows: map[string][]tableRow{"findings": findingRows()}}
		h := tableHarness(t, api)
		h.opts.env.Client = &failingListClient{Client: h.client, err: errors.New("list refused")}
		err := h.execRoot(t, "get", "findings", "--suspended")
		if err == nil || !strings.Contains(err.Error(), "list refused") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad severity", func(t *testing.T) {
		h := tableHarness(t, &tableAPI{})
		err := h.execRoot(t, "get", "findings", "--severity", "urgent")
		if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), `unknown severity "urgent"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad format", func(t *testing.T) {
		h := tableHarness(t, &tableAPI{})
		if err := h.execRoot(t, "get", "all", "-o", "xml"); exitCode(err) != ExitUsage {
			t.Fatalf("err = %v, want usage", err)
		}
	})
	t.Run("finding filter on a non-run noun", func(t *testing.T) {
		h := tableHarness(t, &tableAPI{})
		err := h.execRoot(t, "get", "forges", "--finding", "fnd-1")
		if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "--finding only applies") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestGetAllTablesPartialFailure: a kind the caller may not list is reported
// and skipped, never hiding the kinds that did come back.
func TestGetAllTablesPartialFailure(t *testing.T) {
	api := &tableAPI{
		rows: map[string][]tableRow{
			"findings":       findingRows(),
			"investigations": {{"fnd-b-inv-1", time.Hour, "", "", "Complete"}},
		},
		forbid: map[string]bool{"forges": true, "integrations": true},
	}
	h := tableHarness(t, api)
	if err := h.execRoot(t, "get", "all", "-o", "markdown"); err != nil {
		t.Fatalf("get all: %v", err)
	}
	got := h.out.String()
	for _, want := range []string{"## Findings", "fnd-a", "## Investigations", "fnd-b-inv-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// Empty kinds vanish rather than printing a heading over nothing.
	if strings.Contains(got, "## Remediations") || strings.Contains(got, "## Forges") {
		t.Errorf("empty or failed kinds printed:\n%s", got)
	}
	for _, want := range []string{"forges: ", "integrations: "} {
		if !strings.Contains(h.errOut.String(), want) {
			t.Errorf("stderr missing %q: %q", want, h.errOut.String())
		}
	}
}

func TestGetAllTablesEverythingFails(t *testing.T) {
	forbid := map[string]bool{}
	for _, k := range resource.Kinds {
		forbid[k.Plural] = true
	}
	h := tableHarness(t, &tableAPI{forbid: forbid})
	err := h.execRoot(t, "get", "all")
	if err == nil || !strings.Contains(err.Error(), "no patchy resource could be listed in namespace patchy") {
		t.Fatalf("err = %v", err)
	}
	// The first failure's reason is kept, so a forbidden run still exits 4.
	if exitCode(err) != ExitDenied {
		t.Errorf("exit = %d, want %d", exitCode(err), ExitDenied)
	}
}

func TestGetAllTablesEmptyCluster(t *testing.T) {
	h := tableHarness(t, &tableAPI{})
	if err := h.execRoot(t, "get", "all"); err != nil {
		t.Fatalf("get all: %v", err)
	}
	if h.out.Len() != 0 || !strings.Contains(h.errOut.String(), "No patchy resources found in namespace patchy.") {
		t.Errorf("stdout %q, stderr %q", h.out.String(), h.errOut.String())
	}
}

func TestGetAllObjectsEverythingFails(t *testing.T) {
	h := newHarness(t)
	h.opts.env.Client = &failingListClient{Client: h.client, err: errors.New("list refused")}
	err := h.execRoot(t, "get", "all", "-o", "yaml")
	if err == nil || !strings.Contains(err.Error(), "no patchy resource could be listed") ||
		!strings.Contains(err.Error(), "list refused") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errOut.String(), "findings: list refused") {
		t.Errorf("stderr = %q", h.errOut.String())
	}
}

func TestGetObjectsByNameMissing(t *testing.T) {
	h := newHarness(t, testFinding("fnd-1", v1alpha1.PhaseQueued))
	err := h.execRoot(t, "get", "findings", "fnd-1", "absent", "-o", "name")
	if exitCode(err) != ExitNotFound {
		t.Fatalf("err = %v (exit %d), want not found", err, exitCode(err))
	}
	if h.out.Len() != 0 {
		t.Errorf("a partial result was printed: %q", h.out.String())
	}
}

func TestMatchesFinding(t *testing.T) {
	inv := func(f *v1alpha1.Finding) {
		f.Status.Investigation = &v1alpha1.InvestigationSummary{Recommendation: v1alpha1.RecommendationManual}
		f.Status.Priority = v1alpha1.LevelHigh
		f.Spec.Repository = &v1alpha1.FindingRepository{Name: "acme/Orders"}
	}
	full := testFinding("fnd-1", v1alpha1.PhaseQueued, inv)
	bare := testFinding("fnd-2", v1alpha1.PhaseQueued)
	cases := []struct {
		name string
		f    *getFlags
		fnd  *v1alpha1.Finding
		want bool
	}{
		{"verdict matches", &getFlags{verdict: []string{"MANUAL"}}, full, true},
		{"verdict differs", &getFlags{verdict: []string{"remediate"}}, full, false},
		{"no investigation has no verdict", &getFlags{verdict: []string{"manual"}}, bare, false},
		{"priority matches", &getFlags{priority: []string{"high"}}, full, true},
		{"priority differs", &getFlags{priority: []string{"low"}}, full, false},
		{"repo substring, any case", &getFlags{repo: "orders"}, full, true},
		{"repo differs", &getFlags{repo: "billing"}, full, false},
		{"no repository", &getFlags{repo: "orders"}, bare, false},
		{"not suspended", &getFlags{suspended: true}, bare, false},
		// A queued finding admits suspend and expedite.
		{"awaiting an action", &getFlags{awaiting: true}, bare, true},
		{"no filters", &getFlags{}, bare, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesFinding(tc.fnd, tc.f); got != tc.want {
				t.Errorf("matchesFinding = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCellAtShortRow(t *testing.T) {
	row := metav1.TableRow{Cells: []any{"a"}}
	if got := cellAt(row, 0); got != "a" {
		t.Errorf("cellAt(0) = %v", got)
	}
	if got := cellAt(row, 3); got != "" {
		t.Errorf("cellAt past the end = %v, want empty", got)
	}
}

// TestSortRowsWithoutColumn: sorting by a column the kind does not print is
// a no-op rather than a crash, and rows without metadata keep their order.
func TestSortRowsWithoutColumn(t *testing.T) {
	tbl := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{{Name: "Name"}},
		Rows:              []metav1.TableRow{{Cells: []any{"b"}}, {Cells: []any{"a"}}},
	}
	for _, key := range []string{"severity", "age"} {
		sortRows(tbl, key)
		if tbl.Rows[0].Cells[0] != "b" || tbl.Rows[1].Cells[0] != "a" {
			t.Errorf("sort by %s reordered rows: %v", key, tbl.Rows)
		}
	}
}

// clientObject is the object type the harness takes.
type clientObject = client.Object
