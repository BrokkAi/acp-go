package serdejson

import (
	"strconv"
	"strings"
	"testing"
)

// testdata/debug_escaped.txt lists, as inclusive hex ranges, every scalar
// value rustc 1.99's str Debug writes as \u{..}. DebugString must escape
// exactly those.
func TestDebugStringEscapesWhatRustEscapes(t *testing.T) {
	escaped := map[rune]bool{}
	for _, line := range readLines(t, "testdata/debug_escaped.txt") {
		low, high, _ := strings.Cut(line, " ")
		start, _ := strconv.ParseUint(low, 16, 32)
		end, _ := strconv.ParseUint(high, 16, 32)
		for r := rune(start); r <= rune(end); r++ {
			escaped[r] = true
		}
	}
	mismatches := 0
	for r := rune(0); r <= 0x10ffff; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		if got := strings.Contains(DebugString(string(r)), `\u{`); got != escaped[r] {
			mismatches++
			if mismatches <= 20 {
				t.Errorf("U+%04X: escaped %t, Rust %t", r, got, escaped[r])
			}
		}
	}
}

// testdata/debug.txt holds rustc 1.99's Debug form of "a" followed by each
// listed code point, as "<hex> <debug>" lines.
func TestDebugStringMatchesRust(t *testing.T) {
	for _, line := range readLines(t, "testdata/debug.txt") {
		hex, want, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("bad line %q", line)
		}
		cp, err := strconv.ParseUint(hex, 16, 32)
		if err != nil {
			t.Fatal(err)
		}
		if got := DebugString("a" + string(rune(cp))); got != want {
			t.Errorf("U+%04X: got %s, want %s", cp, got, want)
		}
	}
}
