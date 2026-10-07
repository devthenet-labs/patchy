// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func TestNewCallbacks(t *testing.T) {
	for _, tc := range []struct {
		suffix string
		ok     bool
	}{
		{"preview.example.com", true},
		{"a.b", true},
		{"xn--bcher-kva.example", true},
		{"", false},
		{"example", false},
		{".example.com", false},
		{"example.com.", false},
		{"Example.com", false},
		{"ex_ample.com", false},
		{"-a.example.com", false},
		{"a-.example.com", false},
		{"a..example.com", false},
		{"*.example.com", false},
		{strings.Repeat("a", 64) + ".com", false},
	} {
		_, err := NewCallbacks(tc.suffix)
		if (err == nil) != tc.ok {
			t.Errorf("NewCallbacks(%q) err = %v, want ok %v", tc.suffix, err, tc.ok)
		}
	}
}

func TestCallbackParse(t *testing.T) {
	cb := newCallbacks(t)
	for _, tc := range []struct {
		raw   string
		label string // "" means refused
	}{
		{"https://intent-42.preview.example.com/oauth2/idpresponse", "intent-42"},
		{"https://a.preview.example.com/oauth2/idpresponse", "a"},
		{"https://" + strings.Repeat("a", 63) + ".preview.example.com/oauth2/idpresponse", strings.Repeat("a", 63)},
		{"https://0.preview.example.com/oauth2/idpresponse", "0"},
		// Not a preview callback.
		{"", ""},
		{"http://a.preview.example.com/oauth2/idpresponse", ""},
		{"HTTPS://a.preview.example.com/oauth2/idpresponse", ""},
		{"https://A.preview.example.com/oauth2/idpresponse", ""},
		{"https://a.Preview.example.com/oauth2/idpresponse", ""},
		{"https://a.preview.example.com:443/oauth2/idpresponse", ""},
		{"https://a.preview.example.com:8443/oauth2/idpresponse", ""},
		{"https://user@a.preview.example.com/oauth2/idpresponse", ""},
		{"https://user:pw@a.preview.example.com/oauth2/idpresponse", ""},
		{"https://a.preview.example.com/oauth2/idpresponse?x=1", ""},
		{"https://a.preview.example.com/oauth2/idpresponse?", ""},
		{"https://a.preview.example.com/oauth2/idpresponse#f", ""},
		{"https://a.preview.example.com/oauth2/idpresponse/", ""},
		{"https://a.preview.example.com/oauth2/IDPresponse", ""},
		{"https://a.preview.example.com/oauth2/%69dpresponse", ""},
		{"https://a.preview.example.com//oauth2/idpresponse", ""},
		{"https://a.preview.example.com/x/../oauth2/idpresponse", ""},
		{"https://a.preview.example.com.//oauth2/idpresponse", ""},
		{"https://a.preview.example.com./oauth2/idpresponse", ""},
		{"https://%61.preview.example.com/oauth2/idpresponse", ""},
		{"https://preview.example.com/oauth2/idpresponse", ""},
		{"https://.preview.example.com/oauth2/idpresponse", ""},
		{"https://a.b.preview.example.com/oauth2/idpresponse", ""},
		{"https://evil.com/.preview.example.com/oauth2/idpresponse", ""},
		{"https://evil.com#.preview.example.com/oauth2/idpresponse", ""},
		{"https://evil.com?.preview.example.com/oauth2/idpresponse", ""},
		{"https://evil.com\\.preview.example.com/oauth2/idpresponse", ""},
		{"https://a.preview.example.com.evil.com/oauth2/idpresponse", ""},
		{"https://apreview.example.com/oauth2/idpresponse", ""},
		{"https://a.evilpreview.example.com/oauth2/idpresponse", ""},
		{"https://-a.preview.example.com/oauth2/idpresponse", ""},
		{"https://a-.preview.example.com/oauth2/idpresponse", ""},
		{"https://xn--bcher-kva.preview.example.com/oauth2/idpresponse", ""},
		{"https://ab--c.preview.example.com/oauth2/idpresponse", ""},
		{"https://a_b.preview.example.com/oauth2/idpresponse", ""},
		{"https://a b.preview.example.com/oauth2/idpresponse", ""},
		{" https://a.preview.example.com/oauth2/idpresponse", ""},
		{"https://a.preview.example.com/oauth2/idpresponse ", ""},
		{"https://a.preview.example.com/oauth2/idpresponse\n", ""},
		{"https://bücher.preview.example.com/oauth2/idpresponse", ""},
		{"https://demo-\u0430.preview.example.com/oauth2/idpresponse", ""}, // a Cyrillic a, a homoglyph of a valid label
		{"https://" + strings.Repeat("a", 64) + ".preview.example.com/oauth2/idpresponse", ""},
		{"//a.preview.example.com/oauth2/idpresponse", ""},
		{"https:a.preview.example.com/oauth2/idpresponse", ""},
	} {
		label, err := cb.Parse(tc.raw)
		if tc.label == "" {
			if err == nil {
				t.Errorf("Parse(%q) = %q, want refused", tc.raw, label)
			}
			continue
		}
		if err != nil || label != tc.label {
			t.Errorf("Parse(%q) = %q, %v; want %q", tc.raw, label, err, tc.label)
		}
	}
	if _, err := (Callbacks{}).Parse("https://a.preview.example.com/oauth2/idpresponse"); err == nil {
		t.Error("the zero Callbacks accepted a redirect URI")
	}
}

// TestCallbackAcceptsEveryLabelProperty: every valid label's callback URL is
// accepted, and Parse returns the label, which round-trips through URL.
func TestCallbackAcceptsEveryLabelProperty(t *testing.T) {
	cb := newCallbacks(t)
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) { args[0] = reflect.ValueOf(randLabel(r)) }
	prop := func(label string) bool {
		raw, err := cb.URL(label)
		if err != nil || raw != "https://"+label+"."+testSuffix+CallbackPath {
			return false
		}
		got, err := cb.Parse(raw)
		return err == nil && got == label
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// mutations are the ways the property bends a valid callback URL. Each one
// yields a URL that is not https://<one valid label>.<suffix>/oauth2/idpresponse.
var mutations = []func(r *rand.Rand, label string) string{
	func(r *rand.Rand, l string) string {
		return "https://" + upperOne(r, l) + "." + testSuffix + CallbackPath
	},
	func(r *rand.Rand, l string) string {
		return "https://" + l + "." + upperOne(r, testSuffix) + CallbackPath
	},
	func(r *rand.Rand, l string) string {
		return "https://" + l + "." + testSuffix + ":" + []string{"443", "80", "1", ""}[r.Intn(4)] + CallbackPath
	},
	func(_ *rand.Rand, l string) string { return "https://u@" + l + "." + testSuffix + CallbackPath },
	func(r *rand.Rand, l string) string {
		return "https://" + l + "." + testSuffix + CallbackPath + []string{"?", "?a=b", "#", "#x", "/", "%2F"}[r.Intn(6)]
	},
	func(r *rand.Rand, l string) string { // a percent-escape anywhere in the label
		i := r.Intn(len(l))
		return "https://" + l[:i] + "%" + strings.ToUpper(hexByte(l[i])) + l[i+1:] + "." + testSuffix + CallbackPath
	},
	func(_ *rand.Rand, l string) string { return "https://" + l + "." + testSuffix + "." + CallbackPath },
	func(_ *rand.Rand, l string) string { return "https://xn--" + l + "." + testSuffix + CallbackPath },
	func(r *rand.Rand, l string) string { // a second label
		return "https://" + randLabel(r) + "." + l + "." + testSuffix + CallbackPath
	},
	func(r *rand.Rand, l string) string { // a suffix lookalike
		return "https://" + l + "." + testSuffix + []string{".evil", "-evil.com", ".evil.com"}[r.Intn(3)] + CallbackPath
	},
	func(_ *rand.Rand, l string) string {
		return "https://" + l + "evil." + testSuffix[len("preview."):] + CallbackPath
	},
	func(_ *rand.Rand, l string) string { return "https://" + l + testSuffix + CallbackPath },
	func(r *rand.Rand, l string) string { // a foreign host with the suffix in its path, query or fragment
		sep := []string{"/", "?", "#", "\\", "@"}[r.Intn(5)]
		return "https://" + l + ".evil.com" + sep + "x." + testSuffix + CallbackPath
	},
	func(r *rand.Rand, l string) string { // whitespace or a control byte somewhere
		raw := "https://" + l + "." + testSuffix + CallbackPath
		i := r.Intn(len(raw) + 1)
		return raw[:i] + string([]byte{" \t\n\r\x00\x7f"[r.Intn(6)]}) + raw[i:]
	},
	func(r *rand.Rand, l string) string { // another path
		return "https://" + l + "." + testSuffix + []string{"/oauth2/idpresponse2", "/oauth2/", "/", "",
			"/oauth2//idpresponse", "/Oauth2/idpresponse", "/oauth2/idpresponse/.."}[r.Intn(7)]
	},
	func(r *rand.Rand, l string) string {
		return []string{"http://", "HTTPS://", "https:/", "//", "https:///", "wss://"}[r.Intn(6)] + l + "." + testSuffix +
			CallbackPath
	},
	func(r *rand.Rand, l string) string { // an invalid label character, non-ASCII (an IDN label) included
		i := r.Intn(len(l) + 1)
		c := invalidLabelChars[r.Intn(len(invalidLabelChars))]
		return "https://" + l[:i] + c + l[i:] + "." + testSuffix + CallbackPath
	},
	func(r *rand.Rand, l string) string { // a leading or trailing hyphen
		if r.Intn(2) == 0 {
			return "https://-" + l + "." + testSuffix + CallbackPath
		}
		return "https://" + l + "-." + testSuffix + CallbackPath
	},
}

// invalidLabelChars are inserted whole into a valid label, so a multi-byte
// character is never split.
var invalidLabelChars = []string{
	"_", ".", "*", "~", "!", "$", "&", "'", "(", ")", "+", ",", ";", "=",
	"ü", "é", "ı", "\u0430", // non-ASCII: IDN letters and a Cyrillic homoglyph of a
}

func upperOne(r *rand.Rand, s string) string {
	var letters []int
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'z' {
			letters = append(letters, i)
		}
	}
	if len(letters) == 0 {
		return s + "A" // no letter to upper-case: add an upper-case one
	}
	i := letters[r.Intn(len(letters))]
	return s[:i] + strings.ToUpper(s[i:i+1]) + s[i+1:]
}

func hexByte(c byte) string {
	return string("0123456789abcdef"[c>>4]) + string("0123456789abcdef"[c&15])
}

// TestCallbackRefusesMutationsProperty: no mutation of a valid callback is
// accepted.
func TestCallbackRefusesMutationsProperty(t *testing.T) {
	cb := newCallbacks(t)
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		label := randLabel(r)
		args[0] = reflect.ValueOf(mutations[r.Intn(len(mutations))](r, label))
	}
	prop := func(raw string) bool {
		_, err := cb.Parse(raw)
		return err != nil
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestCallbackNeverForeignProperty: whatever string Parse accepts, out of
// arbitrary strings built from URL fragments, is exactly the callback URL of
// the returned label on this suffix. So no accepted redirect URI names a
// foreign host or another path.
func TestCallbackNeverForeignProperty(t *testing.T) {
	cb := newCallbacks(t)
	pieces := []string{"https://", "http://", "a", "b-1", "-", ".", "..", testSuffix, "preview", "example.com", "evil",
		CallbackPath, "/", "?", "#", "@", ":", ":443", "%2e", "%61", "\\", " ", "A", "xn--", "oauth2", "idpresponse"}
	cfg := quickConfig(20000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		var b strings.Builder
		if r.Intn(2) == 0 {
			b.WriteString("https://")
		}
		for n := 1 + r.Intn(8); n > 0; n-- {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		if r.Intn(2) == 0 {
			b.WriteString("." + testSuffix + CallbackPath)
		}
		args[0] = reflect.ValueOf(b.String())
	}
	accepted := 0
	prop := func(raw string) bool {
		label, err := cb.Parse(raw)
		if err != nil {
			return true
		}
		accepted++
		return ValidLabel(label) && !strings.Contains(label, ".") &&
			raw == "https://"+label+"."+testSuffix+CallbackPath
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
	if accepted == 0 {
		t.Error("the generator never produced an accepted URL, so the property checked nothing")
	}
}

func TestValidLabel(t *testing.T) {
	for _, tc := range []struct {
		l  string
		ok bool
	}{
		{"a", true}, {"0", true}, {"intent-7", true}, {"a-b-c", true}, {"abc--d", true},
		{strings.Repeat("z", 63), true},
		{"", false}, {"-a", false}, {"a-", false}, {"A", false}, {"a.b", false}, {"a_b", false},
		{"xn--abc", false}, {"ab--c", false}, {strings.Repeat("z", 64), false}, {"ü", false},
	} {
		if got := ValidLabel(tc.l); got != tc.ok {
			t.Errorf("ValidLabel(%q) = %v, want %v", tc.l, got, tc.ok)
		}
	}
}
