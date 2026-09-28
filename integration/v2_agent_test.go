//go:build integration

package integration

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/internal/osrun"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	v2runner "github.com/BrokkAi/acp-go/v2/runner"
)

// TestRealAgentV2Prompt drives an externally supplied agent through the
// draft-v2 runner. Real adapters only speak v2 once they advertise it, so the
// test probes with one v2 initialize and skips when the agent selects another
// version or rejects the request. A genuine launch failure still surfaces in
// TestRealAgentPrompt, which runs against the same command.
func TestRealAgentV2Prompt(t *testing.T) {
	command := integrationAgentCommand(t)
	requireIntegrationCredentials(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if version := probeProtocolVersion(ctx, t, command); version != acpv2.Version {
		t.Skipf("agent advertises ACP version %d, not draft ACP v2", version)
	}

	result, err := v2runner.Runner{
		Config: v2runner.Config{
			Directory:      mustAbsoluteTempWorkspace(t),
			StateDirectory: mustStateDirectory(t),
			Agent: v2runner.AgentConfig{
				Command:    command,
				AuthMethod: os.Getenv("ACP_INTEGRATION_AUTH_METHOD"),
			},
			ClientInfo: acpv2.ClientInfo{
				Name:    "acp-go-v2-integration-test",
				Version: "0.0",
			},
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}.Execute(ctx, "Reply with one short sentence confirming that ACP is working.")
	if err != nil {
		t.Fatalf("real draft-v2 ACP agent run failed: %v", err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("real draft-v2 ACP agent returned an empty answer")
	}
	t.Logf("real agent answered: %s (stop reason %v)", strings.TrimSpace(result.Text), result.StopReason)
}

// probeProtocolVersion runs one initialize over an in-memory draft-v2
// connection and reports the protocol version the agent selects.
func probeProtocolVersion(ctx context.Context, t *testing.T, command []string) schema.ProtocolVersion {
	t.Helper()
	cmd := osrun.StartCommand(context.Background(), mustAbsoluteTempWorkspace(t), command, nil)
	diagnostics := &osrun.Tail{Capacity: 64 << 10}
	cmd.Stderr = diagnostics
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("launch ACP agent probe: %v", err)
	}
	defer func() {
		_ = osrun.Kill(cmd)
		_ = cmd.Wait()
		_ = stdin.Close()
		_ = stdout.Close()
	}()

	connection := acpv2.Connect(stdout, stdin, nil, nil)
	defer connection.Close()
	probeCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var response schema.InitializeResponse
	err = connection.Call(probeCtx, schema.InitializeMethodName, schema.InitializeRequest{
		ProtocolVersion: acpv2.Version,
		Info:            schema.Implementation{Name: "acp-go-v2-integration-probe", Version: "0.0"},
	}, &response)
	if err != nil {
		text, _ := diagnostics.Text()
		t.Skipf("agent does not accept a draft-v2 initialize: %v\nAgent diagnostics: %s", err, text)
	}
	return response.ProtocolVersion
}
