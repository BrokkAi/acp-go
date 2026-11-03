package agent

import (
	"encoding/json"

	"github.com/BrokkAi/acp-go"
	unstableschema "github.com/BrokkAi/acp-go/schema/unstable"
)

// unstableClientCapabilities reads the client capabilities of initialize
// params with the unstable schema, as the reference SDK reads them with its
// unstable features, so the fields the stable schema drops
// (clientCapabilities.subagents and .plan) are kept. An omitted, null, or
// malformed capability is not advertised.
func unstableClientCapabilities(params json.RawMessage) unstableschema.ClientCapabilities {
	var request unstableschema.InitializeRequest
	if err := json.Unmarshal(params, &request); err != nil || request.ClientCapabilities == nil {
		return unstableschema.ClientCapabilities{}
	}
	return *request.ClientCapabilities
}

// requireUnstableSessionUpdate applies the session-update capability gate to
// session/update params that are not the stable acp.Update: a typed
// schema/unstable.SessionNotification, or any params that encode as a
// session/update. It refuses, as acp.RequireSessionUpdateCapability does,
// notice without session.notices, compaction_update and
// compaction_summary_chunk without session.compaction, a config_option_update
// carrying a boolean option without session.configOptions.boolean,
// plan_update and plan_removed without the unstable plan capability, and
// subagent_update, session_message, and session_message_chunk without the
// unstable subagents capability. Params that do not decode are refused by
// their sessionUpdate tag for every kind the tag alone decides.
func requireUnstableSessionUpdate(capabilities unstableschema.ClientCapabilities, params any) error {
	var notification unstableschema.SessionNotification
	switch typed := params.(type) {
	case unstableschema.SessionNotification:
		notification = typed
	case *unstableschema.SessionNotification:
		if typed == nil {
			return nil
		}
		notification = *typed
	default:
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil
		}
		if json.Unmarshal(encoded, &notification) != nil {
			return requireUpdateKind(capabilities, updateKind(encoded))
		}
	}
	update := notification.Update
	if update.ConfigOptionUpdate != nil {
		if advertisesBooleanConfig(capabilities) {
			return nil
		}
		for _, option := range update.ConfigOptionUpdate.ConfigOptions {
			if option.Boolean != nil || option.Kind == unstableschema.SessionConfigOptionKindBoolean {
				return sessionUpdateNotAdvertised("session.configOptions.boolean")
			}
		}
		return nil
	}
	return requireUpdateKind(capabilities, gatedKind(update))
}

// gatedKind names the gated kind a typed update carries; Kind is only set on
// decode, so a value built in Go is read from its variant.
func gatedKind(update unstableschema.SessionUpdate) string {
	var kind unstableschema.SessionUpdateKind
	switch {
	case update.Notice != nil:
		kind = unstableschema.SessionUpdateKindNotice
	case update.CompactionUpdate != nil:
		kind = unstableschema.SessionUpdateKindCompactionUpdate
	case update.CompactionSummaryChunk != nil:
		kind = unstableschema.SessionUpdateKindCompactionSummaryChunk
	case update.PlanUpdate != nil:
		kind = unstableschema.SessionUpdateKindPlanUpdate
	case update.PlanRemoved != nil:
		kind = unstableschema.SessionUpdateKindPlanRemoved
	case update.SubagentUpdate != nil:
		kind = unstableschema.SessionUpdateKindSubagentUpdate
	case update.SessionMessage != nil:
		kind = unstableschema.SessionUpdateKindSessionMessage
	case update.SessionMessageChunk != nil:
		kind = unstableschema.SessionUpdateKindSessionMessageChunk
	}
	return string(kind)
}

// requireUpdateKind gates the session-update kinds whose capability the
// sessionUpdate tag alone decides.
func requireUpdateKind(capabilities unstableschema.ClientCapabilities, kind string) error {
	session := capabilities.Session
	switch unstableschema.SessionUpdateKind(kind) {
	case unstableschema.SessionUpdateKindNotice:
		if session == nil || session.Notices == nil {
			return sessionUpdateNotAdvertised("session.notices")
		}
	case unstableschema.SessionUpdateKindCompactionUpdate, unstableschema.SessionUpdateKindCompactionSummaryChunk:
		if session == nil || session.Compaction == nil {
			return sessionUpdateNotAdvertised("session.compaction")
		}
	case unstableschema.SessionUpdateKindPlanUpdate, unstableschema.SessionUpdateKindPlanRemoved:
		if capabilities.Plan == nil {
			return sessionUpdateNotAdvertised("plan")
		}
	case unstableschema.SessionUpdateKindSubagentUpdate, unstableschema.SessionUpdateKindSessionMessage, unstableschema.SessionUpdateKindSessionMessageChunk:
		if capabilities.Subagents == nil {
			return sessionUpdateNotAdvertised("subagents")
		}
	}
	return nil
}

func advertisesBooleanConfig(capabilities unstableschema.ClientCapabilities) bool {
	session := capabilities.Session
	return session != nil && session.ConfigOptions != nil && session.ConfigOptions.Boolean != nil
}

// updateKind reads the sessionUpdate tag from encoded session/update params
// that do not decode, in the named form {"update": {"sessionUpdate": ...}}
// and the positional form [sessionId, update] (with the update itself named
// or positional, its tag first), so a malformed update is refused too.
func updateKind(encoded []byte) string {
	update, ok := member(encoded, "update", 1)
	if !ok {
		return ""
	}
	tag, ok := member(update, "sessionUpdate", 0)
	if !ok {
		return ""
	}
	var kind string
	if json.Unmarshal(tag, &kind) != nil {
		return ""
	}
	return kind
}

// member returns the named member of a JSON object, the last one when it is
// repeated, or the element at index of a JSON array.
func member(raw []byte, name string, index int) (json.RawMessage, bool) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		if index < len(items) {
			return items[index], true
		}
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil, false
	}
	value, ok := object[name]
	return value, ok
}

// sessionUpdateNotAdvertised is the refusal in the shape
// acp.RequireSessionUpdateCapability uses.
func sessionUpdateNotAdvertised(capability string) error {
	return &acp.RPCError{Code: -32601, Message: "client did not advertise " + capability + " support"}
}
