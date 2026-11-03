package clientrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// largeFrameData is longer than the 8 MiB line limit acp-go once had; the
// reference SDK has none. The stdio connection test covers a frame over 16 MiB.
var largeFrameData = strings.Repeat("x", 9<<20)

type blob struct {
	Meta struct {
		Blob string `json:"blob"`
	} `json:"_meta"`
}

func withBlob(value map[string]any) map[string]any {
	value["_meta"] = map[string]any{"blob": largeFrameData}
	return value
}

// largeFrameClient sends a large initialize, the router's first frame, then
// a large request after it, and checks the agent's large answers.
type largeFrameClient struct{}

func (largeFrameClient) Serve(ctx context.Context, connection *acpv2.Connection) error {
	var initialized blob
	if err := connection.Call(ctx, schema2.InitializeMethodName, withBlob(map[string]any{
		"protocolVersion": acpv2.Version,
		"info":            map[string]any{"name": "client", "version": "1"},
	}), &initialized); err != nil {
		return err
	}
	if initialized.Meta.Blob != largeFrameData {
		return fmt.Errorf("initialize response blob is %d bytes", len(initialized.Meta.Blob))
	}
	var echoed blob
	if err := connection.Call(ctx, "test/echo", withBlob(map[string]any{}), &echoed); err != nil {
		return err
	}
	if echoed.Meta.Blob != largeFrameData {
		return fmt.Errorf("echoed blob is %d bytes", len(echoed.Meta.Blob))
	}
	return nil
}

func (largeFrameClient) RequestHandler() acp.Handler            { return nil }
func (largeFrameClient) NotificationHandler() acp.Notifications { return nil }

func TestClientRouterReadsFramesOfAnySize(t *testing.T) {
	agentHandler := acptest.AgentHandler(2)
	handler := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "test/echo":
			return params, nil
		case schema2.InitializeMethodName:
			var request blob
			if err := json.Unmarshal(params, &request); err != nil || request.Meta.Blob != largeFrameData {
				return nil, fmt.Errorf("initialize request blob is %d bytes", len(request.Meta.Blob))
			}
			result, err := agentHandler(ctx, method, params)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				return nil, err
			}
			var response map[string]any
			if err := json.Unmarshal(encoded, &response); err != nil {
				return nil, err
			}
			return withBlob(response), nil
		}
		return agentHandler(ctx, method, params)
	}
	open := func() (io.ReadWriteCloser, error) {
		local, peer := net.Pipe()
		deadline := time.Now().Add(5 * time.Minute)
		_ = local.SetDeadline(deadline)
		_ = peer.SetDeadline(deadline)
		_ = acp.Connect(local, local, handler, nil)
		return peer, nil
	}
	err := New().WithV2(func() V2Client { return largeFrameClient{} }).Connect(context.Background(), open)
	if err != nil {
		t.Fatal(err)
	}
}
