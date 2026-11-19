// Package rustutf8 checks and repairs UTF-8 the way Rust's standard library
// does, which differs from Go's unicode/utf8 in how it splits an invalid
// sequence: Rust reports, and String::from_utf8_lossy replaces, each maximal
// invalid prefix of a sequence (the "substitution of maximal subparts" of the
// Unicode standard), where utf8.DecodeRune treats every invalid byte on its
// own and bytes.ToValidUTF8 collapses a whole run into one replacement.
package rustutf8

import "unicode/utf8"

// replacement is U+FFFD REPLACEMENT CHARACTER in UTF-8.
const replacement = "�"

// SequenceAt checks the multi-byte sequence at the start of data the way
// Rust's run_utf8_validation does. data must start with a byte of 0x80 or
// more. It returns the sequence width when the sequence is valid, an error
// length of 1 to 3 when it is not, or -1 when data ends before the sequence
// does.
func SequenceAt(data []byte) (width, errorLen int) {
	first := data[0]
	switch {
	case first >= 0xC2 && first <= 0xDF:
		width = 2
	case first >= 0xE0 && first <= 0xEF:
		width = 3
	case first >= 0xF0 && first <= 0xF4:
		width = 4
	default:
		return 0, 1
	}
	if len(data) < 2 {
		return 0, -1
	}
	second := data[1]
	low, high := byte(0x80), byte(0xBF)
	switch first {
	case 0xE0:
		low = 0xA0
	case 0xED:
		high = 0x9F
	case 0xF0:
		low = 0x90
	case 0xF4:
		high = 0x8F
	}
	if second < low || second > high {
		return 0, 1
	}
	for k := 2; k < width; k++ {
		if len(data) <= k {
			return 0, -1
		}
		if data[k] < 0x80 || data[k] > 0xBF {
			return 0, k
		}
	}
	return width, 0
}

// Lossy returns data as Rust's String::from_utf8_lossy does: each maximal
// invalid prefix of a sequence, and a sequence cut short by the end of data,
// becomes one U+FFFD, and everything else is kept. Valid UTF-8 is returned
// as is, without copying.
func Lossy(data []byte) []byte {
	if utf8.Valid(data) {
		return data
	}
	out := make([]byte, 0, len(data)+8)
	for i := 0; i < len(data); {
		if data[i] < utf8.RuneSelf {
			out = append(out, data[i])
			i++
			continue
		}
		width, errorLen := SequenceAt(data[i:])
		switch {
		case errorLen < 0:
			// The rest of data is the start of one sequence.
			return append(out, replacement...)
		case errorLen > 0:
			out = append(out, replacement...)
			i += errorLen
		default:
			out = append(out, data[i:i+width]...)
			i += width
		}
	}
	return out
}
