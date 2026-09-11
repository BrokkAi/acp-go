// Package proxyrouter selects an explicit ACP v1 or draft-v2 proxy
// implementation from the first _proxy/initialize request. Unlike the agent
// router, proxy selection is exact and never downgrades or canonicalizes.
package proxyrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
)

const (
	proxyInitialize = "_proxy/initialize"
	maxFrame        = 8 << 20
)

// Proxy serves one selected protocol implementation. The initial
// _proxy/initialize frame is replayed to Serve unchanged.
type Proxy interface {
	Serve(context.Context, io.ReadCloser, io.WriteCloser) error
}

type Router struct {
	v1 Proxy
	v2 Proxy
}

func New() *Router { return &Router{} }

func (r *Router) WithV1(implementation Proxy) *Router {
	r.v1 = implementation
	return r
}

func (r *Router) WithV2(implementation Proxy) *Router {
	r.v2 = implementation
	return r
}

func (r *Router) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	if r.v1 == nil && r.v2 == nil {
		return errors.New("proxy protocol router has no configured implementations")
	}
	reader := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := readLine(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		selection, err := selectProxy(line)
		if errors.Is(err, errResponseOnlyFrame) {
			// Response-only traffic before proxy initialization has no route.
			continue
		}
		if err != nil {
			return rejectInitialize(out, line, err)
		}
		var selected Proxy
		switch selection.version {
		case 1:
			selected = r.v1
		case 2:
			selected = r.v2
		}
		if selected == nil {
			return rejectInitialize(out, line, fmt.Errorf(
				"ACP protocol version %d is not configured", selection.version,
			))
		}
		return selected.Serve(ctx, newPrefixedReader(line, reader, in), out)
	}
}

type selection struct {
	version uint16
}

type entry struct {
	raw          json.RawMessage
	frame        wireFrame
	valid        bool
	responseOnly bool
}

type wireFrame struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

var errResponseOnlyFrame = errors.New("response-only frame")

func selectProxy(line []byte) (selection, error) {
	if len(line) > 0 && line[0] == '[' {
		return selectProxyBatch(line)
	}
	entries, err := parseEntries([]json.RawMessage{json.RawMessage(line)})
	if err != nil {
		return selection{}, err
	}
	if len(entries) == 0 {
		return selection{}, errors.New("empty JSON-RPC batch")
	}
	return selectFromEntries(entries)
}

func selectProxyBatch(line []byte) (selection, error) {
	var rawEntries []json.RawMessage
	if err := json.Unmarshal(line, &rawEntries); err != nil || len(rawEntries) == 0 {
		return selection{}, errors.New("empty or malformed JSON-RPC batch")
	}
	entries, err := parseEntries(rawEntries)
	if err != nil {
		return selection{}, err
	}
	return selectFromEntries(entries)
}

func parseEntries(rawEntries []json.RawMessage) ([]entry, error) {
	entries := make([]entry, 0, len(rawEntries))
	for _, raw := range rawEntries {
		item := entry{raw: raw}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, errors.New("invalid JSON-RPC batch entry")
		}
		_, hasMethod := fields["method"]
		_, hasResult := fields["result"]
		_, hasError := fields["error"]
		item.responseOnly = !hasMethod && (hasResult || hasError)
		if err := json.Unmarshal(raw, &item.frame); err == nil &&
			item.frame.Version == "2.0" &&
			(item.frame.Method == "" || (len(item.frame.ID) > 0 && string(item.frame.ID) != "null")) {
			item.valid = true
		}
		entries = append(entries, item)
	}
	return entries, nil
}

func selectFromEntries(entries []entry) (selection, error) {
	for _, item := range entries {
		if item.responseOnly {
			continue
		}
		if !item.valid || item.frame.Method != proxyInitialize {
			return selection{}, errors.New("first ACP proxy request must be _proxy/initialize")
		}
		var params struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		}
		if err := json.Unmarshal(item.frame.Params, &params); err != nil || params.ProtocolVersion == 0 {
			return selection{}, errors.New("invalid proxy initialize protocolVersion")
		}
		if params.ProtocolVersion == 1 {
			var request schema1.InitializeRequest
			if err := json.Unmarshal(item.frame.Params, &request); err != nil {
				return selection{}, errors.New("invalid v1 proxy initialize parameters")
			}
		} else if params.ProtocolVersion == 2 {
			var request schema2.InitializeRequest
			if err := json.Unmarshal(item.frame.Params, &request); err != nil {
				return selection{}, errors.New("invalid v2 proxy initialize parameters")
			}
			if request.Info.Name == "" || request.Info.Version == "" {
				return selection{}, errors.New("v2 proxy initialize info requires name and version")
			}
		}
		return selection{version: params.ProtocolVersion}, nil
	}
	return selection{}, errResponseOnlyFrame
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > maxFrame {
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
	line = bytes.TrimSpace(line)
	if !utf8.Valid(line) {
		return nil, errors.New("ACP frame is not valid UTF-8")
	}
	return append([]byte(nil), line...), nil
}

func rejectInitialize(out io.Writer, original []byte, err error) error {
	var request wireFrame
	_ = json.Unmarshal(original, &request)
	id := json.RawMessage("null")
	if len(request.ID) > 0 {
		id = request.ID
	}
	return json.NewEncoder(out).Encode(struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		Version: "2.0",
		ID:      id,
		Error: struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: -32600, Message: err.Error()},
	})
}

type prefixedReader struct {
	prefix []byte
	next   io.Reader
	closer io.Closer
}

func newPrefixedReader(prefix []byte, next io.Reader, closer io.Closer) *prefixedReader {
	return &prefixedReader{
		prefix: append(append([]byte(nil), prefix...), '\n'),
		next:   next,
		closer: closer,
	}
}

func (r *prefixedReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return r.next.Read(p)
}

func (r *prefixedReader) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}
