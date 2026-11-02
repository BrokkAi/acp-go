package jsonline

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClassify(t *testing.T) {
	deep := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	deepObject := strings.Repeat(`{"a":`, 127) + "1" + strings.Repeat("}", 127)
	// esc spells a JSON \u escape as U+ so the cases stay readable as escapes.
	esc := func(s string) string { return strings.ReplaceAll(s, "U+", `\`+"u") }
	for frame, want := range map[string]Kind{
		esc(`"U+d83dU+de00"`):       Message,
		esc(`{"U+dbffU+dfff":"x"}`): Message,
		esc(`"U+d800U+0041"`):       Invalid,
		esc(`"U+d800U+d800"`):       Invalid,
		esc(`"U+d800\t"`):           Invalid,
		esc(`"aU+dc00b"`):           Invalid,
		`[]`:                        Batch,
		" \t[1, 2, 3]\r":            Batch,
		`[{"jsonrpc":"2.0"}]`:       Batch,
		deep(127):                   Batch,
		`{"jsonrpc":"2.0"}`:         Message,
		`17`:                        Message,
		`"😀"`:                       Message,
		`"é\n\\u"`:                  Message,
		`1e308`:                     Message,
		`-1e-400`:                   Message,
		`0e99999`:                   Message,
		deepObject:                  Message,
		`[`:                         Invalid,
		`[1,`:                       Invalid,
		``:                          Invalid,
		"  ":                        Invalid,
		`not-json`:                  Invalid,
		`{"a":1} {"b":2}`:           Invalid,
		// serde_json rejects these although encoding/json accepts them.
		`"\ud800"`:                               Invalid,
		`"\ud800x"`:                              Invalid,
		`"\ud800\n"`:                             Invalid,
		`"\ud800A"`:                              Invalid,
		`"\udc00"`:                               Invalid,
		`["\ud800"]`:                             Invalid,
		`{"\udfff":1}`:                           Invalid,
		`1e400`:                                  Invalid,
		`[-1e400]`:                               Invalid,
		`{"n":` + strings.Repeat("9", 400) + `}`: Invalid,
		deep(128):                                Invalid,
		`{"a":` + deep(127) + `}`:                Invalid,
	} {
		if got := Classify([]byte(frame)); got != want {
			t.Errorf("Classify(%.40q) = %v, want %v", frame, got, want)
		}
	}
}

func TestBatchRejectionMatchesReferenceWireShape(t *testing.T) {
	const want = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"JSON-RPC batches are not supported; send each message individually"}}`
	if got := string(BatchRejection()); got != want {
		t.Fatalf("BatchRejection() = %s", got)
	}
	first := BatchRejection()
	first[0] = 'x'
	if BatchRejection()[0] != '{' {
		t.Fatal("BatchRejection returned shared storage")
	}
}

func TestParseErrorMatchesReferenceWireShape(t *testing.T) {
	for line, want := range map[string]string{
		`[`:                                `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":"["}}}`,
		``:                                 `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":""}}}`,
		"\"<a&b> \\\x01\x1f\b\f\t\r\x7f é": `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":"\"<a&b>` + " " + `\\\u0001\u001f\b\f\t\r` + "\x7f é" + `"}}}`,
	} {
		got := ParseError([]byte(line))
		if string(got) != want {
			t.Errorf("ParseError(%q) = %s, want %s", line, got, want)
		}
		var decoded struct {
			Error struct {
				Data struct {
					Line string `json:"line"`
				} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(got, &decoded); err != nil || decoded.Error.Data.Line != line {
			t.Errorf("ParseError(%q) does not round-trip: %v, %q", line, err, decoded.Error.Data.Line)
		}
	}
}

// TestNumberRangeMatchesSerdeJSON pins serde_json's float64 evaluation, which
// is not correctly rounded: it rejects some values that round to
// math.MaxFloat64, and accepts tiny values that underflow to zero.
func TestNumberRangeMatchesSerdeJSON(t *testing.T) {
	for number, want := range map[string]Kind{
		"1.7976931348623157e308":  Message,
		"1.7976931348623158e308":  Invalid,
		"17976931348623157e292":   Message,
		"1e308":                   Message,
		"10e308":                  Invalid,
		"0.1e309":                 Message,
		"1e99999999999":           Invalid,
		"1e-99999999999":          Message,
		"0e99999999999":           Message,
		"-1e400":                  Invalid,
		"18446744073709551615":    Message,
		"18446744073709551616":    Message,
		"-9223372036854775809":    Message,
		"1" + zeros(308):          Message,
		"1" + zeros(309):          Invalid,
		"0." + zeros(400) + "1e5": Message,
	} {
		if got := Classify([]byte(number)); got != want {
			t.Errorf("Classify(%.40s) = %v, want %v", number, got, want)
		}
	}
}

func zeros(n int) string { return strings.Repeat("0", n) }

// FuzzClassify checks that classification never panics, that only valid JSON
// is ever accepted, and that every parse error encodes as valid JSON carrying
// the line.
func FuzzClassify(f *testing.F) {
	for _, seed := range []string{`[]`, `{"a":[1,2]}`, `"\` + `ud83d\` + `ude00"`, `"\` + `ud800`, `1e400`, `-0.5E-3`, `[`, ``} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		kind := Classify(line)
		if kind != Invalid && !json.Valid(line) {
			t.Fatalf("Classify(%q) = %v for invalid JSON", line, kind)
		}
		if kind == Invalid && utf8.Valid(line) {
			var decoded struct {
				Error struct {
					Data struct {
						Line string `json:"line"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(ParseError(line), &decoded); err != nil || decoded.Error.Data.Line != string(line) {
				t.Fatalf("ParseError(%q) = %s", line, ParseError(line))
			}
		}
	})
}
