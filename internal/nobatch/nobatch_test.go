package nobatch

import (
	"encoding/json"
	"testing"
)

func TestIsArray(t *testing.T) {
	for frame, want := range map[string]bool{
		`[]`:                  true,
		" \t[1, 2, 3]\r\n":    true,
		`[{"jsonrpc":"2.0"}]`: true,
		`[`:                   false,
		`[1,`:                 false,
		"[\"\xff\"]":          false,
		`{"jsonrpc":"2.0"}`:   false,
		`17`:                  false,
		``:                    false,
	} {
		if got := IsArray([]byte(frame)); got != want {
			t.Errorf("IsArray(%q) = %v, want %v", frame, got, want)
		}
	}
}

func TestRejectionMatchesReferenceWireShape(t *testing.T) {
	var response struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(Rejection(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Version != "2.0" || string(response.ID) != "null" ||
		response.Error.Code != Code || response.Error.Message != Message {
		t.Fatalf("rejection = %s", Rejection())
	}
	var data string
	if err := json.Unmarshal(response.Error.Data, &data); err != nil || data != Reason {
		t.Fatalf("rejection data = %s", response.Error.Data)
	}
	first := Rejection()
	first[0] = 'x'
	if Rejection()[0] != '{' {
		t.Fatal("Rejection returned shared storage")
	}
}
