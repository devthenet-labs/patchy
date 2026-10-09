// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package evalapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/artifact"
	"github.com/bitwise-media-group/patchy/internal/evalresults"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/pkg/evaluation"
)

// scriptedWorkspaces answers Stat and Put with fixed results.
type scriptedWorkspaces struct {
	statErr error
	cached  bool
	putErr  error
	readAll bool // Put drains the body (to trip the request cap)
}

func (s *scriptedWorkspaces) Stat(context.Context, string) (bool, error) { return s.cached, s.statErr }

func (s *scriptedWorkspaces) Put(_ context.Context, _ string, r io.Reader, _ int64) error {
	if s.readAll {
		if _, err := io.ReadAll(r); err != nil {
			return err
		}
	}
	return s.putErr
}

type errGranter struct{}

func (errGranter) Allowed(context.Context, auth.Identity, string) (bool, error) {
	return false, errors.New("apiserver down")
}

// serverWith builds a Server over a fake client with funcs, the given
// workspace client and limits.
func serverWith(t *testing.T, funcs interceptor.Funcs, ws WorkspaceClient, limits Limits,
	objs ...client.Object) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Evaluation{}, &v1alpha1.EvaluationUnit{}).
		WithInterceptorFuncs(funcs).Build()
	if ws == nil {
		ws = &fakeWorkspaces{present: map[string]bool{digestA: true}}
	}
	return NewServer(c, "patchy", headerAuth{}, evaluation.AuthInfo{}, FullAccess{}, ws, limits, nil)
}

func errorBody(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var e evaluation.SubmissionError
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body %q: %v", rr.Body.String(), err)
	}
	return e.Error
}

func TestFullAccessAllowsEverything(t *testing.T) {
	for _, verb := range []string{VerbCreate, VerbGet, VerbDelete} {
		ok, err := FullAccess{}.Allowed(context.Background(), auth.Identity{}, verb)
		if !ok || err != nil {
			t.Errorf("FullAccess.Allowed(%s) = %v, %v", verb, ok, err)
		}
	}
}

func TestAccessReviewFailureIs500(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).Build()
	srv := NewServer(c, "patchy", headerAuth{}, evaluation.AuthInfo{}, errGranter{},
		&fakeWorkspaces{present: map[string]bool{}}, Limits{}, nil)
	rr := doReq(t, srv, "GET", "/api/v1/evaluations/x", "dev", "")
	if rr.Code != http.StatusInternalServerError || errorBody(t, rr) != "access review failed" {
		t.Fatalf("GET = %d %q, want 500 access review failed", rr.Code, rr.Body.String())
	}
}

func TestResponseHygieneHeaders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := doReq(t, srv, "GET", "/api/v1/auth/info", "", "")
	if rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", rr.Header())
	}
}

func TestWorkspaceHeadErrors(t *testing.T) {
	srv := serverWith(t, interceptor.Funcs{}, &scriptedWorkspaces{statErr: errors.New("cache down")}, Limits{})
	if rr := doReq(t, srv, "HEAD", "/api/v1/workspaces/"+digestA, "dev", ""); rr.Code != http.StatusBadGateway {
		t.Errorf("HEAD with cache down = %d, want 502", rr.Code)
	}
	nonHex := "/api/v1/workspaces/" + strings.Repeat("z", 64)
	if rr := doReq(t, srv, "HEAD", nonHex, "dev", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("HEAD non-hex digest = %d, want 400", rr.Code)
	}
	if rr := doReq(t, srv, "HEAD", "/api/v1/workspaces/abc", "dev", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("HEAD short digest = %d, want 400", rr.Code)
	}
}

func TestWorkspacePutOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		ws     *scriptedWorkspaces
		limits Limits
		body   string
		want   int
		msg    string
	}{
		{name: "stored", ws: &scriptedWorkspaces{}, body: "x", want: http.StatusCreated},
		{
			name: "stat error still uploads", ws: &scriptedWorkspaces{statErr: errors.New("flaky")},
			body: "x", want: http.StatusCreated,
		},
		{
			name: "over the request cap", ws: &scriptedWorkspaces{readAll: true},
			limits: Limits{MaxWorkspaceBytes: 4}, body: "0123456789",
			want: http.StatusRequestEntityTooLarge, msg: "workspace exceeds the configured cap",
		},
		{
			name: "blob store says too large", ws: &scriptedWorkspaces{putErr: fmt.Errorf("wrap: %w", artifact.ErrBlobTooLarge)},
			body: "x", want: http.StatusRequestEntityTooLarge, msg: "workspace exceeds the configured cap",
		},
		{
			name: "digest mismatch", ws: &scriptedWorkspaces{putErr: artifact.ErrDigestMismatch},
			body: "x", want: http.StatusUnprocessableEntity, msg: "uploaded bytes do not match the digest",
		},
		{
			name: "other store failure", ws: &scriptedWorkspaces{putErr: errors.New("disk full")},
			body: "x", want: http.StatusBadGateway, msg: "storing the workspace failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := serverWith(t, interceptor.Funcs{}, c.ws, c.limits)
			rr := doReq(t, srv, "PUT", "/api/v1/workspaces/"+digestB, "dev", c.body)
			if rr.Code != c.want {
				t.Fatalf("PUT = %d %s, want %d", rr.Code, rr.Body.String(), c.want)
			}
			if c.msg != "" && errorBody(t, rr) != c.msg {
				t.Errorf("error = %q, want %q", errorBody(t, rr), c.msg)
			}
		})
	}
}

// unit returns a valid UnitSpec, adjusted by mut.
func unit(mut func(*evaluation.UnitSpec)) evaluation.UnitSpec {
	u := evaluation.UnitSpec{
		Skill: "s", Tier: 1, Model: "m",
		Harnesses: []evaluation.HarnessOption{{Harness: "claude"}},
		Workspace: evaluation.WorkspaceRef{Digest: digestA},
	}
	if mut != nil {
		mut(&u)
	}
	return u
}

func submission(units ...evaluation.UnitSpec) string {
	raw, _ := json.Marshal(evaluation.Submission{Version: evaluation.SubmissionVersion, Units: units})
	return string(raw)
}

func TestSubmitUnitValidation(t *testing.T) {
	nine := make([]evaluation.HarnessOption, 9)
	for i := range nine {
		nine[i] = evaluation.HarnessOption{Harness: "h"}
	}
	cases := []struct {
		name string
		u    evaluation.UnitSpec
		msg  string
	}{
		{"no skill", unit(func(u *evaluation.UnitSpec) { u.Skill = "" }), "unit 0: skill is required"},
		{"no model", unit(func(u *evaluation.UnitSpec) { u.Model = "" }), "unit 0: model is required"},
		{"too many harnesses", unit(func(u *evaluation.UnitSpec) { u.Harnesses = nine }),
			"unit 0: 9 harness options (cap 8)"},
		{"bad digest", unit(func(u *evaluation.UnitSpec) { u.Workspace.Digest = "nope" }),
			`unit 0: malformed workspace digest "nope"`},
		{
			"empty harness id",
			unit(func(u *evaluation.UnitSpec) { u.Harnesses = []evaluation.HarnessOption{{ModelID: "x"}} }),
			"unit 0: harness option with empty harness id",
		},
		{
			"exec JSON over the CRD cap",
			unit(func(u *evaluation.UnitSpec) { u.Skill = strings.Repeat("s", maxExecJSONBytes) }),
			"trim the prior entry",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			rr := doReq(t, srv, "POST", "/api/v1/evaluations", "dev", submission(c.u))
			if rr.Code != http.StatusBadRequest || !strings.Contains(errorBody(t, rr), c.msg) {
				t.Fatalf("POST = %d %q, want 400 containing %q", rr.Code, rr.Body.String(), c.msg)
			}
		})
	}
}

func TestSubmitLimitsAndFailures(t *testing.T) {
	t.Run("too many units", func(t *testing.T) {
		srv := serverWith(t, interceptor.Funcs{}, nil, Limits{MaxUnits: 1})
		rr := doReq(t, srv, "POST", "/api/v1/evaluations", "dev", submission(unit(nil), unit(nil)))
		if rr.Code != http.StatusBadRequest || errorBody(t, rr) != "submission has 2 units (cap 1)" {
			t.Fatalf("POST = %d %q", rr.Code, rr.Body.String())
		}
	})
	t.Run("body over the submission cap", func(t *testing.T) {
		srv := serverWith(t, interceptor.Funcs{}, nil, Limits{MaxSubmissionBytes: 16})
		rr := doReq(t, srv, "POST", "/api/v1/evaluations", "dev", submission(unit(nil)))
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("POST = %d %q, want 413", rr.Code, rr.Body.String())
		}
	})
	t.Run("rendered evaluation over the object cap", func(t *testing.T) {
		srv := serverWith(t, interceptor.Funcs{}, nil, Limits{MaxEvaluationBytes: 64})
		rr := doReq(t, srv, "POST", "/api/v1/evaluations", "dev", submission(unit(nil)))
		if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(errorBody(t, rr), "cap 64") {
			t.Fatalf("POST = %d %q, want 413 naming the cap", rr.Code, rr.Body.String())
		}
	})
	t.Run("workspace cache unavailable", func(t *testing.T) {
		srv := serverWith(t, interceptor.Funcs{}, &scriptedWorkspaces{statErr: errors.New("down")}, Limits{})
		rr := doReq(t, srv, "POST", "/api/v1/evaluations", "dev", submission(unit(nil)))
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("POST = %d %q, want 502", rr.Code, rr.Body.String())
		}
	})
	t.Run("create fails", func(t *testing.T) {
		srv := serverWith(t, interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return errors.New("etcd full")
			},
		}, nil, Limits{})
		rr := doReq(t, srv, "POST", "/api/v1/evaluations", "dev", submission(unit(nil)))
		if rr.Code != http.StatusInternalServerError || errorBody(t, rr) != "creating the evaluation failed" {
			t.Fatalf("POST = %d %q", rr.Code, rr.Body.String())
		}
	})
}

// TestSubmitRendersPlan: optional fields reach the plan, digests are
// deduplicated across units, and an enormous TTL is clamped to int32.
func TestSubmitRendersPlan(t *testing.T) {
	srv, _, _ := newTestServer(t)
	u := unit(func(u *evaluation.UnitSpec) {
		u.Judge = &evaluation.JudgeSpec{Model: "judge-model"}
		u.TimeoutMS = 60_000
		u.MaxTurns = 12
		u.RunsPerQuery = 3
		u.Harnesses = []evaluation.HarnessOption{{Harness: "claude", ModelID: "opus"}, {Harness: "codex"}}
	})
	sub := evaluation.Submission{Version: evaluation.SubmissionVersion, Units: []evaluation.UnitSpec{u, u},
		TTLSeconds: 1 << 40}
	eval, digests, err := srv.buildEvaluation(&sub, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 1 || digests[0] != digestA {
		t.Errorf("digests = %v, want one", digests)
	}
	if eval.Spec.TTLSecondsAfterFinished == nil || *eval.Spec.TTLSecondsAfterFinished != 1<<31-1 {
		t.Errorf("ttl = %v, want clamped to max int32", eval.Spec.TTLSecondsAfterFinished)
	}
	p := eval.Spec.Units[0]
	if p.JudgeModel != "judge-model" || p.TimeoutMilliseconds != 60_000 || p.MaxTurns != 12 || p.RunsPerQuery != 3 ||
		len(p.Harnesses) != 2 || p.Harnesses[0].ModelID != "opus" || p.Harnesses[1].Harness != "codex" {
		t.Errorf("plan = %+v", p)
	}
	sub.TTLSeconds = 0
	eval, _, _ = srv.buildEvaluation(&sub, "dev")
	if eval.Spec.TTLSecondsAfterFinished != nil {
		t.Errorf("ttl = %v with none requested", *eval.Spec.TTLSecondsAfterFinished)
	}
}

func TestCancelFailures(t *testing.T) {
	boom := errors.New("apiserver down")
	t.Run("get fails", func(t *testing.T) {
		srv := serverWith(t, interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}, nil, Limits{}, terminalEval("eval-x"))
		rr := doReq(t, srv, "DELETE", "/api/v1/evaluations/eval-x", "dev", "")
		if rr.Code != http.StatusInternalServerError || errorBody(t, rr) != "reading the evaluation failed" {
			t.Fatalf("DELETE = %d %q", rr.Code, rr.Body.String())
		}
	})
	t.Run("delete fails", func(t *testing.T) {
		srv := serverWith(t, interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return boom },
		}, nil, Limits{}, terminalEval("eval-x"))
		rr := doReq(t, srv, "DELETE", "/api/v1/evaluations/eval-x", "dev", "")
		if rr.Code != http.StatusInternalServerError || errorBody(t, rr) != "deleting the evaluation failed" {
			t.Fatalf("DELETE = %d %q", rr.Code, rr.Body.String())
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		srv, _, _ := newTestServer(t, terminalEval("eval-x"))
		if rr := doReq(t, srv, "DELETE", "/api/v1/evaluations/eval-x", "mallory", ""); rr.Code != http.StatusForbidden {
			t.Fatalf("DELETE = %d, want 403", rr.Code)
		}
	})
}

func TestSnapshotReadFailures(t *testing.T) {
	boom := errors.New("apiserver down")
	cases := map[string]interceptor.Funcs{
		"get": {Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		}},
		"list": {List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return boom
		}},
	}
	for name, funcs := range cases {
		t.Run(name, func(t *testing.T) {
			srv := serverWith(t, funcs, nil, Limits{}, terminalEval("eval-x"))
			rr := doReq(t, srv, "GET", "/api/v1/evaluations/eval-x", "dev", "")
			if rr.Code != http.StatusInternalServerError || errorBody(t, rr) != "reading the evaluation failed" {
				t.Fatalf("GET = %d %q", rr.Code, rr.Body.String())
			}
			rr = doReq(t, srv, "GET", "/api/v1/evaluations/eval-x/events", "dev", "")
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("GET events = %d %q, want 500", rr.Code, rr.Body.String())
			}
		})
	}
	srv, _, _ := newTestServer(t)
	if rr := doReq(t, srv, "GET", "/api/v1/evaluations/absent/events", "dev", ""); rr.Code != http.StatusNotFound {
		t.Errorf("GET events absent = %d, want 404", rr.Code)
	}
}

func gz(t *testing.T, raw string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestSnapshotUnitShapes: units come back index-ordered; a running unit has
// no summary; a failed one reports its reason as the outcome and carries no
// result; a complete one lifts its results entry, and one whose entry cannot
// be read still reports its result without it. The unit total falls back to
// the spec when the status has not counted yet.
func TestSnapshotUnitShapes(t *testing.T) {
	eval := runningEval("eval-s")
	eval.Status.Units = 0
	eval.Spec.Units = append(eval.Spec.Units, eval.Spec.Units[0], eval.Spec.Units[0], eval.Spec.Units[0])

	running := runningUnit("eval-s", 3)
	failed := settledUnit("eval-s", 2)
	failed.Status.Phase = v1alpha1.RunFailed
	failed.Status.Reason = v1alpha1.UnitJobFailed
	withEntry := settledUnit("eval-s", 0)
	withEntry.Status.CasesFailed = 1
	withEntry.Status.Cases = []v1alpha1.CaseSummary{{ID: "c1", Passed: true}, {ID: "c2"}}
	withEntry.Status.Usage.CostUSD = "not-a-number"
	withEntry.Status.ResultsRef = &v1alpha1.TranscriptRef{Name: evalresults.NameFor(withEntry.Name)}
	badEntry := settledUnit("eval-s", 1)
	badEntry.Status.ResultsRef = &v1alpha1.TranscriptRef{Name: evalresults.NameFor(badEntry.Name)}

	good := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: evalresults.NameFor(withEntry.Name)},
		BinaryData: map[string][]byte{evalresults.DataKey: gz(t, `{"score":1}`)}}
	corrupt := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: evalresults.NameFor(badEntry.Name)},
		BinaryData: map[string][]byte{evalresults.DataKey: []byte("not gzip")}}

	srv, _, _ := newTestServer(t, eval, running, failed, withEntry, badEntry, good, corrupt)
	rr := doReq(t, srv, "GET", "/api/v1/evaluations/eval-s", "dev", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rr.Code, rr.Body.String())
	}
	var snap evaluation.EvaluationStatusWire
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	checkUnitShapes(t, &snap)
}

// checkUnitShapes asserts TestSnapshotUnitShapes's four units.
func checkUnitShapes(t *testing.T, snap *evaluation.EvaluationStatusWire) {
	t.Helper()
	if snap.UnitsTotal != 4 || len(snap.Units) != 4 {
		t.Fatalf("snapshot = %+v", snap)
	}
	for i, u := range snap.Units {
		if u.Index != i {
			t.Fatalf("unit %d has index %d: not index-ordered", i, u.Index)
		}
	}
	checkSettledUnit(t, snap.Units[0])
	u1, u2, u3 := snap.Units[1], snap.Units[2], snap.Units[3]
	if u1.Result == nil || u1.Result.Entry != nil {
		t.Errorf("unit 1 result = %+v, want a result without its unreadable entry", u1.Result)
	}
	if u2.Summary == nil || u2.Summary.Outcome != "JobFailed" || u2.Result != nil || u2.Reason != "JobFailed" {
		t.Errorf("unit 2 = %+v, want failed summary with JobFailed outcome and no result", u2)
	}
	if u3.Summary != nil || u3.Result != nil || u3.Phase != string(v1alpha1.RunRunning) {
		t.Errorf("unit 3 = %+v, want running with nothing settled", u3)
	}
}

// checkSettledUnit asserts the complete unit with a readable entry.
func checkSettledUnit(t *testing.T, u0 evaluation.UnitStatusWire) {
	t.Helper()
	if u0.Result == nil || string(u0.Result.Entry) != `{"score":1}` || !u0.Result.Failed {
		t.Errorf("unit 0 result = %+v", u0.Result)
	}
	if s := u0.Summary; s == nil || len(s.Cases) != 2 || !s.Cases[0].Passed || s.TokenUsage.CostUSD != 0 {
		t.Errorf("unit 0 summary = %+v", u0.Summary)
	}
}

// noFlush is a ResponseWriter that cannot stream.
type noFlush struct {
	h    http.Header
	code int
	body bytes.Buffer
}

func (n *noFlush) Header() http.Header         { return n.h }
func (n *noFlush) Write(b []byte) (int, error) { return n.body.Write(b) }
func (n *noFlush) WriteHeader(code int)        { n.code = code }

func TestSSERequiresStreaming(t *testing.T) {
	srv, _, _ := newTestServer(t, terminalEval("eval-x"))
	req := httptest.NewRequest("GET", "/api/v1/evaluations/eval-x/events", nil)
	req.Header.Set("X-Test-User", "dev")
	w := &noFlush{h: http.Header{}}
	srv.Handler().ServeHTTP(w, req)
	if w.code != http.StatusInternalServerError || !strings.Contains(w.body.String(), "streaming unsupported") {
		t.Fatalf("events without a flusher = %d %q", w.code, w.body.String())
	}
}

type (
	cacheInformer       = cache.Informer
	cacheInformerOption = cache.InformerGetOption
	toolsHandler        = toolscache.ResourceEventHandler
	toolsRegistration   = toolscache.ResourceEventHandlerRegistration
)

// signallingCache wraps the fake informers so the test knows when StartWatch
// has registered its handlers on both kinds.
type signallingCache struct {
	*informertest.FakeInformers
	registered chan struct{}
}

type signallingInformer struct {
	cacheInformer
	registered chan struct{}
}

func (c *signallingCache) GetInformer(ctx context.Context, obj client.Object,
	opts ...cacheInformerOption) (cacheInformer, error) {
	inf, err := c.FakeInformers.GetInformer(ctx, obj, opts...)
	if err != nil {
		return nil, err
	}
	return &signallingInformer{cacheInformer: inf, registered: c.registered}, nil
}

func (i *signallingInformer) AddEventHandler(h toolsHandler) (toolsRegistration, error) {
	reg, err := i.cacheInformer.AddEventHandler(h)
	i.registered <- struct{}{}
	return reg, err
}

// TestStartWatchPublishes: informer events on an Evaluation, or on a unit
// labelled with its evaluation, wake that evaluation's monitors; an
// unlabelled unit wakes nobody; Start returns when its context ends.
func TestStartWatchPublishes(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := &informertest.FakeInformers{Scheme: kube.Scheme()}
	sc := &signallingCache{FakeInformers: fi, registered: make(chan struct{}, 2)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.StartWatch(ctx, sc) }()
	<-sc.registered
	<-sc.registered

	evalInf, err := fi.FakeInformerFor(ctx, &v1alpha1.Evaluation{})
	if err != nil {
		t.Fatal(err)
	}
	unitInf, err := fi.FakeInformerFor(ctx, &v1alpha1.EvaluationUnit{})
	if err != nil {
		t.Fatal(err)
	}

	wake := srv.broker.subscribe("eval-w")
	defer srv.broker.unsubscribe("eval-w", wake)
	expectWake := func(what string, want bool) {
		t.Helper()
		select {
		case <-wake:
			if !want {
				t.Errorf("%s woke the monitor", what)
			}
		default:
			if want {
				t.Errorf("%s did not wake the monitor", what)
			}
		}
	}

	ev := &v1alpha1.Evaluation{ObjectMeta: metav1.ObjectMeta{Name: "eval-w", Namespace: "patchy"}}
	evalInf.Add(ev)
	expectWake("evaluation add", true)
	evalInf.Update(ev, ev)
	expectWake("evaluation update", true)
	evalInf.Delete(ev)
	expectWake("evaluation delete", true)

	labelled := &v1alpha1.EvaluationUnit{ObjectMeta: metav1.ObjectMeta{Name: "eval-w-u000", Namespace: "patchy",
		Labels: map[string]string{v1alpha1.LabelEvaluation: "eval-w"}}}
	unitInf.Add(labelled)
	expectWake("labelled unit add", true)
	unitInf.Add(&v1alpha1.EvaluationUnit{ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: "patchy"}})
	expectWake("unlabelled unit add", false)
	other := &v1alpha1.Evaluation{ObjectMeta: metav1.ObjectMeta{Name: "eval-other", Namespace: "patchy"}}
	evalInf.Add(other)
	expectWake("another evaluation", false)

	cancel()
	if err := <-done; err != nil {
		t.Errorf("StartWatch = %v, want nil on cancel", err)
	}
}

func TestStartWatchInformerError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := &informertest.FakeInformers{Scheme: kube.Scheme(), Error: errors.New("no informer")}
	err := srv.StartWatch(context.Background(), fi)
	if err == nil || !strings.Contains(err.Error(), "no informer") {
		t.Fatalf("StartWatch = %v, want the informer error", err)
	}
}
