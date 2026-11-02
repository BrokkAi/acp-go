package schema

import "testing"

func TestStableRegistryExcludesUnstableMethods(t *testing.T) {
	for _, method := range []string{
		"providers/list", "session/fork", "session/archive", "mcp/message",
		"auth/status", "proxy/initialize", "proxy/successor",
	} {
		if _, exists := Methods[method]; exists {
			t.Fatalf("unstable method %q leaked into stable registry", method)
		}
	}
}
