package v2session

import (
	"strings"
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

func sessions() schema.InitializeResponse {
	return schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}}}
}

func TestSessionRequestsNeedTheBaselineSessionSurface(t *testing.T) {
	if err := ValidateNewSessionRequest(schema.InitializeResponse{}, schema.NewSessionRequest{Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session method surface") {
		t.Fatalf("session/new without session capabilities error = %v", err)
	}
	if err := ValidateResumeSessionRequest(schema.InitializeResponse{}, schema.ResumeSessionRequest{SessionID: "s", Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session method surface") {
		t.Fatalf("session/resume without session capabilities error = %v", err)
	}
	if err := ValidateResumeSessionRequest(sessions(), schema.ResumeSessionRequest{SessionID: " ", Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session ID is required") {
		t.Fatalf("blank resume session ID error = %v", err)
	}
}

func TestSessionPathsGateAdditionalDirectories(t *testing.T) {
	request := schema.NewSessionRequest{Cwd: "/repo", AdditionalDirectories: []schema.AbsolutePath{"/additional"}}
	if err := ValidateNewSessionRequest(sessions(), request); err == nil || err.Error() != "agent did not advertise additionalDirectories support" {
		t.Fatalf("additional directories error = %v", err)
	}
	withAdditional := sessions()
	withAdditional.Capabilities.Session.AdditionalDirectories = &schema.SessionAdditionalDirectoriesCapabilities{}
	if err := ValidateNewSessionRequest(withAdditional, request); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNewSessionRequest(sessions(), schema.NewSessionRequest{Cwd: "repo"}); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative cwd error = %v", err)
	}
}

func TestSessionPathsAcceptAbsolutePathsFromEitherPlatform(t *testing.T) {
	withAdditional := sessions()
	withAdditional.Capabilities.Session.AdditionalDirectories = &schema.SessionAdditionalDirectoriesCapabilities{}
	if err := ValidateNewSessionRequest(sessions(), schema.NewSessionRequest{Cwd: "/"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResumeSessionRequest(withAdditional, schema.ResumeSessionRequest{
		SessionID: "s", Cwd: `C:\agent\workspace`, AdditionalDirectories: []schema.AbsolutePath{"/", `C:\agent\extra`},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEveryLifecycleRequestChecksTheSystemPrompt(t *testing.T) {
	prompt := &schema.SystemPrompt{Mode: schema.SystemPromptModeAppend, Content: []schema.ContentBlock{{Text: &schema.TextContent{Text: "x"}}}}
	withFork := sessions()
	withFork.Capabilities.Session.Fork = &schema.SessionForkCapabilities{}
	checks := map[string]func(schema.InitializeResponse) error{
		"session/new": func(initialization schema.InitializeResponse) error {
			return ValidateNewSessionRequest(initialization, schema.NewSessionRequest{Cwd: "/repo", SystemPrompt: prompt})
		},
		"session/resume": func(initialization schema.InitializeResponse) error {
			return ValidateResumeSessionRequest(initialization, schema.ResumeSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: prompt})
		},
		"session/fork": func(initialization schema.InitializeResponse) error {
			return ValidateForkRequest(initialization, schema.ForkSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: prompt})
		},
	}
	for method, check := range checks {
		if err := check(withFork); err == nil || !strings.Contains(err.Error(), "systemPrompt") {
			t.Fatalf("%s without the systemPrompt capability error = %v", method, err)
		}
	}
	withFork.Capabilities.Session.SystemPrompt = &schema.SessionSystemPromptCapabilities{}
	for method, check := range checks {
		if err := check(withFork); err != nil {
			t.Fatalf("%s with the systemPrompt capability: %v", method, err)
		}
	}
}
