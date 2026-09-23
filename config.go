package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BrokkAi/acp-go/schema"
)

// UnknownSelectionError reports that a requested session config option value
// is not among the values the agent advertised for that selector. Category is
// the selector the caller asked for (for example
// schema.SessionConfigOptionCategoryModel from SetModel), which also covers
// selectors matched by their conventional ID rather than an advertised
// category. Available lists the advertised values in order and is nil when the
// selector offers none. No request is sent to the agent when this error is
// returned.
type UnknownSelectionError struct {
	Category  schema.SessionConfigOptionCategory
	ConfigID  schema.SessionConfigId
	Name      string
	Value     string
	Available []string
}

func (e *UnknownSelectionError) Error() string {
	return fmt.Sprintf("unknown %s %q; available values: %s", e.Name, e.Value, strings.Join(e.Available, ", "))
}

// UnsupportedSelectionError reports that the agent advertises no selector for
// the requested category, so the value could not be selected at all. Category
// is schema.SessionConfigOptionCategoryMode, SessionConfigOptionCategoryModel,
// or SessionConfigOptionCategoryThoughtLevel (reasoning effort). Value is the
// requested value; the mode message does not include it.
type UnsupportedSelectionError struct {
	Category schema.SessionConfigOptionCategory
	Value    string
}

func (e *UnsupportedSelectionError) Error() string {
	switch e.Category {
	case schema.SessionConfigOptionCategoryMode:
		return "agent did not advertise session modes"
	case schema.SessionConfigOptionCategoryModel:
		return fmt.Sprintf("agent does not advertise ACP model selection; cannot select %q (update or choose an agent that supports session config options)", e.Value)
	default:
		return fmt.Sprintf("agent does not advertise ACP reasoning effort selection; cannot select %q (update or choose an agent that supports session config options)", e.Value)
	}
}

func sessionSelector(session *Session, category schema.SessionConfigOptionCategory, conventionalID string) *schema.SessionConfigOption {
	for i := range session.ConfigOptions {
		option := &session.ConfigOptions[i]
		if option.Select == nil {
			continue
		}
		if option.Category != nil && *option.Category == category {
			return option
		}
	}
	for i := range session.ConfigOptions {
		option := &session.ConfigOptions[i]
		if option.Select == nil || option.Category != nil {
			continue
		}
		if option.ID == schema.SessionConfigId(conventionalID) {
			return option
		}
	}
	return nil
}

// ConfigOptionsClientCapabilities advertises support for session config
// options. Boolean support is an extension beyond flat selectors.
func ConfigOptionsClientCapabilities(boolean bool) *schema.ClientSessionCapabilities {
	capabilities := schema.ClientSessionCapabilities{}
	capabilities.ConfigOptions = &schema.SessionConfigOptionsCapabilities{
		Boolean: &schema.BooleanConfigOptionCapabilities{},
	}
	if !boolean {
		capabilities.ConfigOptions.Boolean = nil
	}
	return &capabilities
}

// SetMode selects an advertised session mode, using the legacy modes state
// when present and otherwise a mode config option. It returns
// *UnsupportedSelectionError when the agent advertises neither, and
// *UnknownSelectionError when a mode config option does not offer mode.
func (c *Connection) SetMode(ctx context.Context, session *Session, mode string) error {
	if session.Modes != nil {
		for _, available := range session.Modes.AvailableModes {
			if available.ID != schema.SessionModeId(mode) {
				continue
			}
			var result schema.SetSessionModeResponse
			err := c.Call(ctx, schema.SessionSetModeMethodName, schema.SetSessionModeRequest{
				SessionID: session.SessionID, ModeID: schema.SessionModeId(mode),
			}, &result)
			if err == nil {
				session.Modes.CurrentModeID = schema.SessionModeId(mode)
			}
			return err
		}
		return fmt.Errorf("unknown session mode %q", mode)
	}
	if option := sessionSelector(session, schema.SessionConfigOptionCategoryMode, "mode"); option != nil {
		return c.setSelection(ctx, session, schema.SessionConfigOptionCategoryMode, *option, mode)
	}
	return &UnsupportedSelectionError{Category: schema.SessionConfigOptionCategoryMode, Value: mode}
}

// SetModel selects an advertised model and verifies the agent acknowledged it.
// A rejected or unavailable selection must not silently use the default model.
// It returns *UnsupportedSelectionError when the agent advertises no model
// selector and *UnknownSelectionError when model is not an advertised value;
// agent-side rejections wrap the underlying RPC or transport error.
func (c *Connection) SetModel(ctx context.Context, session *Session, model string) error {
	option := sessionSelector(session, schema.SessionConfigOptionCategoryModel, "model")
	if option == nil {
		return &UnsupportedSelectionError{Category: schema.SessionConfigOptionCategoryModel, Value: model}
	}
	return c.setSelection(ctx, session, schema.SessionConfigOptionCategoryModel, *option, model)
}

// SetEffort uses the current model's advertised reasoning levels. Call after
// SetModel because model selection may replace the available effort options.
// Errors follow SetModel, with Category SessionConfigOptionCategoryThoughtLevel.
func (c *Connection) SetEffort(ctx context.Context, session *Session, effort string) error {
	option := sessionSelector(session, schema.SessionConfigOptionCategoryThoughtLevel, "reasoning_effort")
	if option == nil {
		return &UnsupportedSelectionError{Category: schema.SessionConfigOptionCategoryThoughtLevel, Value: effort}
	}
	return c.setSelection(ctx, session, schema.SessionConfigOptionCategoryThoughtLevel, *option, effort)
}

func (c *Connection) setSelection(ctx context.Context, session *Session, category schema.SessionConfigOptionCategory, option schema.SessionConfigOption, value string) error {
	choices, err := selectChoices(option.Select.Options)
	if err != nil {
		return err
	}
	var available []string
	found := false
	for _, choice := range choices {
		available = append(available, string(choice.Value))
		found = found || choice.Value == schema.SessionConfigValueId(value)
	}
	if !found {
		return &UnknownSelectionError{Category: category, ConfigID: option.ID, Name: option.Name, Value: value, Available: available}
	}
	var response schema.SetSessionConfigOptionResponse
	request := schema.SetSessionConfigOptionRequest{
		SessionID: session.SessionID,
		ConfigID:  option.ID,
		ValueID: &schema.SetSessionConfigOptionRequestValueID{
			Value: schema.SessionConfigValueId(value),
		},
	}
	if err := c.Call(ctx, schema.SessionSetConfigOptionMethodName, request, &response); err != nil {
		return fmt.Errorf("select %s %q: %w", option.Name, value, err)
	}
	session.ConfigOptions = response.ConfigOptions
	for _, updated := range session.ConfigOptions {
		if updated.ID == option.ID && updated.Select != nil && updated.Select.CurrentValue == schema.SessionConfigValueId(value) {
			return nil
		}
	}
	return fmt.Errorf("agent did not confirm %s %q", option.Name, value)
}

// SetBooleanConfig sets an advertised boolean session configuration option
// and verifies the agent's returned current value.
func (c *Connection) SetBooleanConfig(ctx context.Context, session *Session, configID string, value bool) error {
	for _, option := range session.ConfigOptions {
		if option.ID != schema.SessionConfigId(configID) || option.Boolean == nil {
			continue
		}
		var response schema.SetSessionConfigOptionResponse
		request := schema.SetSessionConfigOptionRequest{
			SessionID: session.SessionID,
			ConfigID:  option.ID,
			Boolean:   &schema.SetSessionConfigOptionRequestBoolean{Value: value},
		}
		if err := c.Call(ctx, schema.SessionSetConfigOptionMethodName, request, &response); err != nil {
			return fmt.Errorf("set %s %t: %w", option.Name, value, err)
		}
		session.ConfigOptions = response.ConfigOptions
		for _, updated := range session.ConfigOptions {
			if updated.ID == option.ID && updated.Boolean != nil && updated.Boolean.CurrentValue == value {
				return nil
			}
		}
		return fmt.Errorf("agent did not confirm %s %t", option.Name, value)
	}
	return fmt.Errorf("agent did not advertise boolean session config option %q", configID)
}

func selectChoices(value any) ([]schema.SessionConfigSelectOption, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var flat []schema.SessionConfigSelectOption
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &items); err != nil || len(items) == 0 {
		return nil, err
	}
	if _, grouped := items[0]["group"]; !grouped {
		if err := json.Unmarshal(encoded, &flat); err == nil {
			return flat, nil
		}
	}
	var grouped []schema.SessionConfigSelectGroup
	if err := json.Unmarshal(encoded, &grouped); err != nil {
		return nil, fmt.Errorf("unsupported session selector options: %w", err)
	}
	var choices []schema.SessionConfigSelectOption
	for _, group := range grouped {
		choices = append(choices, group.Options...)
	}
	return choices, nil
}
