// Package lines reads newline-delimited frames as the reference Rust SDK
// 5.0.0 does with futures' AsyncBufReadExt::lines: a line ends at "\n", the
// "\n" and then one "\r" before it are removed, a last line without "\n" is
// returned as is, and there is no size limit. A line of any length is read;
// its buffer grows only as far as the line needs.
//
// Unlike Rust's lines, the readers here do not check UTF-8; callers do, so
// each can report the failure its own way.
package lines

import (
	"bufio"
	"errors"
	"io"
)

// retain is the largest line buffer a Reader keeps for the next line, so one
// large frame does not pin its memory for the life of a connection.
const retain = 1 << 20

// Reader reads lines from a buffered reader, reusing its buffer between
// lines.
type Reader struct {
	r   *bufio.Reader
	buf []byte
}

// NewReader returns a Reader over r.
func NewReader(r *bufio.Reader) *Reader { return &Reader{r: r} }

// Next returns the next line without its terminator. The line is valid only
// until the next call. Next returns io.EOF when the input ends before another
// line, and any other read error as is.
func (l *Reader) Next() ([]byte, error) {
	if cap(l.buf) > retain {
		l.buf = nil
	}
	chunk, err := l.r.ReadSlice('\n')
	line := chunk
	if errors.Is(err, bufio.ErrBufferFull) {
		// The line is longer than the reader's buffer; collect it.
		line, err = collect(l.r, append(l.buf[:0], chunk...))
		l.buf = line
	}
	return finish(line, err)
}

// ReadLine returns the next line from r without its terminator, in a slice
// the caller owns. It returns io.EOF when the input ends before another line,
// and any other read error as is. Input after the line stays buffered in r.
func ReadLine(r *bufio.Reader) ([]byte, error) {
	chunk, err := r.ReadSlice('\n')
	line := append([]byte(nil), chunk...)
	if errors.Is(err, bufio.ErrBufferFull) {
		line, err = collect(r, line)
	}
	return finish(line, err)
}

// collect appends to line the rest of a line whose first part filled the
// reader's buffer.
func collect(r *bufio.Reader, line []byte) ([]byte, error) {
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func finish(line []byte, err error) ([]byte, error) {
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(line) == 0 {
		return nil, io.EOF
	}
	if n := len(line); line[n-1] == '\n' {
		line = line[:n-1]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
	}
	return line, nil
}
