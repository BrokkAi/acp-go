package unstable_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	stable "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

func listingInitialize() stable.InitializeResponse {
	return stable.InitializeResponse{
		ProtocolVersion:   acp.Version,
		AgentCapabilities: &stable.AgentCapabilities{SessionCapabilities: &stable.SessionCapabilities{List: &stable.SessionListCapabilities{}}},
	}
}

func TestOnlyInstallAdvertisesArchive(t *testing.T) {
	extended, err := acpunstable.Advertise(newSessionStore())(listingInitialize())
	if err != nil {
		t.Fatal(err)
	}
	if session := extended.(schema.InitializeResponse).AgentCapabilities.SessionCapabilities; session.Archive != nil || session.Status == nil || session.Title == nil {
		t.Fatalf("Advertise session capabilities = %+v", session)
	}

	runtime := agent.New(newSessionStore())
	if err := acpunstable.Install(runtime, runtime.Agent); err != nil {
		t.Fatal(err)
	}
	extended, err = runtime.InitializeResult(listingInitialize())
	if err != nil {
		t.Fatal(err)
	}
	if session := extended.(schema.InitializeResponse).AgentCapabilities.SessionCapabilities; session.Archive == nil {
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
	results := make(chan any, 1)
	connection, requests := fixture(t, func(string) any { return <-results })
	initialization := sessionInitialization()
	for _, result := range []map[string]any{{}, {"status": nil}} {
		results <- result
		if _, err := acpunstable.SessionStatus(ctx, connection, initialization, schema.SessionStatusRequest{SessionID: "s"}); err == nil || !strings.Contains(err.Error(), "status") {
			t.Fatalf("status response %v = %v", result, err)
		}
		<-requests
	}
	for _, result := range []map[string]any{{}, {"title": nil}} {
		results <- result
		if _, err := acpunstable.SetSessionTitle(ctx, connection, initialization, schema.SetSessionTitleRequest{SessionID: "s", Title: "t"}); err == nil || !strings.Contains(err.Error(), "title") {
			t.Fatalf("title response %v = %v", result, err)
		}
		<-requests
	}
}

func TestMalformedArchivedReadsAsNotReported(t *testing.T) {
	var info schema.SessionInfo
	if err := json.Unmarshal([]byte(`{"sessionId":"s","cwd":"/","archived":"yes","title":7}`), &info); err != nil {
		t.Fatal(err)
	}
	if info.Archived != nil || info.Title != nil || info.SessionID != "s" {
		t.Fatalf("SessionInfo = %+v", info)
	}
	var list schema.ListSessionsResponse
	if err := json.Unmarshal([]byte(`{"sessions":[{"sessionId":"a","cwd":"/","archived":{}},{"sessionId":"b","cwd":"/","archived":true}]}`), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 2 || list.Sessions[0].Archived != nil || list.Sessions[1].Archived == nil || !*list.Sessions[1].Archived {
		t.Fatalf("listed sessions = %+v", list.Sessions)
	}
}

func TestSessionNotFoundErrorShape(t *testing.T) {
	encoded, _ := json.Marshal(acp.SessionNotFoundError("s"))
	if string(encoded) != `{"code":-32002,"message":"Resource not found","data":{"sessionId":"s"}}` {
		t.Fatalf("SessionNotFoundError = %s", encoded)
	}
}
