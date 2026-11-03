package jsonvalue_test

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/BrokkAi/acp-go/internal/serdejson"
	"github.com/BrokkAi/acp-go/jsonvalue"
)

var samples = []string{
	`{}`, `[]`, `null`, `true`, `false`, `0`, `-0`, `1.0`, `-0.0`, `"x"`,
	`{"z": 1, "a": 2, "m": 3}`,
	`{"z": 1, "a": 2, "z": 3}`,
	`{"z": {"y": 1}, "a": [], "z": {"x": 2}}`,
	`[9007199254740993, 12345678901234567890, 18446744073709551615, 18446744073709551616]`,
	`[-9223372036854775808, 9223372036854775807, -9223372036854775809, -18446744073709551615]`,
	`[1e2, 1E+2, 0.10, 1.5e-7, 7.2e-300, 0.9405161066866751, 123456789.123456789, 5e-324, 1.7976931348623157e308]`,
	`[1e400]`, `[-1e400]`, `1.7976931348623158e308`,
	`"\u0041\/\ud83d\ude00\u2028<&>\"\\\u0001\b\f\n\r\t\u00e9"`,
	`"\ud83d"`, `"\ude00"`, `"\ud83d\u0041"`,
	" \t\n{ \"a\" : [ 1 , 2 ] , \"b\" : { } } \r\n",
	`{"a":1,}`, `[1,]`, `{"a"}`, `{1:2}`, `01`, `1.`, `.5`, `1e`, `-`, `tru`, `nul`, `"a`, `[`, `{`, `{"a":1}x`,
	"\"\x01\"", "\"\xff\"", "\"é\"",
	`{"k0":0,"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k17":17,"k3":"again","k0":"first"}`,
}

// TestParseMatchesSerdeJSON requires Parse and Marshal to accept, reject, and
// re-encode as the serde_json model in internal/serdejson does.
func TestParseMatchesSerdeJSON(t *testing.T) {
	for _, input := range samples {
		check(t, []byte(input))
	}
}

func FuzzParseMatchesSerdeJSON(f *testing.F) {
	for _, input := range samples {
		f.Add([]byte(input))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		check(t, data)
	})
}

func check(t *testing.T, data []byte) {
	t.Helper()
	want, wantErr := serdejson.ParseAnyDepth(data)
	got, err := jsonvalue.Parse(data)
	if (err != nil) != (wantErr != nil) {
		t.Fatalf("%q: Parse error %v, serdejson error %v", data, err, wantErr)
	}
	if err != nil {
		return
	}
	encoded, err := jsonvalue.Marshal(got)
	if err != nil {
		t.Fatalf("%q: %v", data, err)
	}
	if string(encoded) != string(serdejson.Encode(want)) {
		t.Fatalf("%q:\n got %s\nwant %s", data, encoded, serdejson.Encode(want))
	}
	// The encoding reads back as the same value.
	again, err := jsonvalue.Parse(encoded)
	if err != nil {
		t.Fatalf("%s: %v", encoded, err)
	}
	if reencoded, _ := jsonvalue.Marshal(again); string(reencoded) != string(encoded) && !hasFloat(got) {
		t.Fatalf("%s re-encodes as %s", encoded, reencoded)
	}
}

// hasFloat reports whether v holds a float, whose shortest digits serde_json's
// inexact parsing need not read back as the same float64.
func hasFloat(v any) bool {
	switch v := v.(type) {
	case float64:
		return true
	case []any:
		for _, item := range v {
			if hasFloat(item) {
				return true
			}
		}
	case jsonvalue.Object:
		for _, member := range v {
			if hasFloat(member.Value) {
				return true
			}
		}
	}
	return false
}

func TestDecodedValues(t *testing.T) {
	got, err := jsonvalue.Parse([]byte(`{"b":[1,-2,18446744073709551615,1.0,"s",null,true,{}],"a":{"x":-9223372036854775808}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := jsonvalue.Object{
		{Key: "b", Value: []any{int64(1), int64(-2), uint64(math.MaxUint64), 1.0, "s", nil, true, jsonvalue.Object{}}},
		{Key: "a", Value: jsonvalue.Object{{Key: "x", Value: int64(math.MinInt64)}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	for input, want := range map[string]string{
		`null`:     "invalid type: null, expected a map",
		`true`:     "invalid type: boolean `true`, expected a map",
		`5`:        "invalid type: integer `5`, expected a map",
		`1`:        "invalid type: integer `1`, expected a map",
		`-0`:       "invalid type: floating point `-0.0`, expected a map",
		`1e2`:      "invalid type: floating point `100.0`, expected a map",
		`"a\nb"`:   `invalid type: string "a\nb", expected a map`,
		`[1]`:      "invalid type: sequence, expected a map",
		`{"a":1,}`: jsonvalue.ErrInvalid.Error(),
	} {
		var o jsonvalue.Object
		if err := o.UnmarshalJSON([]byte(input)); err == nil || err.Error() != want {
			t.Errorf("%s: got %v, want %s", input, err, want)
		}
	}
	type carrier struct {
		Meta jsonvalue.Object `json:"_meta"`
	}
	var c carrier
	if err := json.Unmarshal([]byte(`{"_meta":{"z":1,"a":{"y":2.50,"x":[]},"z":3}}`), &c); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(c)
	if err != nil || string(encoded) != `{"_meta":{"z":3,"a":{"y":2.5,"x":[]}}}` {
		t.Fatalf("%s %v", encoded, err)
	}
}

func TestObjectMethods(t *testing.T) {
	o := jsonvalue.ObjectOf(map[string]any{"c": 3, "a": 1, "b": 2})
	if got := o.Keys(); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("keys %v", got)
	}
	o.Set("a", "one")
	o.Set("d", 4.0)
	if v, ok := o.Get("a"); !ok || v != "one" {
		t.Fatalf("a = %v", v)
	}
	if encoded, _ := o.MarshalJSON(); string(encoded) != `{"a":"one","b":2,"c":3,"d":4.0}` {
		t.Fatalf("%s", encoded)
	}
	if !o.SwapDelete("a") || o.SwapDelete("a") {
		t.Fatal("SwapDelete")
	}
	if got := o.Keys(); !reflect.DeepEqual(got, []string{"d", "b", "c"}) {
		t.Fatalf("after SwapDelete %v", got)
	}
	if !o.Delete("d") || o.Delete("d") {
		t.Fatal("Delete")
	}
	if got := o.Keys(); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("after Delete %v", got)
	}

	repeated := jsonvalue.Object{{Key: "k", Value: 1}, {Key: "j", Value: 2}, {Key: "k", Value: 3}}
	if v, _ := repeated.Get("k"); v != 3 {
		t.Fatalf("Get reads %v, not the last value", v)
	}
	if encoded, _ := repeated.MarshalJSON(); string(encoded) != `{"k":3,"j":2}` {
		t.Fatalf("%s", encoded)
	}
	repeated.Set("k", 4)
	if !reflect.DeepEqual(repeated, jsonvalue.Object{{Key: "k", Value: 4}, {Key: "j", Value: 2}}) {
		t.Fatalf("Set left %#v", repeated)
	}

	var empty jsonvalue.Object
	if encoded, _ := empty.MarshalJSON(); string(encoded) != "null" {
		t.Fatalf("nil encodes as %s", encoded)
	}
	empty.Set("x", nil)
	if encoded, _ := empty.MarshalJSON(); string(encoded) != `{"x":null}` {
		t.Fatalf("%s", encoded)
	}
	if jsonvalue.ObjectOf(nil) != nil || jsonvalue.ObjectOf(map[string]any{}) == nil {
		t.Fatal("ObjectOf keeps nil and empty apart")
	}
	if jsonvalue.Object(nil).Clone() != nil || (jsonvalue.Object{}).Clone() == nil {
		t.Fatal("Clone keeps nil and empty apart")
	}
}

func TestAppendGoValues(t *testing.T) {
	for _, test := range []struct {
		value any
		want  string
	}{
		{map[string]any{"b": 1, "a": float32(0.5)}, `{"a":0.5,"b":1}`},
		{[]any{int8(-1), uint16(2), 1.0, math.NaN(), math.Inf(1)}, `[-1,2,1.0,null,null]`},
		{json.Number("1e2"), `100.0`},
		{json.Number("18446744073709551615"), `18446744073709551615`},
		{json.RawMessage(` {"z":1, "a":2, "z":"\u0041"} `), `{"z":"A","a":2}`},
		{json.RawMessage(nil), `null`},
		{[]string{"<&>"}, `["<&>"]`},
		{struct {
			B int     `json:"b"`
			A float64 `json:"a"`
		}{1, 2.5}, `{"b":1,"a":2.5}`},
		{jsonvalue.Object{{Key: "n", Value: jsonvalue.Object(nil)}}, `{"n":null}`},
	} {
		got, err := jsonvalue.Marshal(test.value)
		if err != nil || string(got) != test.want {
			t.Errorf("%#v: got %s %v, want %s", test.value, got, err, test.want)
		}
	}
	if _, err := jsonvalue.Marshal(json.Number("x")); err == nil {
		t.Error("an invalid json.Number encodes")
	}
}

func TestIsNull(t *testing.T) {
	for _, v := range []any{nil, jsonvalue.Object(nil), []any(nil), map[string]any(nil), json.RawMessage("null"), json.RawMessage(nil), (*int)(nil)} {
		if !jsonvalue.IsNull(v) {
			t.Errorf("%#v is null", v)
		}
	}
	for _, v := range []any{false, 0, "", jsonvalue.Object{}, []any{}, json.RawMessage("{}")} {
		if jsonvalue.IsNull(v) {
			t.Errorf("%#v is not null", v)
		}
	}
}
