package serdewire

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// encodeOrRaw is Encode, returning b for JSON the reference SDK cannot hold,
// which is what slowPath's callers compare against.
func encodeOrRaw(b []byte) []byte {
	out, err := Encode(b)
	if err != nil {
		return b
	}
	return out
}

// slowPath is the general path, the definition of serde_json's form.
func slowPath(t testing.TB, b []byte) ([]byte, bool) {
	value, err := serdejson.ParseAnyDepth(b)
	if err != nil {
		return nil, false
	}
	return serdejson.Encode(value), true
}

// TestClassifyAndRewriteMatchTheGeneralPath checks the two fast paths
// against the general one on the cases each must and must not take.
func TestClassifyAndRewriteMatchTheGeneralPath(t *testing.T) {
	for _, test := range []struct {
		json string
		form form
	}{
		{`{}`, serdeExact}, {`[]`, serdeExact}, {`""`, serdeExact}, {`0`, serdeExact}, {`-5`, serdeExact},
		{`123456789012345678`, serdeExact}, {`-123456789012345678`, serdeExact}, {`true`, serdeExact}, {`null`, serdeExact},
		{`{"a":"b\"c\\d\n\r\t\b\f\u0000\u001f\u0007","b":[1,2,{"a":1}],"c":{"a":2}}`, serdeExact},
		{`{"a":{"a":{"a":1}},"b":[{"a":1},{"a":2}]}`, serdeExact},
		{"{\"x\":\"\u2028 é 😀 \x7f\"}", serdeExact},
		{`[{"a":1},{"a":1}]`, serdeExact},
		{`{"x":{"a":1},"y":{"a":1}}`, serdeExact},

		{`"\u003c"`, serdeTokens}, {`"\u0026"`, serdeTokens}, {`"\u2028"`, serdeTokens}, {`"\u0061"`, serdeTokens},
		{`"\/"`, serdeTokens}, {`"\u000a"`, serdeTokens}, {`"\u0008"`, serdeTokens}, {`"\u001F"`, serdeTokens},
		{`"\ufffd"`, serdeTokens}, {`"\uD83D\uDE00"`, serdeTokens}, {`["a\u003cb","\\u003c"]`, serdeTokens},
		{`1.5`, serdeExact}, {`-0.25`, serdeExact}, {`0.0001`, serdeExact}, {`123456789012345.6`, serdeTokens}, {`12345678901234.5`, serdeExact}, {`1.0`, serdeTokens}, {`2.50`, serdeTokens}, {`0.00001`, serdeExact}, {`0.000012`, serdeExact}, {`0.000001`, serdeTokens}, {`0.0000012`, serdeTokens}, {`1.5e1`, serdeTokens}, {`1e2`, serdeTokens}, {`1E-2`, serdeTokens}, {`-0`, serdeTokens}, {`-0.0`, serdeTokens},
		{`1234567890123456789`, serdeTokens}, {`-9223372036854775809`, serdeTokens}, {`18446744073709551616`, serdeTokens},
		{`{"a":[1.0,{"b":"\u0026"}]}`, serdeTokens},

		{`{"a":1,"a":2}`, serdeReparse}, {`{"a":{"b":1},"a":2}`, serdeReparse}, {`[{"a":1,"b":2,"a":3}]`, serdeReparse},
		{`{"\u0061":1}`, serdeReparse}, {"\"\xff\"", serdeReparse},
	} {
		b := []byte(test.json)
		if got := classify(b); got != test.form {
			t.Errorf("classify(%s) = %d, want %d", test.json, got, test.form)
		}
		want, ok := slowPath(t, b)
		if !ok {
			want = b
		}
		if got := encodeOrRaw(b); !bytes.Equal(got, want) {
			t.Errorf("encodeOrRaw(%s) = %s, want %s", test.json, got, want)
		}
	}
	var many strings.Builder
	many.WriteString("{")
	for i := 0; i < maxScannedMembers+1; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`"k` + strings.Repeat("x", i) + `":1`)
	}
	many.WriteString("}")
	// Past maxScannedMembers keys an object's keys move to a set: a large
	// object still takes the fast path, and a repeated key is still found,
	// also in an object nested after the set was made.
	if got := classify([]byte(many.String())); got != serdeExact {
		t.Errorf("a large object classified %d", got)
	}
	repeated := strings.TrimSuffix(many.String(), "}") + `,"k":2}`
	if got := classify([]byte(repeated)); got != serdeReparse {
		t.Errorf("a repeated key in a large object classified %d", got)
	}
	nested := strings.TrimSuffix(many.String(), "}") + `,"inner":{"k":1,"k":2},"k2":3}`
	if got := classify([]byte(nested)); got != serdeReparse {
		t.Errorf("a repeated key nested in a large object classified %d", got)
	}
	siblings := strings.TrimSuffix(many.String(), "}") + `,"inner":{"k":1},"kxx":3}`
	if got := classify([]byte(siblings)); got != serdeReparse {
		t.Errorf("a key repeated after a nested object classified %d", got)
	}
	distinct := strings.TrimSuffix(many.String(), "}") + `,"inner":{"k":1},"other":3}`
	if got := classify([]byte(distinct)); got != serdeExact {
		t.Errorf("a nested object's keys leaked into a large object: %d", got)
	}
	// A lone surrogate is left to the general path, which keeps the bytes.
	for _, lone := range []string{`"\ud800"`, `"\udc00"`, `"\ud800x"`, `"\ud800\u0041"`} {
		if got := encodeOrRaw([]byte(lone)); string(got) != lone {
			t.Errorf("encodeOrRaw(%s) = %s", lone, got)
		}
	}
}

// FuzzSerdeEncode checks that the fast paths never differ from the general
// one: for any compact JSON, serdeEncode writes what serde_json writes.
func FuzzSerdeEncode(f *testing.F) {
	for _, seed := range []string{
		`{"a":"b"}`, `{"a":1,"a":2}`, `"\u003c"`, `[1.5,-0,1e2]`, `{"a":[{"b":"\n"}],"c":"\u0001"}`,
		"{\"\u2028\":\"\\u2028\"}", `{"a":{"a":1},"b":{"a":1}}`, `"\uD83D\uDE00\ud800"`, `[12345678901234567890,1E400]`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var compact bytes.Buffer
		if json.Compact(&compact, data) != nil {
			return
		}
		b := compact.Bytes()
		want, ok := slowPath(t, b)
		if !ok {
			want = b
		}
		if got := encodeOrRaw(b); !bytes.Equal(got, want) {
			t.Fatalf("encodeOrRaw(%q) = %q, want %q", b, got, want)
		}
	})
}

// TestAppendSerdeRuneIsSerdeEscaping checks the per-character escaping the
// token rewrite uses against jsonline.Quote, serde_json's escaping.
func TestAppendSerdeRuneIsSerdeEscaping(t *testing.T) {
	runes := []rune{0x2028, 0x2029, 0xFFFD, 0x1F600}
	for r := rune(0); r < 0x800; r++ {
		runes = append(runes, r)
	}
	for _, r := range runes {
		want := jsonline.Quote([]byte(string(r)))
		if got := append(appendSerdeRune([]byte{'"'}, r), '"'); string(got) != string(want) {
			t.Fatalf("rune %U: %q, want %q", r, got, want)
		}
	}
}

// TestPlainDecimalsAreSerdeForm checks the decimal fast path against the
// general path on random decimals of every shape it might accept.
func TestPlainDecimalsAreSerdeForm(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 4))
	digits := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(byte('0' + random.IntN(10)))
		}
		return b.String()
	}
	accepted := 0
	for i := 0; i < 300000; i++ {
		integer := "0"
		if random.IntN(3) > 0 {
			integer = string(byte('1'+random.IntN(9))) + digits(random.IntN(17))
		}
		fraction := strings.Repeat("0", random.IntN(7)) + digits(1+random.IntN(18))
		token := integer + "." + fraction
		if random.IntN(2) == 0 {
			token = "-" + token
		}
		end, plain := scanNumber([]byte(token), 0)
		if end != len(token) {
			t.Fatalf("scanNumber(%s) ended at %d", token, end)
		}
		want, ok := slowPath(t, []byte(token))
		if !ok {
			t.Fatalf("%s did not parse", token)
		}
		if plain {
			accepted++
			if string(want) != token {
				t.Fatalf("plain decimal %s is written %s", token, want)
			}
		}
		if got := encodeOrRaw([]byte(token)); string(got) != string(want) {
			t.Fatalf("Encode(%s) = %s, want %s", token, got, want)
		}
	}
	if accepted == 0 {
		t.Fatal("no decimal took the fast path")
	}
}
