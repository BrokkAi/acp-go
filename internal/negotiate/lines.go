package negotiate

import (
	"bufio"
	"io"

	"github.com/BrokkAi/acp-go/internal/lines"
	"github.com/BrokkAi/acp-go/internal/rustutf8"
)

// ReadLine reads one frame for a protocol router as the reference SDK's line
// reader does: a frame of any size is read, and one "\n" or "\r\n" terminator
// is stripped, so a parse error reports the same line. It returns io.EOF when
// the input ends before another frame. The line may be followed by more input
// already buffered in reader; hand both to the selected implementation with
// NewPrefixedReader. As in the reference SDK, bytes that are not valid UTF-8
// are replaced with U+FFFD, so such a frame is handled like any other line
// rather than failing the read.
func ReadLine(reader *bufio.Reader) ([]byte, error) {
	line, err := lines.ReadLine(reader)
	if err != nil {
		return nil, err
	}
	return rustutf8.Lossy(line), nil
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
