// Package promptrulev2 checks client-provided system prompts of the stable
// draft-v2 schema types with the rules in promptrule.
package promptrulev2

import (
	"fmt"

	"github.com/BrokkAi/acp-go/internal/promptrule"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// Check checks a draft-v2 prompt against the capability an agent advertised
// at capabilities.session.systemPrompt. A nil prompt is valid.
func Check(advertised *schema.SessionSystemPromptCapabilities, prompt *schema.SystemPrompt) error {
	if prompt == nil {
		return nil
	}
	capability := promptrule.Capability{Advertised: advertised != nil}
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
			{block.Other != nil, fmt.Sprintf("unknown type %q", string(block.Kind))},
		} {
			if set.ok {
				tags = append(tags, set.tag)
			}
		}
		content[i] = promptrule.Variant(tags)
	}
	return promptrule.Check(capability, string(prompt.Mode), content)
}

// Client is Check against the capability in the agent's initialize response.
func Client(initialization schema.InitializeResponse, prompt *schema.SystemPrompt) error {
	var advertised *schema.SessionSystemPromptCapabilities
	if agent := initialization.Capabilities; agent != nil && agent.Session != nil {
		advertised = agent.Session.SystemPrompt
	}
	return Check(advertised, prompt)
}
