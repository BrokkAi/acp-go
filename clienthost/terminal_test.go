package clienthost

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

func TestTerminalOutputByteLimitIsBounded(t *testing.T) {
	h, err := newHost(context.Background(), t.TempDir(), io.Discard, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.SetSession(schema.SessionId("session"))

	var request schema.CreateTerminalRequest
	if err := json.Unmarshal([]byte(`{"outputByteLimit":-1}`), &request); err == nil {
		t.Fatal("negative output byte limit decoded into uint64")
	}
	for _, test := range []struct {
		name  string
		limit uint64
		want  int
	}{
		{"zero", 0, 0},
		{"one byte", 1, 1},
		{"above cap", 1<<20 + 1, 1 << 20},
		{"maximum uint64", ^uint64(0), 1 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			request = schema.CreateTerminalRequest{
				SessionID: h.session, Command: "true", OutputByteLimit: &test.limit,
			}
			value, err := h.createTerminal(request)
			if err != nil {
				t.Fatal(err)
			}
			created := value.(schema.CreateTerminalResponse)
			terminal := h.terminals[string(created.TerminalID)]
			if terminal == nil || terminal.tail.Capacity != test.want {
				t.Fatalf("terminal capacity = %+v, want %d", terminal, test.want)
			}
			terminal.kill()
			<-terminal.finished
			h.mu.Lock()
			delete(h.terminals, string(created.TerminalID))
			h.mu.Unlock()
		})
	}
}

func TestTerminalHostUsesGeneratedRequestAndResponseTypes(t *testing.T) {
	h, err := newHost(context.Background(), t.TempDir(), io.Discard, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.SetSession(schema.SessionId("session"))

	createRaw := json.RawMessage(`{"sessionId":"session","command":"printf","args":["typed terminal\n"]}`)
	createdValue, err := h.Request(context.Background(), schema.TerminalCreateMethodName, createRaw)
	if err != nil {
		t.Fatal(err)
	}
	created, ok := createdValue.(schema.CreateTerminalResponse)
	if !ok || created.TerminalID == "" {
		t.Fatalf("unexpected create response: %#v", createdValue)
	}
	id := string(created.TerminalID)

	waitRaw := json.RawMessage(`{"sessionId":"session","terminalId":"` + id + `"}`)
	waitedValue, err := h.Request(context.Background(), schema.TerminalWaitForExitMethodName, waitRaw)
	if err != nil {
		t.Fatal(err)
	}
	waited, ok := waitedValue.(schema.WaitForTerminalExitResponse)
	if !ok || waited.ExitCode == nil || *waited.ExitCode != 0 || waited.Signal != nil {
		t.Fatalf("unexpected exit response: %#v", waitedValue)
	}

	outputValue, err := h.Request(context.Background(), schema.TerminalOutputMethodName, waitRaw)
	if err != nil {
		t.Fatal(err)
	}
	output, ok := outputValue.(schema.TerminalOutputResponse)
	if !ok || output.Output != "typed terminal\n" || output.ExitStatus == nil {
		t.Fatalf("unexpected output response: %#v", outputValue)
	}
	if _, err = h.Request(context.Background(), schema.TerminalReleaseMethodName, waitRaw); err != nil {
		t.Fatal(err)
	}
}
