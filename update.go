package acp

import (
	"github.com/BrokkAi/acp-go/schema"
)

func sessionUpdate(sessionID schema.SessionId, update schema.SessionUpdate) Update {
	return Update{SessionID: sessionID, Update: update}
}

func NewUserMessageChunkUpdate(sessionID schema.SessionId, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{UserMessageChunk: &chunk})
}

func NewAgentMessageChunkUpdate(sessionID schema.SessionId, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AgentMessageChunk: &chunk})
}

func NewAgentThoughtChunkUpdate(sessionID schema.SessionId, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AgentThoughtChunk: &chunk})
}

func NewToolCallUpdate(sessionID schema.SessionId, call schema.ToolCall) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ToolCall: &call})
}

func NewToolCallChangedUpdate(sessionID schema.SessionId, update schema.ToolCallUpdate) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ToolCallUpdate: &update})
}

func NewPlanUpdate(sessionID schema.SessionId, plan schema.Plan) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{Plan: &plan})
}

func NewAvailableCommandsUpdate(sessionID schema.SessionId, commands []schema.AvailableCommand) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AvailableCommandsUpdate: &schema.AvailableCommandsUpdate{
		AvailableCommands: commands,
	}})
}

func NewCurrentModeUpdate(sessionID schema.SessionId, modeID schema.SessionModeId) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{CurrentModeUpdate: &schema.CurrentModeUpdate{
		CurrentModeID: modeID,
	}})
}

func NewConfigOptionUpdate(sessionID schema.SessionId, options []schema.SessionConfigOption) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ConfigOptionUpdate: &schema.ConfigOptionUpdate{
		ConfigOptions: options,
	}})
}

func NewSessionInfoUpdate(sessionID schema.SessionId, info schema.SessionInfoUpdate) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{SessionInfoUpdate: &info})
}

func NewUsageUpdate(sessionID schema.SessionId, used, size uint64) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{UsageUpdate: &schema.UsageUpdate{
		Used: used, Size: size,
	}})
}
