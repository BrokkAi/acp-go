package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
)

// Declaration is an encoded ACP MCP server declaration and the name it
// declares.
type Declaration struct {
	Name string
	JSON json.RawMessage
}

// AppendDeclarations appends declarations to the mcpServers array of a raw
// session setup params object. Every other member, including fields the
// generated types do not model, is kept byte-for-byte.
//
// With uniqueNames set (ACP v2), it fails with -32602 and leaves params
// untouched when mcpServers already declares one of the names: names must be
// unique within mcpServers and the agent would reject the request.
func AppendDeclarations(params json.RawMessage, declarations []Declaration, uniqueNames bool) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalidSetup("session setup parameters must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, invalidSetup("session setup parameters must be an object: " + err.Error())
	}
	var servers []json.RawMessage
	if raw := bytes.TrimSpace(fields["mcpServers"]); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, invalidSetup("session setup mcpServers must be an array")
		}
	}
	if uniqueNames {
		if err := checkUniqueNames(servers, declarations); err != nil {
			return nil, err
		}
	}
	if servers == nil {
		servers = make([]json.RawMessage, 0, len(declarations))
	}
	for _, declaration := range declarations {
		servers = append(servers, declaration.JSON)
	}
	encoded, err := json.Marshal(servers)
	if err != nil {
		return nil, err
	}
	fields["mcpServers"] = encoded
	return json.Marshal(fields)
}

// CheckUniqueNames fails with -32602 when an attached server's name is
// already declared in servers or by another attached server.
func checkUniqueNames(servers []json.RawMessage, declarations []Declaration) error {
	taken := map[string]bool{}
	for _, server := range servers {
		var declared struct {
			Name *string `json:"name"`
		}
		if json.Unmarshal(server, &declared) == nil && declared.Name != nil {
			taken[*declared.Name] = true
		}
	}
	for _, declaration := range declarations {
		if taken[declaration.Name] {
			return invalidSetup(fmt.Sprintf("MCP server `%s` is attached here, so mcpServers cannot declare another server with that name; names must be unique within mcpServers", declaration.Name))
		}
		taken[declaration.Name] = true
	}
	return nil
}

// CheckDeclaredNames applies the v2 name-uniqueness rule to typed requests:
// names lists the names already in mcpServers.
func CheckDeclaredNames(names []string, declarations []Declaration) error {
	servers := make([]json.RawMessage, 0, len(names))
	for _, name := range names {
		encoded, err := json.Marshal(map[string]string{"name": name})
		if err != nil {
			return err
		}
		servers = append(servers, encoded)
	}
	return checkUniqueNames(servers, declarations)
}

func invalidSetup(detail string) *acp.RPCError {
	return rpcError(-32602, "Invalid params", detail)
}
