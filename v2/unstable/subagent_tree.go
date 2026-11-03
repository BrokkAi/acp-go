package unstable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/rpcctl"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// SubagentTree is the client side of subagent sessions: it folds the
// subagent_update associations, session_message upserts, and
// session_message_chunk appends a client receives, in receive order, into a
// parent/child tree with per-child state and per-session message
// transcripts, and it enforces the restricted-session rules for children.
//
// A subagent_update is an upsert keyed by the child's session ID, sent on its
// immediate parent session. Its patch fields follow the RFD: omitted leaves
// the stored value unchanged, null clears it, and a value replaces the whole
// field (capabilities, state, and additionalDirectories are replaced as a
// whole, never merged), except _meta, which patches the stored metadata by
// top-level key (schema.ApplyMetaUpdate). A child's own state_update is the
// same logical state as the one mirrored on its association: either report
// replaces the child's state, and a session's state_update background list
// names its background children (BackgroundSubagents). An association cannot reparent a child, name
// its own parent, or create a cycle; Observe refuses such an update with an
// error and applies none of it. A cwd or additional directories that are not
// absolute are not applied, and Observe reports them with an error after
// applying the rest of the update.
//
// Each child has two views of its state. Subagent.Association holds every
// field as last reported, including history replayed by session/load, for
// display. Subagent.Capabilities and Subagent.State are the effective
// mutation set and the confirmed current work state, which resuming the
// parent (Restore), closing it (Closed), and losing the connection
// (Disconnected) invalidate: historical data stays visible, but effective
// capabilities start empty and current work state starts unconfirmed until
// the agent reports them again.
//
// A SubagentTree is safe for concurrent use. Compose its Notifications with
// the connection's other notification handlers.
type SubagentTree struct {
	mu          sync.Mutex
	nodes       map[schema.SessionId]*subagentNode
	children    map[schema.SessionId][]schema.SessionId
	directories map[schema.SessionId]sessionDirectories
	transcripts map[schema.SessionId]*transcript
	restores    map[schema.SessionId]*restoreAttempt
	// states holds the latest current state_update of ordinary sessions.
	states map[schema.SessionId]*schema.StateUpdate
}

type subagentNode struct {
	parent       schema.SessionId
	association  schema.SubagentUpdate
	capabilities *schema.SubagentSessionCapabilities
	state        *schema.StateUpdate
}

type sessionDirectories struct {
	cwd        schema.AbsolutePath
	additional []schema.AbsolutePath
}

type transcript struct {
	order    []schema.MessageId
	messages map[schema.MessageId]*schema.SessionMessage
}

// restoreAttempt is one session/resume of a session.
type restoreAttempt struct {
	replay bool
	// answered is set, in wire order, when the request succeeded: snapshots
	// received afterwards are current, not history.
	answered bool
}

// Subagent is a snapshot of one child session. It shares storage with the
// tree, so callers must not modify it.
type Subagent struct {
	// SessionID is the child's session ID.
	SessionID schema.SessionId
	// Parent is the immediate parent session: the session the association
	// was announced on.
	Parent schema.SessionId
	// Root is the ordinary (non-subagent) session at the top of the child's
	// ancestor chain.
	Root schema.SessionId
	// Association holds the association's fields as last reported, patches
	// applied, including values replayed as history; its State is the last
	// state reported on either path. Its Capabilities and State are display
	// information only: use Capabilities and State for controls and current
	// activity.
	Association schema.SubagentUpdate
	// Capabilities is the effective set of client-initiated mutations, or
	// nil when none are permitted: never reported, cleared, or invalidated by
	// a restore, close, or connection loss. It never comes from history.
	Capabilities *schema.SubagentSessionCapabilities
	// State is the confirmed current work state, or nil when it is
	// unconfirmed: never reported, cleared, or invalidated. Nil is not idle
	// and not cancelled.
	State *schema.StateUpdate
}

// CanCancel reports whether the client may send session/cancel for the
// child: the effective capabilities carry cancel and no restore of the
// child's root session is in progress.
func (s Subagent) CanCancel() bool {
	return s.Capabilities != nil && s.Capabilities.Cancel != nil
}

// MessageRef names a session-directed message entry: a message ID in the
// transcript of a session.
type MessageRef struct {
	SessionID schema.SessionId
	MessageID schema.MessageId
}

// NewSubagentTree returns an empty tree.
func NewSubagentTree() *SubagentTree {
	return &SubagentTree{
		nodes:       make(map[schema.SessionId]*subagentNode),
		children:    make(map[schema.SessionId][]schema.SessionId),
		directories: make(map[schema.SessionId]sessionDirectories),
		transcripts: make(map[schema.SessionId]*transcript),
		restores:    make(map[schema.SessionId]*restoreAttempt),
		states:      make(map[schema.SessionId]*schema.StateUpdate),
	}
}

// Notifications adapts session/update notifications into Observe calls and
// ignores other notifications. A notification that does not decode, or an
// association Observe refuses, returns its error.
func (t *SubagentTree) Notifications() acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.SessionUpdateMethodName {
			return nil
		}
		var notification schema.UpdateSessionNotification
		if err := json.Unmarshal(raw, &notification); err != nil {
			return err
		}
		return t.Observe(notification.SessionID, notification.Update)
	}
}

// Observe folds one session update received on the session sessionID.
// Updates other than subagent_update, state_update, session_message, and
// session_message_chunk are ignored.
func (t *SubagentTree) Observe(sessionID schema.SessionId, update schema.SessionUpdate) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case update.SubagentUpdate != nil:
		return t.applyAssociation(sessionID, *update.SubagentUpdate)
	case update.StateUpdate != nil:
		t.applyState(sessionID, *update.StateUpdate)
	case update.SessionMessage != nil:
		message := t.message(sessionID, update.SessionMessage.MessageID)
		message.ApplyUpdate(*update.SessionMessage)
		// Later chunks append, so they must not write into the update's array.
		message.Content.Value = slices.Clip(message.Content.Value)
	case update.SessionMessageChunk != nil:
		chunk := update.SessionMessageChunk
		message := t.message(sessionID, chunk.MessageID)
		message.Content.Set, message.Content.Null = true, false
		message.Content.Value = append(message.Content.Value, chunk.Content)
	}
	return nil
}

func (t *SubagentTree) applyAssociation(parent schema.SessionId, update schema.SubagentUpdate) error {
	child := update.SessionID
	if child == parent {
		return fmt.Errorf("subagent_update: session %q cannot be its own parent", child)
	}
	node, known := t.nodes[child]
	if known && node.parent != parent {
		return fmt.Errorf("subagent_update: session %q belongs to %q and cannot be reparented to %q", child, node.parent, parent)
	}
	if !known {
		for ancestor := parent; ; {
			if ancestor == child {
				return fmt.Errorf("subagent_update: announcing %q under %q would create a cycle", child, parent)
			}
			next, ok := t.nodes[ancestor]
			if !ok {
				break
			}
			ancestor = next.parent
		}
	}
	// A relative directory is not applied, and is reported once the rest of
	// the update is.
	var directoryErr error
	if update.Cwd.Set && !update.Cwd.Null {
		if err := acpvalidate.AbsolutePath("subagent_update cwd", string(update.Cwd.Value)); err != nil {
			directoryErr = err
			update.Cwd = schema.Nullable[schema.AbsolutePath]{}
		}
	}
	if update.AdditionalDirectories.Set && !update.AdditionalDirectories.Null {
		for _, directory := range update.AdditionalDirectories.Value {
			if err := acpvalidate.AbsolutePath("subagent_update additional directory", string(directory)); err != nil {
				directoryErr = errors.Join(directoryErr, err)
				update.AdditionalDirectories = schema.Nullable[[]schema.AbsolutePath]{}
				break
			}
		}
	}
	if !known {
		node = &subagentNode{parent: parent, association: schema.SubagentUpdate{SessionID: child}}
		t.nodes[child] = node
		t.children[parent] = append(t.children[parent], child)
	}
	node.association.ApplyUpdate(update)
	if t.historical(parent) {
		// Replayed before the successful response: history only.
		return directoryErr
	}
	if update.Capabilities.Set {
		node.capabilities = nil
		if !update.Capabilities.Null {
			capabilities := update.Capabilities.Value
			node.capabilities = &capabilities
		}
	}
	if update.State.Set {
		node.state = nil
		if !update.State.Null {
			state := update.State.Value
			node.state = &state
		}
	}
	return directoryErr
}

// historical reports whether an update received on sessionID is replayed
// history: a resume of its root session with replay has not answered yet.
func (t *SubagentTree) historical(sessionID schema.SessionId) bool {
	attempt := t.restores[t.root(sessionID)]
	return attempt != nil && attempt.replay && !attempt.answered
}

// applyState applies a session's own state_update: a child's is the same
// logical state as the one mirrored on its association.
func (t *SubagentTree) applyState(sessionID schema.SessionId, state schema.StateUpdate) {
	node, isChild := t.nodes[sessionID]
	if isChild {
		node.association.State = Value(state)
	}
	if t.historical(sessionID) {
		return
	}
	if isChild {
		node.state = &state
	} else {
		t.states[sessionID] = &state
	}
}

func (t *SubagentTree) message(sessionID schema.SessionId, messageID schema.MessageId) *schema.SessionMessage {
	entries := t.transcripts[sessionID]
	if entries == nil {
		entries = &transcript{messages: make(map[schema.MessageId]*schema.SessionMessage)}
		t.transcripts[sessionID] = entries
	}
	if message, ok := entries.messages[messageID]; ok {
		return message
	}
	message := &schema.SessionMessage{MessageID: messageID}
	entries.messages[messageID] = message
	entries.order = append(entries.order, messageID)
	return message
}

// root returns the top of sessionID's ancestor chain.
func (t *SubagentTree) root(sessionID schema.SessionId) schema.SessionId {
	for {
		node, ok := t.nodes[sessionID]
		if !ok {
			return sessionID
		}
		sessionID = node.parent
	}
}

// descendants returns every known descendant of sessionID, parents before
// children.
func (t *SubagentTree) descendants(sessionID schema.SessionId) []schema.SessionId {
	var result []schema.SessionId
	queue := []schema.SessionId{sessionID}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, child := range t.children[next] {
			result = append(result, child)
			queue = append(queue, child)
		}
	}
	return result
}

// BackgroundSubagents returns the children listed as subagent background
// work in a session's current state: the latest state_update of an ordinary
// session, or a child's current State. ok is false when that state does not
// report background work, which is not the same as reporting none.
func (t *SubagentTree) BackgroundSubagents(sessionID schema.SessionId) (children []schema.SessionId, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.states[sessionID]
	if node, isChild := t.nodes[sessionID]; isChild {
		state = node.state
	}
	if state == nil {
		return nil, false
	}
	var background schema.Nullable[[]schema.BackgroundWork]
	switch {
	case state.Running != nil:
		background = state.Running.Background
	case state.Idle != nil:
		background = state.Idle.Background
	case state.RequiresAction != nil:
		background = state.RequiresAction.Background
	case state.Unknown != nil:
		background = state.Unknown.Background
	}
	if !background.Set || background.Null {
		return nil, false
	}
	children = []schema.SessionId{}
	for _, work := range background.Value {
		if work.Subagent != nil {
			children = append(children, work.Subagent.SessionID)
		}
	}
	return children, true
}

func (t *SubagentTree) invalidate(sessions []schema.SessionId) {
	for _, id := range sessions {
		node := t.nodes[id]
		node.capabilities = nil
		node.state = nil
	}
}

func (t *SubagentTree) snapshot(id schema.SessionId, node *subagentNode) Subagent {
	subagent := Subagent{
		SessionID:   id,
		Parent:      node.parent,
		Root:        t.root(id),
		Association: node.association,
		State:       node.state,
	}
	if t.restores[subagent.Root] == nil {
		subagent.Capabilities = node.capabilities
	}
	return subagent
}

// Subagent returns the child with the given session ID, or false when the
// session is not a known child. Use it to attribute permission, elicitation,
// filesystem, and terminal requests, which a child sends with its own session
// ID: they belong to that child, not to its parent.
func (t *SubagentTree) Subagent(sessionID schema.SessionId) (Subagent, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	node, ok := t.nodes[sessionID]
	if !ok {
		return Subagent{}, false
	}
	return t.snapshot(sessionID, node), true
}

// IsSubagent reports whether sessionID is a known child session.
func (t *SubagentTree) IsSubagent(sessionID schema.SessionId) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.nodes[sessionID]
	return ok
}

// Children returns the immediate children of a session in announcement
// order.
func (t *SubagentTree) Children(sessionID schema.SessionId) []schema.SessionId {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.children[sessionID])
}

// Descendants returns every known descendant of a session, parents before
// children.
func (t *SubagentTree) Descendants(sessionID schema.SessionId) []schema.SessionId {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.descendants(sessionID)
}

// SetSessionDirectories records the working directory and additional
// directories of an ordinary session, from its session setup, so Directories
// can resolve what its children inherit.
func (t *SubagentTree) SetSessionDirectories(sessionID schema.SessionId, cwd schema.AbsolutePath, additionalDirectories []schema.AbsolutePath) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.directories[sessionID] = sessionDirectories{cwd: cwd, additional: slices.Clone(additionalDirectories)}
}

// Directories returns a session's effective working directory and additional
// directories. A child's cwd and additionalDirectories that were never
// reported, or were cleared, are inherited independently from its immediate
// parent's effective values; an ordinary session's come from
// SetSessionDirectories. ok is false when a value is inherited from an
// ordinary session without recorded directories.
//
// In v2 they are informational: they tell the client where the child runs,
// since v2 has no client filesystem or terminal methods for them to drive.
func (t *SubagentTree) Directories(sessionID schema.SessionId) (cwd schema.AbsolutePath, additionalDirectories []schema.AbsolutePath, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var cwdFound, additionalFound bool
	for {
		node, isChild := t.nodes[sessionID]
		if !isChild {
			directories, recorded := t.directories[sessionID]
			if !recorded {
				return cwd, additionalDirectories, cwdFound && additionalFound
			}
			if !cwdFound {
				cwd = directories.cwd
			}
			if !additionalFound {
				additionalDirectories = slices.Clone(directories.additional)
			}
			return cwd, additionalDirectories, true
		}
		if association := node.association; !cwdFound && association.Cwd.Set && !association.Cwd.Null {
			cwd, cwdFound = association.Cwd.Value, true
		}
		if association := node.association; !additionalFound && association.AdditionalDirectories.Set && !association.AdditionalDirectories.Null {
			additionalDirectories, additionalFound = slices.Clone(association.AdditionalDirectories.Value), true
		}
		if cwdFound && additionalFound {
			return cwd, additionalDirectories, true
		}
		sessionID = node.parent
	}
}

// Messages returns the session-directed messages in a session's transcript
// in the order they were first seen. A message's participants and
// references come only from its session_message upserts; chunks append to
// its content.
func (t *SubagentTree) Messages(sessionID schema.SessionId) []schema.SessionMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	entries := t.transcripts[sessionID]
	if entries == nil {
		return nil
	}
	messages := make([]schema.SessionMessage, 0, len(entries.order))
	for _, id := range entries.order {
		messages = append(messages, messageSnapshot(*entries.messages[id]))
	}
	return messages
}

// Message returns one session-directed message of a session's transcript.
func (t *SubagentTree) Message(sessionID schema.SessionId, messageID schema.MessageId) (schema.SessionMessage, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entries := t.transcripts[sessionID]; entries != nil {
		if message, ok := entries.messages[messageID]; ok {
			return messageSnapshot(*message), true
		}
	}
	return schema.SessionMessage{}, false
}

func messageSnapshot(message schema.SessionMessage) schema.SessionMessage {
	message.Content.Value = slices.Clip(message.Content.Value)
	return message
}

// LinkedMessage returns the other view of a session-directed message: the
// incoming view an outgoing view names with recipientMessageId, or the
// outgoing view an incoming view names with senderMessageId. Only those
// references link views; equal message IDs in two transcripts do not. The
// views are unlinked when the other view, once received, names a different
// entry. The other view need not have been received yet.
func (t *SubagentTree) LinkedMessage(sessionID schema.SessionId, messageID schema.MessageId) (MessageRef, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entries := t.transcripts[sessionID]
	if entries == nil || entries.messages[messageID] == nil {
		return MessageRef{}, false
	}
	message := entries.messages[messageID]
	sender, recipient := message.SenderSessionID, message.RecipientSessionID
	var other MessageRef
	var outgoing bool
	switch {
	case sender.Set && !sender.Null && sender.Value == sessionID && present(message.RecipientMessageID) && present(recipient):
		other, outgoing = MessageRef{SessionID: recipient.Value, MessageID: message.RecipientMessageID.Value}, true
	case recipient.Set && !recipient.Null && recipient.Value == sessionID && present(message.SenderMessageID) && present(sender):
		other = MessageRef{SessionID: sender.Value, MessageID: message.SenderMessageID.Value}
	default:
		return MessageRef{}, false
	}
	if peer := t.transcripts[other.SessionID]; peer != nil {
		if view := peer.messages[other.MessageID]; view != nil {
			back := view.SenderMessageID
			if !outgoing {
				back = view.RecipientMessageID
			}
			if back.Set && !back.Null && back.Value != messageID {
				return MessageRef{}, false
			}
		}
	}
	return other, true
}

func present[T any](field schema.Nullable[T]) bool {
	return field.Set && !field.Null
}

// CheckClientMethod applies the restricted-session allowlist to a
// client-initiated method addressed to sessionID. Ordinary sessions are not
// restricted. On a child, session/cancel is permitted only while
// Subagent.CanCancel holds, session/status, a read-only query, keeps its
// ordinary rules, and every other method is refused: prompting, resuming,
// closing, deleting, forking, archiving, configuration or title changes,
// tool_call/cancel and terminal/cancel, and any method a later protocol
// revision adds, since only an
// explicit per-child capability enables a mutation. Responses to the agent's
// requests are not client-initiated and always go out.
func (t *SubagentTree) CheckClientMethod(method string, sessionID schema.SessionId) error {
	subagent, ok := t.Subagent(sessionID)
	if !ok {
		return nil
	}
	switch method {
	case schema.SessionCancelMethodName:
		if !subagent.CanCancel() {
			return fmt.Errorf("subagent session %q does not currently permit %s", sessionID, method)
		}
		return nil
	case schema.SessionStatusMethodName:
		return nil
	}
	return fmt.Errorf("%s is not permitted on subagent session %q", method, sessionID)
}

// Cancel sends session/cancel for a child, which cancels its current work,
// not its parent's or its siblings', and fails before writing unless the
// child currently permits it (see CheckClientMethod). The child confirms with
// an idle state whose stop reason is cancelled; Cancel itself changes no
// state. Cancelling also cascades to descendant work the cancelled work waits
// on; the client learns which descendants stopped from their state reports.
func (t *SubagentTree) Cancel(ctx context.Context, connection *acpv2.Connection, sessionID schema.SessionId) error {
	if err := t.CheckClientMethod(schema.SessionCancelMethodName, sessionID); err != nil {
		return err
	}
	if !t.IsSubagent(sessionID) {
		return fmt.Errorf("session %q is not a known subagent session", sessionID)
	}
	return connection.CancelSession(ctx, acpv2.SessionID(sessionID))
}

// Restore runs send, which resumes the ordinary session sessionID, as a
// reconnection of that session's subtree: replay is true for a
// session/resume with replayFrom {"type": "start"}, which replays history,
// and false for one without replayFrom. It
// first invalidates the effective capabilities and current state of every
// known descendant; child mutations stay disabled until send succeeds.
//
// With replay, associations and state updates received before the
// successful response are history: they update Subagent.Association but
// never the effective capabilities or current state, and transcripts of the session and its
// descendants restart empty, since the replay sends them again. Snapshots
// received after the response are current. Without replay, snapshots
// received during the attempt are current observations, but CanCancel stays
// false until send returns. When send fails, everything received during the
// attempt is invalidated again.
//
// send must make its request with the context it is given: the response to
// the first session/resume made with it marks the boundary in
// wire order with the notifications around it; other calls made with it do
// not. Restoring a session again while an attempt is in progress starts a new
// attempt, and the earlier one no longer changes the tree.
func (t *SubagentTree) Restore(ctx context.Context, sessionID schema.SessionId, replay bool, send func(context.Context) error) error {
	attempt := &restoreAttempt{replay: replay}
	t.mu.Lock()
	if t.nodes[sessionID] != nil {
		t.mu.Unlock()
		return fmt.Errorf("subagent session %q cannot be resumed directly", sessionID)
	}
	// A later attempt replaces one still in progress, which then no longer
	// changes the tree when it completes.
	t.restores[sessionID] = attempt
	subtree := t.descendants(sessionID)
	t.invalidate(subtree)
	delete(t.states, sessionID)
	if replay {
		delete(t.transcripts, sessionID)
		for _, id := range subtree {
			delete(t.transcripts, id)
		}
	}
	t.mu.Unlock()

	settled := false
	err := send(rpcctl.WithSettled(ctx, func(method string, err error) {
		if !restoreMethod(method) {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		// Only the first such response made with this context is the
		// boundary.
		if !settled {
			settled = true
			attempt.answered = err == nil
		}
	}))

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.restores[sessionID] == attempt {
		delete(t.restores, sessionID)
		if err != nil {
			t.invalidate(t.descendants(sessionID))
			delete(t.states, sessionID)
		}
	}
	return err
}

// Closed records that the client closed the ordinary session sessionID,
// which ends the work of all its descendants, and returns them, parents
// first, so the client can resolve their pending permission and elicitation
// requests together with the parent's. Their effective capabilities and
// current state are invalidated; associations and transcripts stay, since
// closing does not erase history.
func (t *SubagentTree) Closed(sessionID schema.SessionId) []schema.SessionId {
	t.mu.Lock()
	defer t.mu.Unlock()
	subtree := t.descendants(sessionID)
	t.invalidate(subtree)
	return subtree
}

// Disconnected records the loss of the ACP connection: no child keeps a
// confirmed current state or an effective capability, since neither a
// running state nor a control survives the connection. Associations and
// transcripts stay for display. State reported after reconnecting, through
// Restore of the parent, is authoritative.
func (t *SubagentTree) Disconnected() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, node := range t.nodes {
		node.capabilities = nil
		node.state = nil
	}
	clear(t.restores)
	clear(t.states)
}

// restoreMethod reports whether a response to method can mark a restore's
// boundary.
func restoreMethod(method string) bool {
	return method == schema.SessionResumeMethodName
}
