package acp

import (
	"github.com/BrokkAi/acp-go/schema"
)

func sessionUpdate(sessionID SessionID, update schema.SessionUpdate) Update {
	return Update{SessionID: sessionID, Update: update}
}

func NewUserMessageChunkUpdate(sessionID SessionID, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{UserMessageChunk: &chunk})
}

func NewAgentMessageChunkUpdate(sessionID SessionID, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AgentMessageChunk: &chunk})
}

func NewAgentThoughtChunkUpdate(sessionID SessionID, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AgentThoughtChunk: &chunk})
}

func NewToolCallUpdate(sessionID SessionID, call schema.ToolCall) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ToolCall: &call})
}

func NewToolCallChangedUpdate(sessionID SessionID, update schema.ToolCallUpdate) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ToolCallUpdate: &update})
}

func NewPlanUpdate(sessionID SessionID, plan schema.Plan) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{Plan: &plan})
}

func NewAvailableCommandsUpdate(sessionID SessionID, commands []schema.AvailableCommand) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AvailableCommandsUpdate: &schema.AvailableCommandsUpdate{
		AvailableCommands: commands,
	}})
}

func NewCurrentModeUpdate(sessionID SessionID, modeID schema.SessionModeId) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{CurrentModeUpdate: &schema.CurrentModeUpdate{
		CurrentModeID: modeID,
	}})
}

func NewConfigOptionUpdate(sessionID SessionID, options []schema.SessionConfigOption) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ConfigOptionUpdate: &schema.ConfigOptionUpdate{
		ConfigOptions: options,
	}})
}

func NewSessionInfoUpdate(sessionID SessionID, info schema.SessionInfoUpdate) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{SessionInfoUpdate: &info})
}

func NewUsageUpdate(sessionID SessionID, used, size uint64) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{UsageUpdate: &schema.UsageUpdate{
		Used: used, Size: size,
	}})
}
