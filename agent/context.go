package agent

import "context"

type clientKey struct{}

// ClientFromContext returns the Client of the connection that delivered the
// request a handler is serving. The runtime puts it on the context of every
// request it dispatches after initialization and its capability checks,
// including the Runtime.Overrides and Runtime.Extensions handlers, which
// receive no Client argument. It is the same Client the runtime passes to the
// Agent's own methods, so a session/load override can replay the
// conversation with session/update notifications before it answers, and an
// override of session/new or session/resume can call the client, as the
// stable methods can. github.com/BrokkAi/acp-go/unstable.Install passes this
// context to the handler methods it routes to, so they can read the client
// this way. ok is false outside a request the runtime dispatched.
func ClientFromContext(ctx context.Context) (client Client, ok bool) {
	client, ok = ctx.Value(clientKey{}).(Client)
	return client, ok
}

func withClient(ctx context.Context, client Client) context.Context {
	return context.WithValue(ctx, clientKey{}, client)
}
