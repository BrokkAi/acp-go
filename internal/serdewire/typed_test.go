package serdewire

import "testing"

// EncodeTyped reads numbers as the encoding of a Go value: shortest digits
// keep the float64 they encode, where Encode reads them as serde_json reads
// wire text.
func TestEncodeTypedKeepsTheFloatsOfAValue(t *testing.T) {
	for _, test := range []struct{ in, typed, wire string }{
		{`[0.9405161066866751]`, `[0.9405161066866751]`, `[0.9405161066866752]`},
		{`{"a":123456789.12345679}`, `{"a":123456789.12345679}`, `{"a":123456789.1234568}`},
		{`[1e-07,1e+21,-0,100]`, `[1e-7,1e+21,-0.0,100]`, `[1e-7,1e+21,-0.0,100]`},
		{`{"s":"<& ","a":1,"a":0.9405161066866751}`, "{\"s\":\"<& \",\"a\":0.9405161066866751}", "{\"s\":\"<& \",\"a\":0.9405161066866752}"},
	} {
		typed, err := EncodeTyped([]byte(test.in))
		if err != nil || string(typed) != test.typed {
			t.Errorf("EncodeTyped(%s) = %s, %v; want %s", test.in, typed, err, test.typed)
		}
		wire, err := Encode([]byte(test.in))
		if err != nil || string(wire) != test.wire {
			t.Errorf("Encode(%s) = %s, %v; want %s", test.in, wire, err, test.wire)
		}
	}
	if got, err := MarshalTyped(map[string]float64{"x": 0.9405161066866751}); err != nil || string(got) != `{"x":0.9405161066866751}` {
		t.Errorf("MarshalTyped = %s, %v", got, err)
	}
}
