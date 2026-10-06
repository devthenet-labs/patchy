// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// tsInterfaces reads every `export interface` in a TypeScript source into
// its field names, an interface's own plus those of the one it extends.
func tsInterfaces(t *testing.T, src string) map[string][]string {
	t.Helper()
	decl := regexp.MustCompile(`(?s)export interface (\w+)(?: extends (\w+))? \{(.*?)\n\}`)
	field := regexp.MustCompile(`^\s*(\w+)\??:`)
	own := map[string][]string{}
	parent := map[string]string{}
	for _, m := range decl.FindAllStringSubmatch(src, -1) {
		var fields []string
		for line := range strings.SplitSeq(m[3], "\n") {
			if f := field.FindStringSubmatch(line); f != nil {
				fields = append(fields, f[1])
			}
		}
		own[m[1]], parent[m[1]] = fields, m[2]
	}
	out := map[string][]string{}
	for name := range own {
		var all []string
		for n := name; n != ""; n = parent[n] {
			all = append(all, own[n]...)
		}
		slices.Sort(all)
		out[name] = all
	}
	return out
}

// jsonFields are a struct's JSON keys, embedded structs flattened as
// encoding/json flattens them.
func jsonFields(typ reflect.Type) []string {
	var out []string
	for f := range typ.Fields() {
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, jsonFields(f.Type)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// TestIntentWireTypesMatchTypeScript keeps every intents wire type and the
// SPA's interface of the same name in lockstep: a field added, renamed or
// dropped on one side fails here.
func TestIntentWireTypesMatchTypeScript(t *testing.T) {
	src, err := os.ReadFile("ui/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	ts := tsInterfaces(t, string(src))
	for _, v := range []any{
		Me{}, ProjectAccess{}, IntentBoard{}, BoardProject{}, ProjectRepo{}, IntentLimits{}, IntentCard{},
		IntentPR{}, PreviewLink{}, RunningRun{}, AttemptCount{}, IntentDetail{}, IntentInputView{},
		IntentPlanView{}, IntentApproval{}, IntentRunRow{}, RunGrant{}, IntentRunDetail{}, RunJobClock{},
		IntentPlanText{}, RunActivity{}, RunOutput{}, StreamNotice{},
	} {
		typ := reflect.TypeOf(v)
		want, ok := ts[typ.Name()]
		if !ok {
			t.Errorf("ui/src/types.ts has no interface %s", typ.Name())
			continue
		}
		if got := jsonFields(typ); !slices.Equal(got, want) {
			t.Errorf("%s: Go fields %v, TypeScript fields %v", typ.Name(), got, want)
		}
	}
	// The parser itself sees the long-standing Finding types, so a regex
	// that silently matched nothing cannot pass the loop above.
	if len(ts) < 30 || !slices.Contains(slices.Collect(maps.Keys(ts)), "FindingSummary") {
		t.Fatalf("parsed %d interfaces from types.ts; the parser is broken", len(ts))
	}
}
