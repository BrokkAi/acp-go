package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

// promptAgent advertises the systemPrompt capability it is built with and
// records the system prompt each lifecycle request delivered to it.
type promptAgent struct {
	paramsAgent
	capability *schema.SessionSystemPromptCapabilities
	got        chan *schema.SystemPrompt
}

func (a promptAgent) Initialize(ctx context.Context, client Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	response, err := a.paramsAgent.Initialize(ctx, client, request)
	response.AgentCapabilities.SessionCapabilities.SystemPrompt = a.capability
	return response, err
}

func (a promptAgent) NewSession(_ context.Context, _ Client, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	a.got <- request.SystemPrompt
	return schema.NewSessionResponse{SessionID: "session"}, nil
}

func (a promptAgent) LoadSession(_ context.Context, _ Client, request schema.LoadSessionRequest) (schema.LoadSessionResponse, error) {
	a.got <- request.SystemPrompt
	return schema.LoadSessionResponse{}, nil
}

func (a promptAgent) ResumeSession(_ context.Context, _ Client, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	a.got <- request.SystemPrompt
	return schema.ResumeSessionResponse{}, nil
}

// TestRuntimeGatesStableSystemPrompts checks the stable system prompt on
// session/new, session/load, and session/resume: a prompt the runtime's
// initialize response advertised reaches the Agent, and any other prompt is
// refused with the reference SDK's -32602 and string data before the Agent
// runs, as the spec requires of an agent that cannot apply it.
func TestRuntimeGatesStableSystemPrompts(t *testing.T) {
	appendOnly := &schema.SessionSystemPromptCapabilities{}
	withOverride := &schema.SessionSystemPromptCapabilities{Mode: &schema.SessionSystemPromptModeCapabilities{Override: &schema.SessionSystemPromptOverrideCapabilities{}}}
	text := `[{"type":"text","text":"Be brief."}]`
	for _, tc := range []struct {
		name       string
		capability *schema.SessionSystemPromptCapabilities
		prompt     string
		reason     string // the rejection's data, or "" when the Agent receives the prompt
	}{
		{"no prompt without capability", nil, ``, ""},
		{"null prompt without capability", nil, `null`, ""},
		{"not advertised", nil, `{"mode":"append","content":` + text + `}`, "agent did not advertise systemPrompt support"},
		{"append", appendOnly, `{"mode":"append","content":` + text + `}`, ""},
		{"override without mode.override", appendOnly, `{"mode":"override","content":` + text + `}`, "agent did not advertise systemPrompt override mode support"},
		{"override", withOverride, `{"mode":"override","content":` + text + `}`, ""},
		{"unknown mode", withOverride, `{"mode":"replace","content":` + text + `}`, `unsupported systemPrompt mode "replace"`},
		{"empty content", appendOnly, `{"mode":"append","content":[]}`, "systemPrompt content must not be empty"},
		{"image content", appendOnly, `{"mode":"append","content":[{"type":"image","mimeType":"image/png","data":"aGk="}]}`, "systemPrompt content block 0 is not text (image); only text content is supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan *schema.SystemPrompt, 3)
			w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
				return New(promptAgent{capability: tc.capability, got: got}).Serve(context.Background(), in, out)
			})
			exchange(t, w, r, 0, schema.InitializeMethodName, `{"protocolVersion":1}`)
			member := ""
			if tc.prompt != "" {
				member = `,"systemPrompt":` + tc.prompt
			}
			for i, request := range []struct{ method, params string }{
				{schema.SessionNewMethodName, `{"cwd":"/w","mcpServers":[]` + member + `}`},
				{schema.SessionLoadMethodName, `{"sessionId":"s","cwd":"/w","mcpServers":[]` + member + `}`},
				{schema.SessionResumeMethodName, `{"sessionId":"s","cwd":"/w"` + member + `}`},
			} {
				answer := exchange(t, w, r, i+1, request.method, request.params)
				if tc.reason != "" {
					data, _ := json.Marshal(tc.reason)
					if want := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"Invalid params","data":%s}}`, i+1, data); answer != want {
						t.Fatalf("%s answer = %s, want %s", request.method, answer, want)
					}
					if len(got) != 0 {
						t.Fatalf("%s reached the Agent", request.method)
					}
					continue
				}
				prompt := <-got
				if (prompt != nil) != (tc.prompt != "" && tc.prompt != "null") {
					t.Fatalf("%s delivered %+v for %s (answer %s)", request.method, prompt, tc.prompt, answer)
				}
				if prompt != nil {
					encoded, _ := json.Marshal(prompt)
					if string(encoded) != tc.prompt {
						t.Fatalf("%s delivered %s, want %s", request.method, encoded, tc.prompt)
					}
				}
			}
		})
	}
}
