// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/internal/controller/preview"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/keydir"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/signer"
)

const (
	fixtures      = "../../hack/testdata/chart-render/"
	keysSecret    = "patchy-preview-auth-keys"
	admitsMarker  = "patchy.bitwisemedia.uk/preview-auth-admits"
	authRequired  = "pinned sign-in annotations (preview sign-in is required)"
	authNotPinned = "exactly one admitted key generation's pinned set"
	idpAnnotation = "alb.ingress.kubernetes.io/auth-idp-oidc"
	timeoutKey    = "alb.ingress.kubernetes.io/auth-session-timeout"
	ceilingMarker = "patchy.bitwisemedia.uk/preview-auth-session-timeout"
)

// TestPreviewAuthUpgradeAgainstAPIServer upgrades a real release, with real
// Helm lookups, from previews without sign-in through the permit stage (B1),
// the require stage (B2), a key rotation in two passes and the end of its
// overlap, then rolls it back by the runbook. Helm applies an Ingress before
// the admission policies that judge it, so each stage meets the previous
// revision's policies: this is the order the critique's F3 split exists for.
// It also holds the chart's client secrets to the relay's own derivation and
// the controller's annotation JSON to the controller's own validation, and
// changes sessionTimeout at the require stage: lowered in one upgrade, raised
// in two, and lowered off an exact-compare policy (a chart before the
// ceiling) in two, none of them refused.
func TestPreviewAuthUpgradeAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	u := startUpgradeEnv(t)
	u.install()
	u.permit()
	u.requireStage()
	u.lowerTimeout()
	u.raiseTimeout()
	u.lowerTimeoutOffExactPolicy()
	u.rotate()
	u.refuseToStopRequiring()
	u.refuseLostKeys()
	u.rollBack()
}

// upgradeEnv is one release on one API server, upgraded stage by stage.
type upgradeEnv struct {
	t     *testing.T
	admin client.Client
	h     helmRunner
	// base is today's preview values; withAuth adds the permit stage,
	// require the require stage, and rotated a rotation to generation 2.
	base, withAuth, require, rotated []string
	keys1, keys2                     map[string][]byte
	set1                             map[string]map[string]string
}

func startUpgradeEnv(t *testing.T) *upgradeEnv {
	t.Helper()
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{ingressClassParamsCRD()},
	}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	for _, name := range []string{"ordinary", "patchy-preview-2"} {
		if err := admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{"kubernetes.io/metadata.name": name},
		}}); err != nil {
			t.Fatalf("namespace %s: %v", name, err)
		}
	}
	u := &upgradeEnv{t: t, admin: admin, h: newHelm(t, env)}
	u.base = []string{"-f", fixtures + "preview-foundation.yaml", "-f", fixtures + "intent-controller.yaml",
		"-f", fixtures + "preview-controller.yaml"}
	// The fixture's keys.renderOffline is for renders with no cluster; here
	// the chart must find the cluster itself and keep the managed keys.
	u.withAuth = append(slices.Clone(u.base), "-f", fixtures+"preview-auth.yaml",
		"--set", "previewAuth.keys.renderOffline=false")
	u.require = append(slices.Clone(u.withAuth), "--set", "previewAuth.stage=require")
	u.rotated = append(slices.Clone(u.require), "--set", "previewAuth.keys.rotate=2")
	return u
}

func (u *upgradeEnv) upgrade(args []string) { u.h.mustRun(u.upgradeArgs(args)...) }

func (u *upgradeEnv) upgradeArgs(args []string) []string {
	// No history limit: rollBack returns to revision 1, more than Helm's
	// default ten upgrades ago.
	return append([]string{"upgrade", "patchy", ".", "--namespace", "patchy", "--history-max", "0"}, args...)
}

// install is today's release: previews without sign-in, and a live
// preview's Ingress in slot 1.
func (u *upgradeEnv) install() {
	t := u.t
	u.h.mustRun(append([]string{"install", "patchy", ".", "--namespace", "patchy", "--create-namespace"},
		u.base...)...)
	// Slot namespaces enforce Pod Security here, so wait on the Ingress
	// policy alone, the one this test exercises.
	waitRefused(t, u.admin, ingress("patchy-preview-0", "probe", "alb", previewHost), "alb-preview")
	live := ingress("patchy-preview-1", "preview-demo-1", "alb-preview", previewHost)
	live.Annotations = map[string]string{"alb.ingress.kubernetes.io/healthcheck-path": "/healthz"}
	live.Finalizers = []string{autoModeFinalizer}
	if err := u.admin.Create(t.Context(), live); err != nil {
		t.Fatalf("create live preview Ingress: %v", err)
	}
}

// permit is B1: keys generated, generation 1 admitted, nothing required.
func (u *upgradeEnv) permit() {
	t, admin := u.t, u.admin
	u.upgrade(u.withAuth)
	u.keys1 = secretData(t, admin, "patchy", keysSecret)
	checkClientSecrets(t, admin, u.keys1, 1)
	if got := admits(t, admin); got != "g1" {
		t.Fatalf("after permit: %s = %q, want g1", admitsMarker, got)
	}
	u.set1 = slotSets(t, admin, 1)
	set1 := u.set1
	waitAdmitted(t, admin, withSet(ingress("patchy-preview-1", "probe-g1", "alb-preview", previewHost),
		set1["patchy-preview-1"]))
	for _, tc := range []struct {
		name string
		obj  *networkingv1.Ingress
		want string
	}{
		{"slot 0's set in slot 1", withSet(ingress("patchy-preview-1", "cross", "alb-preview", previewHost),
			set1["patchy-preview-0"]), authNotPinned},
		{"half a set", withSet(ingress("patchy-preview-1", "half", "alb-preview", previewHost),
			map[string]string{"alb.ingress.kubernetes.io/auth-type": "oidc"}), authNotPinned},
		{"a set beyond slotCount", withSet(ingress("patchy-preview-2", "beyond", "alb-preview", previewHost),
			set1["patchy-preview-1"]), "reserved for the exact preview slot|" + authNotPinned},
		{"a pinned key with another value", withSet(ingress("patchy-preview-1", "allow", "alb-preview", previewHost),
			withValue(set1["patchy-preview-1"], "alb.ingress.kubernetes.io/auth-on-unauthenticated-request", "allow")),
			authNotPinned},
		{"another cookie", withSet(ingress("patchy-preview-1", "cookie", "alb-preview", previewHost),
			withValue(set1["patchy-preview-1"], "alb.ingress.kubernetes.io/auth-session-cookie", "session")),
			authNotPinned},
		{"an auth key outside the set", withSet(ingress("patchy-preview-1", "other", "alb-preview", previewHost),
			map[string]string{"alb.ingress.kubernetes.io/auth-session-cookie-name": "x"}),
			"annotations are restricted"},
	} {
		// A want of "a|b" admits either denial: two policies refuse, and
		// the API server names whichever it evaluated first.
		err := admin.Create(t.Context(), tc.obj, client.DryRunAll)
		if !deniedWith(err, strings.Split(tc.want, "|")...) {
			t.Errorf("permit, %s: want %q, got %v", tc.name, tc.want, err)
		}
	}
	// Nothing is required yet: an Ingress without sign-in is admitted, and
	// the controller and the placeholder are as they were.
	noAuth := ingress("patchy-preview-1", "legacy-noauth", "alb-preview", previewHost)
	noAuth.Finalizers = []string{autoModeFinalizer}
	if err := admin.Create(t.Context(), noAuth); err != nil {
		t.Fatalf("permit: an Ingress without sign-in refused: %v", err)
	}
	if got := configData(t, admin, "patchy-preview-controller-config")["PATCHY_PREVIEW_AUTH_REQUIRED"]; got != "" {
		t.Errorf("permit: PATCHY_PREVIEW_AUTH_REQUIRED = %q, want unset", got)
	}
	if got := authOf(placeholder(t, admin)); len(got) != 0 {
		t.Errorf("permit: placeholder carries sign-in annotations %v", got)
	}
}

// requireStage is B2 with no confirmation: the lookup finds the permit
// marker, and reuses the keys.
func (u *upgradeEnv) requireStage() {
	t, admin, set1 := u.t, u.admin, u.set1
	u.upgrade(u.require)
	if keys := secretData(t, admin, "patchy", keysSecret); !maps.EqualFunc(keys, u.keys1, bytes.Equal) {
		t.Fatal("require: the keys Secret changed; the lookup should have reused it")
	}
	checkController(t, admin, 1, 0)
	if got := authOf(placeholder(t, admin)); !maps.Equal(got, set1["patchy-preview-0"]) {
		t.Errorf("require: placeholder sign-in annotations %v, want slot 0's set", got)
	}
	waitRefused(t, admin, ingress("patchy-preview-1", "probe-noauth", "alb-preview", previewHost), authRequired)
	if err := admin.Create(t.Context(), withSet(ingress("patchy-preview-1", "with-set", "alb-preview", previewHost),
		set1["patchy-preview-1"]), client.DryRunAll); err != nil {
		t.Errorf("require: an Ingress with its slot's set refused: %v", err)
	}
	// The live Ingress gains its set (the controller's patch); a slot beyond
	// slotCount admits nothing.
	patchSet(t, admin, "patchy-preview-1", "preview-demo-1", set1["patchy-preview-1"])
	if err := admin.Create(t.Context(), withSet(ingress("patchy-preview-2", "beyond", "alb-preview", previewHost),
		set1["patchy-preview-1"]), client.DryRunAll); err == nil {
		t.Error("require: an Ingress in a slot beyond slotCount admitted")
	}
}

// shortSession is the require stage's values with a 900-second
// sessionTimeout, below the fixture's 3600.
func (u *upgradeEnv) shortSession() []string {
	return append(slices.Clone(u.require), "--set", "previewAuth.sessionTimeout=900")
}

// lowerTimeout lowers sessionTimeout in ONE upgrade: the placeholder, which
// Helm applies before the policies, carries the shorter session, which the
// live policy's ceiling admits. A chart that compared the timeout exactly
// failed this upgrade on the placeholder.
func (u *upgradeEnv) lowerTimeout() {
	t, admin := u.t, u.admin
	u.upgrade(u.shortSession())
	checkTimeout(t, admin, "lower", "900", "900")
	set := slotSets(t, admin, 1)["patchy-preview-1"]
	refused := []string{authNotPinned, authRequired}
	waitRefused(t, admin, withSet(ingress("patchy-preview-1", "probe-long", "alb-preview", previewHost),
		withValue(set, timeoutKey, "3600")), refused...)
	for _, tc := range []struct {
		value string
		admit bool
	}{
		{"900", true}, {"1", true}, {"600", true}, {"901", false}, {"0", false}, {"0900", false},
		{"+900", false}, {"9e2", false}, {"", false}, {"9000000000000000000000", false},
	} {
		err := admin.Create(t.Context(), withSet(ingress("patchy-preview-1", "timeout", "alb-preview", previewHost),
			withValue(set, timeoutKey, tc.value)), client.DryRunAll)
		if tc.admit && err != nil {
			t.Errorf("lower: auth-session-timeout %q refused: %v", tc.value, err)
		}
		if !tc.admit && !deniedWith(err, refused...) {
			t.Errorf("lower: auth-session-timeout %q: want refused, got %v", tc.value, err)
		}
	}
	// Every other pinned key is still compared exactly.
	if err := admin.Create(t.Context(), withSet(ingress("patchy-preview-1", "cookie", "alb-preview", previewHost),
		withValue(withValue(set, timeoutKey, "600"), "alb.ingress.kubernetes.io/auth-session-cookie", "session")),
		client.DryRunAll); !deniedWith(err, refused...) {
		t.Errorf("lower: another cookie under the ceiling: want refused, got %v", err)
	}
	u.relinkDemo()
}

// raiseTimeout raises sessionTimeout back: the first upgrade records the new
// ceiling and keeps applying the old session (the live policy would refuse a
// longer one on the placeholder), the second applies it.
func (u *upgradeEnv) raiseTimeout() {
	t, admin := u.t, u.admin
	u.upgrade(u.require)
	checkTimeout(t, admin, "raise, first pass", "900", "3600")
	u.upgrade(u.require)
	checkTimeout(t, admin, "raise, second pass", "3600", "3600")
	u.relinkDemo()
}

// exactTimeout matches the policies' ceiling rule for auth-session-timeout,
// leaving the exact comparison every other key gets.
var exactTimeout = regexp.MustCompile(`\(k == 'alb\.ingress\.kubernetes\.io/auth-session-timeout' \?[^:]*:\s*` +
	`(object\.metadata\.annotations\[k\] == s\[k\])\)`)

// lowerTimeoutOffExactPolicy is the first upgrade into the ceiling from a
// chart that compared auth-session-timeout exactly: the live policies are
// rewritten to that chart's rule, with no recorded ceiling. Lowering then
// keeps the pinned value for one upgrade (the old policy would refuse
// anything else on the placeholder) and applies the new one on the next.
func (u *upgradeEnv) lowerTimeoutOffExactPolicy() {
	t, admin := u.t, u.admin
	for _, name := range []string{"patchy-preview-ingresses", "patchy-preview-ingress-auth"} {
		vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
		if err := admin.Get(t.Context(), client.ObjectKey{Name: name}, vap); err != nil {
			t.Fatal(err)
		}
		delete(vap.Annotations, ceilingMarker)
		n := 0
		for i, v := range vap.Spec.Validations {
			if exactTimeout.MatchString(v.Expression) {
				vap.Spec.Validations[i].Expression = exactTimeout.ReplaceAllString(v.Expression, "($1)")
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s: %d validations judge the timeout as a ceiling, want 1", name, n)
		}
		// Applied as Helm's own field manager, as the older chart's upgrade
		// left it, so the next upgrade owns the fields it changes.
		manager := ""
		for _, m := range vap.ManagedFields {
			if m.Operation == metav1.ManagedFieldsOperationApply {
				manager = m.Manager
			}
		}
		if manager == "" {
			t.Fatalf("%s: no field manager applied it", name)
		}
		vap.ManagedFields, vap.ResourceVersion = nil, ""
		vap.APIVersion, vap.Kind = "admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy"
		//nolint:staticcheck // client.Apply with a typed object is what this API server test needs.
		if err := admin.Patch(t.Context(), vap, client.Apply, client.FieldOwner(manager),
			client.ForceOwnership); err != nil {
			t.Fatalf("rewrite %s to the exact rule: %v", name, err)
		}
	}
	set := slotSets(t, admin, 1)["patchy-preview-1"]
	waitRefused(t, admin, withSet(ingress("patchy-preview-1", "probe-exact", "alb-preview", previewHost),
		withValue(set, timeoutKey, "900")), authNotPinned, authRequired)
	// That upgrade would keep the pinned 3600 applied, so it may not open
	// the preview ALB to any address as well.
	public := append(u.shortSession(), "--set-json", "preview.inboundCIDRs=[]",
		"--set", "preview.allowPublicWithAuth=confirmed")
	if out, err := u.h.run(u.upgradeArgs(public)...); err == nil ||
		!strings.Contains(out, "pins auth-session-timeout 3600 exactly") {
		t.Errorf("lower off the exact policy and drop the allowlist: want refused, got %v: %s", err, out)
	}
	u.upgrade(u.shortSession())
	checkTimeout(t, admin, "lower off the exact policy, first pass", "3600", "3600")
	u.upgrade(u.shortSession())
	checkTimeout(t, admin, "lower off the exact policy, second pass", "900", "900")
	// And back, as raiseTimeout.
	u.upgrade(u.require)
	checkTimeout(t, admin, "raise again, first pass", "900", "3600")
	u.upgrade(u.require)
	checkTimeout(t, admin, "raise again, second pass", "3600", "3600")
	u.relinkDemo()
}

// relinkDemo patches the live preview Ingress to generation 1's current set,
// as its reconcile does after a sessionTimeout change.
func (u *upgradeEnv) relinkDemo() {
	patchSet(u.t, u.admin, "patchy-preview-1", "preview-demo-1", slotSets(u.t, u.admin, 1)["patchy-preview-1"])
}

// checkTimeout holds the placeholder, the controller's annotations and the
// policy's sets to the applied auth-session-timeout, and the policy's
// recorded ceiling to ceiling.
func checkTimeout(t *testing.T, c client.Client, stage, applied, ceiling string) {
	t.Helper()
	if got := authOf(placeholder(t, c))[timeoutKey]; got != applied {
		t.Errorf("%s: placeholder %s = %q, want %q", stage, timeoutKey, got, applied)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "patchy", Name: "patchy-preview-controller-config"},
		cm); err != nil {
		t.Fatal(err)
	}
	current, err := preview.ParseAuthAnnotations(cm.Data["PATCHY_PREVIEW_AUTH_ANNOTATIONS"])
	if err != nil {
		t.Fatal(err)
	}
	for ns, set := range current {
		if set[timeoutKey] != applied {
			t.Errorf("%s: controller %s %s = %q, want %q", stage, ns, timeoutKey, set[timeoutKey], applied)
		}
	}
	for ns, set := range slotSets(t, c, 1) {
		if set[timeoutKey] != applied {
			t.Errorf("%s: policy set %s %s = %q, want %q", stage, ns, timeoutKey, set[timeoutKey], applied)
		}
	}
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "patchy-preview-ingresses"}, vap); err != nil {
		t.Fatal(err)
	}
	if got := vap.Annotations[ceilingMarker]; got != ceiling {
		t.Errorf("%s: %s = %q, want %q", stage, ceilingMarker, got, ceiling)
	}
}

// rotate is a key rotation's two passes, then the end of its overlap.
func (u *upgradeEnv) rotate() {
	t, admin := u.t, u.admin
	// First pass: generation 2 admitted, 1 still applied.
	u.upgrade(u.rotated)
	u.keys2 = secretData(t, admin, "patchy", keysSecret)
	keys1, keys2 := u.keys1, u.keys2
	if string(keys2["generation"]) != "2" || string(keys2["previousGeneration"]) != "1" ||
		!bytes.Equal(keys2["previousMaster"], keys1["master"]) || bytes.Equal(keys2["master"], keys1["master"]) {
		t.Fatalf("rotation: keys generation %q, previous %q; want 2 with the old master as previous",
			keys2["generation"], keys2["previousGeneration"])
	}
	checkClientSecrets(t, admin, keys2, 2)
	if got := admits(t, admin); got != "g1,g2" {
		t.Errorf("rotation: %s = %q, want g1,g2", admitsMarker, got)
	}
	checkController(t, admin, 1, 2)
	if got := authOf(placeholder(t, admin))[idpAnnotation]; !strings.Contains(got, "patchy-preview-oidc-g1") {
		t.Errorf("rotation, first pass: placeholder auth-idp-oidc %s, want generation 1's", got)
	}
	// Second pass, the same values: generation 2 applied, 1 still conforms.
	u.upgrade(u.rotated)
	if keys := secretData(t, admin, "patchy", keysSecret); !maps.EqualFunc(keys, keys2, bytes.Equal) {
		t.Fatal("rotation, second pass: the keys changed again")
	}
	checkController(t, admin, 2, 1)
	set2 := slotSets(t, admin, 2)
	if got := authOf(placeholder(t, admin)); !maps.Equal(got, set2["patchy-preview-0"]) {
		t.Errorf("rotation, second pass: placeholder %v, want generation 2's slot 0 set", got)
	}
	waitAdmitted(t, admin, withSet(ingress("patchy-preview-1", "probe-g2", "alb-preview", previewHost),
		set2["patchy-preview-1"]))
	if err := admin.Create(t.Context(), withSet(ingress("patchy-preview-1", "still-g1", "alb-preview", previewHost),
		u.set1["patchy-preview-1"]), client.DryRunAll); err != nil {
		t.Errorf("rotation overlap: generation 1's set refused: %v", err)
	}
	patchSet(t, admin, "patchy-preview-1", "preview-demo-1", set2["patchy-preview-1"])
	// The overlap ends: generation 1 is no longer admitted.
	u.rotated = append(u.rotated, "--set", "previewAuth.keys.dropPrevious=true")
	u.upgrade(u.rotated)
	if got := admits(t, admin); got != "g2" {
		t.Errorf("drop previous: %s = %q, want g2", admitsMarker, got)
	}
	checkController(t, admin, 2, 0)
	if _, ok := secretData(t, admin, "patchy", keysSecret)["previousMaster"]; ok {
		t.Error("drop previous: the keys Secret still holds the previous master")
	}
	// Both policies refuse it now; the API server names the first.
	waitRefused(t, admin, withSet(ingress("patchy-preview-1", "probe-dropped", "alb-preview", previewHost),
		u.set1["patchy-preview-1"]), authNotPinned, authRequired)
}

// refuseToStopRequiring: going back to permit (or off) while sign-in is
// required would have the placeholder refused part-way through, so the
// render refuses first.
func (u *upgradeEnv) refuseToStopRequiring() {
	permit := append(slices.Clone(u.withAuth), "--set", "previewAuth.keys.rotate=2",
		"--set", "previewAuth.keys.dropPrevious=true")
	for _, args := range [][]string{permit, u.base} {
		out, err := u.h.run(u.upgradeArgs(args)...)
		if err == nil || !strings.Contains(out, "preview sign-in is required on this cluster") {
			u.t.Errorf("upgrade that stops requiring sign-in: want refused, got %v: %s", err, out)
		}
	}
}

// refuseLostKeys: a lost keys Secret is never silently replaced.
func (u *upgradeEnv) refuseLostKeys() {
	t, admin := u.t, u.admin
	saved := &corev1.Secret{}
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "patchy", Name: keysSecret}, saved); err != nil {
		t.Fatal(err)
	}
	if err := admin.Delete(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	out, err := u.h.run(u.upgradeArgs(u.rotated)...)
	if err == nil || !strings.Contains(out, "is missing, but this release generated generation 2 before") {
		t.Errorf("upgrade without the keys Secret: want the missing-keys refusal, got %v: %s", err, out)
	}
	saved.ResourceVersion, saved.UID = "", ""
	if err := admin.Create(t.Context(), saved); err != nil {
		t.Fatalf("restore the keys Secret: %v", err)
	}
}

// rollBack: the non-conforming legacy Ingress can still be deleted (Auto
// Mode's finalizer removal is a metadata-only UPDATE); a rollback to the
// release without sign-in is refused by the kept required policy (the
// placeholder would drop its set) until the runbook deletes that policy's
// binding.
func (u *upgradeEnv) rollBack() {
	t, admin := u.t, u.admin
	deleteThroughFinalizer(t, admin, "patchy-preview-1", "legacy-noauth")
	out, err := u.h.run("rollback", "patchy", "1", "--namespace", "patchy", "--history-max", "0")
	if err == nil || !strings.Contains(out, "sign-in") {
		t.Errorf("rollback with the required policy bound: want refused, got %v: %s", err, out)
	}
	if err := admin.Delete(t.Context(), &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "patchy-preview-all-slots-ingress-auth"},
	}); err != nil {
		t.Fatalf("runbook: delete the required policy's binding: %v", err)
	}
	waitAdmitted(t, admin, ingress("patchy-preview-1", "probe-unbound", "alb-preview", previewHost))
	u.h.mustRun("rollback", "patchy", "1", "--namespace", "patchy", "--history-max", "0")
	if got := authOf(placeholder(t, admin)); len(got) != 0 {
		t.Errorf("after rollback: placeholder still carries %v", got)
	}
	if got := configData(t, admin, "patchy-preview-controller-config")["PATCHY_PREVIEW_AUTH_REQUIRED"]; got != "" {
		t.Errorf("after rollback: PATCHY_PREVIEW_AUTH_REQUIRED = %q", got)
	}
}

// deniedWith reports whether err is a denial naming one of wants.
func deniedWith(err error, wants ...string) bool {
	return err != nil && slices.ContainsFunc(wants, func(w string) bool { return strings.Contains(err.Error(), w) })
}

// helmRunner runs the helm binary against the envtest API server.
type helmRunner struct {
	t          *testing.T
	kubeconfig string
}

func newHelm(t *testing.T, env *envtest.Environment) helmRunner {
	t.Helper()
	user, err := env.AddUser(envtest.User{Name: "helm", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("add helm user: %v", err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	return helmRunner{t: t, kubeconfig: path}
}

func (h helmRunner) run(args ...string) (string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "helm", append(args, "--kubeconfig", h.kubeconfig)...)
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+h.t.TempDir(), "HELM_CONFIG_HOME="+h.t.TempDir(),
		"HELM_DATA_HOME="+h.t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h helmRunner) mustRun(args ...string) {
	h.t.Helper()
	if out, err := h.run(args...); err != nil {
		h.t.Fatalf("helm %s: %v\n%s", strings.Join(args[:2], " "), err, out)
	}
}

// ingressClassParamsCRD stands in for EKS Auto Mode's IngressClassParams, so
// the preview foundation installs.
func ingressClassParamsCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "ingressclassparams.eks.amazonaws.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "eks.amazonaws.com", Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "ingressclassparams", Singular: "ingressclassparams", Kind: "IngressClassParams",
				ListKind: "IngressClassParamsList",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: &preserve,
				}},
			}},
		},
	}
}

func secretData(t *testing.T, c client.Client, ns, name string) map[string][]byte {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
		t.Fatalf("get Secret %s/%s: %v", ns, name, err)
	}
	return s.Data
}

func configData(t *testing.T, c client.Client, name string) map[string]string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "patchy", Name: name}, cm); err != nil {
		t.Fatalf("get ConfigMap %s: %v", name, err)
	}
	return cm.Data
}

// checkClientSecrets loads the keys Secret as the relay does (keydir, from
// the mounted files) and holds every slot's client Secret to the relay's own
// derivation: the chart's sha256sum construction and Go's must agree.
func checkClientSecrets(t *testing.T, c client.Client, keys map[string][]byte, gen int) {
	t.Helper()
	dir := t.TempDir()
	for k, v := range keys {
		if err := os.WriteFile(filepath.Join(dir, k), v, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := keydir.Load(dir)
	if err != nil {
		t.Fatalf("the relay cannot load the chart's keys: %v", err)
	}
	if _, err := signer.New(loaded.SigningKey, loaded.PreviousSigningKey); err != nil {
		t.Fatalf("the relay cannot sign with the chart's keys: %v", err)
	}
	if loaded.Ring.Generation() != gen {
		t.Fatalf("keys generation %d, want %d", loaded.Ring.Generation(), gen)
	}
	gens := []int{gen}
	if prev, ok := keys["previousGeneration"]; ok {
		n, _ := strconv.Atoi(string(prev))
		gens = append(gens, n)
	}
	for slot := range 2 {
		for _, g := range gens {
			name := "patchy-preview-oidc-g" + strconv.Itoa(g)
			data := secretData(t, c, "patchy-preview-"+strconv.Itoa(slot), name)
			if got := string(data["clientID"]); got != "patchy-preview-s"+strconv.Itoa(slot) {
				t.Errorf("slot %d %s: clientID %q", slot, name, got)
			}
			secret := string(data["clientSecret"])
			if !loaded.Ring.VerifyClientSecret(slot, secret) {
				t.Errorf("slot %d %s: the relay refuses the chart's client secret", slot, name)
			}
			if g == gen && secret != loaded.Ring.ClientSecret(slot) {
				t.Errorf("slot %d %s: client secret differs from the relay's current derivation", slot, name)
			}
		}
	}
}

func admits(t *testing.T, c client.Client) string {
	t.Helper()
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "patchy-preview-ingresses"}, vap); err != nil {
		t.Fatal(err)
	}
	return vap.Annotations[admitsMarker]
}

// slotSets reads generation gen's pinned sets from the live slot Ingress
// policy's authWant variable: the sets the policies compare.
func slotSets(t *testing.T, c client.Client, gen int) map[string]map[string]string {
	t.Helper()
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "patchy-preview-ingresses"}, vap); err != nil {
		t.Fatal(err)
	}
	var want map[string][]map[string]string
	for _, v := range vap.Spec.Variables {
		if v.Name == "authWant" {
			if err := json.Unmarshal([]byte(v.Expression), &want); err != nil {
				t.Fatalf("authWant is not JSON: %v", err)
			}
		}
	}
	out := map[string]map[string]string{}
	for ns, sets := range want {
		for _, s := range sets {
			if strings.Contains(s[idpAnnotation], `"patchy-preview-oidc-g`+strconv.Itoa(gen)+`"`) {
				out[ns] = s
			}
		}
	}
	if len(out) != 2 {
		t.Fatalf("generation %d: sets for %d slots, want 2", gen, len(out))
	}
	return out
}

// checkController holds the preview-controller's ConfigMap to the
// controller's own settings validation, and to the generations it should
// apply and keep conforming.
func checkController(t *testing.T, c client.Client, apply, other int) {
	t.Helper()
	data := configData(t, c, "patchy-preview-controller-config")
	if data["PATCHY_PREVIEW_AUTH_REQUIRED"] != "true" {
		t.Fatalf("PATCHY_PREVIEW_AUTH_REQUIRED = %q", data["PATCHY_PREVIEW_AUTH_REQUIRED"])
	}
	current, err := preview.ParseAuthAnnotations(data["PATCHY_PREVIEW_AUTH_ANNOTATIONS"])
	if err != nil {
		t.Fatal(err)
	}
	previous, err := preview.ParseAuthAnnotations(data["PATCHY_PREVIEW_AUTH_PREVIOUS_ANNOTATIONS"])
	if err != nil {
		t.Fatal(err)
	}
	slots, _ := strconv.Atoi(data["PATCHY_PREVIEW_SLOT_COUNT"])
	retries, _ := strconv.Atoi(data["PATCHY_PREVIEW_MAX_RETRIES"])
	s := preview.Settings{
		Namespace: "patchy", SlotCount: slots, ImagePrefix: data["PATCHY_PREVIEW_IMAGE_PREFIX"],
		HostSuffix: data["PATCHY_PREVIEW_HOST_SUFFIX"], NodePool: data["PATCHY_PREVIEW_NODE_POOL"],
		NodeClass: data["PATCHY_PREVIEW_NODE_CLASS"], TaintKey: data["PATCHY_PREVIEW_TAINT_KEY"],
		RolloutTimeout: time.Minute, PollInterval: time.Second, MaxRetries: int32(retries),
		Auth: preview.AuthSettings{Required: true, Annotations: current, Previous: previous},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("the controller refuses the chart's settings: %v", err)
	}
	if got := current["patchy-preview-0"][idpAnnotation]; !strings.Contains(got, "-g"+strconv.Itoa(apply)+`"`) {
		t.Errorf("applied set %s, want generation %d", got, apply)
	}
	if other == 0 {
		if len(previous) != 0 {
			t.Errorf("previous sets %v, want none", previous)
		}
	} else if got := previous["patchy-preview-0"][idpAnnotation]; !strings.Contains(got, "-g"+strconv.Itoa(other)+`"`) {
		t.Errorf("previous set %s, want generation %d", got, other)
	}
}

func placeholder(t *testing.T, c client.Client) *networkingv1.Ingress {
	t.Helper()
	i := &networkingv1.Ingress{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "patchy-preview-0", Name: "patchy-preview-placeholder"},
		i); err != nil {
		t.Fatal(err)
	}
	return i
}

// authOf is an Ingress's sign-in annotations.
func authOf(i *networkingv1.Ingress) map[string]string {
	out := map[string]string{}
	for k, v := range i.Annotations {
		if strings.HasPrefix(k, "alb.ingress.kubernetes.io/auth-") {
			out[k] = v
		}
	}
	return out
}

// withValue is set with key's value replaced.
func withValue(set map[string]string, key, value string) map[string]string {
	out := maps.Clone(set)
	out[key] = value
	return out
}

func withSet(i *networkingv1.Ingress, set map[string]string) *networkingv1.Ingress {
	if i.Annotations == nil {
		i.Annotations = map[string]string{}
	}
	maps.Copy(i.Annotations, set)
	return i
}

func patchSet(t *testing.T, c client.Client, ns, name string, set map[string]string) {
	t.Helper()
	i := &networkingv1.Ingress{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, i); err != nil {
		t.Fatal(err)
	}
	for k := range authOf(i) {
		delete(i.Annotations, k)
	}
	if err := c.Update(t.Context(), withSet(i, set)); err != nil {
		t.Fatalf("patch %s/%s to its set: %v", ns, name, err)
	}
}

func deleteThroughFinalizer(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	key := client.ObjectKey{Namespace: ns, Name: name}
	i := &networkingv1.Ingress{}
	if err := c.Get(t.Context(), key, i); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), key, i); err != nil {
		t.Fatal(err)
	}
	i.Finalizers = nil
	if err := c.Update(t.Context(), i); err != nil {
		t.Fatalf("finalizer removal on a non-conforming Ingress refused: %v", err)
	}
	if err := c.Get(t.Context(), key, i); !apierrors.IsNotFound(err) {
		t.Errorf("%s/%s still present: %v", ns, name, err)
	}
}

// waitAdmitted waits until a dry-run create of obj is admitted: a policy
// change takes a moment to reach admission.
func waitAdmitted(t *testing.T, c client.Client, obj *networkingv1.Ingress) {
	t.Helper()
	waitFor(t, func() error { return c.Create(t.Context(), obj.DeepCopy(), client.DryRunAll) })
}

// waitRefused waits until a dry-run create of obj is refused with one of
// wants.
func waitRefused(t *testing.T, c client.Client, obj *networkingv1.Ingress, wants ...string) {
	t.Helper()
	waitFor(t, func() error {
		err := c.Create(t.Context(), obj.DeepCopy(), client.DryRunAll)
		if deniedWith(err, wants...) {
			return nil
		}
		return &waitError{err: err, want: strings.Join(wants, " or ")}
	})
}

type waitError struct {
	err  error
	want string
}

func (e *waitError) Error() string {
	if e.err == nil {
		return "admitted, want refused with " + e.want
	}
	return e.err.Error() + ", want " + e.want
}

func waitFor(t *testing.T, try func() error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := try()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
