// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
)

// controllerNamePattern is the preview-controller's image leaf grammar
// (internal/controller/preview namePattern), which every image name must
// also satisfy under the preview prefix.
var controllerNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestSanitize(t *testing.T) {
	cases := []struct{ in, want, wantErr string }{
		{"Hello.Web", "hello-web", ""},
		{"patchy-target", "patchy-target", ""},
		{"My_App", "my-app", ""},
		{"a..b__c--d", "a-b-c-d", ""},
		{"--x--", "x", ""},
		{".github", "github", ""},
		{"ÄBC", "bc", ""},
		{"Web2.0", "web2-0", ""},
		{strings.Repeat("a", 62) + "-b", strings.Repeat("a", 62), ""},
		{strings.Repeat("ab", 40), strings.Repeat("ab", 31) + "a", ""},
		{"...", "", "no letter or digit"},
		{"", "", "no letter or digit"},
	}
	for _, tc := range cases {
		got, err := Sanitize(tc.in)
		switch {
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("Sanitize(%q) error = %v, want %q", tc.in, err, tc.wantErr)
		case tc.wantErr == "" && (err != nil || got != tc.want):
			t.Errorf("Sanitize(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestValidateSlug(t *testing.T) {
	for _, ok := range []string{"a", "hello-web", "web2", strings.Repeat("a", MaxSlug)} {
		if err := ValidateSlug(ok); err != nil {
			t.Errorf("ValidateSlug(%q) = %v", ok, err)
		}
	}
	// a--b passes the preview-controller's pattern but not ECR's, which
	// refuses doubled separators; the stricter grammar wins.
	for _, bad := range []string{"", "Hello", "a--b", "-a", "a-", "a.b", "a_b", "a/b", strings.Repeat("a", MaxSlug+1)} {
		if err := ValidateSlug(bad); err == nil {
			t.Errorf("ValidateSlug(%q) accepted it", bad)
		}
	}
}

// githubName is a repository name as GitHub allows it, so the property
// below explores the inputs Sanitize really sees as well as arbitrary
// strings.
type githubName string

// Generate implements quick.Generator.
func (githubName) Generate(r *rand.Rand, _ int) reflect.Value {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
	b := make([]byte, 1+r.Intn(100))
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return reflect.ValueOf(githubName(b))
}

// TestSanitizeProperty: whatever the name, Sanitize returns an image name
// that ValidateSlug accepts, the preview-controller's leaf pattern matches,
// fits MaxSlug and sanitizes to itself; or it fails, exactly when the name
// has no letter or digit to keep. Seeded, so the gate is deterministic.
func TestSanitizeProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(20261003))}
	check := func(name string) bool {
		got, err := Sanitize(name)
		keepable := strings.ContainsFunc(strings.ToLower(name), func(r rune) bool {
			return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		})
		if err != nil {
			if keepable {
				t.Logf("Sanitize(%q) failed although it has a letter or digit: %v", name, err)
			}
			return !keepable
		}
		again, err := Sanitize(got)
		ok := ValidateSlug(got) == nil && controllerNamePattern.MatchString(got) && len(got) <= MaxSlug &&
			err == nil && again == got
		if !ok {
			t.Logf("Sanitize(%q) = %q (again %q, %v)", name, got, again, err)
		}
		return ok
	}
	if err := quick.Check(check, cfg); err != nil {
		t.Error(err)
	}
	if err := quick.Check(func(n githubName) bool { return check(string(n)) }, cfg); err != nil {
		t.Error(err)
	}
}
