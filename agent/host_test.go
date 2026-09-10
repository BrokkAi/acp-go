package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

type testClientHost struct{}

func (testClientHost) ReadTextFile(context.Context, schema.ReadTextFileRequest) (schema.ReadTextFileResponse, error) {
	return schema.ReadTextFileResponse{Content: "read"}, nil
}

func (testClientHost) WriteTextFile(context.Context, schema.WriteTextFileRequest) (schema.WriteTextFileResponse, error) {
	return schema.WriteTextFileResponse{}, nil
}

func (testClientHost) CreateTerminal(context.Context, schema.CreateTerminalRequest) (schema.CreateTerminalResponse, error) {
	return schema.CreateTerminalResponse{TerminalID: "terminal"}, nil
}

func (testClientHost) TerminalOutput(context.Context, schema.TerminalOutputRequest) (schema.TerminalOutputResponse, error) {
	return schema.TerminalOutputResponse{Output: "output"}, nil
}

func (testClientHost) WaitForTerminalExit(context.Context, schema.WaitForTerminalExitRequest) (schema.WaitForTerminalExitResponse, error) {
	return schema.WaitForTerminalExitResponse{}, nil
}

func (testClientHost) KillTerminal(context.Context, schema.KillTerminalRequest) (schema.KillTerminalResponse, error) {
	return schema.KillTerminalResponse{}, nil
}

func (testClientHost) ReleaseTerminal(context.Context, schema.ReleaseTerminalRequest) (schema.ReleaseTerminalResponse, error) {
	return schema.ReleaseTerminalResponse{}, nil
}

func (testClientHost) RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
		Selected: &schema.SelectedPermissionOutcome{OptionID: "allow"},
	}}, nil
}

func (testClientHost) CreateElicitation(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error) {
	return schema.CreateElicitationResponse{Accept: &schema.ElicitationAcceptAction{}}, nil
}

func TestHandleClientHostDispatchesTypedMethods(t *testing.T) {
	host := testClientHost{}
	handler := HandleClientHost(host, host, host, host)
	tests := []struct {
		method string
		params string
		check  func(any)
	}{
		{schema.FsReadTextFileMethodName, `{"sessionId":"s","path":"/tmp/x"}`, func(value any) {
			if value.(schema.ReadTextFileResponse).Content != "read" {
				t.Fatal("filesystem read was not dispatched")
			}
		}},
		{schema.FsWriteTextFileMethodName, `{"sessionId":"s","path":"/tmp/x","content":"hi"}`, func(value any) {
			if _, ok := value.(schema.WriteTextFileResponse); !ok {
				t.Fatal("filesystem write was not dispatched")
			}
		}},
		{schema.TerminalCreateMethodName, `{"sessionId":"s","command":"true"}`, func(value any) {
			if value.(schema.CreateTerminalResponse).TerminalID != "terminal" {
				t.Fatal("terminal create was not dispatched")
			}
		}},
		{schema.TerminalOutputMethodName, `{"sessionId":"s","terminalId":"terminal"}`, func(value any) {
			if value.(schema.TerminalOutputResponse).Output != "output" {
				t.Fatal("terminal output was not dispatched")
			}
		}},
		{schema.TerminalWaitForExitMethodName, `{"sessionId":"s","terminalId":"terminal"}`, func(value any) {
			if _, ok := value.(schema.WaitForTerminalExitResponse); !ok {
				t.Fatal("terminal wait was not dispatched")
			}
		}},
		{schema.TerminalKillMethodName, `{"sessionId":"s","terminalId":"terminal"}`, func(value any) {
			if _, ok := value.(schema.KillTerminalResponse); !ok {
				t.Fatal("terminal kill was not dispatched")
			}
		}},
		{schema.TerminalReleaseMethodName, `{"sessionId":"s","terminalId":"terminal"}`, func(value any) {
			if _, ok := value.(schema.ReleaseTerminalResponse); !ok {
				t.Fatal("terminal release was not dispatched")
			}
		}},
		{schema.SessionRequestPermissionMethodName, `{"sessionId":"s","options":[],"toolCall":{"toolCallId":"tool"}}`, func(value any) {
			response := value.(schema.RequestPermissionResponse)
			if response.Outcome.Selected == nil || response.Outcome.Selected.OptionID != "allow" {
				t.Fatal("permission request was not dispatched")
			}
		}},
		{schema.ElicitationCreateMethodName, `{"mode":"url","sessionId":"s","message":"authenticate"}`, func(value any) {
			if value.(schema.CreateElicitationResponse).Accept == nil {
				t.Fatal("elicitation was not dispatched")
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			value, err := handler(context.Background(), test.method, json.RawMessage(test.params))
			if err != nil {
				t.Fatal(err)
			}
			test.check(value)
		})
	}
}

func TestHandleClientHostRejectsMalformedAndUnsupportedMethods(t *testing.T) {
	host := testClientHost{}
	handler := HandleClientHost(host, nil, nil, nil)
	_, err := handler(context.Background(), schema.FsReadTextFileMethodName, json.RawMessage(`{`))
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Fatalf("malformed params error = %v", err)
	}
	_, err = handler(context.Background(), "unknown/method", json.RawMessage(`{}`))
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("unsupported method error = %v", err)
	}
}
