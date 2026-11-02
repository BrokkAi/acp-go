package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type greetInput struct {
	Name string `json:"name"`
}

type greetOutput struct {
	Greeting string `json:"greeting"`
}

var greetSchema = json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)

func greetTool() Tool {
	return NewTool("greet", "Greet someone", greetSchema, func(_ context.Context, input greetInput, _ *RequestContext) (string, error) {
		return "Hello, " + input.Name + "!", nil
	})
}

func modern(params map[string]any) json.RawMessage {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{MetaProtocolVersion: ProtocolVersion, MetaClientCapabilities: map[string]any{}}
	encoded, _ := json.Marshal(params)
	return encoded
}

func execute(t *testing.T, server *Server, method string, params json.RawMessage) (map[string]any, *Error) {
	t.Helper()
	outcome, err := server.Service().Execute(context.Background(), Request{Method: method, Params: params}, &RequestContext{standalone: true})
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	if outcome.Error != nil {
		return nil, outcome.Error
	}
	var result map[string]any
	if err := json.Unmarshal(outcome.Result, &result); err != nil {
		t.Fatalf("%s: result %s: %v", method, outcome.Result, err)
	}
	return result, nil
}

func TestToolRegistryOrderReplacementAndFiltering(t *testing.T) {
	registry := NewToolRegistry()
	for _, name := range []string{"a", "b", "c"} {
		tool := greetTool()
		tool.Name = name
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	replacement := greetTool()
	replacement.Name, replacement.Description = "a", "replaced"
	if err := registry.Register(replacement); err != nil {
		t.Fatal(err)
	}
	names := func(tools []Tool) string {
		var out []string
		for _, tool := range tools {
			out = append(out, tool.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(registry.Tools()); got != "a,b,c" {
		t.Fatalf("tools = %s", got)
	}
	if tool, _ := registry.Tool("a"); tool.Description != "replaced" {
		t.Fatalf("replacement = %+v", tool)
	}
	if err := registry.DisableTool("b"); err != nil {
		t.Fatal(err)
	}
	if got := names(registry.EnabledTools()); got != "a,c" {
		t.Fatalf("deny list = %s", got)
	}
	if _, ok := registry.EnabledTool("b"); ok || !registry.ContainsTool("b") {
		t.Fatal("a disabled tool must stay registered but not enabled")
	}
	registry.DisableAllTools()
	if err := registry.EnableTool("c"); err != nil {
		t.Fatal(err)
	}
	if got := names(registry.EnabledTools()); got != "c" {
		t.Fatalf("allow list = %s", got)
	}
	registry.EnableAllTools()
	if got := names(registry.EnabledTools()); got != "a,b,c" {
		t.Fatalf("enable all = %s", got)
	}
	var unknown *UnknownToolError
	if err := registry.DisableTool("ech"); !errors.As(err, &unknown) || unknown.Name != "ech" || err.Error() != "unknown MCP tool `ech`" {
		t.Fatalf("unknown tool error = %v", err)
	}
	if err := registry.EnableTool("ech"); !errors.As(err, &unknown) {
		t.Fatalf("unknown tool error = %v", err)
	}
}

func TestToolRegistryRejectsInvalidTools(t *testing.T) {
	registry := NewToolRegistry()
	handler := greetTool().Handler
	for name, tool := range map[string]Tool{
		"no name":            {Handler: handler},
		"no handler":         {Name: "x"},
		"array input":        {Name: "x", Handler: handler, InputSchema: json.RawMessage(`[]`)},
		"string input type":  {Name: "x", Handler: handler, InputSchema: json.RawMessage(`{"type":"string"}`)},
		"scalar output type": {Name: "x", Handler: handler, OutputSchema: json.RawMessage(`{"type":"string"}`)},
	} {
		if err := registry.Register(tool); err == nil {
			t.Errorf("%s: registered", name)
		}
	}
	if err := registry.Register(Tool{Name: "x", Handler: handler}); err != nil {
		t.Fatal(err)
	}
	if tool, _ := registry.Tool("x"); string(tool.InputSchema) != `{"type":"object"}` {
		t.Fatalf("default input schema = %s", tool.InputSchema)
	}
}

func TestToolServerListsAndDiscovers(t *testing.T) {
	registry := NewToolRegistry()
	registry.SetInstructions("be nice")
	tool := greetTool()
	tool.Title = "Greeter"
	tool.OutputSchema = json.RawMessage(`{"type":"object"}`)
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	hidden := greetTool()
	hidden.Name = "hidden"
	_ = registry.Register(hidden)
	_ = registry.DisableTool("hidden")
	server := NewToolServer("named", "1.2.3", registry)

	list, _ := execute(t, server, "tools/list", modern(nil))
	tools := list["tools"].([]any)
	descriptor := tools[0].(map[string]any)
	if len(tools) != 1 || descriptor["name"] != "greet" || descriptor["title"] != "Greeter" || descriptor["outputSchema"] == nil {
		t.Fatalf("tools/list = %v", list)
	}
	if list["resultType"] != "complete" || list["ttlMs"] != float64(0) || list["cacheScope"] != "private" {
		t.Fatalf("modern cache metadata = %v", list)
	}
	legacy, _ := execute(t, server, "tools/list", json.RawMessage(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}`))
	if _, ok := legacy["ttlMs"]; ok {
		t.Fatalf("legacy tools/list carried modern metadata: %v", legacy)
	}

	discovery, _ := execute(t, server, "server/discover", modern(nil))
	info := discovery["_meta"].(map[string]any)[MetaServerInfo].(map[string]any)
	if info["name"] != "named" || info["version"] != "1.2.3" || discovery["instructions"] != "be nice" || discovery["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("discovery = %v", discovery)
	}
	if _, invalid := execute(t, server, "server/discover", json.RawMessage(`{}`)); invalid == nil || invalid.Code != codeInvalidParams {
		t.Fatalf("discovery without metadata = %v", invalid)
	}
	if _, invalid := execute(t, server, "tools/list", json.RawMessage(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01"}}`)); invalid == nil || invalid.Code != CodeUnsupportedProtocolVersion {
		t.Fatalf("unsupported version = %v", invalid)
	}

	initialized, _ := execute(t, NewToolServer("plain", "", NewToolRegistry()), "initialize", json.RawMessage(`{"protocolVersion":"2025-06-18"}`))
	if initialized["protocolVersion"] != "2025-06-18" || initialized["serverInfo"].(map[string]any)["version"] != DefaultServerVersion {
		t.Fatalf("initialize = %v", initialized)
	}
	if result, invalid := execute(t, server, "ping", nil); invalid != nil || result == nil {
		t.Fatalf("legacy ping = %v, %v", result, invalid)
	}
	if _, invalid := execute(t, server, "ping", modern(nil)); invalid == nil || invalid.Code != codeMethodNotFound {
		t.Fatalf("modern ping = %v", invalid)
	}
	if _, invalid := execute(t, server, "resources/list", modern(nil)); invalid == nil || invalid.Code != codeMethodNotFound {
		t.Fatalf("unknown method = %v", invalid)
	}
}

func TestToolServerCallSemantics(t *testing.T) {
	registry := NewToolRegistry()
	_ = registry.Register(greetTool())
	structured := NewTool("structured", "", greetSchema, func(_ context.Context, input greetInput, _ *RequestContext) (greetOutput, error) {
		return greetOutput{Greeting: "hi " + input.Name}, nil
	})
	structured.OutputSchema = json.RawMessage(`{"type":"object","properties":{"greeting":{"type":"string"}}}`)
	_ = registry.Register(structured)
	_ = registry.Register(NewTool("fail", "", nil, func(context.Context, map[string]any, *RequestContext) (any, error) {
		return nil, errors.New("tool exploded")
	}))
	_ = registry.Register(Tool{Name: "explicit", Handler: func(context.Context, json.RawMessage, *RequestContext) (any, error) {
		return &ToolResult{Content: []any{TextContent("a"), TextContent("b")}, IsError: true}, nil
	}})
	_ = registry.Register(Tool{Name: "cancelled", Handler: func(ctx context.Context, _ json.RawMessage, _ *RequestContext) (any, error) {
		return nil, ctx.Err()
	}})
	_ = registry.Register(greetTool())
	_ = registry.DisableTool("explicit")
	server := NewToolServer("tools", "", registry)
	call := func(name string, arguments any) (map[string]any, *Error) {
		return execute(t, server, "tools/call", modern(map[string]any{"name": name, "arguments": arguments}))
	}
	text := func(result map[string]any) string {
		return result["content"].([]any)[0].(map[string]any)["text"].(string)
	}

	result, _ := call("greet", map[string]any{"name": "Bob"})
	if text(result) != "Hello, Bob!" || result["isError"] != false || result["resultType"] != "complete" {
		t.Fatalf("greet = %v", result)
	}
	result, _ = call("structured", map[string]any{"name": "Al"})
	if result["structuredContent"].(map[string]any)["greeting"] != "hi Al" || text(result) != `{"greeting":"hi Al"}` {
		t.Fatalf("structured = %v", result)
	}
	result, _ = call("fail", nil)
	if result["isError"] != true || !strings.Contains(text(result), "tool exploded") {
		t.Fatalf("tool error = %v", result)
	}
	for _, arguments := range []any{map[string]any{"wrong": 1}, map[string]any{"name": 7}, nil} {
		result, invalid := call("greet", arguments)
		if invalid != nil || result["isError"] != true || !strings.Contains(text(result), "name") {
			t.Fatalf("bad arguments %v = %v, %v", arguments, result, invalid)
		}
	}
	for _, name := range []string{"missing", "explicit"} {
		if _, invalid := call(name, nil); invalid == nil || invalid.Code != codeInvalidParams {
			t.Fatalf("%s = %v", name, invalid)
		}
	}
	if _, invalid := call("greet", "not-an-object"); invalid == nil || invalid.Code != codeInvalidParams {
		t.Fatalf("non-object arguments = %v", invalid)
	}
	if _, invalid := execute(t, server, "tools/call", modern(nil)); invalid == nil || invalid.Code != codeInvalidParams {
		t.Fatalf("missing name = %v", invalid)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := server.Service().Execute(ctx, Request{Method: "tools/call", Params: modern(map[string]any{"name": "cancelled"})}, &RequestContext{})
	if err != nil || outcome.Error == nil || outcome.Error.Code != codeInternalError {
		t.Fatalf("cancelled call = %+v, %v", outcome, err)
	}

	_ = registry.EnableTool("explicit")
	result, _ = call("explicit", nil)
	if result["isError"] != true || len(result["content"].([]any)) != 2 {
		t.Fatalf("explicit result = %v", result)
	}
}

func TestValidateSchemaKeywords(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{
		"count":{"type":"integer"},
		"mode":{"enum":["a","b"]},
		"tags":{"type":"array","items":{"type":"string"}},
		"either":{"anyOf":[{"type":"string"},{"type":"null"}]},
		"one":{"oneOf":[{"type":"number"},{"type":"integer"}]}
	},"required":["count"],"additionalProperties":false}`)
	for arguments, valid := range map[string]bool{
		`{"count":3}`:                  true,
		`{"count":3.0}`:                true,
		`{"count":3.5}`:                false,
		`{"count":"3"}`:                false,
		`{}`:                           false,
		`{"count":1,"extra":true}`:     false,
		`{"count":1,"mode":"a"}`:       true,
		`{"count":1,"mode":"c"}`:       false,
		`{"count":1,"tags":["x"]}`:     true,
		`{"count":1,"tags":["x",2]}`:   false,
		`{"count":1,"either":null}`:    true,
		`{"count":1,"either":2}`:       false,
		`{"count":1,"one":1.5}`:        true,
		`{"count":1,"one":2}`:          false,
		`{"count":1e400000000}`:        true,
		`{"count":1,"mode":["a","b"]}`: false,
	} {
		err := validateSchema(schema, json.RawMessage(arguments))
		if (err == nil) != valid {
			t.Errorf("%s: err = %v, want valid=%v", arguments, err, valid)
		}
	}
}
