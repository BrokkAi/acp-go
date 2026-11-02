package unstable

import (
	"errors"
	"fmt"

	"github.com/BrokkAi/acp-go/internal/unstabledispatch"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	"github.com/BrokkAi/acp-go/v2/agent"
)

// Install serves handler's draft-v2 unstable methods on runtime and
// advertises their capabilities, in one step:
//
//	runtime := agent.New(implementation)
//	if err := unstable.Install(runtime, implementation); err != nil {
//		return err
//	}
//
// It sets runtime.Extensions to Handle, chained in front of any Extensions
// already set; runtime.InitializeResult to the Advertise hook with customize;
// and runtime.Overrides for each stable method whose unstable form handler
// serves, which the stable dispatch cannot carry. Setting all three together
// keeps a capability from being advertised without the method that serves
// it. For an ArchiveHandler, Install serves session/list from
// ListSessionsWithArchive, so that the archived filter and
// SessionInfo.archived survive, and announces capabilities.session.archive,
// which Advertise alone never does.
//
// Whether archive is announced is decided before the customize functions run,
// from the response the Agent's Initialize returned: a customize function
// that adds the baseline capabilities.session does not gain archive, and one
// that removes archive leaves the session/list override installed, so the
// agent still serves the archived filter it no longer announces.
//
// Install fails without changing runtime when runtime is nil, when
// runtime.InitializeResult is already set (including by an earlier Install),
// or when runtime.Overrides already serves a method Install would override.
func Install(runtime *agent.Runtime, handler any, customize ...func(*schema.InitializeResponse) error) error {
	if runtime == nil {
		return errors.New("unstable.Install: runtime is required")
	}
	if runtime.InitializeResult != nil {
		return errors.New("unstable.Install: runtime.InitializeResult is already set")
	}
	overrides, err := unstabledispatch.MergeOverrides(runtime.Overrides, handler, overrideRoutes)
	if err != nil {
		return fmt.Errorf("unstable.Install: %w", err)
	}
	runtime.Overrides = overrides
	runtime.Extensions = Handle(runtime.Extensions, handler)
	runtime.InitializeResult = advertise(handler, true, customize)
	return nil
}

// overrideRoutes maps each stable runtime method Install replaces to the
// binding that serves its unstable form from a handler. The runtime runs its
// initialization gate and the method's capability check first. Serving the
// unstable form of another stable request, such as session/new or
// session/resume with fields the stable schema drops (the client-provided
// system prompt, for example), takes one entry here: the binding decodes the
// raw params into the unstable request type, and the handler method reaches
// the connection's client through agent.ClientFromContext, the same client
// the stable dispatch passes to NewSession and ResumeSession, to call it or
// send session/update before it answers. Such a handler then serves the whole
// method in place of the Agent's.
var overrideRoutes = map[string]unstabledispatch.Binding{
	schema.SessionListMethodName: unstabledispatch.Route(ArchiveHandler.ListSessionsWithArchive, checkArchiveListResponse),
}
