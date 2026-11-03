package acphttp

import (
	"strconv"
	"unicode/utf8"
)

// utf8Error describes the first invalid UTF-8 sequence in data with the text
// of Rust's std::str::Utf8Error, which the Rust crate puts after
// "Invalid JSON-RPC: ". It returns "" for valid UTF-8. Like Rust, it reports
// the length of the maximal invalid prefix of a sequence, and a sequence cut
// short by the end of data as incomplete.
func utf8Error(data []byte) string {
	for i := 0; i < len(data); {
		if data[i] < utf8.RuneSelf {
			i++
			continue
		}
		width, errorLen := utf8SequenceAt(data[i:])
		if errorLen < 0 {
			return "incomplete utf-8 byte sequence from index " + strconv.Itoa(i)
		}
		if errorLen > 0 {
			return "invalid utf-8 sequence of " + strconv.Itoa(errorLen) + " bytes from index " + strconv.Itoa(i)
		}
		i += width
	}
	return ""
}

// utf8SequenceAt checks the multi-byte sequence at the start of data the way
// Rust's run_utf8_validation does. It returns the sequence width when it is
// valid, an error length of 1 to 3 when it is not, or -1 when data ends
// before the sequence does.
func utf8SequenceAt(data []byte) (width, errorLen int) {
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
