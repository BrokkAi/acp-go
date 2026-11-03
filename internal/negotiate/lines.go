package negotiate

import (
	"bufio"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go/internal/lines"
)

// ReadLine reads one frame for a protocol router as the reference SDK's line
// reader does: a frame of any size is read, and one "\n" or "\r\n" terminator
// is stripped, so a parse error reports the same line. It returns io.EOF when
// the input ends before another frame. The line may be followed by more input
// already buffered in reader; hand both to the selected implementation with
// NewPrefixedReader. A frame that is not valid UTF-8, which the reference SDK
// cannot read as a line, is an error.
func ReadLine(reader *bufio.Reader) ([]byte, error) {
	line, err := lines.ReadLine(reader)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(line) {
		return nil, errors.New("ACP frame is not valid UTF-8")
	}
	return line, nil
}

// PrefixedReader replays the frame a router selected on before the rest of
// the input, and closes the original input.
type PrefixedReader struct {
	prefix []byte
	next   io.Reader
	closer io.Closer
}

// NewPrefixedReader returns a reader that yields prefix and a line
// terminator, then next. Close closes closer, when it is not nil.
func NewPrefixedReader(prefix []byte, next io.Reader, closer io.Closer) *PrefixedReader {
	return &PrefixedReader{prefix: append(append([]byte(nil), prefix...), '\n'), next: next, closer: closer}
}

func (r *PrefixedReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return r.next.Read(p)
}

func (r *PrefixedReader) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}
