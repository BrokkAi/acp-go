package unstable_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	stable "github.com/BrokkAi/acp-go/schema/v2"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/agent"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

func baselineInitialize() stable.InitializeResponse {
	return stable.InitializeResponse{
		ProtocolVersion: 2,
		Info:            stable.Implementation{Name: "a", Version: "1"},
		Capabilities:    &stable.AgentCapabilities{Session: &stable.SessionCapabilities{}},
	}
}

func TestOnlyInstallAdvertisesArchive(t *testing.T) {
	extended, err := acpunstable.Advertise(newSessionStore())(baselineInitialize())
	if err != nil {
		t.Fatal(err)
	}
	if session := extended.(schema.InitializeResponse).Capabilities.Session; session.Archive != nil || session.Status == nil || session.Title == nil {
		t.Fatalf("Advertise session capabilities = %+v", session)
	}

	runtime := agent.New(newSessionStore())
	if err := acpunstable.Install(runtime, runtime.Agent); err != nil {
		t.Fatal(err)
	}
	extended, err = runtime.InitializeResult(baselineInitialize())
	if err != nil {
		t.Fatal(err)
	}
	if session := extended.(schema.InitializeResponse).Capabilities.Session; session.Archive == nil {
		t.Fatalf("Install session capabilities = %+v", session)
	}
	if runtime.Overrides[schema.SessionListMethodName] == nil || runtime.Extensions == nil {
		t.Fatalf("Install left the runtime without its overrides or extensions: %+v", runtime)
	}
}

func TestInstallRefusesConflictsAndChainsExtensions(t *testing.T) {
	ctx := context.Background()
	if err := acpunstable.Install(nil, newSessionStore()); err == nil {
		t.Fatal("Install accepted a nil runtime")
	}

	runtime := agent.New(newSessionStore())
	if err := acpunstable.Install(runtime, runtime.Agent); err != nil {
		t.Fatal(err)
	}
	if err := acpunstable.Install(runtime, runtime.Agent); err == nil || !strings.Contains(err.Error(), "InitializeResult") {
		t.Fatalf("second Install = %v", err)
	}

	existing := func(context.Context, string, json.RawMessage) (any, error) { return "mine", nil }
	runtime = agent.New(newSessionStore())
	runtime.Overrides = map[string]acp.Handler{schema.SessionListMethodName: existing}
	if err := acpunstable.Install(runtime, runtime.Agent); err == nil || !strings.Contains(err.Error(), schema.SessionListMethodName) {
		t.Fatalf("Install over an existing session/list override = %v", err)
	}
	if runtime.InitializeResult != nil || runtime.Extensions != nil || len(runtime.Overrides) != 1 {
		t.Fatalf("failed Install changed the runtime: %+v", runtime)
	}

	// An override for another method is kept, and Extensions already set is
	// chained behind Handle.
	runtime = agent.New(newSessionStore())
	runtime.Overrides = map[string]acp.Handler{"_vendor/override": existing}
	runtime.Extensions = func(_ context.Context, method string, _ json.RawMessage) (any, error) { return method, nil }
	if err := acpunstable.Install(runtime, runtime.Agent); err != nil {
		t.Fatal(err)
	}
	if runtime.Overrides["_vendor/override"] == nil || runtime.Overrides[schema.SessionListMethodName] == nil {
		t.Fatalf("Install overrides = %v", runtime.Overrides)
	}
	if served, err := runtime.Extensions(ctx, "_vendor/next", nil); err != nil || served != "_vendor/next" {
		t.Fatalf("chained extension = %v, %v", served, err)
	}
	if _, err := runtime.Extensions(ctx, schema.SessionStatusMethodName, json.RawMessage(`{"sessionId":"live"}`)); err != nil {
		t.Fatalf("installed session/status: %v", err)
	}
}

func TestSessionFacadesRequireStatusAndTitle(t *testing.T) {
	ctx := context.Background()
	results := make(chan string, 1)
	connection, requests := rawPeer(t, func(string) string { return <-results })
	initialization := sessionInitialization()
	for _, result := range []string{`{}`, `{"status":null}`} {
		results <- result
		if _, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "s"}); err == nil || !strings.Contains(err.Error(), "status") {
			t.Fatalf("status response %s = %v", result, err)
		}
		<-requests
	}
	for _, result := range []string{`{}`, `{"title":null}`} {
		results <- result
		if _, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "s", Title: "t"}); err == nil || !strings.Contains(err.Error(), "title") {
			t.Fatalf("title response %s = %v", result, err)
		}
		<-requests
	}
}

func TestMalformedArchivedReadsAsNotReported(t *testing.T) {
	var info schema.SessionInfo
	if err := json.Unmarshal([]byte(`{"sessionId":"s","cwd":"/","archived":"yes","title":7}`), &info); err != nil {
		t.Fatal(err)
	}
	if info.Archived.Set || info.Title.Set || info.SessionID != "s" {
		t.Fatalf("SessionInfo = %+v", info)
	}
	var list schema.ListSessionsResponse
	if err := json.Unmarshal([]byte(`{"sessions":[{"sessionId":"a","cwd":"/","archived":{}},{"sessionId":"b","cwd":"/","archived":true}]}`), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 2 || list.Sessions[0].Archived.Set || !list.Sessions[1].Archived.Value {
		t.Fatalf("listed sessions = %+v", list.Sessions)
	}
}

func TestSessionNotFoundErrorShape(t *testing.T) {
	err := acpv2.SessionNotFoundError("s")
	encoded, _ := json.Marshal(err)
	if string(encoded) != `{"code":-32002,"message":"Resource not found","data":{"sessionId":"s"}}` {
		t.Fatalf("SessionNotFoundError = %s", encoded)
	}
	if data, ok := acpv2.SessionNotFoundData(err); !ok || data.SessionID != "s" {
		t.Fatalf("SessionNotFoundData = %+v, %v", data, ok)
	}
	if _, ok := acpv2.SessionNotFoundData(&acp.RPCError{Code: -32002, Message: "Resource not found", Data: json.RawMessage(`{"uri":"file:///x"}`)}); ok {
		t.Fatal("SessionNotFoundData accepted a resource error for another resource")
	}
	if _, ok := acpv2.SessionNotFoundData(&acp.RPCError{Code: -32603, Data: json.RawMessage(`{"sessionId":"s"}`)}); ok {
		t.Fatal("SessionNotFoundData accepted another error code")
	}
}
