package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// ToolHandler runs one tools/call. arguments is the call's arguments object
// ({} when the call had none) and has already been checked against the tool's
// input schema.
//
// The returned value becomes the call's result: a ToolResult (or *ToolResult)
// is sent as given; otherwise the value is encoded as JSON and becomes
// structured content when the tool declares an output schema, or a single
// text content block (a string verbatim, anything else as JSON) when it does
// not. A returned error becomes a result with isError set and the error text
// as its content, so the model can see and correct it.
type ToolHandler func(ctx context.Context, arguments json.RawMessage, rc *RequestContext) (any, error)

// Tool describes one MCP tool. Schemas are JSON Schema objects supplied by
// the caller; no schema is generated.
type Tool struct {
	// Name identifies the tool in tools/call. It is required.
	Name string
	// Title is an optional human-readable title.
	Title string
	// Description tells the model what the tool does.
	Description string
	// InputSchema is the JSON Schema object for the call arguments. Empty
	// means {"type":"object"}; a non-empty schema must be an object whose type
	// is "object".
	InputSchema json.RawMessage
	// OutputSchema is an optional JSON Schema object for structured output.
	// When set, its type must be "object" and results carry
	// structuredContent.
	OutputSchema json.RawMessage
	// Handler runs the tool. It is required.
	Handler ToolHandler
}

// HasStructuredOutput reports whether the tool declares an output schema.
func (t Tool) HasStructuredOutput() bool { return len(t.OutputSchema) > 0 }

// NewTool returns a tool whose handler decodes the arguments into In and
// encodes the Out it returns. Arguments that do not match inputSchema, or do
// not decode into In, become an isError result without calling fn.
func NewTool[In, Out any](name, description string, inputSchema json.RawMessage, fn func(ctx context.Context, input In, rc *RequestContext) (Out, error)) Tool {
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: inputSchema,
		Handler: func(ctx context.Context, arguments json.RawMessage, rc *RequestContext) (any, error) {
			var input In
			if err := json.Unmarshal(arguments, &input); err != nil {
				return nil, &argumentsError{err: err}
			}
			return fn(ctx, input, rc)
		},
	}
}

// argumentsError marks arguments that did not decode into the tool's input
// type.
type argumentsError struct{ err error }

func (e *argumentsError) Error() string { return "invalid tool arguments: " + e.err.Error() }
func (e *argumentsError) Unwrap() error { return e.err }

// ToolResult is an explicit tools/call result.
type ToolResult struct {
	// Content holds MCP content blocks; see TextContent.
	Content []any
	// StructuredContent is optional structured output.
	StructuredContent any
	// IsError marks a tool execution error the model can act on.
	IsError bool
}

// TextContent returns an MCP text content block.
func TextContent(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// UnknownToolError reports that ToolRegistry.EnableTool or DisableTool named
// a tool that is not registered. It is a local configuration error, not a
// JSON-RPC error.
type UnknownToolError struct {
	Name string
}

func (e *UnknownToolError) Error() string { return fmt.Sprintf("unknown MCP tool `%s`", e.Name) }

// ToolRegistry is a concurrency-safe set of tools with allow/deny filtering
// and server instructions. Registering a tool whose name is taken replaces it
// in place. Every tool is enabled by default (a deny list); after
// DisableAllTools only explicitly enabled tools are (an allow list).
type ToolRegistry struct {
	mu           sync.RWMutex
	instructions string
	order        []string
	tools        map[string]Tool
	allowList    bool
	listed       map[string]bool
}

// NewToolRegistry returns an empty registry with every tool enabled.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: map[string]Tool{}, listed: map[string]bool{}}
}

// SetInstructions sets the server instructions reported to clients.
func (r *ToolRegistry) SetInstructions(instructions string) {
	r.mu.Lock()
	r.instructions = instructions
	r.mu.Unlock()
}

// Instructions returns the server instructions.
func (r *ToolRegistry) Instructions() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.instructions
}

// Register adds a tool, replacing any registered tool with the same name
// while keeping its position. It rejects a tool without a name or handler,
// and schemas that are not JSON objects of type "object".
func (r *ToolRegistry) Register(tool Tool) error {
	if strings.TrimSpace(tool.Name) == "" {
		return errors.New("mcp tool requires a name")
	}
	if tool.Handler == nil {
		return fmt.Errorf("mcp tool %q requires a handler", tool.Name)
	}
	if len(tool.InputSchema) == 0 {
		tool.InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	if err := checkObjectSchema(tool.InputSchema); err != nil {
		return fmt.Errorf("mcp tool %q input schema: %w", tool.Name, err)
	}
	if len(tool.OutputSchema) > 0 {
		if err := checkObjectSchema(tool.OutputSchema); err != nil {
			return fmt.Errorf("mcp tool %q output schema: %w", tool.Name, err)
		}
	}
	tool.InputSchema = slices.Clone(tool.InputSchema)
	tool.OutputSchema = slices.Clone(tool.OutputSchema)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[tool.Name]; !exists {
		r.order = append(r.order, tool.Name)
	}
	r.tools[tool.Name] = tool
	return nil
}

func checkObjectSchema(schema json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil || fields == nil {
		return errors.New("must be a JSON object")
	}
	var kind string
	if json.Unmarshal(fields["type"], &kind) != nil || kind != "object" {
		return errors.New(`must have "type": "object"`)
	}
	return nil
}

// Tools returns every registered tool in registration order.
func (r *ToolRegistry) Tools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tools := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		tools = append(tools, r.tools[name])
	}
	return tools
}

// EnabledTools returns the enabled tools in registration order.
func (r *ToolRegistry) EnabledTools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tools := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		if r.enabledLocked(name) {
			tools = append(tools, r.tools[name])
		}
	}
	return tools
}

// Tool returns a registered tool by name, even if it is disabled.
func (r *ToolRegistry) Tool(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

// EnabledTool returns an enabled tool by name.
func (r *ToolRegistry) EnabledTool(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	if !ok || !r.enabledLocked(name) {
		return Tool{}, false
	}
	return tool, true
}

// ContainsTool reports whether a tool is registered, enabled or not.
func (r *ToolRegistry) ContainsTool(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

func (r *ToolRegistry) enabledLocked(name string) bool {
	return r.listed[name] == r.allowList
}

// DisableAllTools switches to an allow list: only tools enabled afterwards
// with EnableTool are available.
func (r *ToolRegistry) DisableAllTools() {
	r.mu.Lock()
	r.allowList = true
	r.listed = map[string]bool{}
	r.mu.Unlock()
}

// EnableAllTools switches to a deny list: every tool is available except
// those disabled afterwards with DisableTool.
func (r *ToolRegistry) EnableAllTools() {
	r.mu.Lock()
	r.allowList = false
	r.listed = map[string]bool{}
	r.mu.Unlock()
}

// EnableTool enables one registered tool. It returns *UnknownToolError when
// no tool has that name.
func (r *ToolRegistry) EnableTool(name string) error {
	return r.setEnabled(name, true)
}

// DisableTool disables one registered tool. It returns *UnknownToolError
// when no tool has that name.
func (r *ToolRegistry) DisableTool(name string) error {
	return r.setEnabled(name, false)
}

func (r *ToolRegistry) setEnabled(name string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return &UnknownToolError{Name: name}
	}
	if enabled == r.allowList {
		r.listed[name] = true
	} else {
		delete(r.listed, name)
	}
	return nil
}

// NewToolServer returns a server whose service answers MCP requests from the
// registry: server/discover, tools/list, tools/call, and, for standalone
// clients on revisions before 2026-07-28, initialize and ping. serverInfo
// reports name and version; an empty version reports DefaultServerVersion.
//
// Tool calls follow the reference SDK: an unknown or disabled tool is an
// invalid-params MCP error (-32602); arguments that do not match the input
// schema and errors returned by the tool are results with isError set; a
// cancelled call is an internal MCP error (-32603).
func NewToolServer(name, version string, registry *ToolRegistry) *Server {
	if version == "" {
		version = DefaultServerVersion
	}
	if registry == nil {
		registry = NewToolRegistry()
	}
	return NewServer(name, &toolService{name: name, version: version, registry: registry})
}

type toolService struct {
	name     string
	version  string
	registry *ToolRegistry
}

type requestMeta struct {
	version      string
	hasVersion   bool
	capabilities json.RawMessage
}

func readMeta(params json.RawMessage) (requestMeta, *Error) {
	var meta requestMeta
	raw := metaOf(params)
	if raw == nil {
		return meta, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return meta, nil
	}
	if value, ok := fields[MetaProtocolVersion]; ok {
		meta.hasVersion = true
		if json.Unmarshal(value, &meta.version) != nil {
			return meta, invalidParams("request _meta has a malformed " + MetaProtocolVersion)
		}
	}
	meta.capabilities = fields[MetaClientCapabilities]
	return meta, nil
}

func (s *toolService) Execute(ctx context.Context, request Request, rc *RequestContext) (Outcome, error) {
	if len(request.Params) > 0 && request.Params[0] != '{' {
		return Outcome{Error: invalidParams("params must be an object")}, nil
	}
	meta, invalid := readMeta(request.Params)
	if invalid != nil {
		return Outcome{Error: invalid}, nil
	}
	inline := request.Method != "initialize"
	if inline && meta.hasVersion && !slices.Contains(knownProtocolVersions, meta.version) {
		return Outcome{Error: unsupportedVersion(meta.version, knownProtocolVersions)}, nil
	}
	modern := request.Method == "server/discover" || (meta.hasVersion && meta.version >= ProtocolVersion)
	if inline && modern {
		var missing []string
		if !meta.hasVersion {
			missing = append(missing, MetaProtocolVersion)
		}
		if len(meta.capabilities) == 0 || meta.capabilities[0] != '{' {
			missing = append(missing, MetaClientCapabilities)
		}
		if len(missing) > 0 {
			return Outcome{Error: invalidParams("request _meta is missing or has malformed required fields: " + strings.Join(missing, ", "))}, nil
		}
	}
	switch request.Method {
	case "initialize":
		return s.initialize(request.Params)
	case "ping":
		if modern {
			return Outcome{Error: methodNotFound("ping")}, nil
		}
		return ResultOutcome(struct{}{})
	case "server/discover":
		return s.discover()
	case "tools/list":
		return s.listTools(modern)
	case "tools/call":
		return s.callTool(ctx, request.Params, rc, modern)
	default:
		return Outcome{Error: methodNotFound(request.Method)}, nil
	}
}

func (s *toolService) serverInfo() map[string]string {
	return map[string]string{"name": s.name, "version": s.version}
}

func (s *toolService) capabilities() map[string]any {
	return map[string]any{"tools": map[string]any{}}
}

func (s *toolService) initialize(params json.RawMessage) (Outcome, error) {
	var request struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &request)
	}
	version := LatestLegacyProtocolVersion
	if request.ProtocolVersion != ProtocolVersion && slices.Contains(knownProtocolVersions, request.ProtocolVersion) {
		version = request.ProtocolVersion
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    s.capabilities(),
		"serverInfo":      s.serverInfo(),
	}
	if instructions := s.registry.Instructions(); instructions != "" {
		result["instructions"] = instructions
	}
	return ResultOutcome(result)
}

func (s *toolService) discover() (Outcome, error) {
	result := map[string]any{
		"resultType":        "complete",
		"supportedVersions": knownProtocolVersions,
		"capabilities":      s.capabilities(),
		"ttlMs":             0,
		"cacheScope":        "private",
		"_meta":             map[string]any{MetaServerInfo: s.serverInfo()},
	}
	if instructions := s.registry.Instructions(); instructions != "" {
		result["instructions"] = instructions
	}
	return ResultOutcome(result)
}

type toolDescriptor struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

func (s *toolService) listTools(modern bool) (Outcome, error) {
	tools := s.registry.EnabledTools()
	descriptors := make([]toolDescriptor, len(tools))
	for i, tool := range tools {
		descriptors[i] = toolDescriptor{
			Name:         tool.Name,
			Title:        tool.Title,
			Description:  tool.Description,
			InputSchema:  tool.InputSchema,
			OutputSchema: tool.OutputSchema,
		}
	}
	result := map[string]any{"tools": descriptors}
	if modern {
		// Modern MCP requires cache metadata. Do not share catalogs across
		// authorization contexts or promise a cache lifetime.
		result["resultType"] = "complete"
		result["ttlMs"] = 0
		result["cacheScope"] = "private"
	}
	return ResultOutcome(result)
}

type callResult struct {
	ResultType        string          `json:"resultType,omitempty"`
	Content           []any           `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
}

func (s *toolService) callTool(ctx context.Context, params json.RawMessage, rc *RequestContext, modern bool) (Outcome, error) {
	var call struct {
		Name      *string         `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) == 0 || json.Unmarshal(params, &call) != nil || call.Name == nil {
		return Outcome{Error: invalidParams("tools/call requires a tool name")}, nil
	}
	arguments := bytes.TrimSpace(call.Arguments)
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	} else if arguments[0] != '{' {
		return Outcome{Error: invalidParams("tools/call arguments must be an object")}, nil
	}
	tool, ok := s.registry.EnabledTool(*call.Name)
	if !ok {
		return Outcome{Error: invalidParams(fmt.Sprintf("tool `%s` not found", *call.Name))}, nil
	}
	resultType := ""
	if modern {
		resultType = "complete"
	}
	toolError := func(text string) (Outcome, error) {
		return ResultOutcome(callResult{ResultType: resultType, Content: []any{TextContent(text)}, IsError: true})
	}
	if err := validateSchema(tool.InputSchema, arguments); err != nil {
		return toolError(err.Error())
	}
	value, err := tool.Handler(ctx, arguments, rc)
	if ctx.Err() != nil {
		return Outcome{Error: newError(codeInternalError, "operation cancelled", nil)}, nil
	}
	if err != nil {
		return toolError(err.Error())
	}
	result, err := s.toolResult(tool, value, resultType)
	if err != nil {
		return toolError(err.Error())
	}
	return ResultOutcome(result)
}

func (s *toolService) toolResult(tool Tool, value any, resultType string) (callResult, error) {
	switch explicit := value.(type) {
	case *ToolResult:
		if explicit == nil {
			break
		}
		return explicitResult(*explicit, resultType)
	case ToolResult:
		return explicitResult(explicit, resultType)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return callResult{}, fmt.Errorf("encode tool output: %w", err)
	}
	if tool.HasStructuredOutput() {
		return callResult{ResultType: resultType, Content: []any{TextContent(string(encoded))}, StructuredContent: encoded}, nil
	}
	text := string(encoded)
	if s, ok := value.(string); ok {
		text = s
	}
	return callResult{ResultType: resultType, Content: []any{TextContent(text)}}, nil
}

func explicitResult(result ToolResult, resultType string) (callResult, error) {
	out := callResult{ResultType: resultType, Content: result.Content, IsError: result.IsError}
	if out.Content == nil {
		out.Content = []any{}
	}
	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return callResult{}, fmt.Errorf("encode structured content: %w", err)
		}
		out.StructuredContent = encoded
	}
	return out, nil
}
