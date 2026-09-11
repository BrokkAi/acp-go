package schema

import "testing"

func TestStableRegistryExcludesUnstableMethods(t *testing.T) {
	for _, method := range []string{
		"providers/list", "session/fork", "nes/start", "mcp/connect",
		"mcp/message", "mcp/disconnect", "document/didOpen",
	} {
		if _, exists := Methods[method]; exists {
			t.Fatalf("unstable method %q leaked into stable registry", method)
		}
	}
}
