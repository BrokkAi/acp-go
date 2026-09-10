package schema

import (
	"encoding/json"
	"testing"
)

func TestProtocolVersionMatchesRustUInt16(t *testing.T) {
	for _, test := range []struct {
		name string
		wire string
		ok   bool
	}{
		{name: "zero", wire: "0", ok: true},
		{name: "one", wire: "1", ok: true},
		{name: "maximum", wire: "65535", ok: true},
		{name: "overflow", wire: "65536", ok: false},
		{name: "large", wire: "100000", ok: false},
		{name: "string", wire: `"2"`, ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var version ProtocolVersion
			err := json.Unmarshal([]byte(test.wire), &version)
			if test.ok && err != nil {
				t.Fatal(err)
			}
			if !test.ok && err == nil {
				t.Fatalf("accepted %s", test.wire)
			}
		})
	}
}
