// Package initresult carries the initialize response an agent runtime sent on
// a connection to the handlers it dispatches later requests to, so that the
// v1 github.com/BrokkAi/acp-go/unstable and draft-v2
// github.com/BrokkAi/acp-go/v2/unstable dispatch can check a request against
// the capabilities that connection was actually told about.
package initresult

import (
	"context"
	"encoding/json"
)

type key struct{}

// With returns ctx carrying sent, the encoded initialize response the
// runtime sent on the connection.
func With(ctx context.Context, sent json.RawMessage) context.Context {
	return context.WithValue(ctx, key{}, sent)
}

// From returns the encoded initialize response With put on ctx, or nil
// outside a request a runtime dispatched after initialize.
func From(ctx context.Context) json.RawMessage {
	sent, _ := ctx.Value(key{}).(json.RawMessage)
	return sent
}
