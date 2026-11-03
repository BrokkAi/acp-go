package agentrouter

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/acptest"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
)

// largeFrameData is longer than the 8 MiB line limit acp-go once had; the
// reference SDK has none. The stdio connection test covers a frame over 16 MiB.
var largeFrameData = strings.Repeat("x", 9<<20)

const largeMeta = `"_meta":{"blob":"`

// TestRouterReadsFramesOfAnySize sends a large initialize, the frame the
// router reads, and a large request after it, which the selected agent reads.
func TestRouterReadsFramesOfAnySize(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	router := New().WithV2(&acptest.V2Agent{Requests: requests})
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Minute)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- router.Serve(context.Background(), local, local) }()
	t.Cleanup(func() { _ = peer.Close(); <-done })
	rw := bufio.NewReadWriter(bufio.NewReader(peer), bufio.NewWriter(peer))

	initialized := exchange(t, rw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"client","version":"1"},`+largeMeta+largeFrameData+`"}}}`)
	if version := selectedVersion(t, initialized); version != 2 {
		t.Fatalf("selected protocol = %d", version)
	}
	request := <-requests
	value, _ := request.Meta.Value.Get("blob")
	if blob, _ := value.(string); blob != largeFrameData {
		t.Fatalf("initialize _meta.blob is %d bytes", len(blob))
	}

	listed := exchange(t, rw, `{"jsonrpc":"2.0","id":2,"method":"session/list","params":{`+largeMeta+largeFrameData+`"}}}`)
	var response struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(listed), &response); err != nil || response.ID != 2 || response.Result == nil {
		t.Fatalf("session/list response = %.200s", listed)
	}
}
