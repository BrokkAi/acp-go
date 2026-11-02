package acphttp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// burstEntry answers initialize and then sends frames notifications on the
// connection stream.
type burstEntry struct{ frames int }

func (e burstEntry) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	reader := bufio.NewReader(in)
	if _, err := reader.ReadBytes('\n'); err != nil {
		return err
	}
	_, _ = io.WriteString(out, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`+"\n")
	for i := range e.frames {
		_, _ = fmt.Fprintf(out, `{"jsonrpc":"2.0","method":"_test/n","params":{"n":%d}}`+"\n", i)
	}
	<-ctx.Done()
	return nil
}

// failingWriter is a flushable ResponseWriter whose writes start failing
// after okWrites of them.
type failingWriter struct {
	header   http.Header
	okWrites int
	written  strings.Builder
}

func (w *failingWriter) Header() http.Header { return w.header }
func (w *failingWriter) WriteHeader(int)     {}
func (w *failingWriter) Flush()              {}
func (w *failingWriter) Write(p []byte) (int, error) {
	if w.okWrites == 0 {
		return 0, errors.New("client went away")
	}
	w.okWrites--
	w.written.Write(p)
	return len(p), nil
}

// A stream whose write fails leaves the frames it had not taken queued for
// the next subscriber, as the Rust crate's channel does.
func TestServerStreamWriteFailureKeepsLaterFrames(t *testing.T) {
	server := &Server{Entry: burstEntry{frames: 3}}
	defer server.Close()
	initialize := httptest.NewRequest(http.MethodPost, DefaultPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	initialize.Header.Set("Content-Type", jsonMIME)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, initialize)
	connectionID := recorder.Header().Get(HeaderConnectionID)
	connection := server.lookup(connectionID)
	if connection == nil {
		t.Fatalf("initialize = %d %s", recorder.Code, recorder.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		connection.outbound.mu.Lock()
		queued := len(connection.outbound.connection.queue)
		connection.outbound.mu.Unlock()
		if queued == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued %d frames", queued)
		}
		time.Sleep(time.Millisecond)
	}

	get := httptest.NewRequest(http.MethodGet, DefaultPath, nil)
	get.Header.Set("Accept", eventStreamMIME)
	get.Header.Set(HeaderConnectionID, connectionID)
	writer := &failingWriter{header: http.Header{}, okWrites: 1}
	server.ServeHTTP(writer, get)
	if got := writer.written.String(); got != "data: {\"jsonrpc\":\"2.0\",\"method\":\"_test/n\",\"params\":{\"n\":0}}\n\n" {
		t.Fatalf("written = %q", got)
	}

	// The frame whose write failed is lost, as in Rust; the one after it is
	// still queued.
	stream, err := connection.outbound.leaseConnection()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.release()
	var rest []string
	for {
		payload, ok := stream.next()
		if !ok {
			break
		}
		rest = append(rest, string(payload))
	}
	if want := `{"jsonrpc":"2.0","method":"_test/n","params":{"n":2}}`; len(rest) != 1 || rest[0] != want {
		t.Fatalf("queued after the failed write = %q, want [%s]", rest, want)
	}
}

// TestUTF8ErrorMatchesRust compares utf8Error with the Display text of Rust's
// std::str::Utf8Error for the same bytes, as printed by a Rust program.
func TestUTF8ErrorMatchesRust(t *testing.T) {
	for _, test := range []struct {
		data []byte
		want string
	}{
		{[]byte(`{"a":"ok"}`), ""},
		{[]byte("{\"a\":\"€\U0001F600\"}"), ""},
		{[]byte{0xff}, "invalid utf-8 sequence of 1 bytes from index 0"},
		{[]byte{'a', 0x80}, "invalid utf-8 sequence of 1 bytes from index 1"},
		{[]byte{0xc2}, "incomplete utf-8 byte sequence from index 0"},
		{[]byte{0xc2, 0x41}, "invalid utf-8 sequence of 1 bytes from index 0"},
		{[]byte{0xe0}, "incomplete utf-8 byte sequence from index 0"},
		{[]byte{0xe0, 0xa0}, "incomplete utf-8 byte sequence from index 0"},
		{[]byte{0xe0, 0x80}, "invalid utf-8 sequence of 1 bytes from index 0"},
		{[]byte{0xe0, 0xa0, 0x41}, "invalid utf-8 sequence of 2 bytes from index 0"},
		{[]byte{0xed, 0xa0, 0x80}, "invalid utf-8 sequence of 1 bytes from index 0"},
		{[]byte{0xf0, 0x90, 0x80}, "incomplete utf-8 byte sequence from index 0"},
		{[]byte{0xf0, 0x90, 0x80, 0x41}, "invalid utf-8 sequence of 3 bytes from index 0"},
		{[]byte{0xf0, 0x90, 0x41}, "invalid utf-8 sequence of 2 bytes from index 0"},
		{[]byte{0xf4, 0x90}, "invalid utf-8 sequence of 1 bytes from index 0"},
		{[]byte{0xf5}, "invalid utf-8 sequence of 1 bytes from index 0"},
		{[]byte{0xc0, 0x80}, "invalid utf-8 sequence of 1 bytes from index 0"},
		{[]byte{'{', '"', 0xe2, 0x82, 0xac, 0xe2, 0x82}, "incomplete utf-8 byte sequence from index 5"},
		{[]byte{0xe1, 0x80}, "incomplete utf-8 byte sequence from index 0"},
		{[]byte{0xf1, 0x80, 0x80}, "incomplete utf-8 byte sequence from index 0"},
	} {
		if got := utf8Error(test.data); got != test.want {
			t.Errorf("utf8Error(% x) = %q, want %q", test.data, got, test.want)
		}
	}
}
