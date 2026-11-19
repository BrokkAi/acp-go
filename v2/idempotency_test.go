package v2

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func TestNewIdempotencyKeyIsARandomUUIDv4(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[IdempotencyKey]bool{}
	for range 64 {
		key := NewIdempotencyKey()
		if !uuid.MatchString(string(key)) {
			t.Fatalf("key %q is not a hyphenated lowercase UUIDv4", key)
		}
		if err := key.Validate(); err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if seen[key] {
			t.Fatalf("key %q repeated", key)
		}
		seen[key] = true
	}
}

// TestPromptContentWithKeySendsTheKey checks that the key goes out on
// session/prompt, that PromptContent sends none, and that a key the agent
// would reject fails before the request is written.
func TestPromptContentWithKeySendsTheKey(t *testing.T) {
	keys := make(chan *schema.IdempotencyKey, 4)
	client, _ := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case schema.InitializeMethodName:
			return sessionInitialization(), nil
		case schema.SessionPromptMethodName:
			keys <- decode[schema.PromptRequest](t, raw).IdempotencyKey
			return schema.PromptResponse{MessageID: "user-1"}, nil
		default:
			return nil, &acp.RPCError{Code: -32601}
		}
	}, nil)
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "fixture-client", Version: "2.0"})
	if err != nil {
		t.Fatal(err)
	}
	session := Session{SessionID: "s"}
	prompt := []Content{NewTextContent("hi")}

	key := NewIdempotencyKey()
	if id, err := client.PromptContentWithKey(ctx, initialization, session, prompt, key); err != nil || id != "user-1" {
		t.Fatalf("PromptContentWithKey = %q, %v", id, err)
	}
	if got := <-keys; got == nil || *got != key {
		t.Fatalf("sent key %v, want %q", got, key)
	}
	if _, err := client.PromptContent(ctx, initialization, session, prompt); err != nil {
		t.Fatal(err)
	}
	if got := <-keys; got != nil {
		t.Fatalf("PromptContent sent key %q", *got)
	}

	for key, want := range map[IdempotencyKey]string{
		"": "idempotency key must not be empty",
		IdempotencyKey(strings.Repeat("é", 256)): "idempotency key must be at most 255 characters",
	} {
		if _, err := client.PromptContentWithKey(ctx, initialization, session, prompt, key); err == nil || err.Error() != want {
			t.Fatalf("key of %d bytes: err = %v, want %q", len(key), err, want)
		}
	}
	if _, err := client.PromptContentWithKey(ctx, initialization, session, prompt, IdempotencyKey(strings.Repeat("é", 255))); err != nil {
		t.Fatalf("255-character key: %v", err)
	}
	<-keys
	if len(keys) != 0 {
		t.Fatal("an invalid key reached the wire")
	}
}
