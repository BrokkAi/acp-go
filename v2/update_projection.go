package v2

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"sync"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// UpdateProjection folds v2 session updates into stored entity state: user
// messages, Agent messages, Agent thoughts, tool calls, terminals,
// compactions, session info, and usage. It also keeps notices, which are live
// events rather than entities: Notices returns those received since the last
// DrainNotices, and the background list of the latest state_update (see
// Background). Plans and other session updates are not stored. Live
// delivery and replay go through the same Apply, so replaying the original
// updates in receive order reproduces the stored state, including removals.
//
// Each upsert is applied with the generated ApplyUpdate of its schema type:
// omitted fields leave the stored value unchanged, null is stored as an
// explicit clear, other values replace the stored value, and the top-level
// _meta is patched by key as described by schema.ApplyMetaUpdate. Chunks
// append to the current content of their entity. An update or chunk for an
// unseen ID creates the entity and fixes its position in Timeline. Entities
// are keyed by kind and ID, so a thought and a message may share a messageId.
//
// Accessors return snapshots: later updates never change a value already
// returned. Snapshots share storage with the projection, so callers must not
// modify them.
type UpdateProjection struct {
	mu sync.RWMutex
	// agentTextOnly limits the projection to Agent messages, for Text.
	agentTextOnly bool
	timeline      []EntityRef
	userMessages  map[schema.MessageId]*schema.UserMessage
	agentMessages map[schema.MessageId]*schema.AgentMessage
	agentThoughts map[schema.MessageId]*schema.AgentThought
	toolCalls     map[schema.ToolCallId]*schema.ToolCallUpdate
	terminals     map[schema.TerminalId]*TerminalState
	compactions   map[schema.CompactionId]*schema.CompactionUpdate
	sessionInfo   *schema.SessionInfoUpdate
	usage         *schema.UsageUpdate
	notices       []schema.Notice
	// background is the session's latest background list, valid while
	// backgroundReported is set.
	background         []schema.BackgroundWork
	backgroundReported bool
}

// EntityKind names a type of session entity. An entity ID is unique only
// among entities of the same kind.
type EntityKind string

const (
	EntityUserMessage  EntityKind = "user_message"
	EntityAgentMessage EntityKind = "agent_message"
	EntityAgentThought EntityKind = "agent_thought"
	EntityToolCall     EntityKind = "tool_call"
	EntityTerminal     EntityKind = "terminal"
	EntityCompaction   EntityKind = "compaction"
)

// EntityRef identifies one session entity by its kind and ID.
type EntityRef struct {
	Kind EntityKind
	ID   string
}

// TerminalState is the stored state of one Agent-owned terminal.
type TerminalState struct {
	// Terminal holds the stored terminal_update fields, as Rust's
	// TerminalUpdate::apply_update stores them. Its Output is the latest
	// output snapshot as received, including its _meta; it does not include
	// chunks received after it.
	Terminal schema.TerminalUpdate
	// Output is the current output: the bytes of the latest valid output
	// snapshot followed by the chunks received after it. An output snapshot
	// set to null clears it. Snapshots and chunks whose data is not valid
	// base64 are ignored, keeping the bytes already stored.
	Output []byte
}

func NewUpdateProjection() *UpdateProjection {
	return newUpdateProjection(false)
}

// newTextProjection keeps only Agent messages, which is all Text needs.
func newTextProjection() *UpdateProjection {
	return newUpdateProjection(true)
}

func newUpdateProjection(agentTextOnly bool) *UpdateProjection {
	return &UpdateProjection{
		agentTextOnly: agentTextOnly,
		userMessages:  make(map[schema.MessageId]*schema.UserMessage),
		agentMessages: make(map[schema.MessageId]*schema.AgentMessage),
		agentThoughts: make(map[schema.MessageId]*schema.AgentThought),
		toolCalls:     make(map[schema.ToolCallId]*schema.ToolCallUpdate),
		terminals:     make(map[schema.TerminalId]*TerminalState),
		compactions:   make(map[schema.CompactionId]*schema.CompactionUpdate),
	}
}

// Apply folds one session update in receive order. Updates the projection
// does not store, including unknown variants, are ignored.
func (p *UpdateProjection) Apply(update schema.SessionUpdate) {
	if p.agentTextOnly && update.AgentMessage == nil && update.AgentMessageChunk == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case update.UserMessage != nil:
		message := entity(p, EntityUserMessage, p.userMessages, update.UserMessage.MessageID, newUserMessage)
		message.ApplyUpdate(*update.UserMessage)
		ownContent(&message.Content)
	case update.UserMessageChunk != nil:
		chunk := update.UserMessageChunk
		message := entity(p, EntityUserMessage, p.userMessages, chunk.MessageID, newUserMessage)
		appendContent(&message.Content, chunk.Content)
	case update.AgentMessage != nil:
		message := entity(p, EntityAgentMessage, p.agentMessages, update.AgentMessage.MessageID, newAgentMessage)
		message.ApplyUpdate(*update.AgentMessage)
		ownContent(&message.Content)
	case update.AgentMessageChunk != nil:
		chunk := update.AgentMessageChunk
		message := entity(p, EntityAgentMessage, p.agentMessages, chunk.MessageID, newAgentMessage)
		appendContent(&message.Content, chunk.Content)
	case update.AgentThought != nil:
		thought := entity(p, EntityAgentThought, p.agentThoughts, update.AgentThought.MessageID, newAgentThought)
		thought.ApplyUpdate(*update.AgentThought)
		ownContent(&thought.Content)
	case update.AgentThoughtChunk != nil:
		chunk := update.AgentThoughtChunk
		thought := entity(p, EntityAgentThought, p.agentThoughts, chunk.MessageID, newAgentThought)
		appendContent(&thought.Content, chunk.Content)
	case update.ToolCallUpdate != nil:
		p.applyToolCall(*update.ToolCallUpdate)
	case update.ToolCallContentChunk != nil:
		chunk := update.ToolCallContentChunk
		call := entity(p, EntityToolCall, p.toolCalls, chunk.ToolCallID, newToolCall)
		appendContent(&call.Content, chunk.Content)
	case update.TerminalUpdate != nil:
		p.applyTerminal(*update.TerminalUpdate)
	case update.TerminalOutputChunk != nil:
		chunk := update.TerminalOutputChunk
		// Even an invalid chunk creates the terminal and fixes its position.
		terminal := entity(p, EntityTerminal, p.terminals, chunk.TerminalID, newTerminal)
		if data, err := base64.StdEncoding.DecodeString(chunk.Data); err == nil {
			terminal.Output = append(terminal.Output, data...)
		}
	case update.CompactionUpdate != nil:
		compaction := entity(p, EntityCompaction, p.compactions, update.CompactionUpdate.CompactionID, newCompaction)
		compaction.ApplyUpdate(*update.CompactionUpdate)
		ownContent(&compaction.Summary)
	case update.CompactionSummaryChunk != nil:
		chunk := update.CompactionSummaryChunk
		compaction := entity(p, EntityCompaction, p.compactions, chunk.CompactionID, newCompaction)
		appendContent(&compaction.Summary, chunk.Content)
	case update.SessionInfoUpdate != nil:
		if p.sessionInfo == nil {
			p.sessionInfo = &schema.SessionInfoUpdate{}
		}
		p.sessionInfo.ApplyUpdate(*update.SessionInfoUpdate)
	case update.UsageUpdate != nil:
		p.applyUsage(*update.UsageUpdate)
	case update.Notice != nil:
		p.notices = append(p.notices, *update.Notice)
	case update.StateUpdate != nil:
		p.applyBackground(stateBackground(*update.StateUpdate))
	}
}

// stateBackground returns the background list a state_update carries. A
// custom or future state may carry one too; it is read from the preserved
// payload, and one that does not decode reads as not reported.
func stateBackground(state schema.StateUpdate) schema.Nullable[[]schema.BackgroundWork] {
	switch {
	case state.Running != nil:
		return state.Running.Background
	case state.Idle != nil:
		return state.Idle.Background
	case state.RequiresAction != nil:
		return state.RequiresAction.Background
	case state.Other != nil:
		// A running state has only _meta and background, so its decoder
		// reads exactly the background member of any state payload.
		var other schema.RunningStateUpdate
		if json.Unmarshal(state.Other.Raw, &other) == nil {
			return other.Background
		}
	}
	return schema.Nullable[[]schema.BackgroundWork]{}
}

// applyBackground records a state_update's background list. A concrete list
// replaces the previous one. An omitted or null list, which the decoder also
// produces for a malformed one, is not reported: background work is unknown
// until a list is reported again.
func (p *UpdateProjection) applyBackground(background schema.Nullable[[]schema.BackgroundWork]) {
	if background.Set && !background.Null {
		p.background = slices.Clip(background.Value)
		if p.background == nil {
			p.background = []schema.BackgroundWork{}
		}
		p.backgroundReported = true
		return
	}
	p.background = nil
	p.backgroundReported = false
}

// ApplyPermissionRequest applies the toolCall of a permission request's
// tool_call subject to the stored tool call, exactly like a tool_call_update.
// Clients call it in receive order, before showing the prompt. Other subjects
// are ignored.
func (p *UpdateProjection) ApplyPermissionRequest(request schema.RequestPermissionRequest) {
	if request.Subject == nil || request.Subject.ToolCall == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applyToolCall(request.Subject.ToolCall.ToolCall)
}

func (p *UpdateProjection) applyToolCall(update schema.ToolCallUpdate) {
	call := entity(p, EntityToolCall, p.toolCalls, update.ToolCallID, newToolCall)
	call.ApplyUpdate(update)
	ownContent(&call.Content)
}

func (p *UpdateProjection) applyTerminal(update schema.TerminalUpdate) {
	terminal := entity(p, EntityTerminal, p.terminals, update.TerminalID, newTerminal)
	terminal.Terminal.ApplyUpdate(update)
	switch output := update.Output; {
	case !output.Set:
	case output.Null:
		terminal.Output = nil
	default:
		// A snapshot replaces the stored bytes; an invalid one is ignored
		// while the update's other fields still apply.
		if data, err := base64.StdEncoding.DecodeString(output.Value.Data); err == nil {
			terminal.Output = data
		}
	}
}

// applyUsage keeps the latest usage report. cost is a patch field: omission
// keeps the last reported cost and null clears it. The other fields,
// including _meta, describe only the latest report.
func (p *UpdateProjection) applyUsage(update schema.UsageUpdate) {
	if p.usage != nil && !update.Cost.Set {
		update.Cost = p.usage.Cost
	}
	p.usage = &update
}

func newUserMessage(id schema.MessageId) schema.UserMessage {
	return schema.UserMessage{MessageID: id}
}

func newAgentMessage(id schema.MessageId) schema.AgentMessage {
	return schema.AgentMessage{MessageID: id}
}

func newAgentThought(id schema.MessageId) schema.AgentThought {
	return schema.AgentThought{MessageID: id}
}

func newToolCall(id schema.ToolCallId) schema.ToolCallUpdate {
	return schema.ToolCallUpdate{ToolCallID: id}
}

func newTerminal(id schema.TerminalId) TerminalState {
	return TerminalState{Terminal: schema.TerminalUpdate{TerminalID: id}}
}

// newCompaction starts a first-seen compaction in progress, which matters
// when a summary chunk arrives before its first compaction_update.
func newCompaction(id schema.CompactionId) schema.CompactionUpdate {
	return schema.CompactionUpdate{CompactionID: id, Status: schema.CompactionStatusInProgress}
}

// entity returns the stored entity for id. A first-seen ID creates the entity
// from its defaults and records its timeline position.
func entity[K ~string, V any](p *UpdateProjection, kind EntityKind, store map[K]*V, id K, create func(K) V) *V {
	if stored, ok := store[id]; ok {
		return stored
	}
	created := create(id)
	store[id] = &created
	p.timeline = append(p.timeline, EntityRef{Kind: kind, ID: string(id)})
	return &created
}

// ownContent clips a collection taken from an update, so later chunk appends
// allocate instead of writing into the update's backing array.
func ownContent[T any](content *schema.Nullable[[]T]) {
	content.Value = slices.Clip(content.Value)
}

// appendContent appends one chunk item to the current content. Omitted or
// null content is empty.
func appendContent[T any](content *schema.Nullable[[]T], item T) {
	content.Set, content.Null = true, false
	content.Value = append(content.Value, item)
}

// Text returns the text of every Agent message in timeline order.
func (p *UpdateProjection) Text() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]byte, 0, 128)
	for _, ref := range p.timeline {
		if ref.Kind != EntityAgentMessage {
			continue
		}
		for _, block := range p.agentMessages[schema.MessageId(ref.ID)].Content.Value {
			if block.Text != nil {
				result = append(result, block.Text.Text...)
			}
		}
	}
	return string(result)
}

// Timeline returns every stored entity in the order it was first seen.
func (p *UpdateProjection) Timeline() []EntityRef {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.timeline)
}

// UserMessage returns the stored user message with the given ID.
func (p *UpdateProjection) UserMessage(id schema.MessageId) (schema.UserMessage, bool) {
	return lookup(p, p.userMessages, id, func(message schema.UserMessage) schema.UserMessage {
		message.Content.Value = slices.Clip(message.Content.Value)
		return message
	})
}

// AgentMessage returns the stored Agent message with the given ID.
func (p *UpdateProjection) AgentMessage(id schema.MessageId) (schema.AgentMessage, bool) {
	return lookup(p, p.agentMessages, id, func(message schema.AgentMessage) schema.AgentMessage {
		message.Content.Value = slices.Clip(message.Content.Value)
		return message
	})
}

// AgentThought returns the stored Agent thought with the given ID.
func (p *UpdateProjection) AgentThought(id schema.MessageId) (schema.AgentThought, bool) {
	return lookup(p, p.agentThoughts, id, func(thought schema.AgentThought) schema.AgentThought {
		thought.Content.Value = slices.Clip(thought.Content.Value)
		return thought
	})
}

// ToolCall returns the stored tool call with the given ID. Fields keep their
// wire states, as Rust's ToolCallUpdate::apply_update does: an unset or null
// kind means other, and an unset or null status means pending.
func (p *UpdateProjection) ToolCall(id schema.ToolCallId) (schema.ToolCallUpdate, bool) {
	return lookup(p, p.toolCalls, id, func(call schema.ToolCallUpdate) schema.ToolCallUpdate {
		call.Content.Value = slices.Clip(call.Content.Value)
		return call
	})
}

// Terminal returns the stored terminal with the given ID.
func (p *UpdateProjection) Terminal(id schema.TerminalId) (TerminalState, bool) {
	return lookup(p, p.terminals, id, func(terminal TerminalState) TerminalState {
		terminal.Output = slices.Clip(terminal.Output)
		return terminal
	})
}

// Compaction returns the stored compaction with the given ID.
func (p *UpdateProjection) Compaction(id schema.CompactionId) (schema.CompactionUpdate, bool) {
	return lookup(p, p.compactions, id, compactionSnapshot)
}

func compactionSnapshot(compaction schema.CompactionUpdate) schema.CompactionUpdate {
	compaction.Summary.Value = slices.Clip(compaction.Summary.Value)
	return compaction
}

// Compactions returns every stored compaction in the order it was first
// seen, the order Timeline lists them in.
func (p *UpdateProjection) Compactions() []schema.CompactionUpdate {
	p.mu.RLock()
	defer p.mu.RUnlock()
	compactions := make([]schema.CompactionUpdate, 0, len(p.compactions))
	for _, ref := range p.timeline {
		if ref.Kind != EntityCompaction {
			continue
		}
		compactions = append(compactions, compactionSnapshot(*p.compactions[schema.CompactionId(ref.ID)]))
	}
	return compactions
}

// Notices returns the notices received since the last DrainNotices, in
// receive order, without consuming them. Notices are live events: they are
// not session entities and do not appear in Timeline.
func (p *UpdateProjection) Notices() []schema.Notice {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.notices)
}

// DrainNotices returns and clears the stored notices.
func (p *UpdateProjection) DrainNotices() []schema.Notice {
	p.mu.Lock()
	defer p.mu.Unlock()
	notices := p.notices
	p.notices = nil
	return notices
}

// SessionInfo returns the stored session info once a session_info_update has
// been applied.
func (p *UpdateProjection) SessionInfo() (schema.SessionInfoUpdate, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.sessionInfo == nil {
		return schema.SessionInfoUpdate{}, false
	}
	return *p.sessionInfo, true
}

// Usage returns the latest usage report, carrying the last reported cost
// forward while later reports omit it.
func (p *UpdateProjection) Usage() (schema.UsageUpdate, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.usage == nil {
		return schema.UsageUpdate{}, false
	}
	return *p.usage, true
}

func lookup[K comparable, V any](p *UpdateProjection, store map[K]*V, id K, snapshot func(V) V) (V, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	stored, ok := store[id]
	if !ok {
		var zero V
		return zero, false
	}
	return snapshot(*stored), true
}

// Background returns the background list of the latest state_update: the
// work running independently of foreground work, as received, including
// items of unknown type. reported is false when that state_update omitted the
// list, set it to null, or sent a malformed one, and before any state_update
// has reported one: background work is then unknown, which is not the same
// as none. A reported empty list means no background work is running. The
// list is a snapshot; a later state_update replaces it.
func (p *UpdateProjection) Background() (work []schema.BackgroundWork, reported bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.backgroundReported {
		return nil, false
	}
	return p.background, true
}

// ToolCallInBackground reports whether the latest reported background list
// covers the tool call: it lists the tool call, or it lists a terminal that
// the stored tool call references in its content. It reports false when
// background work is not reported.
func (p *UpdateProjection) ToolCallInBackground(id schema.ToolCallId) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.backgroundReported && p.toolCallInBackgroundLocked(id)
}

// TerminalInBackground reports whether the latest reported background list
// lists the terminal. It reports false when background work is not reported.
func (p *UpdateProjection) TerminalInBackground(id schema.TerminalId) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.backgroundReported && p.terminalListedLocked(id)
}

func (p *UpdateProjection) terminalListedLocked(id schema.TerminalId) bool {
	for _, work := range p.background {
		if work.Terminal != nil && work.Terminal.TerminalID == id {
			return true
		}
	}
	return false
}

func (p *UpdateProjection) toolCallInBackgroundLocked(id schema.ToolCallId) bool {
	for _, work := range p.background {
		if work.ToolCall != nil && work.ToolCall.ToolCallID == id {
			return true
		}
	}
	// Process-backed work is listed as its terminal, never also as the tool
	// call whose live work it is.
	if call := p.toolCalls[id]; call != nil {
		for _, content := range call.Content.Value {
			if content.Terminal != nil && p.terminalListedLocked(content.Terminal.TerminalID) {
				return true
			}
		}
	}
	return false
}

// CanCancelToolCall reports whether the stored tool call currently advertises
// capabilities.cancel, which tool_call/cancel requires. capabilities is a
// patch: omitted keeps the stored value, null clears it, and an object
// replaces it whole, so a later update without cancel withdraws it. An
// unknown tool call cannot be cancelled.
func (p *UpdateProjection) CanCancelToolCall(id schema.ToolCallId) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	call := p.toolCalls[id]
	return call != nil && present(call.Capabilities) && call.Capabilities.Value.Cancel != nil
}

// CanCancelTerminal reports whether the stored terminal currently advertises
// capabilities.cancel, which terminal/cancel requires, with the same patch
// rules as CanCancelToolCall. An unknown terminal cannot be cancelled.
func (p *UpdateProjection) CanCancelTerminal(id schema.TerminalId) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	terminal := p.terminals[id]
	return terminal != nil && present(terminal.Terminal.Capabilities) && terminal.Terminal.Capabilities.Value.Cancel != nil
}

func present[T any](field schema.Nullable[T]) bool {
	return field.Set && !field.Null
}
