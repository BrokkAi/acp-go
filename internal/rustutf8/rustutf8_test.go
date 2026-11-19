package rustutf8

import (
	"bytes"
	"testing"
)

// TestLossyMatchesRust compares Lossy with Rust's String::from_utf8_lossy,
// whose outputs these are: each maximal invalid prefix of a sequence is one
// U+FFFD (a truncated three-byte sequence is one, a surrogate or overlong
// encoding is one per byte), and a sequence cut short at the end is one.
func TestLossyMatchesRust(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"inva\xfflid", "inva�lid"},
		{"\xff\xfe", "��"},
		{"\xe2\x82", "�"},
		{"\xe2\x82x", "�x"},
		{"\xe2\x82\xac", "€"},
		{"\xf0\x9f\x98", "�"},
		{"\xf0\x9f\x98\x80\xf0", "\U0001F600�"},
		{"\xed\xa0\x80", "���"},
		{"\xc0\xaf", "��"},
		{"\xf4\x90\x80\x80", "����"},
		{"\xe0\x80\x80", "���"},
		{"a\xf1\x80\x80\xe1\x80\xc2b", "a���b"},
		{"\x80\xbf", "��"},
	} {
		if got := Lossy([]byte(tc.in)); !bytes.Equal(got, []byte(tc.want)) {
			t.Errorf("Lossy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLossyKeepsValidInputWithoutCopying(t *testing.T) {
	in := []byte(`{"text":"héllo 😀"}`)
	if got := Lossy(in); &got[0] != &in[0] {
		t.Fatal("Lossy copied valid UTF-8")
	}
}
