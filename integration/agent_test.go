//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/runner"
)

const agentCommandEnv = "ACP_INTEGRATION_AGENT"

// TestRealAgentPrompt drives an externally supplied ACP agent through the
// public runner. The command is a JSON argv array, for example:
//
//	ACP_INTEGRATION_AGENT='["npx","-y","@agentclientprotocol/codex-acp"]'
//
// ACP_INTEGRATION_AUTH_METHOD optionally selects one of the methods advertised
// by that agent, and ACP_INTEGRATION_REQUIRED_ENV is a comma-separated list of
// variables that must be present. The build tag keeps credential-backed tests
// out of every ordinary local and CI invocation; GitHub's opt-in integration
// job supplies the matrix values.
func TestRealAgentPrompt(t *testing.T) {
	encoded := strings.TrimSpace(os.Getenv(agentCommandEnv))
	if encoded == "" {
		t.Skipf("%s is not set", agentCommandEnv)
	}
	var command []string
	if err := json.Unmarshal([]byte(encoded), &command); err != nil {
		t.Fatalf("%s must be a JSON argv array: %v", agentCommandEnv, err)
	}
	if len(command) == 0 || command[0] == "" {
		t.Fatalf("%s must contain a non-empty argv array", agentCommandEnv)
	}
	for _, name := range strings.Split(os.Getenv("ACP_INTEGRATION_REQUIRED_ENV"), ",") {
		name = strings.TrimSpace(name)
		if name != "" && strings.TrimSpace(os.Getenv(name)) == "" {
			t.Fatalf("required integration credential %s is empty", name)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	answer, err := runner.Runner{
		Config: runner.Config{
			Directory:      mustAbsoluteTempWorkspace(t),
			StateDirectory: mustStateDirectory(t),
			Agent: runner.AgentConfig{
				Command:    command,
				AuthMethod: os.Getenv("ACP_INTEGRATION_AUTH_METHOD"),
			},
			ClientInfo: acp.ClientInfo{
				Name:    "acp-go-integration-test",
				Version: "0.0",
			},
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}.Execute(ctx, "Reply with one short sentence confirming that ACP is working.")
	if err != nil {
		t.Fatalf("real ACP agent run failed: %v", err)
	}
	if strings.TrimSpace(answer) == "" {
		t.Fatal("real ACP agent returned an empty answer")
	}
	t.Logf("real agent answered: %s", strings.TrimSpace(answer))
}

func mustAbsoluteTempWorkspace(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func mustStateDirectory(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
