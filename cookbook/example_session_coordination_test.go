package cookbook_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/BrokkAi/acp-go/acptest"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// sessions shares draft-v2 sessions between independent parts of one
// application. The first Open of a session resumes it with replay, concurrent
// Opens join that resume, releasing the last lease closes the session, and an
// Open during a close waits for it and then resumes afresh.
//
// One coordinator goroutine (run) owns every operation. Application calls and
// SDK callbacks only send it messages, and it never waits for the network: it
// starts requests on their own goroutines and learns their results from the
// same FIFO that carries session updates.
type sessions struct {
	ctx            context.Context
	connection     *acpv2.Connection
	initialization acpv2.Initialization
	directory      string
	commands       chan sessionCommand // unbuffered: a sent command is always handled
	wire           chan wireEvent
	stopped        chan struct{}
	tickets        atomic.Uint64
	operations     map[acpv2.SessionID]*operation // owned by run
}

var errSessionsStopped = errors.New("session coordinator stopped")

// sessionView is the shared result of one resume: its complete response and a
// lossless log of the replayed and later updates. It is a log, not a reducer.
type sessionView struct {
	Response schema.ResumeSessionResponse

	mu           sync.Mutex
	updates      []schema.SessionUpdate
	disconnected bool
}

func (v *sessionView) Updates() []schema.SessionUpdate {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]schema.SessionUpdate(nil), v.updates...)
}

// Disconnected reports that the connection ended while the view was open.
func (v *sessionView) Disconnected() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.disconnected
}

func (v *sessionView) append(update schema.SessionUpdate) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.updates = append(v.updates, update)
}

func (v *sessionView) disconnect() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.disconnected = true
}

// lease keeps its session open until Release.
type lease struct {
	sessions  *sessions
	sessionID acpv2.SessionID
	ticket    uint64
	view      *sessionView
	once      sync.Once
}

func (l *lease) View() *sessionView { return l.view }

func (l *lease) Release() {
	l.once.Do(func() {
		l.sessions.send(sessionCommand{sessionID: l.sessionID, ticket: l.ticket})
	})
}

// sessionCommand acquires a session when reply is set and releases the
// ticket otherwise.
type sessionCommand struct {
	sessionID acpv2.SessionID
	ticket    uint64
	reply     chan openResult
}

type openResult struct {
	view *sessionView
	err  error
}

type wireKind int

const (
	wireUpdate wireKind = iota
	wireResumed
	wireSessionClosed
	wireDisconnected
)

// wireEvent is one entry in the single FIFO for inbound traffic. Updates are
// queued by the notification callback in wire order; request results are
// queued after their call returns, behind every update that preceded them.
type wireEvent struct {
	kind      wireKind
	sessionID acpv2.SessionID
	update    schema.SessionUpdate
	response  schema.ResumeSessionResponse
	err       error
}

type phase int

const (
	resuming phase = iota // one resume in flight; waiters join it
	ready                 // leases share the view; later updates append to it
	closing               // one close in flight; new opens wait in next
	blocked               // close failed: remote state is uncertain, so never reopen
)

type operation struct {
	phase     phase
	waiters   map[uint64]chan openResult // resuming
	replay    []schema.SessionUpdate     // resuming
	abandoned bool                       // resuming: every waiter gave up
	leases    map[uint64]bool            // ready
	view      *sessionView               // ready
	next      map[uint64]chan openResult // opens queued behind an abandoned resume or a close
	err       error                      // blocked
}

// withSessions initializes one connection, runs the coordinator, and hands it
// to application. When application returns, the connection is closed without
// waiting for outstanding session closes.
func withSessions(ctx context.Context, in io.ReadCloser, out io.WriteCloser, directory string, application func(*sessions) error) error {
	s := &sessions{
		ctx:        ctx,
		directory:  directory,
		commands:   make(chan sessionCommand),
		wire:       make(chan wireEvent, 64),
		stopped:    make(chan struct{}),
		operations: make(map[acpv2.SessionID]*operation),
	}
	s.connection = acpv2.Connect(in, out,
		acpv2.HandlePermissions(nil, cancelPermissionsV2{}),
		acpv2.SessionUpdates(func(update acpv2.Update) error {
			s.deliver(wireEvent{kind: wireUpdate, sessionID: update.SessionID, update: update.Update})
			return nil
		}),
	)
	initialization, err := s.connection.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{
		Name: "cookbook-sessions", Version: "0.1.0",
	})
	if err != nil {
		close(s.stopped)
		_ = s.connection.Close()
		return err
	}
	s.initialization = initialization
	go func() {
		// On EOF every update is queued before Done closes.
		<-s.connection.Done()
		s.deliver(wireEvent{kind: wireDisconnected})
	}()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.run()
	}()

	err = application(s)
	_ = s.connection.Close()
	<-finished
	return err
}

// Open returns a lease on sessionID's shared view, resuming the session with
// replay unless another lease already holds it open.
func (s *sessions) Open(ctx context.Context, sessionID acpv2.SessionID) (*lease, error) {
	ticket := s.tickets.Add(1)
	reply := make(chan openResult, 1)
	if !s.send(sessionCommand{sessionID: sessionID, ticket: ticket, reply: reply}) {
		return nil, errSessionsStopped
	}
	select {
	case result := <-reply:
		if result.err != nil {
			return nil, result.err
		}
		return &lease{sessions: s, sessionID: sessionID, ticket: ticket, view: result.view}, nil
	case <-ctx.Done():
		// Abandon the open: the coordinator drops the waiter, or releases the
		// lease if the resume already succeeded.
		s.send(sessionCommand{sessionID: sessionID, ticket: ticket})
		return nil, ctx.Err()
	}
}

func (s *sessions) send(command sessionCommand) bool {
	select {
	case s.commands <- command:
		return true
	case <-s.stopped:
		return false
	}
}

func (s *sessions) deliver(event wireEvent) {
	select {
	case s.wire <- event:
	case <-s.stopped:
	}
}

func (s *sessions) run() {
	defer s.stop()
	for {
		// Prefer queued application decisions over wire events, so an
		// already-sent release is honored before a response is exposed.
		select {
		case command := <-s.commands:
			s.command(command)
			continue
		default:
		}
		select {
		case command := <-s.commands:
			s.command(command)
		case event := <-s.wire:
			if event.kind == wireDisconnected {
				return
			}
			s.receive(event)
		}
	}
}

func (s *sessions) command(command sessionCommand) {
	current := s.operations[command.sessionID]
	if command.reply == nil {
		s.release(command.sessionID, current, command.ticket)
		return
	}
	switch {
	case current == nil:
		s.resume(command.sessionID, map[uint64]chan openResult{command.ticket: command.reply})
	case current.phase == resuming && !current.abandoned:
		current.waiters[command.ticket] = command.reply
	case current.phase == ready:
		current.leases[command.ticket] = true
		command.reply <- openResult{view: current.view}
	case current.phase == blocked:
		command.reply <- openResult{err: current.err}
	default:
		current.next[command.ticket] = command.reply
	}
}

func (s *sessions) release(sessionID acpv2.SessionID, current *operation, ticket uint64) {
	if current == nil {
		return
	}
	delete(current.next, ticket)
	switch current.phase {
	case resuming:
		delete(current.waiters, ticket)
		if len(current.waiters) == 0 {
			// Keep the in-flight request's slot, but drop its data.
			current.abandoned, current.replay = true, nil
		}
	case ready:
		if !current.leases[ticket] {
			return
		}
		delete(current.leases, ticket)
		if len(current.leases) == 0 {
			current.phase, current.view = closing, nil
			s.close(sessionID)
		}
	}
}

func (s *sessions) receive(event wireEvent) {
	current := s.operations[event.sessionID]
	if current == nil {
		return
	}
	switch event.kind {
	case wireUpdate:
		switch {
		case current.phase == resuming && !current.abandoned:
			current.replay = append(current.replay, event.update)
		case current.phase == ready:
			current.view.append(event.update)
		} // Abandoned replay and closing traffic have no recipient.
	case wireResumed:
		if event.err != nil {
			fail(current.waiters, event.err)
			delete(s.operations, event.sessionID)
			if len(current.next) > 0 {
				s.resume(event.sessionID, current.next)
			}
			return
		}
		if current.abandoned {
			// Close before any queued open can register a new replay target.
			current.phase = closing
			s.close(event.sessionID)
			return
		}
		view := &sessionView{Response: event.response, updates: current.replay}
		current.phase, current.view, current.leases = ready, view, make(map[uint64]bool)
		for ticket, reply := range current.waiters {
			current.leases[ticket] = true
			reply <- openResult{view: view}
		}
		current.waiters, current.replay = nil, nil
	case wireSessionClosed:
		if event.err != nil {
			current.phase, current.err = blocked, event.err
			fail(current.next, event.err)
			current.next = make(map[uint64]chan openResult)
			return
		}
		delete(s.operations, event.sessionID)
		if len(current.next) > 0 {
			s.resume(event.sessionID, current.next)
		}
	}
}

func (s *sessions) resume(sessionID acpv2.SessionID, waiters map[uint64]chan openResult) {
	s.operations[sessionID] = &operation{
		phase:   resuming,
		waiters: waiters,
		next:    make(map[uint64]chan openResult),
	}
	go func() {
		response, err := s.connection.ResumeSessionFromStart(s.ctx, s.initialization, sessionID, s.directory, nil)
		s.deliver(wireEvent{kind: wireResumed, sessionID: sessionID, response: response, err: err})
	}()
}

func (s *sessions) close(sessionID acpv2.SessionID) {
	go func() {
		err := s.connection.CloseSession(s.ctx, s.initialization, sessionID)
		s.deliver(wireEvent{kind: wireSessionClosed, sessionID: sessionID, err: err})
	}()
}

// stop fails every waiting open and marks open views disconnected. Results
// of requests still in flight are dropped.
func (s *sessions) stop() {
	close(s.stopped)
	for _, current := range s.operations {
		fail(current.waiters, errSessionsStopped)
		fail(current.next, errSessionsStopped)
		if current.view != nil {
			current.view.disconnect()
		}
	}
}

func fail(waiters map[uint64]chan openResult, err error) {
	for _, reply := range waiters {
		reply <- openResult{err: err}
	}
}

func Example_v2SessionCoordination() {
	ctx := context.Background()
	notes := acpv2.SessionID("release-notes")
	fixture := newHistoryAgent(map[schema.SessionId][]schema.SessionUpdate{
		notes: {
			agentText("m1", "Drafted the release notes."),
			agentText("m2", "Added the upgrade section."),
		},
	})
	link := acptest.NewPair()
	go agentv2.New(fixture).Serve(ctx, link.B, link.B)

	directory, err := os.Getwd()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	err = withSessions(ctx, link.A, link.A, directory, func(sessions *sessions) error {
		// Two parts of the application open the same session concurrently and
		// share one resume and its replay.
		var first, second *lease
		var firstErr, secondErr error
		var opening sync.WaitGroup
		opening.Add(2)
		go func() { defer opening.Done(); first, firstErr = sessions.Open(ctx, notes) }()
		go func() { defer opening.Done(); second, secondErr = sessions.Open(ctx, notes) }()
		opening.Wait()
		if err := errors.Join(firstErr, secondErr); err != nil {
			return err
		}
		resumes, closes := fixture.counts(notes)
		fmt.Printf("shared view: %t, replayed %d updates, resumes %d, closes %d\n",
			first.View() == second.View(), len(first.View().Updates()), resumes, closes)

		// Releasing the last lease closes the session on the agent. An Open
		// that arrives meanwhile waits for the close, then resumes afresh.
		first.Release()
		second.Release()
		reopened, err := sessions.Open(ctx, notes)
		if err != nil {
			return err
		}
		defer reopened.Release()
		resumes, closes = fixture.counts(notes)
		fmt.Printf("fresh view: %t, replayed %d updates, resumes %d, closes %d\n",
			reopened.View() != first.View(), len(reopened.View().Updates()), resumes, closes)
		return nil
	})
	if err != nil {
		fmt.Println("error:", err)
	}
	// Output:
	// shared view: true, replayed 2 updates, resumes 1, closes 0
	// fresh view: true, replayed 2 updates, resumes 2, closes 1
}
