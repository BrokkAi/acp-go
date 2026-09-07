package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ConfigOption describes an agent-advertised session selector. Unknown option
// types and their values are preserved without requiring client support.
type ConfigOption struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Category     string          `json:"category,omitempty"`
	Type         string          `json:"type"`
	CurrentValue json.RawMessage `json:"currentValue"`
	Options      []ConfigValue   `json:"options,omitempty"`
}

type ConfigValue struct {
	Value string `json:"value"`
	Name  string `json:"name"`
}

func (s *Session) selector(category string) *ConfigOption {
	for i := range s.ConfigOptions {
		option := &s.ConfigOptions[i]
		if option.Type == "select" && option.Category == category {
			return option
		}
	}
	// Categories are optional; conventional IDs also identify model/mode.
	for i := range s.ConfigOptions {
		option := &s.ConfigOptions[i]
		if option.Type == "select" && option.ID == category && option.Category == "" {
			return option
		}
	}
	return nil
}

// SetModel selects an advertised model and verifies the agent acknowledged it.
// A rejected or unavailable selection must not silently use the default model.
func (c *Connection) SetModel(ctx context.Context, s *Session, model string) error {
	option := s.selector("model")
	if option == nil {
		return fmt.Errorf("agent does not advertise ACP model selection; cannot select %q (update or choose an agent that supports session config options)", model)
	}
	return c.setSelection(ctx, s, *option, model)
}

// SetEffort uses the current model's advertised reasoning levels. Call after
// SetModel because model selection may replace the available effort options.
func (c *Connection) SetEffort(ctx context.Context, s *Session, effort string) error {
	option := s.selector("thought_level")
	if option == nil {
		// Codex's conventional ID also works when categories are omitted.
		option = s.selector("reasoning_effort")
	}
	if option == nil {
		return fmt.Errorf("agent does not advertise ACP reasoning effort selection; cannot select %q (update or choose an agent that supports session config options)", effort)
	}
	return c.setSelection(ctx, s, *option, effort)
}

func (c *Connection) setSelection(ctx context.Context, s *Session, option ConfigOption, value string) error {
	var available []string
	found := false
	for _, choice := range option.Options {
		available = append(available, choice.Value)
		found = found || choice.Value == value
	}
	if !found {
		return fmt.Errorf("unknown %s %q; available values: %s", option.Name, value, strings.Join(available, ", "))
	}
	var response struct {
		ConfigOptions []ConfigOption `json:"configOptions"`
	}
	if err := c.Call(ctx, "session/set_config_option", map[string]string{"sessionId": s.ID, "configId": option.ID, "value": value}, &response); err != nil {
		return fmt.Errorf("select %s %q: %w", option.Name, value, err)
	}
	s.ConfigOptions = response.ConfigOptions
	for _, updated := range s.ConfigOptions {
		if updated.ID == option.ID && updated.Type == "select" {
			var current string
			if json.Unmarshal(updated.CurrentValue, &current) == nil && current == value {
				return nil
			}
		}
	}
	return fmt.Errorf("agent did not confirm %s %q", option.Name, value)
}
