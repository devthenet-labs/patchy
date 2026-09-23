// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import "testing"

func TestCheckVolumes(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		wantErr string
	}{
		{"nil", nil, ""},
		{"empty", []string{}, ""},
		{"one", []string{"/data"}, "image declares VOLUME `/data`; only patchy's emptyDirs are writable"},
		{"several, sorted", []string{"/var/lib/x", "/data", "/cache"},
			"image declares VOLUME `/cache`, `/data`, `/var/lib/x`; only patchy's emptyDirs are writable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckVolumes(c.in)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("CheckVolumes(%v) = %v, want nil", c.in, err)
				}
				return
			}
			if err == nil || err.Error() != c.wantErr {
				t.Errorf("CheckVolumes(%v) = %v, want %q", c.in, err, c.wantErr)
			}
			if !IsRejection(err) {
				t.Errorf("error is not a Rejection: %T", err)
			}
		})
	}
	in := []string{"/b", "/a"}
	_ = CheckVolumes(in)
	if in[0] != "/b" {
		t.Error("CheckVolumes must not sort the caller's slice")
	}
}
