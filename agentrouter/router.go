// Package agentrouter selects an explicit ACP v1 or draft-v2 agent
// implementation from the first initialize request. It performs protocol
// routing only; it never converts subsequent traffic between versions.
package agentrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go/agent"
	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
	agent2 "github.com/BrokkAi/acp-go/v2/agent"
)

const maxFrame = 8 << 20

// Agent routes each connection to one configured protocol implementation.
// The selected implementation owns the connection after initialization.
type Agent struct {
	v1 agent.Agent
	v2 agent2.Agent
}

func New() *Agent { return &Agent{} }

func (r *Agent) WithV1(implementation agent.Agent) *Agent {
	r.v1 = implementation
	return r
}

func (r *Agent) WithV2(implementation agent2.Agent) *Agent {
	r.v2 = implementation
	return r
}

func (r *Agent) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	if r.v1 == nil && r.v2 == nil {
		return errors.New("agent protocol router has no configured implementations")
	}
	reader := bufio.NewReaderSize(in, 64<<10)
	line, err := readInitialLine(reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}

	frame, params, requested, routeErr := routeFrame(line)
	if routeErr == nil {
		switch {
		case requested >= 2 && r.v2 != nil:
			if requested == 2 {
				var request schema2.InitializeRequest
				routeErr = json.Unmarshal(params, &request)
				if routeErr == nil {
					routeErr = validateV2Initialize(request)
				}
			} else {
				var request schema2.InitializeRequest
				if routeErr = json.Unmarshal(params, &request); routeErr == nil {
					routeErr = validateV2Initialize(request)
					if routeErr == nil {
						request.ProtocolVersion = 2
						frame.Params, routeErr = json.Marshal(request)
					}
				}
			}
			if routeErr == nil {
				rewritten, marshalErr := json.Marshal(frame)
				if marshalErr != nil {
					routeErr = marshalErr
				} else {
					return agent2.New(r.v2).Serve(ctx, newPrefixedReader(rewritten, reader, in), out)
				}
			}
		case requested >= 1 && r.v1 != nil:
			if requested == 1 {
				var request schema1.InitializeRequest
				routeErr = json.Unmarshal(params, &request)
			} else {
				var request schema2.InitializeRequest
				if routeErr = json.Unmarshal(params, &request); routeErr == nil {
					routeErr = validateV2Initialize(request)
					if routeErr == nil {
						v1Request, convertErr := v1InitializeRequest(request)
						if convertErr != nil {
							routeErr = convertErr
						} else {
							frame.Params, routeErr = json.Marshal(v1Request)
						}
					}
				}
			}
			if routeErr == nil {
				rewritten, marshalErr := json.Marshal(frame)
				if marshalErr != nil {
					routeErr = marshalErr
				} else {
					return agent.New(r.v1).Serve(ctx, newPrefixedReader(rewritten, reader, in), out)
				}
			}
		default:
			routeErr = fmt.Errorf("ACP protocol version %d is not configured", requested)
		}
	}

	if len(frame.ID) == 0 {
		return routeErr
	}
	if err := rejectInitialize(out, frame.ID, routeErr); err != nil {
		return err
	}
	return nil
}

type wireFrame struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func readInitialLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > maxFrame {
				return nil, errors.New("initial ACP frame exceeds 8 MiB")
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(line) == 0 && errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		break
	}
	// The line can be followed by more input already buffered in reader.
	// prependReader below receives both this line and the original stream.
	return append(bytes.TrimRight(line, "\r\n"), '\n'), nil
}

func routeFrame(line []byte) (wireFrame, json.RawMessage, uint16, error) {
	var frame wireFrame
	if !utf8.Valid(line) {
		return frame, nil, 0, errors.New("initial ACP frame is not valid UTF-8")
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		return frame, nil, 0, err
	}
	if frame.Version != "2.0" {
		return frame, nil, 0, errors.New("initial ACP frame is not JSON-RPC 2.0")
	}
	if frame.Method != "initialize" {
		return frame, nil, 0, errors.New("first ACP request must be initialize")
	}
	if len(frame.ID) == 0 || string(frame.ID) == "null" {
		return frame, nil, 0, errors.New("first ACP message must be an initialize request")
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		return frame, nil, 0, errors.New("initialize params must be an object")
	}
	rawVersion, ok := params["protocolVersion"]
	if !ok {
		return frame, nil, 0, errors.New("initialize protocolVersion is required")
	}
	var version uint16
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return frame, nil, 0, errors.New("initialize protocolVersion must be a uint16")
	}
	return frame, frame.Params, version, nil
}

func v1InitializeRequest(request schema2.InitializeRequest) (schema1.InitializeRequest, error) {
	info := schema1.Implementation{
		Name:    request.Info.Name,
		Title:   nullableString(request.Info.Title),
		Version: request.Info.Version,
	}
	result := schema1.InitializeRequest{
		ClientInfo:      &info,
		ProtocolVersion: 1,
		Meta:            nullableMeta(request.Meta),
	}
	if request.Capabilities != nil {
		capabilities := schema1.ClientCapabilities{}
		if request.Capabilities.Elicitation != nil {
			capabilities.Elicitation = &schema1.ElicitationCapabilities{
				Form: formCapability(request.Capabilities.Elicitation.Form),
				URL:  urlCapability(request.Capabilities.Elicitation.URL),
			}
		}
		if auth := request.Capabilities.Auth; auth != nil && auth.Terminal != nil {
			terminal := true
			capabilities.Auth = &schema1.AuthCapabilities{Terminal: &terminal}
		}
		capabilities.Session = &schema1.ClientSessionCapabilities{
			ConfigOptions: &schema1.SessionConfigOptionsCapabilities{
				Boolean: &schema1.BooleanConfigOptionCapabilities{},
			},
		}
		result.ClientCapabilities = &capabilities
	}
	return result, nil
}

func nullableString(value schema2.Nullable[string]) *string {
	if !value.Set || value.Null {
		return nil
	}
	return &value.Value
}

func nullableMeta(value schema2.Nullable[schema2.Meta]) schema1.Meta {
	if !value.Set || value.Null || value.Value == nil {
		return nil
	}
	result := make(schema1.Meta, len(value.Value))
	for key, item := range value.Value {
		result[key] = item
	}
	return result
}

func formCapability(value *schema2.ElicitationFormCapabilities) *schema1.ElicitationFormCapabilities {
	if value == nil {
		return nil
	}
	return &schema1.ElicitationFormCapabilities{}
}

func urlCapability(value *schema2.ElicitationUrlCapabilities) *schema1.ElicitationUrlCapabilities {
	if value == nil {
		return nil
	}
	return &schema1.ElicitationUrlCapabilities{}
}

func validateV2Initialize(request schema2.InitializeRequest) error {
	if request.Info.Name == "" || request.Info.Version == "" {
		return errors.New("v2 initialize info requires name and version")
	}
	return nil
}

func rejectInitialize(out io.Writer, id json.RawMessage, err error) error {
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
	return &prefixedReader{prefix: append(append([]byte(nil), prefix...), '\n'), next: next, closer: closer}
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
