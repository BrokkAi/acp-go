package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

func TestTerminalHostUsesGeneratedRequestAndResponseTypes(t *testing.T) {
	h, err := newHost(context.Background(), t.TempDir(), io.Discard, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	h.session = schema.SessionId("session")

	createRaw := json.RawMessage(`{"sessionId":"session","command":"printf","args":["typed terminal\n"]}`)
	createdValue, err := h.request(context.Background(), schema.TerminalCreateMethodName, createRaw)
	if err != nil {
		t.Fatal(err)
	}
	created, ok := createdValue.(schema.CreateTerminalResponse)
	if !ok || created.TerminalID == "" {
		t.Fatalf("unexpected create response: %#v", createdValue)
	}
	id := string(created.TerminalID)

	waitRaw := json.RawMessage(`{"sessionId":"session","terminalId":"` + id + `"}`)
	waitedValue, err := h.request(context.Background(), schema.TerminalWaitForExitMethodName, waitRaw)
	if err != nil {
		t.Fatal(err)
	}
	waited, ok := waitedValue.(schema.WaitForTerminalExitResponse)
	if !ok || waited.ExitCode == nil || *waited.ExitCode != 0 || waited.Signal != nil {
		t.Fatalf("unexpected exit response: %#v", waitedValue)
	}

	outputValue, err := h.request(context.Background(), schema.TerminalOutputMethodName, waitRaw)
	if err != nil {
		t.Fatal(err)
	}
	output, ok := outputValue.(schema.TerminalOutputResponse)
	if !ok || output.Output != "typed terminal\n" || output.ExitStatus == nil {
		t.Fatalf("unexpected output response: %#v", outputValue)
	}
	if _, err = h.request(context.Background(), schema.TerminalReleaseMethodName, waitRaw); err != nil {
		t.Fatal(err)
	}
}
