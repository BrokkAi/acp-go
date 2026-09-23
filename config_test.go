package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

func TestSessionConfigSelectorsAndBooleanWire(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	peerDone := make(chan error, 1)
	go func() {
		defer close(peerDone)
		decoder := json.NewDecoder(peer)
		encoder := json.NewEncoder(peer)
		replies := []string{
			`{"configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"large","options":[{"value":"large","name":"Large"},{"value":"small","name":"Small"}]},{"id":"verbose","name":"Verbose","type":"boolean","currentValue":false}]}`,
			`{"configOptions":[{"id":"verbose","name":"Verbose","type":"boolean","currentValue":true}]}`,
			`{}`,
		}
		for _, reply := range replies {
			var request packet
			if err := decoder.Decode(&request); err != nil {
				peerDone <- err
				return
			}
			if err := encoder.Encode(packet{Version: "2.0", ID: request.ID, Result: json.RawMessage(reply)}); err != nil {
				peerDone <- err
				return
			}
		}
	}()
	session := Session{
		SessionID: "session",
		ConfigOptions: []schema.SessionConfigOption{{
			ID:       "model",
			Name:     "Model",
			Category: categoryPtr(schema.SessionConfigOptionCategoryModel),
			Select: &schema.SessionConfigSelect{
				CurrentValue: "large",
				Options: []any{map[string]any{
					"group": "fast", "name": "Fast", "options": []map[string]any{{"value": "large", "name": "Large"}},
				}},
			},
		}, {
			ID:       "verbose",
			Name:     "Verbose",
			Category: categoryPtr(schema.SessionConfigOptionCategoryModelConfig),
			Boolean:  &schema.SessionConfigBoolean{CurrentValue: false},
		}},
	}
	ctx := context.Background()
	if err := c.SetModel(ctx, &session, "large"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetBooleanConfig(ctx, &session, "verbose", true); err != nil {
		t.Fatal(err)
	}
	session.Modes = &schema.SessionModeState{
		CurrentModeID: "plan",
		AvailableModes: []schema.SessionMode{
			{ID: "plan", Name: "Plan"},
			{ID: "code", Name: "Code"},
		},
	}
	if err := c.SetMode(ctx, &session, "code"); err != nil {
		t.Fatal(err)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

func TestSetModeRejectsUnknownAndUnadvertisedModes(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	wireRequests := unexpectedRequestRecorder(t, peer)
	session := Session{SessionID: "session", Modes: &schema.SessionModeState{
		AvailableModes: []schema.SessionMode{{ID: "plan", Name: "Plan"}},
	}}
	err := c.SetMode(context.Background(), &session, "code")
	if err == nil || err.Error() != `unknown session mode "code"` {
		t.Fatalf("unknown mode error = %v", err)
	}
	session.Modes = nil
	err = c.SetMode(context.Background(), &session, "plan")
	if err == nil || err.Error() != "agent did not advertise session modes" {
		t.Fatalf("unadvertised mode error = %v", err)
	}
	select {
	case request := <-wireRequests:
		t.Fatalf("mode validation wrote %s request to wire: %s", request.Method, request.Params)
	default:
	}
}

func TestSelectionErrorsAreTypedWithUnchangedText(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	wireRequests := unexpectedRequestRecorder(t, peer)
	ctx := context.Background()
	selector := func(id, name string, category *schema.SessionConfigOptionCategory, values ...string) schema.SessionConfigOption {
		options := make([]any, 0, len(values))
		for _, value := range values {
			options = append(options, map[string]any{"value": value, "name": value})
		}
		return schema.SessionConfigOption{ID: schema.SessionConfigId(id), Name: name, Category: category,
			Select: &schema.SessionConfigSelect{CurrentValue: schema.SessionConfigValueId(values[0]), Options: options}}
	}
	session := Session{SessionID: "session", ConfigOptions: []schema.SessionConfigOption{
		selector("model", "Model", categoryPtr(schema.SessionConfigOptionCategoryModel), "large", "small"),
		// Matched by conventional ID rather than an advertised category.
		selector("reasoning_effort", "Effort", nil, "low", "high"),
		selector("agent-mode", "Mode", categoryPtr(schema.SessionConfigOptionCategoryMode), "plan"),
	}}

	unknown := []struct {
		set       func() error
		category  schema.SessionConfigOptionCategory
		id, name  string
		value     string
		available []string
		text      string
	}{
		{func() error { return c.SetModel(ctx, &session, "huge") }, schema.SessionConfigOptionCategoryModel,
			"model", "Model", "huge", []string{"large", "small"}, `unknown Model "huge"; available values: large, small`},
		{func() error { return c.SetEffort(ctx, &session, "max") }, schema.SessionConfigOptionCategoryThoughtLevel,
			"reasoning_effort", "Effort", "max", []string{"low", "high"}, `unknown Effort "max"; available values: low, high`},
		{func() error { return c.SetMode(ctx, &session, "code") }, schema.SessionConfigOptionCategoryMode,
			"agent-mode", "Mode", "code", []string{"plan"}, `unknown Mode "code"; available values: plan`},
	}
	for _, test := range unknown {
		err := test.set()
		if err == nil || err.Error() != test.text {
			t.Fatalf("error = %v, want %q", err, test.text)
		}
		var typed *UnknownSelectionError
		if !errors.As(fmt.Errorf("wrapped: %w", err), &typed) {
			t.Fatalf("%v is not *UnknownSelectionError", err)
		}
		if typed.Category != test.category || string(typed.ConfigID) != test.id || typed.Name != test.name ||
			typed.Value != test.value || !reflect.DeepEqual(typed.Available, test.available) {
			t.Fatalf("typed error = %+v", typed)
		}
		var unsupported *UnsupportedSelectionError
		if errors.As(err, &unsupported) {
			t.Fatalf("unknown value reported as unsupported selector: %v", err)
		}
	}

	session.ConfigOptions = nil
	unsupported := []struct {
		set      func() error
		category schema.SessionConfigOptionCategory
		value    string
		text     string
	}{
		{func() error { return c.SetModel(ctx, &session, "large") }, schema.SessionConfigOptionCategoryModel, "large",
			`agent does not advertise ACP model selection; cannot select "large" (update or choose an agent that supports session config options)`},
		{func() error { return c.SetEffort(ctx, &session, "high") }, schema.SessionConfigOptionCategoryThoughtLevel, "high",
			`agent does not advertise ACP reasoning effort selection; cannot select "high" (update or choose an agent that supports session config options)`},
		{func() error { return c.SetMode(ctx, &session, "plan") }, schema.SessionConfigOptionCategoryMode, "plan",
			"agent did not advertise session modes"},
	}
	for _, test := range unsupported {
		err := test.set()
		if err == nil || err.Error() != test.text {
			t.Fatalf("error = %v, want %q", err, test.text)
		}
		var typed *UnsupportedSelectionError
		if !errors.As(fmt.Errorf("wrapped: %w", err), &typed) {
			t.Fatalf("%v is not *UnsupportedSelectionError", err)
		}
		if typed.Category != test.category || typed.Value != test.value {
			t.Fatalf("typed error = %+v", typed)
		}
	}
	select {
	case request := <-wireRequests:
		t.Fatalf("selection validation wrote %s request to wire: %s", request.Method, request.Params)
	default:
	}
}

func TestSelectionRejectedByAgentWrapsRPCError(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	go func() {
		decoder := json.NewDecoder(peer)
		encoder := json.NewEncoder(peer)
		var request packet
		if err := decoder.Decode(&request); err != nil {
			return
		}
		_ = encoder.Encode(packet{Version: "2.0", ID: request.ID, Error: &RPCError{Code: -32602, Message: "model unavailable"}})
	}()
	session := Session{SessionID: "session", ConfigOptions: []schema.SessionConfigOption{{
		ID: "model", Name: "Model", Category: categoryPtr(schema.SessionConfigOptionCategoryModel),
		Select: &schema.SessionConfigSelect{CurrentValue: "large", Options: []any{map[string]any{"value": "large", "name": "Large"}}},
	}}}
	err := c.SetModel(context.Background(), &session, "large")
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != -32602 {
		t.Fatalf("error = %v, want wrapped RPC error", err)
	}
	var unknown *UnknownSelectionError
	var unsupported *UnsupportedSelectionError
	if errors.As(err, &unknown) || errors.As(err, &unsupported) {
		t.Fatalf("agent rejection classified as a local selection error: %v", err)
	}
}

func categoryPtr(value schema.SessionConfigOptionCategory) *schema.SessionConfigOptionCategory {
	return &value
}
