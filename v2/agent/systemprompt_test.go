package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// promptAgent advertises the systemPrompt capability it is built with and
// records the system prompt each lifecycle request delivered to it.
type promptAgent struct {
	*echoAgent
	capability *schema.SessionSystemPromptCapabilities
	got        chan *schema.SystemPrompt
}

func (a promptAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: version,
		Info:            schema.Implementation{Name: "prompt", Version: "test"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{SystemPrompt: a.capability}},
	}, nil
}

func (a promptAgent) NewSession(_ context.Context, _ Client, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	a.got <- request.SystemPrompt
	return schema.NewSessionResponse{SessionID: "session"}, nil
}

func (a promptAgent) ResumeSession(_ context.Context, _ Client, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	a.got <- request.SystemPrompt
	return schema.ResumeSessionResponse{}, nil
}

// TestRuntimeGatesStableSystemPrompts is the draft-v2 counterpart of the v1
// runtime check, on session/new and session/resume.
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
		{"not advertised", nil, `{"mode":"append","content":` + text + `}`, "agent did not advertise systemPrompt support"},
		{"append", appendOnly, `{"mode":"append","content":` + text + `}`, ""},
		{"override without mode.override", appendOnly, `{"mode":"override","content":` + text + `}`, "agent did not advertise systemPrompt override mode support"},
		{"override", withOverride, `{"mode":"override","content":` + text + `}`, ""},
		{"unknown mode", withOverride, `{"mode":"replace","content":` + text + `}`, `unsupported systemPrompt mode "replace"`},
		{"unknown content type", appendOnly, `{"mode":"append","content":[{"type":"hologram","x":1}]}`, `systemPrompt content block 0 is not text (unknown type "hologram"); only text content is supported`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan *schema.SystemPrompt, 2)
			w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
				return New(promptAgent{echoAgent: &echoAgent{}, capability: tc.capability, got: got}).Serve(context.Background(), in, out)
			})
			exchange(t, w, r, 0, schema.InitializeMethodName, `{"protocolVersion":2,"info":{"name":"c","version":"1"}}`)
			member := ""
			if tc.prompt != "" {
				member = `,"systemPrompt":` + tc.prompt
			}
			for i, request := range []struct{ method, params string }{
				{schema.SessionNewMethodName, `{"cwd":"/w"` + member + `}`},
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
				if (prompt != nil) != (tc.prompt != "") {
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
