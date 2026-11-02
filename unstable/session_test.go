package unstable_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	stable "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

func sessionInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		SessionCapabilities: &schema.SessionCapabilities{
			List:    &schema.SessionListCapabilities{},
			Archive: &schema.SessionArchiveCapabilities{},
			Status:  &schema.SessionStatusCapabilities{},
			Title:   &schema.SessionTitleCapabilities{},
		},
	}}
}

func filter(value schema.SessionArchiveFilter) *schema.SessionArchiveFilter { return &value }

func requireNothingWritten(t *testing.T, requests chan recorded) {
	t.Helper()
	select {
	case request := <-requests:
		t.Fatalf("a refused call wrote %s to the wire", request.method)
	default:
	}
}

func TestSessionFacadesGateBeforeTheWire(t *testing.T) {
	ctx := context.Background()
	connection, requests := fixture(t, func(string) any { return map[string]any{} })
	listOnly := schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		SessionCapabilities: &schema.SessionCapabilities{List: &schema.SessionListCapabilities{}},
	}}
	relative := "relative"
	for name, call := range map[string]func(schema.InitializeResponse) error{
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
		"list": func(initialization schema.InitializeResponse) error {
			_, err := acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{})
			return err
		},
	} {
		if err := call(schema.InitializeResponse{}); err == nil || !strings.Contains(err.Error(), "did not advertise") {
			t.Fatalf("%s without its capability: %v", name, err)
		}
		if name != "list" {
			if err := call(listOnly); err == nil {
				t.Fatalf("%s with only sessionCapabilities.list was allowed", name)
			}
		}
	}
	if _, err := acpunstable.ListSessions(ctx, connection, listOnly, schema.ListSessionsRequest{Archived: filter(schema.SessionArchiveFilterInclude)}); err == nil || !strings.Contains(err.Error(), "archived filter") {
		t.Fatalf("archived filter without the archive capability: %v", err)
	}

	initialization := sessionInitialization()
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
			_, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "s", Title: " \t"})
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
	requireNothingWritten(t, requests)
}

func TestSessionFacadesEncodeAndPreserveOpenEnums(t *testing.T) {
	ctx := context.Background()
	archivedTrue, archivedFalse := true, false
	connection, requests := fixture(t, func(method string) any {
		switch method {
		case schema.SessionStatusMethodName:
			return map[string]any{"status": "paused", "state": "not a v1 field"}
		case schema.SessionSetTitleMethodName:
			return schema.SetSessionTitleResponse{Title: "Normalized"}
		case schema.SessionListMethodName:
			return schema.ListSessionsResponse{Sessions: []schema.SessionInfo{
				{SessionID: "a", Cwd: "/", Archived: &archivedTrue},
				{SessionID: "b", Cwd: "/", Archived: &archivedFalse},
				{SessionID: "c", Cwd: "/"},
			}}
		}
		return map[string]any{}
	})
	initialization := sessionInitialization()

	if _, err := acpunstable.ArchiveSession(ctx, connection, initialization, schema.ArchiveSessionRequest{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != schema.SessionArchiveMethodName || string(request.params) != `{"sessionId":"s"}` {
		t.Fatalf("session/archive frame = %s %s", request.method, request.params)
	}
	if _, err := acpunstable.UnarchiveSession(ctx, connection, initialization, schema.UnarchiveSessionRequest{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != schema.SessionUnarchiveMethodName || string(request.params) != `{"sessionId":"s"}` {
		t.Fatalf("session/unarchive frame = %s %s", request.method, request.params)
	}

	status, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	// An unknown status decodes and is preserved; callers treat it as saved.
	if status.Status != "paused" || status.Status == schema.SessionStatusOpen {
		t.Fatalf("status = %q", status.Status)
	}
	if request := <-requests; request.method != schema.SessionStatusMethodName || string(request.params) != `{"sessionId":"s"}` {
		t.Fatalf("session/status frame = %s %s", request.method, request.params)
	}
	encoded, _ := json.Marshal(status)
	if string(encoded) != `{"status":"paused"}` {
		t.Fatalf("status re-encodes as %s", encoded)
	}

	title, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "s", Title: "  Normalized  "})
	if err != nil || title.Title != "Normalized" {
		t.Fatalf("set_title = %+v, %v", title, err)
	}
	if request := <-requests; request.method != schema.SessionSetTitleMethodName || string(request.params) != `{"sessionId":"s","title":"  Normalized  "}` {
		t.Fatalf("session/set_title frame = %s %s", request.method, request.params)
	}

	for _, value := range []*schema.SessionArchiveFilter{nil, filter(schema.SessionArchiveFilterExclude), filter(schema.SessionArchiveFilterInclude), filter(schema.SessionArchiveFilterOnly), filter("_vendor_pinned")} {
		listed, err := acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{Archived: value})
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Sessions) != 3 || listed.Sessions[0].Archived == nil || !*listed.Sessions[0].Archived || listed.Sessions[1].Archived == nil || *listed.Sessions[1].Archived || listed.Sessions[2].Archived != nil {
			t.Fatalf("listed sessions = %+v", listed.Sessions)
		}
		request := <-requests
		want := `{}`
		if value != nil {
			want = `{"archived":"` + string(*value) + `"}`
		}
		if request.method != schema.SessionListMethodName || string(request.params) != want {
			t.Fatalf("session/list frame = %s %s, want %s", request.method, request.params, want)
		}
	}
	var decoded schema.ListSessionsRequest
	if err := json.Unmarshal([]byte(`{"archived":"_vendor_pinned"}`), &decoded); err != nil || decoded.Archived == nil || *decoded.Archived != "_vendor_pinned" {
		t.Fatalf("unknown archived filter decodes as %+v, %v", decoded.Archived, err)
	}
}

// sessionStore is a v1 agent that serves archive, status, and title over an
// in-memory session table, and advertises session/list itself, as the
// archive capability requires.
type sessionStore struct {
	*acptest.V1Agent
	mu       sync.Mutex
	archived map[schema.SessionId]bool
	open     map[schema.SessionId]bool
	titles   map[schema.SessionId]string
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		V1Agent:  &acptest.V1Agent{},
		archived: map[schema.SessionId]bool{"live": false, "old": true},
		open:     map[schema.SessionId]bool{"live": true},
		titles:   map[schema.SessionId]string{},
	}
}

func (a *sessionStore) Initialize(ctx context.Context, client agent.Client, request stable.InitializeRequest) (stable.InitializeResponse, error) {
	response, err := a.V1Agent.Initialize(ctx, client, request)
	response.AgentCapabilities = &stable.AgentCapabilities{SessionCapabilities: &stable.SessionCapabilities{List: &stable.SessionListCapabilities{}}}
	return response, err
}

func (a *sessionStore) ArchiveSession(_ context.Context, request schema.ArchiveSessionRequest) (schema.ArchiveSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.archived[request.SessionID]; !ok {
		return schema.ArchiveSessionResponse{}, acp.SessionNotFoundError(stable.SessionId(request.SessionID))
	}
	a.archived[request.SessionID] = true
	delete(a.open, request.SessionID)
	return schema.ArchiveSessionResponse{}, nil
}

func (a *sessionStore) UnarchiveSession(_ context.Context, request schema.UnarchiveSessionRequest) (schema.UnarchiveSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.archived[request.SessionID]; !ok {
		return schema.UnarchiveSessionResponse{}, acp.SessionNotFoundError(stable.SessionId(request.SessionID))
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
			response.Sessions = append(response.Sessions, schema.SessionInfo{SessionID: id, Cwd: "/", Archived: &archived})
		}
	}
	return response, nil
}

func (a *sessionStore) SessionStatus(_ context.Context, request schema.SessionStatusRequest) (schema.SessionStatusResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.archived[request.SessionID]; !ok {
		return schema.SessionStatusResponse{}, acp.SessionNotFoundError(stable.SessionId(request.SessionID))
	}
	if a.open[request.SessionID] {
		return schema.SessionStatusResponse{Status: schema.SessionStatusOpen}, nil
	}
	return schema.SessionStatusResponse{Status: schema.SessionStatusSaved}, nil
}

func (a *sessionStore) SetSessionTitle(_ context.Context, request schema.SetSessionTitleRequest) (schema.SetSessionTitleResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.archived[request.SessionID]; !ok {
		return schema.SetSessionTitleResponse{}, acp.SessionNotFoundError(stable.SessionId(request.SessionID))
	}
	a.titles[request.SessionID] = strings.TrimSpace(request.Title)
	return schema.SetSessionTitleResponse{Title: a.titles[request.SessionID]}, nil
}

// serveSessions runs the v1 runtime with Install, as the package
// documentation describes.
func serveSessions(t *testing.T, implementation agent.Agent) *acp.Connection {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	runtime := agent.New(implementation)
	if err := acpunstable.Install(runtime, implementation); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.A, pair.A) }()
	client := acp.Connect(pair.B, pair.B, nil, nil)
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

func listedIDs(t *testing.T, response schema.ListSessionsResponse, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, session := range response.Sessions {
		ids = append(ids, string(session.SessionID))
	}
	return strings.Join(ids, ",")
}

func TestRuntimeServesSessionArchiveStatusAndTitle(t *testing.T) {
	ctx := context.Background()
	implementation := newSessionStore()
	connection := serveSessions(t, implementation)
	initialization := initializeUnstable(t, connection)
	session := initialization.AgentCapabilities.SessionCapabilities
	if session == nil || session.List == nil || session.Archive == nil || session.Status == nil || session.Title == nil {
		t.Fatalf("advertised session capabilities = %+v", session)
	}

	status, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "live"})
	if err != nil || status.Status != schema.SessionStatusOpen {
		t.Fatalf("live status = %+v, %v", status, err)
	}
	list := func(value *schema.SessionArchiveFilter) string {
		response, err := acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{Archived: value})
		return listedIDs(t, response, err)
	}
	if got := list(nil); got != "live" {
		t.Fatalf("default list = %s", got)
	}
	if _, err := acpunstable.ArchiveSession(ctx, connection, initialization, schema.ArchiveSessionRequest{SessionID: "live"}); err != nil {
		t.Fatal(err)
	}
	// Archiving closes the session, so it reads as saved afterwards, and a
	// repeat archive succeeds.
	if status, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "live"}); err != nil || status.Status != schema.SessionStatusSaved {
		t.Fatalf("archived status = %+v, %v", status, err)
	}
	if _, err := acpunstable.ArchiveSession(ctx, connection, initialization, schema.ArchiveSessionRequest{SessionID: "live"}); err != nil {
		t.Fatalf("repeat archive: %v", err)
	}
	if got := list(filter(schema.SessionArchiveFilterExclude)); got != "" {
		t.Fatalf("exclude list = %s", got)
	}
	if got := list(filter(schema.SessionArchiveFilterOnly)); got != "live,old" {
		t.Fatalf("only list = %s", got)
	}
	if _, err := acpunstable.UnarchiveSession(ctx, connection, initialization, schema.UnarchiveSessionRequest{SessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	if got := list(filter(schema.SessionArchiveFilterInclude)); got != "live,old" {
		t.Fatalf("include list = %s", got)
	}
	if got := list(filter(schema.SessionArchiveFilterOnly)); got != "live" {
		t.Fatalf("only list after unarchive = %s", got)
	}
	_, err = acpunstable.ListSessions(ctx, connection, initialization, schema.ListSessionsRequest{Archived: filter("_vendor")})
	if rpc := rpcError(t, err); rpc.Code != -32602 {
		t.Fatalf("unsupported filter error = %+v", rpc)
	}

	title, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "old", Title: "  Investigate  "})
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
		if !acp.IsResourceNotFound(err) || rpc.Message != "Resource not found" || string(rpc.Data) != `{"sessionId":"missing"}` {
			t.Fatalf("%s of an unknown session = %+v (data %s)", name, rpc, rpc.Data)
		}
	}
}

// archiveWithoutList implements archive but advertises no session/list.
type archiveWithoutList struct {
	*acptest.V1Agent
	acpunstable.ArchiveHandler
}

func TestArchiveNeedsSessionList(t *testing.T) {
	ctx := context.Background()
	implementation := archiveWithoutList{&acptest.V1Agent{}, newSessionStore()}
	connection := serveSessions(t, implementation)
	initialization := initializeUnstable(t, connection)
	if capabilities := initialization.AgentCapabilities; capabilities != nil && capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.Archive != nil {
		t.Fatal("archive advertised without sessionCapabilities.list")
	}
	// The session/list override keeps the runtime's sessionCapabilities.list
	// gate.
	rpc := rpcError(t, connection.Call(ctx, schema.SessionListMethodName, schema.ListSessionsRequest{}, nil))
	if rpc.Code != -32601 {
		t.Fatalf("session/list without the list capability = %+v", rpc)
	}
	// Methods the handler does not implement fall through to -32601.
	rpc = rpcError(t, connection.Call(ctx, schema.SessionStatusMethodName, schema.SessionStatusRequest{SessionID: "live"}, nil))
	if rpc.Code != -32601 || string(rpc.Data) != `"session/status"` {
		t.Fatalf("session/status without a StatusHandler = %+v (data %s)", rpc, rpc.Data)
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
	return schema.ListSessionsResponse{Sessions: []schema.SessionInfo{{SessionID: "s", Cwd: "/"}}}, nil
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
	list := listOverride(t, badSessionResponses{})
	_, err := list(ctx, schema.SessionListMethodName, json.RawMessage(`{}`))
	if rpc := rpcError(t, err); rpc.Code != -32603 || !strings.Contains(string(rpc.Data), "archive state") {
		t.Fatalf("list entry without archived = %+v (data %s)", rpc, rpc.Data)
	}
	_, err = list(ctx, schema.SessionListMethodName, json.RawMessage(`{"archived":7}`))
	if rpc := rpcError(t, err); rpc.Code != -32602 {
		t.Fatalf("malformed archived filter = %+v", rpc)
	}
	if listOverride(t, newUnstableAgent()) != nil {
		t.Fatal("Install overrode session/list for an agent without ArchiveHandler")
	}
}

// listOverride returns the session/list override Install sets for handler.
func listOverride(t *testing.T, handler any) acp.Handler {
	t.Helper()
	runtime := agent.New(&acptest.V1Agent{})
	if err := acpunstable.Install(runtime, handler); err != nil {
		t.Fatal(err)
	}
	return runtime.Overrides[schema.SessionListMethodName]
}
