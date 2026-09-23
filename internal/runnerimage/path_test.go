// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

func TestSanitizePath(t *testing.T) {
	def := strings.Split(DefaultPath, ":")
	cases := []struct {
		name    string
		env     []string
		want    []string
		wantErr string
	}{
		{"no PATH substitutes the runc default", nil, def, ""},
		{"no PATH among other vars", []string{"HOME=/root", "GOPATH=/go"}, def, ""},
		{"image PATH survives", []string{"PATH=/usr/local/go/bin:/usr/bin"}, []string{"/usr/local/go/bin", "/usr/bin"}, ""},
		{"empty components are dropped", []string{"PATH=/usr/bin::/bin"}, []string{"/usr/bin", "/bin"}, ""},
		{"trailing colon is dropped", []string{"PATH=/usr/bin:"}, []string{"/usr/bin"}, ""},
		{"leading colon is dropped", []string{"PATH=:/usr/bin"}, []string{"/usr/bin"}, ""},
		{"relative entries are dropped", []string{"PATH=bin:./x:../y:.:/usr/bin"}, []string{"/usr/bin"}, ""},
		{"the last PATH wins", []string{"PATH=/a", "PATH=/b"}, []string{"/b"}, ""},
		{"a similarly named variable is not PATH", []string{"PATHX=/x", "path=/y"}, def, ""},
		{"empty PATH is rejected", []string{"PATH="}, nil,
			"image PATH `` has no absolute entries; the pod would have no search path"},
		{"all-relative PATH is rejected", []string{"PATH=bin:."}, nil,
			"image PATH `bin:.` has no absolute entries; the pod would have no search path"},
		{"only colons is rejected", []string{"PATH=::"}, nil,
			"image PATH `::` has no absolute entries; the pod would have no search path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SanitizePath(c.env)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("SanitizePath(%v) error = %v, want %q", c.env, err, c.wantErr)
				}
				if !IsRejection(err) {
					t.Errorf("error is not a Rejection: %T", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SanitizePath(%v): %v", c.env, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("SanitizePath(%v) = %v, want %v", c.env, got, c.want)
			}
		})
	}
}

func TestSanitizePathProperty(t *testing.T) {
	alphabet := []byte("/:ab. -")
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20260922)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			b := make([]byte, r.Intn(20))
			for i := range b {
				b[i] = alphabet[r.Intn(len(alphabet))]
			}
			args[0] = reflect.ValueOf(string(b))
		},
	}
	// For any PATH value: every emitted component is non-empty, absolute and
	// came from the input, and the sanitizer only refuses when the input has
	// no absolute component at all.
	clean := func(value string) bool {
		got, err := SanitizePath([]string{"PATH=" + value})
		parts := strings.Split(value, ":")
		hasAbsolute := slices.ContainsFunc(parts, func(p string) bool { return strings.HasPrefix(p, "/") })
		if err != nil {
			return !hasAbsolute && IsRejection(err)
		}
		if len(got) == 0 || !hasAbsolute {
			return false
		}
		joined := strings.Join(got, ":")
		if strings.Contains(joined, "::") || strings.HasPrefix(joined, ":") || strings.HasSuffix(joined, ":") {
			return false
		}
		for _, p := range got {
			if p == "" || !strings.HasPrefix(p, "/") || !slices.Contains(parts, p) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(clean, cfg); err != nil {
		t.Error(err)
	}
}
