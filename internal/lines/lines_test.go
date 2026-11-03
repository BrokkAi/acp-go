package lines

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// Each case is the lines futures' AsyncBufReadExt::lines yields for the input.
var cases = map[string][]string{
	"":              nil,
	"\n":            {""},
	"a\nb":          {"a", "b"},
	"a\r\nb\r\n":    {"a", "b"},
	"a\r\r\n":       {"a\r"},
	"a\rb\n":        {"a\rb"},
	"tail\r":        {"tail\r"},
	"\n\r\n\n":      {"", "", ""},
	"x\n" + big:     {"x", big},
	big + "\r\nend": {big, "end"},
}

var big = strings.Repeat("y", 3*retain)

func TestReaderMatchesRustLines(t *testing.T) {
	for input, want := range cases {
		// A small buffer makes every non-trivial line span several reads.
		frames := NewReader(bufio.NewReaderSize(strings.NewReader(input), 16))
		var got []string
		for {
			line, err := frames.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%.20q: %v", input, err)
			}
			got = append(got, string(line))
		}
		if !slices.Equal(got, want) {
			t.Errorf("%.20q: %s", input, difference(got, want))
		}
	}
}

func TestReadLineMatchesRustLines(t *testing.T) {
	for input, want := range cases {
		reader := bufio.NewReaderSize(strings.NewReader(input), 16)
		var got [][]byte
		for {
			line, err := ReadLine(reader)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%.20q: %v", input, err)
			}
			got = append(got, line)
		}
		// Each line is the caller's: later reads do not change it.
		var texts []string
		for _, line := range got {
			texts = append(texts, string(line))
		}
		if !slices.Equal(texts, want) {
			t.Errorf("%.20q: %s", input, difference(texts, want))
		}
	}
}

func TestReaderReleasesLargeBuffer(t *testing.T) {
	frames := NewReader(bufio.NewReaderSize(strings.NewReader(big+"\nsmall\n"), 16))
	if _, err := frames.Next(); err != nil {
		t.Fatal(err)
	}
	if line, err := frames.Next(); err != nil || string(line) != "small" {
		t.Fatalf("Next = %q, %v", line, err)
	}
	if cap(frames.buf) > retain {
		t.Fatalf("kept a %d byte buffer", cap(frames.buf))
	}
}

func TestReadError(t *testing.T) {
	failure := errors.New("read failed")
	input := io.MultiReader(strings.NewReader("partial"), &failingReader{failure})
	if _, err := NewReader(bufio.NewReader(input)).Next(); !errors.Is(err, failure) {
		t.Fatalf("Next = %v, want %v", err, failure)
	}
	input = io.MultiReader(strings.NewReader("partial"), &failingReader{failure})
	if _, err := ReadLine(bufio.NewReader(input)); !errors.Is(err, failure) {
		t.Fatalf("ReadLine = %v, want %v", err, failure)
	}
}

type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

// difference describes where got first differs from want.
func difference(got, want []string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			j := 0
			for j < len(got[i]) && j < len(want[i]) && got[i][j] == want[i][j] {
				j++
			}
			return fmt.Sprintf("line %d differs at byte %d: got %d bytes %.20q, want %d bytes %.20q",
				i, j, len(got[i]), got[i][j:], len(want[i]), want[i][j:])
		}
	}
	return fmt.Sprintf("got %d lines, want %d", len(got), len(want))
}
