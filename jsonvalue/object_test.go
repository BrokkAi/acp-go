package jsonvalue_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/jsonvalue"
)

func TestParseDepthLimit(t *testing.T) {
	nested := func(depth int) []byte {
		return []byte(strings.Repeat("[", depth) + strings.Repeat("]", depth))
	}
	if _, err := jsonvalue.Parse(nested(jsonvalue.MaxDepth)); err != nil {
		t.Fatalf("MaxDepth deep: %v", err)
	}
	if _, err := jsonvalue.Parse(nested(jsonvalue.MaxDepth + 1)); !errors.Is(err, jsonvalue.ErrTooDeep) {
		t.Fatalf("MaxDepth+1 deep: %v", err)
	}
	// A very deep input fails without exhausting the stack.
	deep := []byte(strings.Repeat(`{"a":`, 5_000_000))
	var o jsonvalue.Object
	if err := o.UnmarshalJSON(deep); !errors.Is(err, jsonvalue.ErrTooDeep) {
		t.Fatalf("deep object: %v", err)
	}
}

func TestCloneIsDeep(t *testing.T) {
	original := jsonvalue.Object{
		{Key: "o", Value: jsonvalue.Object{{Key: "x", Value: int64(1)}}},
		{Key: "a", Value: []any{jsonvalue.Object{{Key: "y", Value: "s"}}}},
	}
	clone := original.Clone()
	nested, _ := clone.Get("o")
	inner := nested.(jsonvalue.Object)
	inner.Set("x", int64(2))
	list, _ := clone.Get("a")
	item := list.([]any)[0].(jsonvalue.Object)
	item.Set("y", "changed")
	if encoded, _ := original.MarshalJSON(); string(encoded) != `{"o":{"x":1},"a":[{"y":"s"}]}` {
		t.Fatalf("original changed: %s", encoded)
	}
	clone.Set("new", true)
	if len(original) != 2 {
		t.Fatal("original grew")
	}
}

func TestEqual(t *testing.T) {
	parse := func(s string) any {
		v, err := jsonvalue.Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, test := range []struct {
		a, b any
		want bool
	}{
		{parse(`{"a":1,"b":[1.0,"x",null]}`), parse(`{"a":1,"b":[1.0,"x",null]}`), true},
		{parse(`{"a":1,"b":2}`), parse(`{"b":2,"a":1}`), false},
		{parse(`{"a":1,"a":2}`), parse(`{"a":2}`), true},
		{parse(`1`), parse(`1.0`), false},
		{parse(`-0.0`), parse(`0.0`), true},
		{parse(`18446744073709551615`), uint64(math.MaxUint64), true},
		{parse(`5`), 5, true},
		{parse(`-5`), int8(-5), true},
		{parse(`5`), 5.0, false},
		{1.5, float32(1.5), true},
		{math.NaN(), math.NaN(), false},
		{json.RawMessage(` {"z":1, "a":[]} `), parse(`{"z":1,"a":[]}`), true},
		{json.Number("1e2"), 100.0, true},
		{map[string]any{"b": 1, "a": 2}, jsonvalue.Object{{Key: "a", Value: 2}, {Key: "b", Value: 1}}, true},
		{jsonvalue.Object{{Key: "k", Value: 1}, {Key: "j", Value: 2}, {Key: "k", Value: 3}}, parse(`{"k":3,"j":2}`), true},
		{jsonvalue.Object(nil), nil, true},
		{jsonvalue.Object(nil), jsonvalue.Object{}, false},
		{[]any{}, parse(`[]`), true},
		{struct {
			A int `json:"a"`
		}{1}, parse(`{"a":1}`), true},
		{func() {}, func() {}, false},
	} {
		if got := jsonvalue.Equal(test.a, test.b); got != test.want {
			t.Errorf("Equal(%#v, %#v) = %v", test.a, test.b, got)
		}
		if got := jsonvalue.Equal(test.b, test.a); got != test.want {
			t.Errorf("Equal(%#v, %#v) = %v", test.b, test.a, got)
		}
	}
	if !(jsonvalue.Object{}).Equal(jsonvalue.Object{}) || jsonvalue.Object(nil).Equal(jsonvalue.Object{}) {
		t.Error("Object.Equal on empty objects")
	}
}

func largeObject(n int, repeat bool) jsonvalue.Object {
	o := make(jsonvalue.Object, 0, n+1)
	for i := range n {
		o = append(o, jsonvalue.Member{Key: fmt.Sprintf("key-%d", i), Value: int64(i)})
	}
	if repeat {
		o = append(o, jsonvalue.Member{Key: "key-1", Value: "last"})
	}
	return o
}

func TestLargeObjectsEncode(t *testing.T) {
	for _, n := range []int{17, 200, 256, 300} {
		o := largeObject(n, true)
		encoded, err := o.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(encoded), `{"key-0":0,"key-1":"last","key-2":2,`) || strings.Count(string(encoded), `"key-1"`) != 1 {
			t.Fatalf("%d members: %s", n, encoded[:60])
		}
	}
	// Encoding an object without repeated keys allocates only the output
	// buffer, up to 256 members.
	o := largeObject(256, false)
	buf := make([]byte, 0, 1<<16)
	if allocs := testing.AllocsPerRun(20, func() {
		if _, err := o.AppendJSON(buf[:0]); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("AppendJSON allocates %v times", allocs)
	}
}
