package unstable

import "testing"

func TestUnstableFeatureMethodsAreImportOptIn(t *testing.T) {
	required := []string{
		"providers/list", "providers/set", "providers/disable",
		"session/fork", "nes/start", "nes/suggest", "nes/accept",
		"nes/reject", "nes/close", "mcp/connect", "mcp/message",
		"mcp/disconnect", "document/didOpen",
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
}
