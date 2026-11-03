package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/v2/mcp"
)

// TestRouterAnswersUnservedMethodsAsTheReferenceSDK requires the reference
// SDK's answer to a request no handler serves: its MCP-over-ACP handlers
// decline every method but mcp/message (and the session setup they edit),
// and reject_unhandled answers -32601 "Method not found" with the method as
// data.
func TestRouterAnswersUnservedMethodsAsTheReferenceSDK(t *testing.T) {
	router := mcp.NewMessageRouter(nil)
	attachment, err := router.Attach(mcp.NewToolServer("unserved", "1.0.0", mcp.NewToolRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = attachment.Close(context.Background()) }()
	for _, handler := range []acp.Handler{router.Handle(nil), attachment.HandleSessionSetup(nil)} {
		for _, method := range []string{"foo/bar", "_ext/thing", "session/prompt"} {
			_, err := handler(context.Background(), method, json.RawMessage(`{}`))
			var rpcErr *acp.RPCError
			if !errors.As(err, &rpcErr) {
				t.Fatalf("%s: error = %v", method, err)
			}
			got, _ := json.Marshal(rpcErr)
			quoted, _ := json.Marshal(method)
			if want := `{"code":-32601,"message":"Method not found","data":` + string(quoted) + `}`; string(got) != want {
				t.Fatalf("%s: got %s, want %s", method, got, want)
			}
		}
	}
}

// TestRouterAnswersMalformedParamsAsTheReferenceSDK requires json_cast_params'
// -32602 for an mcp/message envelope that does not decode, and for session
// setup params the reference SDK's validate_session_setup rejects before it
// declares a server; such a setup request is not forwarded. The wants are
// captured from agent-client-protocol 5.0.0 with its unstable features.
func TestRouterAnswersMalformedParamsAsTheReferenceSDK(t *testing.T) {
	router := mcp.NewMessageRouter(nil)
	attachment, err := router.Attach(mcp.NewToolServer("params", "1.0.0", mcp.NewToolRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = attachment.Close(context.Background()) }()
	forwarded := false
	setup := attachment.HandleSessionSetup(func(context.Context, string, json.RawMessage) (any, error) {
		forwarded = true
		return nil, nil
	})
	for _, tc := range []struct {
		method, params, want string
	}{
		{"mcp/message", `{"serverId":7}`, "{\"code\":-32602,\"message\":\"Invalid params\",\"data\":{\"error\":\"invalid type: integer `7`, expected a string\",\"json\":{\"serverId\":7},\"phase\":\"deserialization\"}}"},
		{"mcp/message", ``, "{\"code\":-32602,\"message\":\"Invalid params\",\"data\":{\"error\":\"invalid type: null, expected struct MessageMcpRequest\",\"json\":null,\"phase\":\"deserialization\"}}"},
		{"session/new", ``, "{\"code\":-32602,\"message\":\"Invalid params\",\"data\":{\"error\":\"invalid type: null, expected struct NewSessionRequest\",\"json\":null,\"phase\":\"deserialization\"}}"},
		{"session/new", `{"cwd":5}`, "{\"code\":-32602,\"message\":\"Invalid params\",\"data\":{\"error\":\"invalid type: integer `5`, expected path string\",\"json\":{\"cwd\":5},\"phase\":\"deserialization\"}}"},
		{"session/resume", `{"sessionId":"s"}`, "{\"code\":-32602,\"message\":\"Invalid params\",\"data\":{\"error\":\"missing field `cwd`\",\"json\":{\"sessionId\":\"s\"},\"phase\":\"deserialization\"}}"},
		{"session/fork", `null`, "{\"code\":-32602,\"message\":\"Invalid params\",\"data\":{\"error\":\"invalid type: null, expected struct ForkSessionRequest\",\"json\":null,\"phase\":\"deserialization\"}}"},
	} {
		handler := setup
		if tc.method == "mcp/message" {
			handler = router.Handle(nil)
		}
		_, err := handler(context.Background(), tc.method, json.RawMessage(tc.params))
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || forwarded {
			t.Fatalf("%s %s: error = %v, forwarded = %t", tc.method, tc.params, err, forwarded)
		}
		if got, _ := json.Marshal(rpcErr); string(got) != tc.want {
			t.Fatalf("%s %s:\n got %s\nwant %s", tc.method, tc.params, got, tc.want)
		}
	}
}
