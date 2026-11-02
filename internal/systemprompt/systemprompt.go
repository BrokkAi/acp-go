// Package systemprompt holds the client-provided system prompt rules shared by
// the v1 github.com/BrokkAi/acp-go/unstable and github.com/BrokkAi/acp-go/mcp
// facades and their draft-v2 counterparts, so every lifecycle request that can
// carry a systemPrompt is checked the same way on both sides of the wire.
//
// The rules follow the client-provided system prompt RFD: the agent must
// advertise the systemPrompt session capability; append is the baseline mode
// and override needs the nested mode.override capability; the mode is required
// and an unknown one is unsupported rather than treated as append or override;
// content must be a non-empty list of text blocks, because text is the only
// content type the capability covers.
package systemprompt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/initresult"
	v1 "github.com/BrokkAi/acp-go/schema/unstable"
	v2 "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

// capability is the advertised systemPrompt capability of either version.
type capability struct {
	advertised bool
	override   bool
}

// check applies the rules to a prompt reduced to its mode and the content
// type of each block ("text" for a text block).
func check(capability capability, mode string, content []string) error {
	if !capability.advertised {
		return fmt.Errorf("agent did not advertise systemPrompt support")
	}
	switch mode {
	case "":
		return fmt.Errorf("systemPrompt mode is required")
	case string(v1.SystemPromptModeAppend):
	case string(v1.SystemPromptModeOverride):
		if !capability.override {
			return fmt.Errorf("agent did not advertise systemPrompt override mode support")
		}
	default:
		return fmt.Errorf("unsupported systemPrompt mode %q", mode)
	}
	if len(content) == 0 {
		return fmt.Errorf("systemPrompt content must not be empty")
	}
	for i, kind := range content {
		if kind != "text" {
			return fmt.Errorf("systemPrompt content block %d is not text (%s); only text content is supported", i, kind)
		}
	}
	return nil
}

// V1 checks a v1 prompt against the capability an agent advertised at
// agentCapabilities.sessionCapabilities.systemPrompt. A nil prompt is valid.
func V1(advertised *v1.SessionSystemPromptCapabilities, prompt *v1.SystemPrompt) error {
	if prompt == nil {
		return nil
	}
	capability := capability{advertised: advertised != nil}
	if advertised != nil && advertised.Mode != nil {
		capability.override = advertised.Mode.Override != nil
	}
	content := make([]string, len(prompt.Content))
	for i, block := range prompt.Content {
		content[i] = v1Kind(block)
	}
	return check(capability, string(prompt.Mode), content)
}

// V1Client is V1 against the capability in the agent's initialize response.
func V1Client(initialization v1.InitializeResponse, prompt *v1.SystemPrompt) error {
	var advertised *v1.SessionSystemPromptCapabilities
	if agent := initialization.AgentCapabilities; agent != nil && agent.SessionCapabilities != nil {
		advertised = agent.SessionCapabilities.SystemPrompt
	}
	return V1(advertised, prompt)
}

// V2 checks a draft-v2 prompt against the capability an agent advertised at
// capabilities.session.systemPrompt. A nil prompt is valid.
func V2(advertised *v2.SessionSystemPromptCapabilities, prompt *v2.SystemPrompt) error {
	if prompt == nil {
		return nil
	}
	capability := capability{advertised: advertised != nil}
	if advertised != nil && advertised.Mode != nil {
		capability.override = advertised.Mode.Override != nil
	}
	content := make([]string, len(prompt.Content))
	for i, block := range prompt.Content {
		content[i] = v2Kind(block)
	}
	return check(capability, string(prompt.Mode), content)
}

// V2Client is V2 against the capability in the agent's initialize response.
func V2Client(initialization v2.InitializeResponse, prompt *v2.SystemPrompt) error {
	var advertised *v2.SessionSystemPromptCapabilities
	if agent := initialization.Capabilities; agent != nil && agent.Session != nil {
		advertised = agent.Session.SystemPrompt
	}
	return V2(advertised, prompt)
}

// variant names a content block by the variants set on it: its type tag when
// exactly one is set.
func variant(tags []string) string {
	switch len(tags) {
	case 0:
		return "no content type"
	case 1:
		return tags[0]
	default:
		return "several content types"
	}
}

func v1Kind(block v1.ContentBlock) string {
	var tags []string
	for _, set := range []struct {
		ok  bool
		tag string
	}{
		{block.Text != nil, "text"},
		{block.Image != nil, "image"},
		{block.Audio != nil, "audio"},
		{block.ResourceLink != nil, "resource_link"},
		{block.Resource != nil, "resource"},
	} {
		if set.ok {
			tags = append(tags, set.tag)
		}
	}
	return variant(tags)
}

func v2Kind(block v2.ContentBlock) string {
	var tags []string
	for _, set := range []struct {
		ok  bool
		tag string
	}{
		{block.Text != nil, "text"},
		{block.Image != nil, "image"},
		{block.Audio != nil, "audio"},
		{block.ResourceLink != nil, "resource_link"},
		{block.Resource != nil, "resource"},
		{block.Other != nil, fmt.Sprintf("unknown type %q", string(block.Kind))},
	} {
		if set.ok {
			tags = append(tags, set.tag)
		}
	}
	return variant(tags)
}

// Rejection is the reference SDK's -32602 for a system prompt an agent cannot
// apply (Error::invalid_params().data(message)): the generic JSON-RPC message,
// with the reason as string data. A nil err stays nil.
func Rejection(err error) error {
	if err == nil {
		return nil
	}
	data, _ := json.Marshal(err.Error())
	return &acp.RPCError{Code: -32602, Message: "Invalid params", Data: data}
}

// AgentV1 is the agent-side check of a v1 prompt against the capability the
// runtime announced on the request's connection (initresult.From), answered
// as Rejection. Without a recorded initialize response no capability was
// announced, so any prompt is refused.
func AgentV1(ctx context.Context, prompt *v1.SystemPrompt) error {
	var sent struct {
		AgentCapabilities *struct {
			SessionCapabilities *struct {
				SystemPrompt *v1.SessionSystemPromptCapabilities `json:"systemPrompt"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	var advertised *v1.SessionSystemPromptCapabilities
	if raw := initresult.From(ctx); raw != nil && json.Unmarshal(raw, &sent) == nil {
		if agent := sent.AgentCapabilities; agent != nil && agent.SessionCapabilities != nil {
			advertised = agent.SessionCapabilities.SystemPrompt
		}
	}
	return Rejection(V1(advertised, prompt))
}

// AgentV2 is AgentV1 for a draft-v2 prompt and
// capabilities.session.systemPrompt.
func AgentV2(ctx context.Context, prompt *v2.SystemPrompt) error {
	var sent struct {
		Capabilities *struct {
			Session *struct {
				SystemPrompt *v2.SessionSystemPromptCapabilities `json:"systemPrompt"`
			} `json:"session"`
		} `json:"capabilities"`
	}
	var advertised *v2.SessionSystemPromptCapabilities
	if raw := initresult.From(ctx); raw != nil && json.Unmarshal(raw, &sent) == nil {
		if agent := sent.Capabilities; agent != nil && agent.Session != nil {
			advertised = agent.Session.SystemPrompt
		}
	}
	return Rejection(V2(advertised, prompt))
}

// StrictV1 reports the reference SDK's deserialization error, with its text,
// for the first fault in document order within the systemPrompt member of v1
// lifecycle params: a mode that is not a string, a null content, a content
// block that is null or has no type, and a text block whose text is missing
// or null, then a missing mode or content. Most of these encoding/json would
// decode leniently. It returns nil when the member is absent or null, and
// when the first fault is one encoding/json rejects as well (a string prompt,
// a content that is not an array, a malformed non-text block), which the
// full decode then reports. It runs before that decode, so a prompt fault
// wins over a bad sibling member of the params, even one earlier in the
// document.
func StrictV1(raw json.RawMessage) error { return strict(raw, false) }

// StrictV2 is StrictV1 for draft-v2 lifecycle params, whose text blocks fail
// with the reference SDK's untagged ContentBlock error.
func StrictV2(raw json.RawMessage) error { return strict(raw, true) }

// strict walks the systemPrompt the way the reference SDK deserializes it:
// the params become a JSON value whose objects keep their members in
// document order (a repeated member keeps its first position and its last
// value), and serde then reads each member in that order, failing at the
// first fault it reaches and reporting missing fields only after the whole
// object, mode before content. A value encoding/json rejects too ends the
// walk with nil, so the full decode reports it in the same position.
func strict(raw json.RawMessage, draft bool) error {
	// Exact member names: the reference SDK does not match them
	// case-insensitively, as encoding/json does.
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil || isNull(params["systemPrompt"]) {
		return nil
	}
	names, members, ok := orderedMembers(params["systemPrompt"])
	if !ok {
		return nil
	}
	for _, name := range names {
		value := members[name]
		switch name {
		case "mode":
			// SystemPromptMode is an untagged enum whose open variant is a
			// string, so anything else fails the same way.
			var mode string
			if isNull(value) || json.Unmarshal(value, &mode) != nil {
				return errors.New("data did not match any variant of untagged enum SystemPromptMode")
			}
		case "content":
			if isNull(value) {
				return errors.New("invalid type: null, expected a sequence")
			}
			var blocks []json.RawMessage
			if json.Unmarshal(value, &blocks) != nil {
				return nil
			}
			for _, block := range blocks {
				if stop, err := strictBlock(block, draft); stop {
					return err
				}
			}
		}
	}
	if _, ok := members["mode"]; !ok {
		return errors.New("missing field `mode`")
	}
	if _, ok := members["content"]; !ok {
		return errors.New("missing field `content`")
	}
	return nil
}

// strictBlock checks one content block in the walk. stop ends the walk, with
// the reference SDK's error, or with nil when encoding/json rejects the block
// as well and the full decode reports it.
func strictBlock(raw json.RawMessage, draft bool) (stop bool, err error) {
	untagged := errors.New("data did not match any variant of untagged enum ContentBlock")
	var members map[string]json.RawMessage
	if isNull(raw) || json.Unmarshal(raw, &members) != nil {
		switch {
		case draft:
			return true, untagged
		case isNull(raw):
			return true, errors.New("invalid type: null, expected internally tagged enum ContentBlock")
		}
		return true, nil
	}
	kind, tagged := members["type"]
	if !tagged {
		if draft {
			return true, untagged
		}
		return true, errors.New("missing field `type`")
	}
	var tag string
	if json.Unmarshal(kind, &tag) == nil && tag == "text" {
		text, ok := members["text"]
		switch {
		case ok && !isNull(text):
		case draft:
			return true, untagged
		case !ok:
			return true, errors.New("missing field `text`")
		default:
			return true, errors.New("invalid type: null, expected a string")
		}
	}
	if draft {
		var block v2.ContentBlock
		return json.Unmarshal(raw, &block) != nil, nil
	}
	var block v1.ContentBlock
	return json.Unmarshal(raw, &block) != nil, nil
}

// orderedMembers returns the member names of a JSON object in document
// order, each once at its first position, with each member's last value.
func orderedMembers(raw json.RawMessage) (names []string, members map[string]json.RawMessage, ok bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, nil, false
	}
	members = map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, false
		}
		name, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, false
		}
		if _, seen := members[name]; !seen {
			names = append(names, name)
		}
		members[name] = value
	}
	return names, members, true
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
