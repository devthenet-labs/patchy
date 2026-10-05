// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resourceclass

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Limits on the menu itself.
const (
	// MaxClasses bounds how many classes an operator may define.
	MaxClasses = 16
	// MaxNameLength bounds a class name; the Project schema holds
	// agentResourceClass to the same length.
	MaxNameLength = 32
)

// The bounds every CPU and memory quantity must sit within, request or
// limit. They are sanity bounds, there to turn a typo into a startup error:
// 3500 CPUs (meant 3500m) or 7Mi of memory (meant 7Gi). An agent pod runs
// agent-runner and the claude CLI, which need more memory than MinMemory
// before any tool the build starts.
const (
	MinCPU    = "10m"
	MaxCPU    = "64"
	MinMemory = "128Mi"
	MaxMemory = "512Gi"
)

var (
	minCPU    = resource.MustParse(MinCPU)
	maxCPU    = resource.MustParse(MaxCPU)
	minMemory = resource.MustParse(MinMemory)
	maxMemory = resource.MustParse(MaxMemory)
)

// namePattern is a DNS label: what a Project's agentResourceClass may hold.
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidName reports whether name may name a class: a DNS label of at most
// MaxNameLength characters.
func ValidName(name string) bool {
	return len(name) <= MaxNameLength && namePattern.MatchString(name)
}

// Quantities are a CPU and a memory quantity, either of which may be unset.
type Quantities struct {
	CPU    *resource.Quantity `json:"cpu,omitempty"`
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// Resources are an agent Job's CPU and memory requests and limits: the
// default's shape and every class's. Ephemeral storage is not among them;
// it is the repository-image wall on disk and stays the job controllers'
// own setting.
type Resources struct {
	Requests Quantities `json:"requests,omitzero"`
	Limits   Quantities `json:"limits,omitzero"`
}

// IsZero reports Resources with nothing set: today's agent Job, with no CPU
// or memory requests or limits.
func (r Resources) IsZero() bool {
	return r.Requests.CPU == nil && r.Requests.Memory == nil && r.Limits.CPU == nil && r.Limits.Memory == nil
}

// Strings are the four quantities in their canonical form, each empty when
// unset: what jobs.Config and a jobs.Spec override carry.
func (r Resources) Strings() (cpuRequest, memoryRequest, cpuLimit, memoryLimit string) {
	return str(r.Requests.CPU), str(r.Requests.Memory), str(r.Limits.CPU), str(r.Limits.Memory)
}

func str(q *resource.Quantity) string {
	if q == nil {
		return ""
	}
	return q.String()
}

// String describes the resources for a human: "requests cpu 4, memory 8Gi;
// limits memory 10Gi", or "no CPU or memory requests or limits".
func (r Resources) String() string {
	part := func(q Quantities) string {
		var s []string
		if q.CPU != nil {
			s = append(s, "cpu "+q.CPU.String())
		}
		if q.Memory != nil {
			s = append(s, "memory "+q.Memory.String())
		}
		return strings.Join(s, ", ")
	}
	var out []string
	if p := part(r.Requests); p != "" {
		out = append(out, "requests "+p)
	}
	if p := part(r.Limits); p != "" {
		out = append(out, "limits "+p)
	}
	if len(out) == 0 {
		return "no CPU or memory requests or limits"
	}
	return strings.Join(out, "; ")
}

// Validate checks every quantity that is set: positive, a whole number of
// millicores or bytes, within the bounds, and each request at or below its
// limit. It reports every problem, each named by its path in the chart's
// shape (requests.cpu, limits.memory).
func (r Resources) Validate() error {
	var errs []error
	check := func(field string, q *resource.Quantity, lo, hi resource.Quantity, whole func(*resource.Quantity) bool,
		unit string) {
		switch {
		case q == nil:
		case q.Sign() <= 0:
			errs = append(errs, fmt.Errorf("%s %s must be positive", field, q))
		case !whole(q):
			errs = append(errs, fmt.Errorf("%s %s is not a whole number of %s", field, q, unit))
		case q.Cmp(lo) < 0:
			errs = append(errs, fmt.Errorf("%s %s is below %s, the least patchy accepts (a typo?)", field, q, lo.String()))
		case q.Cmp(hi) > 0:
			errs = append(errs, fmt.Errorf("%s %s is above %s, the most patchy accepts (a typo?)", field, q, hi.String()))
		}
	}
	check("requests.cpu", r.Requests.CPU, minCPU, maxCPU, wholeMillis, "millicores")
	check("limits.cpu", r.Limits.CPU, minCPU, maxCPU, wholeMillis, "millicores")
	check("requests.memory", r.Requests.Memory, minMemory, maxMemory, wholeUnits, "bytes")
	check("limits.memory", r.Limits.Memory, minMemory, maxMemory, wholeUnits, "bytes")
	if req, lim := r.Requests.CPU, r.Limits.CPU; req != nil && lim != nil && lim.Cmp(*req) < 0 {
		errs = append(errs, fmt.Errorf("limits.cpu %s is below requests.cpu %s", lim, req))
	}
	if req, lim := r.Requests.Memory, r.Limits.Memory; req != nil && lim != nil && lim.Cmp(*req) < 0 {
		errs = append(errs, fmt.Errorf("limits.memory %s is below requests.memory %s", lim, req))
	}
	return errors.Join(errs...)
}

// wholeMillis reports a CPU quantity of a whole number of millicores, the
// finest the API server keeps.
func wholeMillis(q *resource.Quantity) bool {
	return q.Cmp(*resource.NewMilliQuantity(q.MilliValue(), resource.DecimalSI)) == 0
}

// wholeUnits reports a quantity of a whole number of units (bytes).
func wholeUnits(q *resource.Quantity) bool {
	return q.Cmp(*resource.NewQuantity(q.Value(), resource.BinarySI)) == 0
}

// ValidateClass checks one class: Validate, plus the three quantities every
// class must set. A class is a whole size that replaces the default, so it
// requests CPU and memory (the scheduler places the pod by them) and caps
// memory (a pod with no memory limit can take a whole node's); a CPU limit
// is optional.
func ValidateClass(r Resources) error {
	var errs []error
	if r.Requests.CPU == nil {
		errs = append(errs, errors.New("requests.cpu is required"))
	}
	if r.Requests.Memory == nil {
		errs = append(errs, errors.New("requests.memory is required"))
	}
	if r.Limits.Memory == nil {
		errs = append(errs, errors.New("limits.memory is required"))
	}
	if err := r.Validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Set is the menu: class name to its resources. A nil Set defines no class.
type Set map[string]Resources

// Lookup returns the named class.
func (s Set) Lookup(name string) (Resources, bool) {
	r, ok := s[name]
	return r, ok
}

// Names are the defined class names, sorted.
func (s Set) Names() []string {
	return slices.Sorted(maps.Keys(s))
}

// Describe names the defined classes for a message: "large, medium", or
// "none".
func (s Set) Describe() string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s.Names(), ", ")
}

// Validate checks the menu as a whole: at most MaxClasses classes, each
// named by a DNS label of at most MaxNameLength characters, and each a
// valid class (ValidateClass). It reports every class's problems.
func (s Set) Validate() error {
	var errs []error
	if len(s) > MaxClasses {
		errs = append(errs, fmt.Errorf("%d classes are defined; at most %d may be", len(s), MaxClasses))
	}
	for _, name := range s.Names() {
		if !ValidName(name) {
			errs = append(errs, fmt.Errorf("class name %q is not a DNS label of at most %d characters",
				name, MaxNameLength))
			continue
		}
		if err := ValidateClass(s[name]); err != nil {
			errs = append(errs, fmt.Errorf("class %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Parse reads and validates a menu from its JSON form: an object of class
// name to {"requests": {"cpu", "memory"}, "limits": {"memory", "cpu"}}, each
// quantity a string or a number. Unknown keys, trailing data and a class
// that fails Validate are errors; empty or whitespace (or null) defines no
// class.
func Parse(raw string) (Set, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var s Set
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("resource classes are not a JSON object of classes: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("resource classes: trailing data after the JSON object")
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Encode is the menu's canonical JSON form, which Parse reads back: class
// names sorted, quantities as their canonical strings, unset ones left out.
func (s Set) Encode() string {
	if len(s) == 0 {
		return ""
	}
	raw, err := json.Marshal(s)
	if err != nil {
		// A map of strings to structs of quantities always marshals.
		panic(fmt.Sprintf("resourceclass: encode: %v", err))
	}
	return string(raw)
}
