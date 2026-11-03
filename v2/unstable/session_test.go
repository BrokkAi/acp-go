package unstable_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	stable "github.com/BrokkAi/acp-go/schema/v2"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/agent"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

func archiveFilter(value schema.SessionArchiveFilter) *schema.SessionArchiveFilter { return &value }

type rawRequest struct {
	method string
	params json.RawMessage
}

// rawPeer runs a v2 connection whose peer records each request and answers
// with the raw JSON result respond returns, so tests can send results the
// typed schema could not produce.
func rawPeer(t *testing.T, respond func(method string) string) (*acpv2.Connection, chan rawRequest) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	connection := acpv2.Connect(local, local, nil, nil)
	requests := make(chan rawRequest, 16)
	go func() {
		decoder := json.NewDecoder(peer)
		encoder := json.NewEncoder(peer)
		for {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := decoder.Decode(&request); err != nil {
				return
			}
			if request.Method == "initialize" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": json.RawMessage(`{"protocolVersion":2,"info":{"name":"agent","version":"1"},"capabilities":{"session":{}}}`)})
				continue
			}
			requests <- rawRequest{method: request.Method, params: request.Params}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": json.RawMessage(respond(request.Method))})
		}
	}()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = connection.Close()
	})
	if _, err := connection.Initialize(context.Background(), acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
	return connection, requests
}

func sessionInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{
		Archive: &schema.SessionArchiveCapabilities{},
		Status:  &schema.SessionStatusCapabilities{},
		Title:   &schema.SessionTitleCapabilities{},
	}}}
}

func TestSessionFacadesFailBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, requests := rawPeer(t, func(string) string { return `{}` })
	baseline := schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}}}
	calls := map[string]func(schema.InitializeResponse) error{
		"archive": func(initialization schema.InitializeResponse) error {
			_, err := acpunstable.ArchiveSession(ctx, connection, initialization, schema.ArchiveSessionRequest{SessionID: "s"})
			return err
		},
		"unarchive": func(initialization schema.InitializeResponse) error {
			_, err := acpunstable.UnarchiveSession(ctx, connection, initialization, schema.UnarchiveSessionRequest{SessionID: "s"})
			return err
		},
		"status": func(initialization schema.InitializeResponse) error {
			_, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "s"})
			return err
		},
		"set title": func(initialization schema.InitializeResponse) error {
			_, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "s", Title: "t"})
			return err
		},
		"archived list": func(initialization schema.InitializeResponse) error {
			_, err := acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{Archived: archiveFilter(schema.SessionArchiveFilterOnly)})
			return err
		},
	}
	for name, call := range calls {
		for _, initialization := range []schema.InitializeResponse{{}, baseline} {
			if err := call(initialization); err == nil || !strings.Contains(err.Error(), "did not advertise") {
				t.Fatalf("%s without its capability: %v", name, err)
			}
		}
	}
	if _, err := acpunstable.ListSessions(ctx, connection, schema.InitializeResponse{}, schema.ListSessionsRequest{}); err == nil {
		t.Fatal("session/list without the session capabilities was allowed")
	}

	initialization := sessionInitialization()
	relative := schema.AbsolutePath("relative")
	for name, err := range map[string]error{
		"blank archive session": func() error {
			_, err := acpunstable.ArchiveSession(ctx, connection, initialization, schema.ArchiveSessionRequest{SessionID: " "})
			return err
		}(),
		"blank unarchive session": func() error {
			_, err := acpunstable.UnarchiveSession(ctx, connection, initialization, schema.UnarchiveSessionRequest{})
			return err
		}(),
		"blank status session": func() error {
			_, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{})
			return err
		}(),
		"blank title session": func() error {
			_, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{Title: "t"})
			return err
		}(),
		"blank title": func() error {
			_, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "s"})
			return err
		}(),
		"relative list cwd": func() error {
			_, err := acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{Cwd: &relative})
			return err
		}(),
	} {
		if err == nil {
			t.Fatalf("%s was allowed", name)
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("a refused call wrote %s to the wire", request.method)
	default:
	}
}

func TestSessionStatusStateAndOpenStatus(t *testing.T) {
	ctx := context.Background()
	results := make(chan string, 1)
	connection, requests := rawPeer(t, func(string) string { return <-results })
	initialization := sessionInitialization()
	for _, test := range []struct {
		name, result string
		status       schema.SessionStatus
		state        schema.StateUpdateKind // empty: no state reported
	}{
		{"absent", `{"status":"saved"}`, schema.SessionStatusSaved, ""},
		{"null", `{"status":"open","state":null}`, schema.SessionStatusOpen, ""},
		{"malformed", `{"status":"open","state":"running"}`, schema.SessionStatusOpen, ""},
		{"malformed shape", `{"status":"open","state":[1]}`, schema.SessionStatusOpen, ""},
		{"running", `{"status":"open","state":{"state":"running"}}`, schema.SessionStatusOpen, schema.StateUpdateKindRunning},
		{"requires action", `{"status":"open","state":{"state":"requires_action"}}`, schema.SessionStatusOpen, schema.StateUpdateKindRequiresAction},
		{"idle", `{"status":"open","state":{"state":"idle","stopReason":"end_turn"}}`, schema.SessionStatusOpen, schema.StateUpdateKindIdle},
		{"unknown status", `{"status":"hibernating"}`, "hibernating", ""},
	} {
		results <- test.result
		response, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "s"})
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if request := <-requests; request.method != schema.SessionStatusMethodName || string(request.params) != `{"sessionId":"s"}` {
			t.Fatalf("%s: frame = %s %s", test.name, request.method, request.params)
		}
		if response.Status != test.status {
			t.Fatalf("%s: status = %q", test.name, response.Status)
		}
		switch {
		case test.state == "" && response.State != nil:
			t.Fatalf("%s: state = %+v, want none reported", test.name, response.State)
		case test.state != "" && (response.State == nil || response.State.Kind != test.state):
			t.Fatalf("%s: state = %+v, want %s", test.name, response.State, test.state)
		}
	}
}

func TestSessionFacadesEncodeArchiveFilterAndTitle(t *testing.T) {
	ctx := context.Background()
	connection, requests := rawPeer(t, func(method string) string {
		switch method {
		case schema.SessionListMethodName:
			return `{"sessions":[{"sessionId":"a","cwd":"/","archived":true},{"sessionId":"b","cwd":"/","archived":false},{"sessionId":"c","cwd":"/","archived":null}]}`
		case schema.SessionSetTitleMethodName:
			return `{"title":"Stored"}`
		}
		return `{}`
	})
	initialization := sessionInitialization()
	for _, value := range []*schema.SessionArchiveFilter{nil, archiveFilter(schema.SessionArchiveFilterExclude), archiveFilter(schema.SessionArchiveFilterInclude), archiveFilter(schema.SessionArchiveFilterOnly), archiveFilter("_vendor_recent")} {
		cwd := schema.AbsolutePath("/repo")
		listed, err := acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{Cwd: &cwd, Archived: value})
		if err != nil {
			t.Fatal(err)
		}
		sessions := listed.Sessions
		if len(sessions) != 3 || !sessions[0].Archived.Set || !sessions[0].Archived.Value || sessions[1].Archived.Value || sessions[1].Archived.Null || !sessions[2].Archived.Null {
			t.Fatalf("listed sessions = %+v", sessions)
		}
		want := `{"cwd":"/repo"}`
		if value != nil {
			want = `{"cwd":"/repo","archived":"` + string(*value) + `"}`
		}
		if request := <-requests; request.method != schema.SessionListMethodName || string(request.params) != want {
			t.Fatalf("session/list frame = %s %s, want %s", request.method, request.params, want)
		}
	}
	for method, call := range map[string]func() error{
		schema.SessionArchiveMethodName: func() error {
			_, err := acpunstable.ArchiveSession(ctx, connection, initialization, schema.ArchiveSessionRequest{SessionID: "s"})
			return err
		},
		schema.SessionUnarchiveMethodName: func() error {
			_, err := acpunstable.UnarchiveSession(ctx, connection, initialization, schema.UnarchiveSessionRequest{SessionID: "s"})
			return err
		},
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
		if request := <-requests; request.method != method || string(request.params) != `{"sessionId":"s"}` {
			t.Fatalf("%s frame = %s %s", method, request.method, request.params)
		}
	}
	title, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "s", Title: "Requested"})
	if err != nil || title.Title != "Stored" {
		t.Fatalf("set_title = %+v, %v", title, err)
	}
	if request := <-requests; request.method != schema.SessionSetTitleMethodName || string(request.params) != `{"sessionId":"s","title":"Requested"}` {
		t.Fatalf("session/set_title frame = %s %s", request.method, request.params)
	}
}

// sessionStore is a draft-v2 agent serving archive, status, and title over an
// in-memory session table.
type sessionStore struct {
	*acptest.V2Agent
	mu       sync.Mutex
	archived map[schema.SessionId]bool
	open     map[schema.SessionId]bool
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		V2Agent:  &acptest.V2Agent{},
		archived: map[schema.SessionId]bool{"live": false, "old": true},
		open:     map[schema.SessionId]bool{"live": true},
	}
}

func (a *sessionStore) known(id schema.SessionId) error {
	if _, ok := a.archived[id]; !ok {
		return acpv2.SessionNotFoundError(stable.SessionId(id))
	}
	return nil
}

func (a *sessionStore) ArchiveSession(_ context.Context, request schema.ArchiveSessionRequest) (schema.ArchiveSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.known(request.SessionID); err != nil {
		return schema.ArchiveSessionResponse{}, err
	}
	a.archived[request.SessionID] = true
	delete(a.open, request.SessionID)
	return schema.ArchiveSessionResponse{}, nil
}

func (a *sessionStore) UnarchiveSession(_ context.Context, request schema.UnarchiveSessionRequest) (schema.UnarchiveSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.known(request.SessionID); err != nil {
		return schema.UnarchiveSessionResponse{}, err
	}
	a.archived[request.SessionID] = false
	return schema.UnarchiveSessionResponse{}, nil
}

func (a *sessionStore) ListSessionsWithArchive(_ context.Context, request schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	want := func(bool) bool { return true }
	switch {
	case request.Archived == nil || *request.Archived == schema.SessionArchiveFilterExclude:
		want = func(archived bool) bool { return !archived }
	case *request.Archived == schema.SessionArchiveFilterOnly:
		want = func(archived bool) bool { return archived }
	case *request.Archived != schema.SessionArchiveFilterInclude:
		return schema.ListSessionsResponse{}, &acp.RPCError{Code: -32602, Message: "unsupported archived filter"}
	}
	response := schema.ListSessionsResponse{Sessions: []schema.SessionInfo{}}
	for _, id := range []schema.SessionId{"live", "old"} {
		if archived := a.archived[id]; want(archived) {
			response.Sessions = append(response.Sessions, schema.SessionInfo{SessionID: id, Cwd: "/", Archived: schema.Nullable[bool]{Set: true, Value: archived}})
		}
	}
	return response, nil
}

func (a *sessionStore) SessionStatus(_ context.Context, request schema.SessionStatusRequest) (schema.SessionStatusResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.known(request.SessionID); err != nil {
		return schema.SessionStatusResponse{}, err
	}
	if a.open[request.SessionID] {
		return schema.SessionStatusResponse{Status: schema.SessionStatusOpen, State: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}}, nil
	}
	return schema.SessionStatusResponse{Status: schema.SessionStatusSaved}, nil
}

func (a *sessionStore) SetSessionTitle(_ context.Context, request schema.SetSessionTitleRequest) (schema.SetSessionTitleResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.known(request.SessionID); err != nil {
		return schema.SetSessionTitleResponse{}, err
	}
	return schema.SetSessionTitleResponse{Title: strings.TrimSpace(request.Title)}, nil
}

// serveSessions runs the v2 runtime with Install, as the package
// documentation describes.
func serveSessions(t *testing.T, implementation agent.Agent) *acpv2.Connection {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	runtime := agent.New(implementation)
	if err := acpunstable.Install(runtime, implementation); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.B, pair.B) }()
	client := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = client.Close()
		pair.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return client
}

func TestSessionMethodsRoundTripThroughTheRuntime(t *testing.T) {
	ctx := context.Background()
	connection := serveSessions(t, newSessionStore())
	initialization := initialize(t, connection)
	session := initialization.Capabilities.Session
	if session.Archive == nil || session.Status == nil || session.Title == nil {
		t.Fatalf("advertised session capabilities = %+v", session)
	}

	status, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "live"})
	if err != nil || status.Status != schema.SessionStatusOpen || status.State == nil || status.State.Kind != schema.StateUpdateKindRunning {
		t.Fatalf("live status = %+v, %v", status, err)
	}
	list := func(value *schema.SessionArchiveFilter) string {
		response, err := acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{Archived: value})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, session := range response.Sessions {
			ids = append(ids, string(session.SessionID))
		}
		return strings.Join(ids, ",")
	}
	if got := list(nil); got != "live" {
		t.Fatalf("default list = %s", got)
	}
	if _, err := acpunstable.ArchiveSession(ctx, connection, initialization, schema.ArchiveSessionRequest{SessionID: "live"}); err != nil {
		t.Fatal(err)
	}
	status, err = acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "live"})
	if err != nil || status.Status != schema.SessionStatusSaved || status.State != nil {
		t.Fatalf("archived status = %+v, %v", status, err)
	}
	if got := list(archiveFilter(schema.SessionArchiveFilterOnly)); got != "live,old" {
		t.Fatalf("only list = %s", got)
	}
	if _, err := acpunstable.UnarchiveSession(ctx, connection, initialization, schema.UnarchiveSessionRequest{SessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	if got := list(archiveFilter(schema.SessionArchiveFilterInclude)); got != "live,old" {
		t.Fatalf("include list = %s", got)
	}
	if got := list(archiveFilter(schema.SessionArchiveFilterExclude)); got != "old" {
		t.Fatalf("exclude list = %s", got)
	}
	_, err = acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{Archived: archiveFilter("_vendor_recent")})
	if rpc := rpcError(t, err); rpc.Code != -32602 {
		t.Fatalf("unsupported filter error = %+v", rpc)
	}
	title, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "old", Title: " Investigate "})
	if err != nil || title.Title != "Investigate" {
		t.Fatalf("set_title = %+v, %v", title, err)
	}

	for name, call := range map[string]func() error{
		"archive": func() error {
			_, err := acpunstable.ArchiveSession(ctx, connection, initialization, schema.ArchiveSessionRequest{SessionID: "missing"})
			return err
		},
		"unarchive": func() error {
			_, err := acpunstable.UnarchiveSession(ctx, connection, initialization, schema.UnarchiveSessionRequest{SessionID: "missing"})
			return err
		},
		"status": func() error {
			_, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "missing"})
			return err
		},
		"set title": func() error {
			_, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "missing", Title: "t"})
			return err
		},
	} {
		err := call()
		rpc := rpcError(t, err)
		var data schema.SessionNotFoundErrorData
		if !acp.IsResourceNotFound(err) || rpc.Message != "Resource not found" || json.Unmarshal(rpc.Data, &data) != nil || data.SessionID != "missing" {
			t.Fatalf("%s of an unknown session = %+v (data %s)", name, rpc, rpc.Data)
		}
		var typed schema.Error
		if err := json.Unmarshal([]byte(`{"code":-32002,"message":"Resource not found","data":`+string(rpc.Data)+`}`), &typed); err != nil || typed.Data == nil || typed.Data.SessionNotFound == nil || typed.Data.SessionNotFound.SessionID != "missing" {
			t.Fatalf("%s error data does not decode as SessionNotFoundErrorData: %+v, %v", name, typed.Data, err)
		}
		if data, ok := acpv2.SessionNotFoundData(err); !ok || data.SessionID != "missing" {
			t.Fatalf("%s: SessionNotFoundData = %+v, %v", name, data, ok)
		}
	}
}

// statusOnly implements the baseline and session/status only.
type statusOnly struct {
	*acptest.V2Agent
	acpunstable.StatusHandler
}

func TestSessionHandlersAdvertiseAndFallThrough(t *testing.T) {
	ctx := context.Background()
	connection := serveSessions(t, statusOnly{&acptest.V2Agent{}, newSessionStore()})
	initialization := initialize(t, connection)
	session := initialization.Capabilities.Session
	if session.Status == nil || session.Archive != nil || session.Title != nil {
		t.Fatalf("status-only agent advertised %+v", session)
	}
	for _, method := range []string{schema.SessionArchiveMethodName, schema.SessionUnarchiveMethodName, schema.SessionSetTitleMethodName} {
		rpc := rpcError(t, connection.Call(ctx, method, map[string]any{"sessionId": "live", "title": "t"}, nil))
		if rpc.Code != -32601 || string(rpc.Data) != `"`+method+`"` {
			t.Fatalf("%s without its handler = %+v (data %s)", method, rpc, rpc.Data)
		}
	}
	// Without an ArchiveHandler the runtime keeps serving session/list from
	// the stable Agent.
	if listOverride(t, statusOnly{}) != nil {
		t.Fatal("Install overrode session/list for an agent without ArchiveHandler")
	}
	if _, err := connection.ListSessions(ctx, acpv2.Initialization{Capabilities: &stable.AgentCapabilities{Session: &stable.SessionCapabilities{}}}, stable.ListSessionsRequest{}); err != nil {
		t.Fatalf("stable session/list: %v", err)
	}

	// The baseline is never advertised on the agent's behalf.
	hook := acpunstable.Advertise(newSessionStore())
	extended, err := hook(stable.InitializeResponse{ProtocolVersion: 2, Info: stable.Implementation{Name: "a", Version: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if response := extended.(schema.InitializeResponse); response.Capabilities != nil && response.Capabilities.Session != nil {
		t.Fatalf("Advertise added session capabilities to an agent without the baseline: %+v", response.Capabilities)
	}
}

type badSessionResponses struct{}

func (badSessionResponses) SessionStatus(context.Context, schema.SessionStatusRequest) (schema.SessionStatusResponse, error) {
	return schema.SessionStatusResponse{}, nil
}

func (badSessionResponses) SetSessionTitle(context.Context, schema.SetSessionTitleRequest) (schema.SetSessionTitleResponse, error) {
	return schema.SetSessionTitleResponse{}, nil
}

func (badSessionResponses) ArchiveSession(context.Context, schema.ArchiveSessionRequest) (schema.ArchiveSessionResponse, error) {
	return schema.ArchiveSessionResponse{}, nil
}

func (badSessionResponses) UnarchiveSession(context.Context, schema.UnarchiveSessionRequest) (schema.UnarchiveSessionResponse, error) {
	return schema.UnarchiveSessionResponse{}, nil
}

func (badSessionResponses) ListSessionsWithArchive(context.Context, schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	return schema.ListSessionsResponse{Sessions: []schema.SessionInfo{{SessionID: "s", Cwd: "/", Archived: schema.Nullable[bool]{Set: true, Null: true}}}}, nil
}

func TestSessionHandlersCheckResponses(t *testing.T) {
	ctx := context.Background()
	handler := acpunstable.Handle(nil, badSessionResponses{})
	for method, params := range map[string]string{
		schema.SessionStatusMethodName:   `{"sessionId":"s"}`,
		schema.SessionSetTitleMethodName: `{"sessionId":"s","title":"t"}`,
	} {
		// The reference SDK sends status and title responses as returned.
		response, err := handler(ctx, method, json.RawMessage(params))
		if err != nil {
			t.Fatalf("%s empty response = %v", method, err)
		}
		if encoded, err := json.Marshal(response); err != nil || (string(encoded) != `{"status":""}` && string(encoded) != `{"title":""}`) {
			t.Fatalf("%s empty response = %s, %v", method, encoded, err)
		}
	}
	_, err := listOverride(t, badSessionResponses{})(ctx, schema.SessionListMethodName, json.RawMessage(`{}`))
	if rpc := rpcError(t, err); rpc.Code != -32603 || !strings.Contains(string(rpc.Data), "archive state") {
		t.Fatalf("list entry with null archived = %+v (data %s)", rpc, rpc.Data)
	}
	_, err = handler(ctx, schema.SessionArchiveMethodName, json.RawMessage(`{"sessionId":7}`))
	if rpc := rpcError(t, err); rpc.Code != -32602 || !strings.Contains(string(rpc.Data), `"phase":"deserialization"`) {
		t.Fatalf("malformed archive params = %+v (data %s)", rpc, rpc.Data)
	}
}

// listOverride returns the session/list override Install sets for handler.
func listOverride(t *testing.T, handler any) acp.Handler {
	t.Helper()
	runtime := agent.New(&acptest.V2Agent{})
	if err := acpunstable.Install(runtime, handler); err != nil {
		t.Fatal(err)
	}
	return runtime.Overrides[schema.SessionListMethodName]
}
