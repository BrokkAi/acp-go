package cookbook_test

import (
	"context"
	"os"
	"testing"

	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// processAgentEnv makes this test binary serve a fixture agent over stdio, so
// the runner recipes can launch a real agent process without credentials.
const processAgentEnv = "ACP_GO_COOKBOOK_AGENT"

func TestMain(m *testing.M) {
	var err error
	switch os.Getenv(processAgentEnv) {
	case "v1":
		err = agent.New(&acptest.TestAgent{}).Serve(context.Background(), os.Stdin, os.Stdout)
	case "v2":
		err = agentv2.New(newHistoryAgent(nil)).Serve(context.Background(), os.Stdin, os.Stdout)
	default:
		os.Exit(m.Run())
	}
	if err != nil {
		panic(err)
	}
}

// fixtureAgentProcess returns the command and environment that launch this
// test binary as a v1 or draft-v2 fixture agent. Applications pass their real
// agent command instead.
func fixtureAgentProcess(version string) ([]string, map[string]string) {
	// The runners start the agent in its workspace, so a relative os.Args[0]
	// would resolve against the wrong directory.
	executable, err := os.Executable()
	if err != nil {
		executable = os.Args[0]
	}
	return []string{executable}, map[string]string{processAgentEnv: version}
}
