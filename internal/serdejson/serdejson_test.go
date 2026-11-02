package serdejson

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// The expected outputs were produced by serde_json 1 with preserve_order, as
// the reference SDK 5.0.0 builds it, printing serde_json::to_string of each
// line read as a serde_json::Value, or ERR when from_str rejects it.
func TestEncodeMatchesSerdeJSON(t *testing.T) {
	compareWithSerde(t, "testdata/cases.txt", "testdata/expected.txt")
}

// floats.txt holds generated numbers that exercise serde_json's float parsing,
// which without float_roundtrip is not always correctly rounded, and its
// float formatting.
func TestFloatsMatchSerdeJSON(t *testing.T) {
	compareWithSerde(t, "testdata/floats.txt", "testdata/floats_expected.txt")
}

func compareWithSerde(t *testing.T, casesPath, expectedPath string) {
	t.Helper()
	cases := readLines(t, casesPath)
	expected := readLines(t, expectedPath)
	if len(cases) != len(expected) {
		t.Fatalf("%d cases, %d expectations", len(cases), len(expected))
	}
	for i, line := range cases {
		got := "ERR"
		if value, err := Parse([]byte(line)); err == nil {
			got = string(Encode(value))
		}
		if got != expected[i] {
			t.Errorf("%q: got %q, want %q", line, got, expected[i])
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestValueAccessors(t *testing.T) {
	value, err := Parse([]byte(`{"a":1,"b":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if item, ok := value.Get("b"); !ok || item.Kind != String || item.String != "x" {
		t.Fatalf("Get(b) = %+v, %t", item, ok)
	}
	if _, ok := value.Get("missing"); ok {
		t.Fatal("Get found a missing member")
	}
	value.Set("a", &Value{Kind: Integer, Integer: "2"})
	value.Set("c", &Value{Kind: Null})
	if got := string(Encode(value)); got != `{"a":2,"b":"x","c":null}` {
		t.Fatalf("Encode = %s", got)
	}
	if got := Encode(nil); string(got) != "null" {
		t.Fatalf("Encode(nil) = %s", got)
	}
}

// The messages match serde's, as the reference SDK reports them for initialize
// params (confirmed with the reference probe).
func TestInvalidTypeMatchesSerde(t *testing.T) {
	for raw, want := range map[string]string{
		`null`:                         "invalid type: null, expected x",
		`true`:                         "invalid type: boolean `true`, expected x",
		`5`:                            "invalid type: integer `5`, expected x",
		`-3`:                           "invalid type: integer `-3`, expected x",
		`1.5`:                          "invalid type: floating point `1.5`, expected x",
		`1e3`:                          "invalid type: floating point `1000.0`, expected x",
		`1e300`:                        "invalid type: floating point `1e+300`, expected x",
		`"a\"\\b\u0000"`:               `invalid type: string "a\"\\b\0", expected x`,
		"\"x\u0301\u00a0\"":            `invalid type: string "x\u{301}\u{a0}", expected x`,
		"\"\ue000\u3000\u0378\ufe0f\"": `invalid type: string "\u{e000}\u{3000}\u{378}\u{fe0f}", expected x`,
		`[]`:                           "invalid type: sequence, expected x",
		`{}`:                           "invalid type: map, expected x",
	} {
		value, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := InvalidType(value, "x"); got != want {
			t.Errorf("%s: %q, want %q", raw, got, want)
		}
	}
	if got := DebugString("\t\r\n\x01\u2028é"); got != `"\t\r\n\u{1}\u{2028}é"` {
		t.Errorf("DebugString = %s", got)
	}
	if strings.Contains(FormatFloat(0), "e") {
		t.Error("FormatFloat(0) used an exponent")
	}
}
