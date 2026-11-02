package unstable

import "testing"

func TestUnstableFeatureMethodsAreImportOptIn(t *testing.T) {
	required := []string{
		"providers/list", "providers/set", "providers/disable",
		"session/fork", "session/archive", "session/unarchive",
		"session/status", "session/set_title", "auth/status",
		"auth/status_update", "mcp/message", "proxy/initialize",
		"proxy/successor",
	}
	for _, method := range required {
		if _, ok := Methods[method]; !ok {
			t.Errorf("method %q missing from unstable registry", method)
		}
	}
	if Methods["mcp/message"].Side != SideBoth {
		t.Fatalf("mcp/message side = %q", Methods["mcp/message"].Side)
	}
	if Methods["mcp/message"].NotificationParams == nil {
		t.Fatal("mcp/message does not retain its distinct notification params")
	}
	for _, method := range []string{"proxy/initialize", "proxy/successor"} {
		if Methods[method].Side != SideProxy {
			t.Fatalf("%s side = %q", method, Methods[method].Side)
		}
	}
	if Methods["proxy/successor"].NotificationParams == nil {
		t.Fatal("proxy/successor does not retain its distinct notification params")
	}
}

// Next Edit Suggestions left the unstable schema in schema crate 1.15.
func TestNextEditSuggestionsAreGone(t *testing.T) {
	for _, method := range []string{
		"nes/start", "nes/suggest", "nes/accept", "nes/reject", "nes/close",
		"document/didOpen", "document/didChange", "document/didClose",
		"document/didSave", "document/didFocus",
	} {
		if _, exists := Methods[method]; exists {
			t.Errorf("removed method %q is still registered", method)
		}
	}
}
