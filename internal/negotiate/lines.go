package negotiate

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"unicode/utf8"
)

// MaxFrame is the largest frame a protocol router reads, matching the
// connection's frame limit.
const MaxFrame = 8 << 20

// ReadLine reads one frame for a protocol router and strips one line
// terminator, "\n" or "\r\n", as the reference SDK's line reader does, so a
// parse error reports the same line. It returns io.EOF when the input ends
// before another frame. The line may be followed by more input already
// buffered in reader; hand both to the selected implementation with
// NewPrefixedReader. A frame larger than MaxFrame or not valid UTF-8, which
// the reference SDK cannot read as a line, is an error.
func ReadLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > MaxFrame {
				return nil, errors.New("ACP frame exceeds 8 MiB")
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(line) == 0 {
			return nil, io.EOF
		}
		break
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
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
