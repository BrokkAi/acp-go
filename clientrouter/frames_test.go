package clientrouter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRouterFramesAreWrittenAsTheReferenceSDKWritesThem checks the two frames
// the router writes itself: the initialize it sends the agent and the reused
// initialize response it hands a v1 client. Both are written as serde_json
// writes them, and the response keeps its members where they are.
func TestRouterFramesAreWrittenAsTheReferenceSDKWritesThem(t *testing.T) {
	var agent bytes.Buffer
	reader := bufio.NewReader(strings.NewReader("{}\n"))
	request := wireFrame{Version: "2.0", ID: json.RawMessage(`1`), Method: "initialize", Params: json.RawMessage(`{"info":{"name":"\u003c&>\u0061"},"protocolVersion":2,"n":1E2}`)}
	if _, err := exchangeInitialize(reader, &agent, request); err != nil {
		t.Fatal(err)
	}
	if got, want := agent.String(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"info":{"name":"<&>a"},"protocolVersion":2,"n":100.0}}`+"\n"; got != want {
		t.Fatalf("initialize frame %q, want %q", got, want)
	}

	response := []byte(`{"result":{"protocolVersion":1,"agentInfo":{"name":"<a>"}},"jsonrpc":"2.0","id":7}`)
	got, err := responseWithID(response, json.RawMessage(`"\u0062"`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"result":{"protocolVersion":1,"agentInfo":{"name":"<a>"}},"jsonrpc":"2.0","id":"b"}`; string(got) != want {
		t.Fatalf("reused response %s, want %s", got, want)
	}
}
