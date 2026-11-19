package acphttp

import (
	"strconv"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go/internal/rustutf8"
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

// utf8SequenceAt is rustutf8.SequenceAt.
func utf8SequenceAt(data []byte) (width, errorLen int) { return rustutf8.SequenceAt(data) }
