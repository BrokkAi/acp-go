package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

// The runtime's SessionUpdater refuses notices and compactions the client
// did not advertise before writing them, and delivers them once advertised.
func TestSessionUpdaterGatesNoticesAndCompactionOnClientCapabilities(t *testing.T) {
	gated := []schema.SessionUpdate{
		{Notice: &schema.Notice{Severity: schema.NoticeSeverityWarning, Title: "Provider degraded"}},
		{CompactionUpdate: &schema.CompactionUpdate{CompactionID: "c1", Status: schema.CompactionStatusInProgress}},
		{CompactionSummaryChunk: &schema.CompactionSummaryChunk{CompactionID: "c1", Content: acp.NewTextContent("kept")}},
	}
	kinds := []schema.SessionUpdateKind{
		schema.SessionUpdateKindNotice, schema.SessionUpdateKindCompactionUpdate, schema.SessionUpdateKindCompactionSummaryChunk,
	}
	for _, test := range []struct {
		name         string
		capabilities acp.Capabilities
		allowed      []bool
	}{
		{"none", acp.Capabilities{}, []bool{false, false, false}},
		{"notices", acp.Capabilities{Session: acp.SessionNoticesClientCapabilities()}, []bool{true, false, false}},
		{"compaction", acp.Capabilities{Session: acp.SessionCompactionClientCapabilities()}, []bool{false, true, true}},
		{"both", acp.Capabilities{Session: acp.SessionClientCapabilities(
			acp.SessionNoticesClientCapabilities(), acp.SessionCompactionClientCapabilities(),
		)}, []bool{true, true, true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := make([]error, len(gated))
			prompt := func(_ context.Context, _ Client, _ schema.PromptRequest, updates SessionUpdater) (schema.PromptResponse, error) {
				for i, update := range gated {
					results[i] = updates.Update(update)
				}
				return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
			}
			projection := acp.NewSessionProjection()
			var received []schema.SessionUpdateKind
			connection := startRuntime(t, testAgent{prompt: prompt}, nil, projection.Notifications(acp.SessionUpdates(func(update acp.Update) error {
				received = append(received, update.Update.Kind)
				return nil
			})))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			initialization := initializeClient(t, connection, test.capabilities)
			session, err := connection.NewSession(ctx, hostRoot+"/tmp")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("go")}); err != nil {
				t.Fatal(err)
			}
			// Notifications run in wire order before the prompt response.
			var want []schema.SessionUpdateKind
			for i, allowed := range test.allowed {
				if allowed != (results[i] == nil) {
					t.Fatalf("update %d: allowed %t, error %v", i, allowed, results[i])
				}
				if !allowed {
					requireNotAdvertised(t, results[i])
				}
				if allowed {
					want = append(want, kinds[i])
				}
			}
			if len(received) != len(want) {
				t.Fatalf("received %v, want %v", received, want)
			}
			for i := range want {
				if received[i] != want[i] {
					t.Fatalf("received %v, want %v", received, want)
				}
			}
			if notices := projection.Notices(session.SessionID); len(notices) != boolCount(test.allowed[0]) {
				t.Fatalf("notices = %+v", notices)
			}
			if compactions := projection.Compactions(session.SessionID); len(compactions) != boolCount(test.allowed[1]) {
				t.Fatalf("compactions = %+v", compactions)
			}
		})
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

// requireNotAdvertised checks that a refused update carries the error shape
// of the host-method gate: an *acp.RPCError with code -32601.
func requireNotAdvertised(t *testing.T, err error) {
	t.Helper()
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("refusal = %#v, want *acp.RPCError -32601", err)
	}
}

type replayingAgent struct {
	testAgent
	replay  []schema.SessionUpdate
	results chan []error
}

func (replayingAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	enabled := true
	return schema.InitializeResponse{
		ProtocolVersion:   acp.Version,
		AgentCapabilities: &schema.AgentCapabilities{LoadSession: &enabled},
	}, nil
}

// LoadSession replays history with Client.Notify, alternating the value and
// pointer forms of acp.Update.
func (a replayingAgent) LoadSession(ctx context.Context, client Client, request schema.LoadSessionRequest) (schema.LoadSessionResponse, error) {
	results := make([]error, len(a.replay))
	for i, update := range a.replay {
		notification := acp.Update{SessionID: request.SessionID, Update: update}
		if i%2 == 0 {
			results[i] = client.Notify(ctx, schema.SessionUpdateMethodName, notification)
		} else {
			results[i] = client.Notify(ctx, schema.SessionUpdateMethodName, &notification)
		}
	}
	a.results <- results
	return schema.LoadSessionResponse{}, nil
}

// session/load history replay goes through Client.Notify, which applies the
// same gate as the SessionUpdater before writing to the wire.
func TestLoadSessionReplayIsGatedOnClientCapabilities(t *testing.T) {
	replay := []schema.SessionUpdate{
		{AgentMessageChunk: &schema.ContentChunk{Content: acp.NewTextContent("history")}},
		{CompactionUpdate: &schema.CompactionUpdate{CompactionID: "c1", Status: schema.CompactionStatusCompleted}},
		{CompactionSummaryChunk: &schema.CompactionSummaryChunk{CompactionID: "c1", Content: acp.NewTextContent("kept")}},
		{Notice: &schema.Notice{Severity: schema.NoticeSeverityInfo, Title: "replayed"}},
	}
	kinds := []schema.SessionUpdateKind{
		schema.SessionUpdateKindAgentMessageChunk, schema.SessionUpdateKindCompactionUpdate,
		schema.SessionUpdateKindCompactionSummaryChunk, schema.SessionUpdateKindNotice,
	}
	for _, test := range []struct {
		name         string
		capabilities acp.Capabilities
		allowed      []bool
	}{
		{"none", acp.Capabilities{}, []bool{true, false, false, false}},
		{"compaction", acp.Capabilities{Session: acp.SessionCompactionClientCapabilities()}, []bool{true, true, true, false}},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := make(chan []error, 1)
			var received []schema.SessionUpdateKind
			connection := startRuntime(t, replayingAgent{replay: replay, results: results}, nil, acp.SessionUpdates(func(update acp.Update) error {
				received = append(received, update.Update.Kind)
				return nil
			}))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			initialization := initializeClient(t, connection, test.capabilities)
			if _, err := connection.LoadSession(ctx, initialization, schema.LoadSessionRequest{
				SessionID: "old", Cwd: hostRoot + "/tmp",
			}); err != nil {
				t.Fatal(err)
			}
			errs := <-results
			var want []schema.SessionUpdateKind
			for i, allowed := range test.allowed {
				if allowed != (errs[i] == nil) {
					t.Fatalf("replay %d: allowed %t, error %v", i, allowed, errs[i])
				}
				if allowed {
					want = append(want, kinds[i])
				} else {
					requireNotAdvertised(t, errs[i])
				}
			}
			// Replayed notifications arrive in wire order before the
			// session/load response.
			if len(received) != len(want) {
				t.Fatalf("received %v, want %v", received, want)
			}
			for i := range want {
				if received[i] != want[i] {
					t.Fatalf("received %v, want %v", received, want)
				}
			}
		})
	}
}

// A config_option_update carrying a boolean option needs
// session.configOptions.boolean; one with only select options does not.
func TestSessionUpdaterGatesBooleanConfigOptions(t *testing.T) {
	selectOption := schema.SessionConfigOption{ID: "model", Name: "Model", Select: &schema.SessionConfigSelect{
		CurrentValue: "large",
		Options:      []schema.SessionConfigSelectOption{{Value: "large", Name: "Large"}},
	}}
	booleanOption := schema.SessionConfigOption{ID: "fast", Name: "Fast", Boolean: &schema.SessionConfigBoolean{CurrentValue: true}}
	updates := []schema.SessionUpdate{
		{ConfigOptionUpdate: &schema.ConfigOptionUpdate{ConfigOptions: []schema.SessionConfigOption{selectOption}}},
		{ConfigOptionUpdate: &schema.ConfigOptionUpdate{ConfigOptions: []schema.SessionConfigOption{selectOption, booleanOption}}},
	}
	for _, test := range []struct {
		name         string
		capabilities acp.Capabilities
		allowed      []bool
	}{
		{"none", acp.Capabilities{}, []bool{true, false}},
		{"select only", acp.Capabilities{Session: acp.ConfigOptionsClientCapabilities(false)}, []bool{true, false}},
		{"boolean", acp.Capabilities{Session: acp.ConfigOptionsClientCapabilities(true)}, []bool{true, true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := make([]error, len(updates))
			prompt := func(_ context.Context, _ Client, _ schema.PromptRequest, updater SessionUpdater) (schema.PromptResponse, error) {
				for i, update := range updates {
					results[i] = updater.Update(update)
				}
				return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
			}
			received := 0
			connection := startRuntime(t, testAgent{prompt: prompt}, nil, acp.SessionUpdates(func(acp.Update) error {
				received++
				return nil
			}))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			initialization := initializeClient(t, connection, test.capabilities)
			session, err := connection.NewSession(ctx, hostRoot+"/tmp")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("go")}); err != nil {
				t.Fatal(err)
			}
			want := 0
			for i, allowed := range test.allowed {
				if allowed != (results[i] == nil) {
					t.Fatalf("update %d: allowed %t, error %v", i, allowed, results[i])
				}
				if allowed {
					want++
				} else {
					requireNotAdvertised(t, results[i])
				}
			}
			if received != want {
				t.Fatalf("received %d updates, want %d", received, want)
			}
		})
	}
}
