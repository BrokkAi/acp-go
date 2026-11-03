package castparams

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"
)

// TestDataEchoesParamsAsSerdeJSONHoldsThem checks the json member against
// the reference SDK's json_cast_params data for the same params: compact,
// a repeated member at its first position with its last value, numbers as
// serde_json writes them, strings with only serde_json's escapes, and null
// for absent params.
func TestDataEchoesParamsAsSerdeJSONHoldsThem(t *testing.T) {
	failure := errors.New("missing field `cwd`")
	for _, tc := range []struct {
		params string
		want   string
	}{
		{"", `{"error":"missing field ` + "`cwd`" + `","json":null,"phase":"deserialization"}`},
		{`null`, `{"error":"missing field ` + "`cwd`" + `","json":null,"phase":"deserialization"}`},
		{`{"sessionId": 1.50e1 , "x":[1,2.0,-0.0]}`, `{"error":"missing field ` + "`cwd`" + `","json":{"sessionId":15.0,"x":[1,2.0,-0.0]},"phase":"deserialization"}`},
		{`{"sessionId":"a","b":1,"sessionId":7}`, `{"error":"missing field ` + "`cwd`" + `","json":{"sessionId":7,"b":1},"phase":"deserialization"}`},
		{`{"sessionId":"\u00e9\n\u2028"}`, "{\"error\":\"missing field `cwd`\",\"json\":{\"sessionId\":\"\u00e9\\n\u2028\"},\"phase\":\"deserialization\"}"},
	} {
		if got := string(Data(failure, json.RawMessage(tc.params))); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.params, got, tc.want)
		}
	}
}

// TestUnmarshalReadsAbsentParamsAsNull requires absent params to decode
// exactly as null, as the reference SDK's params_from_transport hands them
// to every message.
func TestUnmarshalReadsAbsentParamsAsNull(t *testing.T) {
	var absent, null *struct{}
	if err := Unmarshal(nil, &absent); err != nil || absent != nil {
		t.Fatalf("absent params = %v, %v", absent, err)
	}
	if err := Unmarshal(json.RawMessage(`null`), &null); err != nil || null != nil {
		t.Fatalf("null params = %v, %v", null, err)
	}
	if string(Params(nil)) != "null" || string(Params(json.RawMessage(`{}`))) != "{}" {
		t.Fatal("Params does not read absent params as null")
	}
}

// TestDataIsLinearInParamsSize guards the echo of a huge params object:
// holding it as serde_json does must stay linear in its size, and a
// repeated key keeps its first position with its last value.
func TestDataIsLinearInParamsSize(t *testing.T) {
	b := []byte(`{"a":1`)
	for i := range 100_000 {
		b = append(b, `,"k`...)
		b = strconv.AppendInt(b, int64(i), 10)
		b = append(b, `":0`...)
	}
	b = append(b, `,"a":2}`...)
	start := time.Now()
	data := Data(errors.New("e"), b)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("echoing 100k members took %v", elapsed)
	}
	if !bytes.HasPrefix(data, []byte(`{"error":"e","json":{"a":2,"k0":0,`)) {
		t.Fatalf("data starts %.60s", data)
	}
}
