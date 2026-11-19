// Package promptrule holds the client-provided system prompt rules of the
// system prompt RFD, stable since schema crate 1.16, with the check for the
// stable v1 schema types: the agent must advertise the systemPrompt session
// capability; append is the baseline mode and override needs the nested
// mode.override capability; the mode is required and an unknown one is
// unsupported rather than treated as append or override; and content must be
// a non-empty list of text blocks, because text is the only content type the
// capability covers.
//
// It depends only on the stable v1 schema, so the stable acp package and
// every facade can share it. promptrulev2 checks the draft-v2 types.
package promptrule

import (
	"fmt"

	"github.com/BrokkAi/acp-go/schema"
)

// Capability is the advertised systemPrompt capability of either version.
type Capability struct {
	Advertised bool
	Override   bool
}

// Check applies the rules to a prompt reduced to its mode and the content
// type of each block ("text" for a text block; see Variant).
func Check(capability Capability, mode string, content []string) error {
	if !capability.Advertised {
		return fmt.Errorf("agent did not advertise systemPrompt support")
	}
	switch mode {
	case "":
		return fmt.Errorf("systemPrompt mode is required")
	case string(schema.SystemPromptModeAppend):
	case string(schema.SystemPromptModeOverride):
		if !capability.Override {
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

// Variant names a content block by the variants set on it: its type tag when
// exactly one is set.
func Variant(tags []string) string {
	switch len(tags) {
	case 0:
		return "no content type"
	case 1:
		return tags[0]
	default:
		return "several content types"
	}
}

// V1 checks a stable v1 prompt against the capability an agent advertised at
// agentCapabilities.sessionCapabilities.systemPrompt. A nil prompt is valid.
func V1(advertised *schema.SessionSystemPromptCapabilities, prompt *schema.SystemPrompt) error {
	if prompt == nil {
		return nil
	}
	capability := Capability{Advertised: advertised != nil}
	if advertised != nil && advertised.Mode != nil {
		capability.Override = advertised.Mode.Override != nil
	}
	content := make([]string, len(prompt.Content))
	for i, block := range prompt.Content {
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
		content[i] = Variant(tags)
	}
	return Check(capability, string(prompt.Mode), content)
}

// V1Client is V1 against the capability in the agent's initialize response.
func V1Client(initialization schema.InitializeResponse, prompt *schema.SystemPrompt) error {
	var advertised *schema.SessionSystemPromptCapabilities
	if agent := initialization.AgentCapabilities; agent != nil && agent.SessionCapabilities != nil {
		advertised = agent.SessionCapabilities.SystemPrompt
	}
	return V1(advertised, prompt)
}
