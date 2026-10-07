// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"sigs.k8s.io/yaml"
)

const testIssuer = "https://preview-auth.patchy.example.com"

// testIDP is the auth-idp-oidc value for key generation gen.
func testIDP(gen int) string {
	return fmt.Sprintf(`{"issuer":%[1]q,"authorizationEndpoint":"%[1]s/authorize","tokenEndpoint":"%[1]s/token",`+
		`"userInfoEndpoint":"%[1]s/userinfo","secretName":"patchy-preview-oidc-g%[2]d"}`, testIssuer, gen)
}

// testAuth is the pinned set of every slot of a slots-slot install at key
// generation gen, as the chart renders it.
func testAuth(slots, gen int) AuthAnnotations {
	out := AuthAnnotations{}
	for n := range slots {
		out[fmt.Sprintf("patchy-preview-%d", n)] = map[string]string{
			annotationAuthType:            "oidc",
			annotationAuthIDPOIDC:         testIDP(gen),
			annotationAuthOnUnauthRequest: "authenticate",
			annotationAuthScope:           "openid",
			annotationAuthSessionCookie:   fmt.Sprintf("patchy-preview-s%d", n),
			annotationAuthSessionTimeout:  "3600",
		}
	}
	return out
}

func authSettings() Settings {
	s := testSettings()
	s.Auth = AuthSettings{Required: true, Annotations: testAuth(s.SlotCount, 1)}
	return s
}

func TestAuthSettingsValidate(t *testing.T) {
	set := func(ns, key, value string) func(*Settings) {
		return func(s *Settings) { s.Auth.Annotations[ns][key] = value }
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Settings)
		ok     bool
	}{
		{"valid", func(*Settings) {}, true},
		{"off", func(s *Settings) { s.Auth = AuthSettings{} }, true},
		{"off with annotations", func(s *Settings) { s.Auth.Required = false }, false},
		{"off with previous annotations", func(s *Settings) {
			s.Auth = AuthSettings{Previous: testAuth(2, 1)}
		}, false},
		{"required without annotations", func(s *Settings) { s.Auth.Annotations = nil }, false},
		{"previous generation", func(s *Settings) { s.Auth.Previous = testAuth(2, 0) }, true},
		{"invalid previous generation", func(s *Settings) {
			s.Auth.Previous = testAuth(2, 0)
			s.Auth.Previous["patchy-preview-1"][annotationAuthType] = "cognito"
		}, false},
		{"a slot missing", func(s *Settings) { delete(s.Auth.Annotations, "patchy-preview-1") }, false},
		{"a slot beyond the slot count", func(s *Settings) { s.Auth.Annotations = testAuth(3, 1) }, false},
		{"a namespace that is no slot", func(s *Settings) {
			s.Auth.Annotations["default"] = s.Auth.Annotations["patchy-preview-1"]
			delete(s.Auth.Annotations, "patchy-preview-1")
		}, false},
		{"a key missing", func(s *Settings) { delete(s.Auth.Annotations["patchy-preview-0"], annotationAuthScope) }, false},
		{"an unknown key", set("patchy-preview-0", "alb.ingress.kubernetes.io/actions.x", "{}"), false},
		{"the health-check key", set("patchy-preview-0", annotationHealthcheck, "/"), false},
		{"auth-type cognito", set("patchy-preview-0", annotationAuthType, "cognito"), false},
		{"auth-type none", set("patchy-preview-0", annotationAuthType, "none"), false},
		{"unauthenticated requests allowed", set("patchy-preview-0", annotationAuthOnUnauthRequest, "allow"), false},
		{"unauthenticated requests denied", set("patchy-preview-0", annotationAuthOnUnauthRequest, "deny"), false},
		{"scope with more than openid", set("patchy-preview-0", annotationAuthScope, "openid email"), true},
		{"scope without openid", set("patchy-preview-0", annotationAuthScope, "email"), false},
		{"another slot's cookie", set("patchy-preview-0", annotationAuthSessionCookie, "patchy-preview-s1"), false},
		{"the ALB's default cookie", set("patchy-preview-0", annotationAuthSessionCookie,
			"AWSELBAuthSessionCookie"), false},
		{"timeout zero", set("patchy-preview-0", annotationAuthSessionTimeout, "0"), false},
		{"timeout leading zero", set("patchy-preview-0", annotationAuthSessionTimeout, "0900"), false},
		{"timeout signed", set("patchy-preview-0", annotationAuthSessionTimeout, "+900"), false},
		{"timeout past the ALB's limit", set("patchy-preview-0", annotationAuthSessionTimeout, "604801"), false},
		{"timeout at the ALB's limit", set("patchy-preview-0", annotationAuthSessionTimeout, "604800"), true},
		{"timeout not a number", set("patchy-preview-0", annotationAuthSessionTimeout, "1h"), false},
		{"idp not JSON", set("patchy-preview-0", annotationAuthIDPOIDC, "issuer=x"), false},
		{"idp trailing data", set("patchy-preview-0", annotationAuthIDPOIDC, testIDP(1)+"{}"), false},
		{"idp unknown field", set("patchy-preview-0", annotationAuthIDPOIDC,
			strings.Replace(testIDP(1), `{"issuer"`, `{"clientID":"x","issuer"`, 1)), false},
		{"idp http issuer", set("patchy-preview-0", annotationAuthIDPOIDC,
			strings.ReplaceAll(testIDP(1), "https://", "http://")), false},
		{"idp issuer with a trailing slash", set("patchy-preview-0", annotationAuthIDPOIDC,
			strings.Replace(testIDP(1), testIssuer+`"`, testIssuer+`/"`, 1)), false},
		{"idp endpoint elsewhere", set("patchy-preview-0", annotationAuthIDPOIDC,
			strings.Replace(testIDP(1), testIssuer+"/token", "https://evil.example/token", 1)), false},
		{"idp secret name not a name", set("patchy-preview-0", annotationAuthIDPOIDC,
			strings.Replace(testIDP(1), "patchy-preview-oidc-g1", "Bad_Name", 1)), false},
		{"idp secret name empty", set("patchy-preview-0", annotationAuthIDPOIDC,
			strings.Replace(testIDP(1), "patchy-preview-oidc-g1", "", 1)), false},
		{"another slot's issuer differs", set("patchy-preview-1", annotationAuthIDPOIDC,
			strings.ReplaceAll(testIDP(1), testIssuer, "https://other.example.com")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := authSettings()
			tc.mutate(&s)
			if err := s.Validate(); tc.ok != (err == nil) {
				t.Fatalf("Validate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestParseAuthAnnotations(t *testing.T) {
	for _, raw := range []string{"", "  \n"} {
		if got, err := ParseAuthAnnotations(raw); err != nil || got != nil {
			t.Errorf("ParseAuthAnnotations(%q) = %v, %v, want no set", raw, got, err)
		}
	}
	want := testAuth(2, 1)
	encoded := mustJSON(t, want)
	got, err := ParseAuthAnnotations(encoded)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %v, %v, want %v", got, err, want)
	}
	// auth-idp-oidc is a JSON string inside the JSON: it decodes to exactly
	// the bytes the policy compares, never re-encoded.
	if got["patchy-preview-0"][annotationAuthIDPOIDC] != testIDP(1) {
		t.Errorf("auth-idp-oidc = %q, want %q", got["patchy-preview-0"][annotationAuthIDPOIDC], testIDP(1))
	}
	for _, bad := range []string{"{", "[]", `{"patchy-preview-0":"x"}`, encoded + "{}"} {
		if _, err := ParseAuthAnnotations(bad); err == nil {
			t.Errorf("ParseAuthAnnotations(%q) accepted it", bad)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestAuthOffRendersTodaysIngress: off, the Ingress carries the health-check
// path alone, so every slot Ingress renders byte for byte as before the relay
// (TestGoldenSingleComponent pins the bytes).
func TestAuthOffRendersTodaysIngress(t *testing.T) {
	_, p := testPreview("demo-1", time.Now())
	for _, s := range []Settings{testSettings(), func() Settings {
		s := testSettings()
		s.Auth = AuthSettings{Required: false}
		return s
	}()} {
		got := s.ingress(p, 1).Annotations
		if want := map[string]string{annotationHealthcheck: "/health"}; !maps.Equal(got, want) {
			t.Errorf("annotations = %v, want %v", got, want)
		}
	}
}

// TestAuthIngressCarriesItsSlotsSet: on, each slot's Ingress carries that
// slot's pinned set, verbatim, beside the health-check path, and nothing
// else; the Deployment and Service render exactly as with auth off.
func TestAuthIngressCarriesItsSlotsSet(t *testing.T) {
	_, p := testPreview("demo-1", time.Now())
	s := authSettings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for slot := range int32(2) {
		want := maps.Clone(s.Auth.Annotations[s.slotName(slot)])
		want[annotationHealthcheck] = "/health"
		if got := s.ingress(p, slot).Annotations; !maps.Equal(got, want) {
			t.Errorf("slot %d annotations = %v, want %v", slot, got, want)
		}
		if !s.authConforms(s.slotName(slot), s.ingress(p, slot).Annotations) {
			t.Errorf("slot %d: rendered Ingress does not conform", slot)
		}
		other := s.slotName(1 - slot)
		if s.authConforms(other, s.ingress(p, slot).Annotations) {
			t.Errorf("slot %d's Ingress conforms in %s, whose cookie differs", slot, other)
		}
		off := testSettings()
		if !reflect.DeepEqual(s.deployment(p, 0, slot), off.deployment(p, 0, slot)) ||
			!reflect.DeepEqual(s.service(p, 0, slot), off.service(p, 0, slot)) {
			t.Errorf("slot %d: auth changed the Deployment or Service", slot)
		}
	}
}

// TestGoldenSingleComponentAuth pins a single-component Preview's objects
// with auth required. Beside single_component.yaml only the Ingress's
// annotations differ: the Deployment and Service bytes are the same, so
// turning auth on never restarts a live preview.
func TestGoldenSingleComponentAuth(t *testing.T) {
	_, p := testPreview("preview-demo-5", time.Date(2026, 10, 3, 7, 0, 0, 0, time.UTC))
	docs := make([]string, 0, 3)
	for _, obj := range renderSingle(authSettings(), p, 1) {
		raw, err := yaml.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, string(raw))
	}
	got := strings.Join(docs, "---\n")
	path := filepath.Join("testdata", "single_component_auth.yaml")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("single-component auth render differs from %s:\n--- want\n%s\n--- got\n%s", path, want, got)
	}
	off, err := os.ReadFile(filepath.Join("testdata", "single_component.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	offDocs := strings.Split(string(off), "---\n")
	if len(offDocs) != 3 || offDocs[0] != docs[0] || offDocs[1] != docs[1] {
		t.Error("auth changed the Deployment or Service bytes")
	}
}

// TestAuthConformsProperty: an Ingress conforms exactly when it carries one
// generation's whole pinned set for its own namespace. Adding any other
// annotation never changes that; dropping a pinned key or changing a pinned
// value to anything but the other generation's makes it unauthenticated.
func TestAuthConformsProperty(t *testing.T) {
	s := authSettings()
	s.Auth.Previous = testAuth(2, 0)
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261007))}
	property := func(slotSeed, genSeed uint8, extraKey, extraValue string, mutate uint8, value string) bool {
		slot := int32(slotSeed % 2)
		ns := s.slotName(slot)
		gen := s.Auth.Annotations
		if genSeed%2 == 1 {
			gen = s.Auth.Previous
		}
		ann := maps.Clone(gen[ns])
		if _, pinned := ann[extraKey]; !pinned && extraKey != "" {
			ann[extraKey] = extraValue
		}
		if !s.authConforms(ns, ann) {
			return false
		}
		key := authKeys[int(mutate)%len(authKeys)]
		switch mutate % 3 {
		case 0:
			delete(ann, key)
			return !s.authConforms(ns, ann)
		case 1:
			if value == s.Auth.Annotations[ns][key] || value == s.Auth.Previous[ns][key] {
				return true
			}
			ann[key] = value
			// Only auth-idp-oidc differs between the generations, so a
			// changed value matches neither.
			return !s.authConforms(ns, ann)
		default:
			// Its own set never conforms in the other slot: the cookie
			// differs.
			return !s.authConforms(s.slotName(1-slot), ann)
		}
	}
	if err := quick.Check(property, cfg); err != nil {
		t.Fatal(err)
	}
	// Off, or a namespace with no set, nothing conforms.
	if testSettings().authConforms("patchy-preview-0", testAuth(1, 1)["patchy-preview-0"]) {
		t.Error("conforms with auth off")
	}
	if s.authConforms("patchy-preview-3", testAuth(4, 1)["patchy-preview-3"]) {
		t.Error("conforms in a namespace with no pinned set")
	}
	if got := slices.Sorted(maps.Keys(testAuth(1, 1)["patchy-preview-0"])); !slices.Equal(got, authKeys) {
		t.Errorf("authKeys = %q, want sorted %q", authKeys, got)
	}
}
