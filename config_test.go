package acp

import (
	"context"
	"encoding/json"
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

func categoryPtr(value schema.SessionConfigOptionCategory) *schema.SessionConfigOptionCategory {
	return &value
}
