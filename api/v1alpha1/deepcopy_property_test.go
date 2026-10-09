// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/randfill"
)

// deepCopySeed pins the property's randomness so the gate stays
// deterministic; deepCopyIterations bounds how many filled values each type
// is checked against.
const (
	deepCopySeed       = 20261009
	deepCopyIterations = 25
)

// deepCopyExtraRoots lists the generated-deepcopy types no registered kind
// reaches through its fields (values the API package hands out from helper
// functions rather than stores on an object). TestDeepCopyCoversGenerated
// fails when a new such type appears without being listed here.
var deepCopyExtraRoots = []reflect.Type{
	reflect.TypeFor[RepositoryPreview](),
}

// deepCopyTypes returns this package's registered kinds by name and every
// other struct of this package reachable from them or from
// deepCopyExtraRoots.
func deepCopyTypes(t *testing.T) (kinds map[string]reflect.Type, nested map[reflect.Type]bool) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	pkg := reflect.TypeFor[Finding]().PkgPath()
	kinds = map[string]reflect.Type{}
	nested = map[reflect.Type]bool{}
	for name, typ := range scheme.KnownTypes(GroupVersion) {
		// The scheme also registers meta/v1's shared option and status
		// kinds under every group-version; only this package's are ours.
		if typ.PkgPath() != pkg {
			continue
		}
		kinds[name] = typ
		collectLocalStructs(typ, pkg, nested)
	}
	for _, typ := range deepCopyExtraRoots {
		collectLocalStructs(typ, pkg, nested)
	}
	for _, typ := range kinds {
		delete(nested, typ)
	}
	return kinds, nested
}

// TestDeepCopyCoversGenerated holds the property's type discovery to the
// generated file: every type zz_generated.deepcopy.go gives a DeepCopy is
// either a registered kind or reached from one (or an extra root), so no
// generated copy escapes TestDeepCopyProperties.
func TestDeepCopyCoversGenerated(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "zz_generated.deepcopy.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse generated deepcopy: %v", err)
	}
	kinds, nested := deepCopyTypes(t)
	reached := map[string]bool{}
	for _, typ := range kinds {
		reached[typ.Name()] = true
	}
	for typ := range nested {
		reached[typ.Name()] = true
	}
	generated := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "DeepCopy" {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			t.Errorf("DeepCopy with a value receiver %T: extend the property to cover it", fn.Recv.List[0].Type)
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok {
			t.Errorf("DeepCopy receiver %T is not a plain named type", star.X)
			continue
		}
		generated++
		if !reached[ident.Name] {
			t.Errorf("generated (*%s).DeepCopy is not reached from any kind: add it to deepCopyExtraRoots", ident.Name)
		}
	}
	if generated == 0 {
		t.Fatal("found no generated DeepCopy methods")
	}
}

// TestDeepCopyProperties checks the generated deepcopy of every kind
// registered for this group-version, and of every struct of this package
// reachable from one, against four invariants:
//
//   - a copy deep-equals its original;
//   - writing through every field of the copy (pointers, slices, maps
//     included) leaves the original exactly as it was, so nothing aliases;
//   - DeepCopyObject returns a runtime.Object of the same concrete type;
//   - a nil receiver copies to nil.
//
// Types come from the scheme and the reflected field graph, so a new kind or
// a new nested struct is covered without touching this test.
func TestDeepCopyProperties(t *testing.T) {
	kinds, local := deepCopyTypes(t)
	if len(kinds) == 0 {
		t.Fatal("no kinds registered for", GroupVersion)
	}
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		typ := kinds[name]
		t.Run("object/"+name, func(t *testing.T) {
			checkDeepCopyObject(t, typ)
		})
	}

	nested := make([]reflect.Type, 0, len(local))
	for typ := range local {
		nested = append(nested, typ)
	}
	sort.Slice(nested, func(i, j int) bool { return nested[i].Name() < nested[j].Name() })
	for _, typ := range nested {
		t.Run("type/"+typ.Name(), func(t *testing.T) {
			checkDeepCopy(t, typ)
		})
	}
}

// newFiller returns a seeded filler shaped so every optional field is
// exercised both set and unset across the iterations.
//
// metav1.Time fills itself, but as a self-filler a nil *metav1.Time field
// receives the call on its nil receiver and stays nil; a custom func makes
// the filler allocate it first, so optional timestamps are exercised too.
func newFiller(seed int64) *randfill.Filler {
	return randfill.NewWithSeed(seed).NilChance(0.2).NumElements(1, 3).Funcs(
		func(ts *metav1.Time, c randfill.Continue) { ts.RandFill(c.Rand) },
	)
}

// checkDeepCopy holds the generated DeepCopy of one struct type to the
// copy, aliasing and nil invariants.
func checkDeepCopy(t *testing.T, typ reflect.Type) {
	t.Helper()
	ptr := reflect.PointerTo(typ)
	method, ok := ptr.MethodByName("DeepCopy")
	if !ok {
		t.Fatalf("%s has no DeepCopy method", ptr)
	}

	if got := method.Func.Call([]reflect.Value{reflect.Zero(ptr)})[0]; !got.IsNil() {
		t.Errorf("(*%s)(nil).DeepCopy() = %v, want nil", typ.Name(), got)
	}

	for i := range deepCopyIterations {
		seed := int64(deepCopySeed + i)
		orig := reflect.New(typ)
		newFiller(seed).Fill(orig.Interface())
		// A second fill from the same seed is an independent snapshot of
		// the original, built without trusting DeepCopy.
		snapshot := reflect.New(typ)
		newFiller(seed).Fill(snapshot.Interface())

		dup := method.Func.Call([]reflect.Value{orig})[0]
		assertCopyIsolated(t, seed, orig, snapshot, dup)
	}
}

// checkDeepCopyObject holds a registered kind to checkDeepCopy's invariants
// and to DeepCopyObject's: same concrete type, equal value, no aliasing.
func checkDeepCopyObject(t *testing.T, typ reflect.Type) {
	t.Helper()
	checkDeepCopy(t, typ)

	nilObj, ok := reflect.Zero(reflect.PointerTo(typ)).Interface().(runtime.Object)
	if !ok {
		t.Fatalf("*%s does not implement runtime.Object", typ.Name())
	}
	if got := nilObj.DeepCopyObject(); got != nil {
		t.Errorf("(*%s)(nil).DeepCopyObject() = %#v, want nil", typ.Name(), got)
	}

	for i := range deepCopyIterations {
		seed := int64(deepCopySeed + 1000 + i)
		orig := reflect.New(typ)
		newFiller(seed).Fill(orig.Interface())
		snapshot := reflect.New(typ)
		newFiller(seed).Fill(snapshot.Interface())

		obj, ok := orig.Interface().(runtime.Object)
		if !ok {
			t.Fatalf("*%s does not implement runtime.Object", typ.Name())
		}
		copied := obj.DeepCopyObject()
		if copied == nil {
			t.Fatalf("seed %d: DeepCopyObject() = nil", seed)
		}
		if got, want := reflect.TypeOf(copied), orig.Type(); got != want {
			t.Fatalf("seed %d: DeepCopyObject() type = %v, want %v", seed, got, want)
		}
		assertCopyIsolated(t, seed, orig, snapshot, reflect.ValueOf(copied))
	}
}

// assertCopyIsolated checks dup equals orig and is a distinct value, then
// writes through every field of dup and checks orig still equals snapshot.
func assertCopyIsolated(t *testing.T, seed int64, orig, snapshot, dup reflect.Value) {
	t.Helper()
	if dup.IsNil() {
		t.Fatalf("seed %d: copy is nil", seed)
	}
	if dup.Pointer() == orig.Pointer() {
		t.Fatalf("seed %d: copy is the original pointer", seed)
	}
	if !reflect.DeepEqual(dup.Interface(), orig.Interface()) {
		t.Fatalf("seed %d: copy differs from original\n got: %+v\nwant: %+v",
			seed, dup.Elem().Interface(), orig.Elem().Interface())
	}
	if !mutate(dup.Elem()) {
		// Nothing writable means nothing could alias; the equality check
		// above is the whole property.
		return
	}
	if !reflect.DeepEqual(orig.Interface(), snapshot.Interface()) {
		t.Fatalf("seed %d: mutating the copy changed the original\n got: %+v\nwant: %+v",
			seed, orig.Elem().Interface(), snapshot.Elem().Interface())
	}
}

var timeType = reflect.TypeFor[time.Time]()

// mutate writes a different value into every settable leaf reachable from v,
// in place: through pointers, into slice and array elements, and into map
// entries (rewritten under the same key, plus nothing removed), so any
// storage the copy shares with its original shows up as a changed original.
// It reports whether it changed anything.
func mutate(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return false
		}
		return mutate(v.Elem())
	case reflect.Struct:
		return mutateStruct(v)
	case reflect.Slice, reflect.Array:
		changed := false
		for i := range v.Len() {
			if mutate(v.Index(i)) {
				changed = true
			}
		}
		return changed
	case reflect.Map:
		return mutateMap(v)
	default:
		return mutateScalar(v)
	}
}

// mutateStruct mutates every exported field of v; a time.Time, whose fields
// are all unexported, is moved forward as a whole instead.
func mutateStruct(v reflect.Value) bool {
	if v.Type() == timeType {
		if !v.CanSet() {
			return false
		}
		v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Hour)))
		return true
	}
	changed := false
	for i := range v.NumField() {
		if v.Type().Field(i).IsExported() && mutate(v.Field(i)) {
			changed = true
		}
	}
	return changed
}

// mutateMap rewrites every entry of v with a mutated element and adds a
// fresh key, so a map shared with the original shows up even when its
// elements hold nothing writable.
func mutateMap(v reflect.Value) bool {
	if v.IsNil() || v.Len() == 0 {
		return false
	}
	changed := false
	for _, key := range v.MapKeys() {
		elem := reflect.New(v.Type().Elem()).Elem()
		elem.Set(v.MapIndex(key))
		if mutate(elem) {
			changed = true
		}
		v.SetMapIndex(key, elem)
	}
	if v.Type().Key().Kind() == reflect.String {
		fresh := reflect.New(v.Type().Key()).Elem()
		fresh.SetString("\x00mutated")
		v.SetMapIndex(fresh, reflect.New(v.Type().Elem()).Elem())
		changed = true
	}
	return changed
}

// mutateScalar changes a settable basic value in place.
func mutateScalar(v reflect.Value) bool {
	if !v.CanSet() {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "~")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() ^ 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(v.Uint() ^ 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 1)
	default:
		return false
	}
	return true
}

// collectLocalStructs records every struct type declared in pkg that is
// reachable from typ through fields, pointers, slices, arrays and maps.
func collectLocalStructs(typ reflect.Type, pkg string, seen map[reflect.Type]bool) {
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		collectLocalStructs(typ.Elem(), pkg, seen)
	case reflect.Map:
		collectLocalStructs(typ.Key(), pkg, seen)
		collectLocalStructs(typ.Elem(), pkg, seen)
	case reflect.Struct:
		if typ.PkgPath() != pkg || seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			collectLocalStructs(typ.Field(i).Type, pkg, seen)
		}
	default:
	}
}
