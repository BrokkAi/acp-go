// Package hintcheck checks generated schema types against decode outcomes
// captured from the Rust schema crate.
//
// cmd/acpgen generates, for each schema package, decode cases that exercise
// the schema's x-deserialize-* hints, required fields, enums, unions, and the
// array form of structs (hints_gen_test.go). scripts/capture_hint_fixtures.py
// runs those cases through a probe built on the Rust schema crate and records
// each outcome in the package's testdata/hints_rust.json. Run decodes every
// case with the Go type and requires the same outcome: an error where Rust
// fails, and otherwise the same value.
package hintcheck

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var dump = flag.String("hints.dump", "", "write the deserialize-hint cases as Rust probe requests to this file and skip the checks")

// Case is one generated decode case.
type Case struct {
	Name  string
	Type  reflect.Type
	Def   string // Rust type name
	Input string
}

// Field describes how to compare one generated field, keyed in
// Suite.Fields by Go type name and wire key ("ToolCall.status").
type Field struct {
	// Default is the value (JSON) a missing field means: the schema's
	// default, or a value Rust leaves out when encoding.
	Default string
	// Patch is set on a Nullable field whose null differs from omission
	// (Rust MaybeUndefined). Elsewhere a Nullable null equals omission.
	Patch bool
}

// Suite is the generated cases of one schema package.
type Suite struct {
	Pin      string // schema release the cases were generated from
	Protocol string // "v1" or "v2": the Rust module holding the types
	Fixtures string // path of the captured Rust outcomes
	Fields   map[string]Field
	Cases    []Case
}

// Fixtures is the file format of the captured Rust outcomes.
type Fixtures struct {
	Pin      string             `json:"pin"`
	Protocol string             `json:"protocol"`
	Crate    string             `json:"crate"`
	Cases    map[string]Outcome `json:"cases"`
}

// Outcome is the Rust result of decoding one case's input: the re-encoded
// value, or the decode error.
type Outcome struct {
	Input string          `json:"input"`
	OK    json.RawMessage `json:"ok,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Request is one line of the probe input written by -hints.dump.
type Request struct {
	ID       string `json:"id"`
	Protocol string `json:"pkg"`
	Type     string `json:"type"`
	Input    string `json:"input"`
}

// Run checks every case against its captured Rust outcome.
func Run(t *testing.T, s Suite) {
	t.Helper()
	if *dump != "" {
		writeRequests(t, s)
		t.Skipf("wrote %d probe requests to %s", len(s.Cases), *dump)
	}
	data, err := os.ReadFile(s.Fixtures)
	if err != nil {
		t.Fatalf("read Rust fixtures (capture them with scripts/capture_hint_fixtures.py): %v", err)
	}
	var fx Fixtures
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatalf("decode %s: %v", s.Fixtures, err)
	}
	if fx.Pin != s.Pin || fx.Protocol != s.Protocol {
		t.Fatalf("%s was captured for %s %s, the cases are for %s %s; recapture with scripts/capture_hint_fixtures.py", s.Fixtures, fx.Pin, fx.Protocol, s.Pin, s.Protocol)
	}
	seen := map[string]bool{}
	for _, c := range s.Cases {
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			want, ok := fx.Cases[c.Name]
			if !ok || want.Input != c.Input {
				t.Fatalf("no Rust outcome captured for this input; recapture with scripts/capture_hint_fixtures.py\ninput: %s", c.Input)
			}
			s.check(t, c, want)
		})
	}
	var stale []string
	for name := range fx.Cases {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%s has outcomes for cases that are no longer generated: %v", s.Fixtures, stale)
	}
}

// check decodes the input and, where Rust accepts it, also the Rust
// re-encoding, and requires the two Go values to be the same.
func (s Suite) check(t *testing.T, c Case, want Outcome) {
	got := reflect.New(c.Type)
	err := json.Unmarshal([]byte(c.Input), got.Interface())
	if want.OK == nil {
		if err == nil {
			encoded, _ := json.Marshal(got.Interface())
			t.Fatalf("Go accepted what Rust rejects (%s)\ninput: %s\n   go: %s", want.Error, c.Input, encoded)
		}
		return
	}
	if err != nil {
		t.Fatalf("Go rejected what Rust accepts: %v\ninput: %s\n rust: %s", err, c.Input, want.OK)
	}
	rust := reflect.New(c.Type)
	if err := json.Unmarshal(want.OK, rust.Interface()); err != nil {
		t.Fatalf("Go cannot decode the Rust re-encoding: %v\n rust: %s", err, want.OK)
	}
	if path := s.diff(got.Elem(), rust.Elem(), c.Type.Name(), ""); path != "" {
		encoded, _ := json.Marshal(got.Interface())
		t.Fatalf("decoded value differs from Rust at %s\ninput: %s\n   go: %s\n rust: %s", path, c.Input, encoded, want.OK)
	}
}

// diff compares two values of one generated type field by field and returns
// the path of the first difference, or "". A value Go leaves unset equals the
// Rust one when that is the field's default; an empty list or map equals an
// absent one; a Nullable null equals omission unless the field is a patch.
// owner and key name the struct field holding a and b.
func (s Suite) diff(a, b reflect.Value, owner, key string) string {
	path := owner + "." + key
	rule := s.Fields[path]
	switch {
	case a.Kind() == reflect.Pointer:
		switch {
		case a.IsNil() && b.IsNil():
			return ""
		case a.IsNil():
			return s.defaultDiff(b.Elem(), rule, owner, key)
		case b.IsNil():
			return s.defaultDiff(a.Elem(), rule, owner, key)
		}
		return s.diff(a.Elem(), b.Elem(), owner, key)
	case a.Kind() == reflect.Struct && strings.HasPrefix(a.Type().Name(), "Nullable["):
		aSet, bSet := a.FieldByName("Set").Bool(), b.FieldByName("Set").Bool()
		aNull, bNull := aSet && a.FieldByName("Null").Bool(), bSet && b.FieldByName("Null").Bool()
		aVal, bVal := aSet && !aNull, bSet && !bNull
		switch {
		case aVal && bVal:
			return s.diff(a.FieldByName("Value"), b.FieldByName("Value"), owner, key)
		case aVal:
			if bNull && rule.Patch {
				return path
			}
			return s.defaultDiff(a.FieldByName("Value"), rule, owner, key)
		case bVal:
			if aNull && rule.Patch {
				return path
			}
			return s.defaultDiff(b.FieldByName("Value"), rule, owner, key)
		case rule.Patch && aSet != bSet:
			return path // null against omission
		}
		return ""
	case a.Kind() == reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			sf := a.Type().Field(i)
			if !sf.IsExported() {
				continue // wire bytes kept for re-encoding
			}
			name := strings.Split(sf.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				name = sf.Name
			}
			if p := s.diff(a.Field(i), b.Field(i), a.Type().Name(), name); p != "" {
				return p
			}
		}
		return ""
	case a.Type() == reflect.TypeOf(json.RawMessage(nil)):
		return sameJSON(a.Interface(), b.Interface(), path)
	case a.Kind() == reflect.Slice:
		if a.Len() == 0 && b.Len() == 0 {
			return ""
		}
		if a.Len() != b.Len() {
			return path
		}
		for i := 0; i < a.Len(); i++ {
			if p := s.diff(a.Index(i), b.Index(i), owner, fmt.Sprintf("%s[%d]", key, i)); p != "" {
				return p
			}
		}
		return ""
	case a.Kind() == reflect.Map:
		if a.Len() == 0 && b.Len() == 0 {
			return ""
		}
		if a.Len() != b.Len() {
			return path
		}
		for _, k := range a.MapKeys() {
			bv := b.MapIndex(k)
			if !bv.IsValid() {
				return path
			}
			if p := s.diff(a.MapIndex(k), bv, owner, fmt.Sprintf("%s[%v]", key, k)); p != "" {
				return p
			}
		}
		return ""
	case a.Kind() == reflect.Interface:
		return sameJSON(a.Interface(), b.Interface(), path)
	}
	if !reflect.DeepEqual(a.Interface(), b.Interface()) {
		return path
	}
	return ""
}

// defaultDiff reports whether v, set on one side only, differs from what an
// unset field means: its default, or for a list or map, empty.
func (s Suite) defaultDiff(v reflect.Value, rule Field, owner, key string) string {
	if rule.Default != "" {
		fresh := reflect.New(v.Type())
		if err := json.Unmarshal([]byte(rule.Default), fresh.Interface()); err == nil {
			return s.diff(v, fresh.Elem(), owner, key)
		}
	}
	if (v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.Len() == 0 {
		return ""
	}
	return owner + "." + key
}

func sameJSON(a, b any, path string) string {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil || canonical(ja) != canonical(jb) {
		return path
	}
	return ""
}

func canonical(doc []byte) string {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return string(doc)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func writeRequests(t *testing.T, s Suite) {
	t.Helper()
	f, err := os.Create(*dump)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(map[string]string{"pin": s.Pin, "pkg": s.Protocol}); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Cases {
		if err := enc.Encode(Request{ID: c.Name, Protocol: s.Protocol, Type: c.Def, Input: c.Input}); err != nil {
			t.Fatal(err)
		}
	}
}
