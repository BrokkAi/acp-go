package systemprompt

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	v1 "github.com/BrokkAi/acp-go/schema/unstable"
	v2 "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

func v1Text(text string) v1.ContentBlock {
	return v1.ContentBlock{Text: &v1.TextContent{Text: text}}
}

func v2Text(text string) v2.ContentBlock {
	return v2.ContentBlock{Text: &v2.TextContent{Text: text}}
}

func TestV1Rules(t *testing.T) {
	appendOnly := &v1.SessionSystemPromptCapabilities{}
	withOverride := &v1.SessionSystemPromptCapabilities{Mode: &v1.SessionSystemPromptModeCapabilities{
		Override: &v1.SessionSystemPromptOverrideCapabilities{},
	}}
	text := []v1.ContentBlock{v1Text("Review only.")}
	for _, test := range []struct {
		name       string
		advertised *v1.SessionSystemPromptCapabilities
		prompt     *v1.SystemPrompt
		want       string // empty when valid
	}{
		{"no prompt without capability", nil, nil, ""},
		{"capability absent", nil, &v1.SystemPrompt{Mode: v1.SystemPromptModeAppend, Content: text}, "did not advertise systemPrompt support"},
		{"append", appendOnly, &v1.SystemPrompt{Mode: v1.SystemPromptModeAppend, Content: text}, ""},
		{"override without mode.override", appendOnly, &v1.SystemPrompt{Mode: v1.SystemPromptModeOverride, Content: text}, "override mode"},
		{"override with empty mode object", &v1.SessionSystemPromptCapabilities{Mode: &v1.SessionSystemPromptModeCapabilities{}}, &v1.SystemPrompt{Mode: v1.SystemPromptModeOverride, Content: text}, "override mode"},
		{"override with mode.override", withOverride, &v1.SystemPrompt{Mode: v1.SystemPromptModeOverride, Content: text}, ""},
		{"mode missing", withOverride, &v1.SystemPrompt{Content: text}, "mode is required"},
		{"unknown mode", withOverride, &v1.SystemPrompt{Mode: "replace", Content: text}, `unsupported systemPrompt mode "replace"`},
		{"extension mode", withOverride, &v1.SystemPrompt{Mode: "_vendor", Content: text}, `unsupported systemPrompt mode "_vendor"`},
		{"empty content", appendOnly, &v1.SystemPrompt{Mode: v1.SystemPromptModeAppend, Content: []v1.ContentBlock{}}, "must not be empty"},
		{"null content", appendOnly, &v1.SystemPrompt{Mode: v1.SystemPromptModeAppend}, "must not be empty"},
		{"image content", appendOnly, &v1.SystemPrompt{Mode: v1.SystemPromptModeAppend, Content: []v1.ContentBlock{
			v1Text("ok"), {Image: &v1.ImageContent{Data: "AA==", MimeType: "image/png"}},
		}}, "content block 1 is not text (image)"},
		{"resource link content", appendOnly, &v1.SystemPrompt{Mode: v1.SystemPromptModeAppend, Content: []v1.ContentBlock{
			{ResourceLink: &v1.ResourceLink{Name: "guide", URI: "file:///guide.md"}},
		}}, "not text (resource_link)"},
		{"empty block", appendOnly, &v1.SystemPrompt{Mode: v1.SystemPromptModeAppend, Content: []v1.ContentBlock{{}}}, "not text (no content type)"},
	} {
		err := V1(test.advertised, test.prompt)
		if test.want == "" {
			if err != nil {
				t.Errorf("%s: %v", test.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want it to mention %q", test.name, err, test.want)
		}
	}
}

func TestV2Rules(t *testing.T) {
	appendOnly := &v2.SessionSystemPromptCapabilities{}
	withOverride := &v2.SessionSystemPromptCapabilities{Mode: &v2.SessionSystemPromptModeCapabilities{
		Override: &v2.SessionSystemPromptOverrideCapabilities{},
	}}
	text := []v2.ContentBlock{v2Text("Review only.")}
	var unknown v2.ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"video","uri":"file:///clip.mp4"}`), &unknown); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		advertised *v2.SessionSystemPromptCapabilities
		prompt     *v2.SystemPrompt
		want       string
	}{
		{"no prompt", appendOnly, nil, ""},
		{"capability absent", nil, &v2.SystemPrompt{Mode: v2.SystemPromptModeAppend, Content: text}, "did not advertise systemPrompt support"},
		{"append", appendOnly, &v2.SystemPrompt{Mode: v2.SystemPromptModeAppend, Content: text}, ""},
		{"override without mode.override", appendOnly, &v2.SystemPrompt{Mode: v2.SystemPromptModeOverride, Content: text}, "override mode"},
		{"override with mode.override", withOverride, &v2.SystemPrompt{Mode: v2.SystemPromptModeOverride, Content: text}, ""},
		{"mode missing", withOverride, &v2.SystemPrompt{Content: text}, "mode is required"},
		{"unknown mode", withOverride, &v2.SystemPrompt{Mode: "replace", Content: text}, `unsupported systemPrompt mode "replace"`},
		{"empty content", appendOnly, &v2.SystemPrompt{Mode: v2.SystemPromptModeAppend}, "must not be empty"},
		{"audio content", appendOnly, &v2.SystemPrompt{Mode: v2.SystemPromptModeAppend, Content: []v2.ContentBlock{
			{Audio: &v2.AudioContent{Data: "AA==", MimeType: "audio/wav"}},
		}}, "not text (audio)"},
		{"unknown content type", appendOnly, &v2.SystemPrompt{Mode: v2.SystemPromptModeAppend, Content: []v2.ContentBlock{unknown}}, `not text (unknown type "video")`},
		{"several variants", appendOnly, &v2.SystemPrompt{Mode: v2.SystemPromptModeAppend, Content: []v2.ContentBlock{
			{Text: &v2.TextContent{Text: "a"}, Image: &v2.ImageContent{Data: "AA==", MimeType: "image/png"}},
		}}, "several content types"},
	} {
		err := V2(test.advertised, test.prompt)
		if test.want == "" {
			if err != nil {
				t.Errorf("%s: %v", test.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want it to mention %q", test.name, err, test.want)
		}
	}
}

func TestClientChecksReadTheAdvertisedCapability(t *testing.T) {
	prompt1 := &v1.SystemPrompt{Mode: v1.SystemPromptModeAppend, Content: []v1.ContentBlock{v1Text("x")}}
	if err := V1Client(v1.InitializeResponse{}, prompt1); err == nil {
		t.Fatal("v1 prompt accepted without agent capabilities")
	}
	if err := V1Client(v1.InitializeResponse{AgentCapabilities: &v1.AgentCapabilities{SessionCapabilities: &v1.SessionCapabilities{
		SystemPrompt: &v1.SessionSystemPromptCapabilities{},
	}}}, prompt1); err != nil {
		t.Fatal(err)
	}
	prompt2 := &v2.SystemPrompt{Mode: v2.SystemPromptModeAppend, Content: []v2.ContentBlock{v2Text("x")}}
	if err := V2Client(v2.InitializeResponse{Capabilities: &v2.AgentCapabilities{Session: &v2.SessionCapabilities{}}}, prompt2); err == nil {
		t.Fatal("v2 prompt accepted without the capability")
	}
	if err := V2Client(v2.InitializeResponse{Capabilities: &v2.AgentCapabilities{Session: &v2.SessionCapabilities{
		SystemPrompt: &v2.SessionSystemPromptCapabilities{},
	}}}, prompt2); err != nil {
		t.Fatal(err)
	}
}

func TestRejectionIsTheReferenceInvalidParamsShape(t *testing.T) {
	if Rejection(nil) != nil {
		t.Fatal("nil error was wrapped")
	}
	var rpc *acp.RPCError
	if !errors.As(Rejection(errors.New(`unsupported systemPrompt mode "x"`)), &rpc) {
		t.Fatal("rejection is not an RPC error")
	}
	encoded, err := json.Marshal(rpc)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"code":-32602,"message":"Invalid params","data":"unsupported systemPrompt mode \"x\""}` {
		t.Fatalf("rejection = %s", encoded)
	}
}

func TestStrictMatchesTheReferenceDeserialization(t *testing.T) {
	text := `[{"type":"text","text":"x"}]`
	untagged := "data did not match any variant of untagged enum ContentBlock"
	nullMode := "data did not match any variant of untagged enum SystemPromptMode"
	nullContent := "invalid type: null, expected a sequence"
	for prompt, want := range map[string][2]string{
		// Absent, null, well-formed, and values the full decode rejects.
		``:     {},
		`null`: {},
		`{"mode":"append","content":` + text + `}`: {},
		`{"mode":"append","content":[]}`:           {},
		`"instructions"`:                           {},
		`{"mode":1,"content":` + text + `}`:        {nullMode, nullMode},
		`{"mode":"append","content":[{"type":"image","data":"AA==","mimeType":"image/png"}]}`: {},
		// The values encoding/json would decode leniently.
		`{"content":` + text + `}`:                                  {"missing field `mode`", "missing field `mode`"},
		`{"mode":null,"content":` + text + `}`:                      {"data did not match any variant of untagged enum SystemPromptMode", "data did not match any variant of untagged enum SystemPromptMode"},
		`{"mode":"append"}`:                                         {"missing field `content`", "missing field `content`"},
		`{"mode":"append","content":null}`:                          {"invalid type: null, expected a sequence", "invalid type: null, expected a sequence"},
		`{"mode":"append","content":[{"type":"text"}]}`:             {"missing field `text`", untagged},
		`{"mode":"append","content":[{"type":"text","text":null}]}`: {"invalid type: null, expected a string", untagged},
		// encoding/json matches member names case-insensitively; the
		// reference SDK does not.
		`{"Mode":"append","content":` + text + `}`:                 {"missing field `mode`", "missing field `mode`"},
		`{"mode":"append","Content":` + text + `}`:                 {"missing field `content`", "missing field `content`"},
		`{"mode":"append","content":[{"type":"text","Text":"x"}]}`: {"missing field `text`", untagged},
		// Several faults: the first in document order wins, and missing
		// fields come last, mode before content (probed against the
		// reference SDK).
		`{"mode":null}`: {nullMode, nullMode},
		`{"content":[{"type":"text"}],"mode":"append"}`:        {"missing field `text`", untagged},
		`{"content":[{"type":"text"}]}`:                        {"missing field `text`", untagged},
		`{"content":null}`:                                     {nullContent, nullContent},
		`{"content":[{"type":"text","text":"x"}],"mode":null}`: {nullMode, nullMode},
		`{"mode":1,"content":null}`:                            {nullMode, nullMode},
		`{"mode":null,"content":null}`:                         {nullMode, nullMode},
		`{"content":null,"mode":null}`:                         {nullContent, nullContent},
		`{"content":[{"type":"text","text":"x"},{"type":"text","text":null}],"mode":"append"}`: {"invalid type: null, expected a string", untagged},
		`{"content":[{"text":"x"}],"mode":"append"}`:                                           {"missing field `type`", untagged},
		`{"content":[null],"mode":"append"}`:                                                   {"invalid type: null, expected internally tagged enum ContentBlock", untagged},
		`{"content":[5],"mode":"append"}`:                                                      {"", untagged},
		// A repeated member keeps its first position and its last value.
		`{"content":null,"mode":null,"content":[{"type":"text","text":"x"}]}`: {nullMode, nullMode},
		`{"mode":"append","mode":"append","content":` + text + `}`:            {},
		// A value encoding/json rejects as well ends the walk; the full
		// decode reports it.

		`{"content":[{"type":"image","data":5,"mimeType":"image/png"},{"type":"text"}],"mode":"append"}`: {},
	} {
		params := json.RawMessage(`{"cwd":"/repo"}`)
		if prompt != "" {
			params = json.RawMessage(`{"cwd":"/repo","systemPrompt":` + prompt + `}`)
		}
		for i, strict := range []func(json.RawMessage) error{StrictV1, StrictV2} {
			got := ""
			if err := strict(params); err != nil {
				got = err.Error()
			}
			if got != want[i] {
				t.Fatalf("strict v%d %s = %q, want %q", i+1, prompt, got, want[i])
			}
		}
	}
}
