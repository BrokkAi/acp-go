// Package proxyrouter selects an explicit ACP v1 or draft-v2 proxy
// implementation from the first proxy/initialize request. It negotiates
// proxy/initialize exactly as the agent router negotiates initialize, like
// the reference Rust SDK's proxy protocol router: it selects the highest
// configured version at or below the requested one, hands a request for
// exactly that version over unchanged after validating it, and rewrites a
// request for another version to keep what parses under the selected one.
// When a v1 proxy is configured, everything that does not select v2 is handed
// to it. A request no configured version can serve gets -32022 (Unsupported
// protocol version), listing the configured versions; a router with none
// configured answers every proxy/initialize that way. A rejected frame leaves
// the connection uninitialized and the router keeps reading; responses and
// unanswered notifications before initialization are ignored. The router
// never converts traffic after selection.
package proxyrouter

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/BrokkAi/acp-go/internal/negotiate"
	unstable "github.com/BrokkAi/acp-go/schema/unstable"
)

// proxyInitialize is the proxy-chains RFD's proxy initialization method, as
// the pinned unstable schema names it.
const proxyInitialize = unstable.ProxyInitializeMethodName

// Proxy serves one selected protocol implementation. The initial
// proxy/initialize frame is replayed to Serve: unchanged when it requests
// exactly the selected version, otherwise with its params normalized for it.
type Proxy interface {
	Serve(context.Context, io.ReadCloser, io.WriteCloser) error
}

type Router struct {
	v1 Proxy
	v2 Proxy
}

func New() *Router { return &Router{} }

func (r *Router) WithV1(implementation Proxy) *Router {
	r.v1 = implementation
	return r
}

func (r *Router) WithV2(implementation Proxy) *Router {
	r.v2 = implementation
	return r
}

func (r *Router) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	supported := negotiate.Supported{V1: r.v1 != nil, V2: r.v2 != nil}
	reader := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := negotiate.ReadLine(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		route := negotiate.Select(line, proxyInitialize, supported)
		if route.Answer != nil {
			// A rejected frame selects nothing: the conductor may send
			// proxy/initialize again.
			if _, err := out.Write(append(route.Answer, '\n')); err != nil {
				return err
			}
			continue
		}
		switch route.Version {
		case negotiate.V1:
			return r.v1.Serve(ctx, negotiate.NewPrefixedReader(route.Frame, reader, in), out)
		case negotiate.V2:
			return r.v2.Serve(ctx, negotiate.NewPrefixedReader(route.Frame, reader, in), out)
		}
		// Responses, and notifications no configured implementation answers,
		// are ignored before proxy initialization.
	}
}
