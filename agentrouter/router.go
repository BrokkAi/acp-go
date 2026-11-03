// Package agentrouter selects an explicit ACP v1 or draft-v2 agent
// implementation from the first initialize request, as the reference Rust
// SDK's agent protocol router does. It performs protocol routing only; it
// never converts subsequent traffic between versions.
//
// The router selects the highest configured version at or below the
// requested one. A request for v2 or a newer version selects the v2
// implementation; a newer request is first normalized to keep what parses
// under v2. When a v1 implementation is configured, everything else is handed
// to it: a request for v1 or an older version, a malformed initialize, and any
// other first message, so a v1 client sees exactly the v1 implementation. A
// v2 initialize request is normalized to v1's shape when only v1 is
// configured. A v2-only endpoint answers a request below v2 with -32022
// (Unsupported protocol version) and data {"requested", "supported": [2]}. A
// router with no configured implementation answers every initialize with
// -32022 and "supported": [], as the reference SDK's router does.
//
// A rejected frame leaves the connection uninitialized and the router keeps
// reading, so the client may initialize again. Responses, and notifications
// that select no implementation, are ignored before initialization. The
// selected implementation owns the connection: a repeated initialize is
// negotiated by it alone and never re-routed.
package agentrouter

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/internal/negotiate"
	schema1 "github.com/BrokkAi/acp-go/schema"
	agent2 "github.com/BrokkAi/acp-go/v2/agent"
)

// Agent routes each connection to one configured protocol implementation.
// The selected implementation owns the connection after initialization.
type Agent struct {
	v1 agent.Agent
	v2 agent2.Agent
}

func New() *Agent { return &Agent{} }

func (r *Agent) WithV1(implementation agent.Agent) *Agent {
	r.v1 = implementation
	return r
}

func (r *Agent) WithV2(implementation agent2.Agent) *Agent {
	r.v2 = implementation
	return r
}

func (r *Agent) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	supported := negotiate.Supported{V1: r.v1 != nil, V2: r.v2 != nil}
	reader := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := negotiate.ReadLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		route := negotiate.Select(line, schema1.InitializeMethodName, supported)
		if route.Answer != nil {
			// A rejected frame selects nothing: the connection stays
			// uninitialized and the client may initialize again.
			if _, err := out.Write(append(route.Answer, '\n')); err != nil {
				return err
			}
			continue
		}
		switch route.Version {
		case negotiate.V1:
			return agent.New(r.v1).Serve(ctx, negotiate.NewPrefixedReader(route.Frame, reader, in), out)
		case negotiate.V2:
			return agent2.New(r.v2).Serve(ctx, negotiate.NewPrefixedReader(route.Frame, reader, in), out)
		}
		// Responses, and notifications no configured implementation answers,
		// are ignored before initialization.
	}
}
