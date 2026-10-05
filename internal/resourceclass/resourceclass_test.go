// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resourceclass

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func q(s string) *resource.Quantity {
	v := resource.MustParse(s)
	return &v
}

// large is the class the docs use as the example: a CPU request with no CPU
// limit, and a memory limit above the request.
func large() Resources {
	return Resources{
		Requests: Quantities{CPU: q("4"), Memory: q("8Gi")},
		Limits:   Quantities{Memory: q("10Gi")},
	}
}

// TestParse: the menu's JSON form, as the chart renders it through toJson,
// and every way it can be wrong. Each refusal names what is wrong.
func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Set
		wantErr string
	}{
		{name: "empty", raw: "", want: nil},
		{name: "whitespace", raw: "  \n", want: nil},
		{name: "null", raw: "null", want: nil},
		{name: "empty object", raw: "{}", want: Set{}},
		{name: "quoted quantities",
			raw:  `{"large":{"requests":{"cpu":"4","memory":"8Gi"},"limits":{"memory":"10Gi"}}}`,
			want: Set{"large": large()}},
		{name: "unquoted numbers (toJson of cpu: 4)",
			raw:  `{"large":{"requests":{"cpu":4,"memory":"8Gi"},"limits":{"memory":"10Gi"}}}`,
			want: Set{"large": large()}},
		{name: "fractional number",
			raw: `{"half":{"requests":{"cpu":0.5,"memory":"1Gi"},"limits":{"memory":"1Gi","cpu":1}}}`,
			want: Set{"half": {Requests: Quantities{CPU: q("500m"), Memory: q("1Gi")},
				Limits: Quantities{CPU: q("1"), Memory: q("1Gi")}}}},
		{name: "memory as a number of bytes",
			raw:  `{"m":{"requests":{"cpu":"1","memory":1073741824},"limits":{"memory":2147483648}}}`,
			want: Set{"m": {Requests: Quantities{CPU: q("1"), Memory: q("1Gi")}, Limits: Quantities{Memory: q("2Gi")}}}},
		{name: "not an object", raw: `["large"]`, wantErr: "not a JSON object"},
		{name: "not JSON", raw: `large: {}`, wantErr: "not a JSON object"},
		{name: "unknown key in a class", raw: `{"large":{"requests":{"cpu":"4","memory":"8Gi"},` +
			`"limits":{"memory":"10Gi"},"nodeSelector":{}}}`, wantErr: "unknown field"},
		{name: "unknown resource", raw: `{"large":{"requests":{"cpu":"4","memory":"8Gi","ephemeral-storage":"8Gi"},` +
			`"limits":{"memory":"10Gi"}}}`, wantErr: "unknown field"},
		{name: "trailing data", raw: `{} {}`, wantErr: "trailing data"},
		{name: "bad quantity", raw: `{"large":{"requests":{"cpu":"four","memory":"8Gi"},"limits":{"memory":"10Gi"}}}`,
			wantErr: "not a JSON object"},
		{name: "negative cpu", raw: `{"large":{"requests":{"cpu":-1,"memory":"8Gi"},"limits":{"memory":"10Gi"}}}`,
			wantErr: `class "large": requests.cpu -1 must be positive`},
		{name: "zero memory limit", raw: `{"large":{"requests":{"cpu":"4","memory":"8Gi"},"limits":{"memory":"0"}}}`,
			wantErr: "limits.memory 0 must be positive"},
		{name: "request above limit", raw: `{"large":{"requests":{"cpu":"4","memory":"8Gi"},"limits":{"memory":"6Gi"}}}`,
			wantErr: "limits.memory 6Gi is below requests.memory 8Gi"},
		{name: "cpu request above cpu limit",
			raw:     `{"large":{"requests":{"cpu":"4","memory":"8Gi"},"limits":{"cpu":"2","memory":"8Gi"}}}`,
			wantErr: "limits.cpu 2 is below requests.cpu 4"},
		{name: "3500 CPUs", raw: `{"large":{"requests":{"cpu":3500,"memory":"8Gi"},"limits":{"memory":"8Gi"}}}`,
			wantErr: "requests.cpu 3500 is above 64"},
		{name: "7Mi of memory", raw: `{"large":{"requests":{"cpu":"4","memory":"7Mi"},"limits":{"memory":"7Mi"}}}`,
			wantErr: "requests.memory 7Mi is below 128Mi"},
		{name: "missing memory limit", raw: `{"large":{"requests":{"cpu":"4","memory":"8Gi"}}}`,
			wantErr: "limits.memory is required"},
		{name: "missing requests", raw: `{"large":{"limits":{"memory":"8Gi"}}}`,
			wantErr: "requests.cpu is required"},
		{name: "null class", raw: `{"large":null}`, wantErr: "requests.memory is required"},
		{name: "bad name", raw: `{"Large":{"requests":{"cpu":"4","memory":"8Gi"},"limits":{"memory":"8Gi"}}}`,
			wantErr: `class name "Large" is not a DNS label`},
		{name: "long name", raw: `{"` + strings.Repeat("a", 33) + `":{"requests":{"cpu":"4","memory":"8Gi"},` +
			`"limits":{"memory":"8Gi"}}}`, wantErr: "is not a DNS label of at most 32"},
		{name: "fraction of a millicore",
			raw:     `{"x":{"requests":{"cpu":"10.5m","memory":"1Gi"},"limits":{"memory":"1Gi"}}}`,
			wantErr: "requests.cpu 10500u is not a whole number of millicores"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse(%s) err = %v, want one containing %q", tt.raw, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%s): %v", tt.raw, err)
			}
			if !sameSet(got, tt.want) {
				t.Errorf("Parse(%s) = %v, want %v", tt.raw, got.Encode(), tt.want.Encode())
			}
		})
	}
}

// TestParseRefusesTooManyClasses: the menu itself is bounded.
func TestParseRefusesTooManyClasses(t *testing.T) {
	s := Set{}
	for i := range MaxClasses + 1 {
		s[fmt.Sprintf("c%d", i)] = large()
	}
	if _, err := Parse(s.Encode()); err == nil || !strings.Contains(err.Error(), "at most 16") {
		t.Fatalf("Parse of %d classes err = %v, want the class limit", len(s), err)
	}
	delete(s, "c0")
	if _, err := Parse(s.Encode()); err != nil {
		t.Fatalf("Parse of %d classes: %v", len(s), err)
	}
}

// TestValidateReportsEveryProblem: an operator fixing a class sees every
// problem at once, not one per restart.
func TestValidateReportsEveryProblem(t *testing.T) {
	err := ValidateClass(Resources{Requests: Quantities{CPU: q("-1")}, Limits: Quantities{CPU: q("1000")}})
	if err == nil {
		t.Fatal("ValidateClass accepted a broken class")
	}
	for _, want := range []string{"requests.memory is required", "limits.memory is required",
		"requests.cpu -1 must be positive", "limits.cpu 1k is above 64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestDefaultMayBePartial: the default is not a class; any subset of the
// four quantities is a valid default, and none is today's Job.
func TestDefaultMayBePartial(t *testing.T) {
	for _, r := range []Resources{
		{},
		{Requests: Quantities{CPU: q("250m")}},
		{Requests: Quantities{Memory: q("512Mi")}},
		{Limits: Quantities{Memory: q("2Gi")}},
		{Requests: Quantities{CPU: q("250m"), Memory: q("512Mi")}, Limits: Quantities{CPU: q("2"), Memory: q("2Gi")}},
	} {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%s): %v", r, err)
		}
	}
	if !(Resources{}).IsZero() {
		t.Error("empty Resources is not IsZero")
	}
	if got := (Resources{}).String(); got != "no CPU or memory requests or limits" {
		t.Errorf("String() = %q", got)
	}
	if got := large().String(); got != "requests cpu 4, memory 8Gi; limits memory 10Gi" {
		t.Errorf("String() = %q", got)
	}
}

// TestStringsAreCanonical: what reaches the Job is the canonical quantity,
// whatever spelling the operator wrote.
func TestStringsAreCanonical(t *testing.T) {
	s, err := Parse(`{"x":{"requests":{"cpu":0.5,"memory":"1024Mi"},"limits":{"cpu":"2000m","memory":1073741824}}}`)
	if err != nil {
		t.Fatal(err)
	}
	cr, mr, cl, ml := s["x"].Strings()
	// A number of bytes stays a decimal quantity: the same size, spelled
	// as the API server will store it.
	if got := []string{cr, mr, cl, ml}; strings.Join(got, " ") != "500m 1Gi 2 1073741824" {
		t.Errorf("Strings() = %v", got)
	}
	if cr, mr, cl, ml := (Resources{}).Strings(); cr+mr+cl+ml != "" {
		t.Errorf("empty Resources renders %q %q %q %q", cr, mr, cl, ml)
	}
}

// genQuantity draws a CPU or memory quantity around and across the bounds,
// in the spellings operators write.
func genQuantity(r *rand.Rand, memory bool) *resource.Quantity {
	if memory {
		units := []string{"Mi", "Gi", "M", "G", ""}
		n := []int64{-1, 0, 1, 7, 64, 128, 256, 512, 1000, 1024, 8192, 600000}
		v := resource.MustParse(fmt.Sprintf("%d%s", n[r.Intn(len(n))], units[r.Intn(len(units))]))
		return &v
	}
	forms := []string{"-1", "0", "1m", "10m", "100m", "500m", "1", "2", "3500m", "4", "16", "64", "65", "3500", "0.5",
		"1.5"}
	v := resource.MustParse(forms[r.Intn(len(forms))])
	return &v
}

// within reports whether a set quantity is inside the bounds by
// construction, the property's own oracle.
func within(v *resource.Quantity, lo, hi resource.Quantity) bool {
	return v == nil || v.Sign() > 0 && v.Cmp(lo) >= 0 && v.Cmp(hi) <= 0
}

// TestPropertyValidateIsExact: for any class, ValidateClass accepts it
// exactly when the three required quantities are set, every set quantity is
// inside its bounds and every request is at or below its limit; and every
// accepted class survives the JSON round trip unchanged and renders the
// strings the Job carries in an order the API server accepts (request <=
// limit). Seeded, so the gate is deterministic; the generator reaches both
// verdicts.
func TestPropertyValidateIsExact(t *testing.T) {
	rng := rand.New(rand.NewSource(20261004))
	maybe := func(memory bool) *resource.Quantity {
		if rng.Intn(5) == 0 {
			return nil
		}
		return genQuantity(rng, memory)
	}
	// sane draws a class that is valid unless a draw lands on a bound: a
	// request, and a limit at or above it.
	sane := func() Resources {
		cpus := []string{"10m", "250m", "1", "2", "3500m", "4", "16", "64"}
		mems := []string{"128Mi", "512Mi", "1Gi", "6Gi", "8Gi", "10Gi", "64Gi", "512Gi"}
		ci, mi := rng.Intn(len(cpus)), rng.Intn(len(mems))
		r := Resources{
			Requests: Quantities{CPU: q(cpus[ci]), Memory: q(mems[mi])},
			Limits:   Quantities{Memory: q(mems[mi+rng.Intn(len(mems)-mi)])},
		}
		if rng.Intn(2) == 0 {
			r.Limits.CPU = q(cpus[ci+rng.Intn(len(cpus)-ci)])
		}
		return r
	}
	var accepted, refused int
	for i := range 5000 {
		r := Resources{
			Requests: Quantities{CPU: maybe(false), Memory: maybe(true)},
			Limits:   Quantities{CPU: maybe(false), Memory: maybe(true)},
		}
		if rng.Intn(2) == 0 {
			r = sane()
		}
		want := r.Requests.CPU != nil && r.Requests.Memory != nil && r.Limits.Memory != nil &&
			within(r.Requests.CPU, minCPU, maxCPU) && within(r.Limits.CPU, minCPU, maxCPU) &&
			within(r.Requests.Memory, minMemory, maxMemory) && within(r.Limits.Memory, minMemory, maxMemory) &&
			(r.Limits.CPU == nil || r.Limits.CPU.Cmp(*r.Requests.CPU) >= 0) &&
			r.Limits.Memory.Cmp(*r.Requests.Memory) >= 0
		err := ValidateClass(r)
		if (err == nil) != want {
			t.Fatalf("iteration %d: ValidateClass(%s) = %v, want accepted=%v", i, r, err, want)
		}
		if err != nil {
			refused++
			continue
		}
		accepted++
		s := Set{"c": r}
		back, err := Parse(s.Encode())
		if err != nil || !sameSet(back, s) {
			t.Fatalf("iteration %d: %s does not round-trip: %v (%s)", i, s.Encode(), err, back.Encode())
		}
		cr, mr, cl, ml := r.Strings()
		for _, pair := range [][2]string{{cr, cl}, {mr, ml}} {
			if pair[1] == "" {
				continue
			}
			if req, lim := resource.MustParse(pair[0]), resource.MustParse(pair[1]); req.Cmp(lim) > 0 {
				t.Fatalf("iteration %d: rendered request %s above limit %s", i, pair[0], pair[1])
			}
		}
	}
	if accepted < 100 || refused < 100 {
		t.Errorf("generator did not reach both verdicts often: accepted=%d refused=%d", accepted, refused)
	}
}

// sameSet compares two menus by value, quantities by Cmp.
func sameSet(a, b Set) bool {
	if len(a) != len(b) {
		return false
	}
	for name, ra := range a {
		rb, ok := b[name]
		if !ok || !sameQ(ra.Requests.CPU, rb.Requests.CPU) || !sameQ(ra.Requests.Memory, rb.Requests.Memory) ||
			!sameQ(ra.Limits.CPU, rb.Limits.CPU) || !sameQ(ra.Limits.Memory, rb.Limits.Memory) {
			return false
		}
	}
	return true
}

func sameQ(a, b *resource.Quantity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Cmp(*b) == 0
}

// TestValidName pins the class name grammar the Project schema repeats.
func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"large": true, "x": true, "gpu-2x": true, "4cpu": true, strings.Repeat("a", 32): true,
		"": false, "Large": false, "-large": false, "large-": false, "la_rge": false, "large.1": false,
		strings.Repeat("a", 33): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}
