// Package acpbridge is what a sandbox's ACP Bridge passes: it reads the
// messages of the Agent Client Protocol that an ACP client on this Mac
// and an agent in the sandbox's workstation send each other, judges
// each, and writes what crosses anew.
//
// A workstation is not trusted, and the protocol lets an agent ask its
// client to read and write files and to run commands where the client
// runs. So nothing is passed on as it was written, either way. A
// message is read with each member under its exact name, rebuilt from
// the members this package knows, and written again: what it does not
// know by name does not cross, and `_meta` crosses nowhere. The shapes
// are those of the protocol's first version, as its schema 1.35.0 of
// 23 November 2026 has them.
//
// The agent is told that its client offers nothing: no files, no
// terminal, no login in a terminal, no forms and none of the kinds of
// update a client has to ask for. What an agent asks of its client all
// the same, this package answers itself, with an error; the client
// never sees it. The agent works in the sandbox's Shared Folder: a path
// in it is written as each side has it, a session whose folder is
// outside it is refused, and no other path of this Mac reaches the
// workstation, nor any other path of the workstation the client, as a
// path. Paths are mapped by their text alone: the package reads no file
// and follows no link, starts nothing and opens nothing.
package acpbridge

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"

	"zeolite/internal/sandbox"
)

// maxAgentRequests is how many of the agent's requests may wait for the
// client's answer at once: more than an agent asks its user at a time,
// and few enough that a workstation cannot make this Mac hold any
// number of them.
const maxAgentRequests = 64

// The protocol's methods that cross, by who asks.
const (
	methodInitialize      = "initialize"
	methodAuthenticate    = "authenticate"
	methodLogout          = "logout"
	methodNewSession      = "session/new"
	methodLoadSession     = "session/load"
	methodResumeSession   = "session/resume"
	methodListSessions    = "session/list"
	methodDeleteSession   = "session/delete"
	methodCloseSession    = "session/close"
	methodSetMode         = "session/set_mode"
	methodSetConfigOption = "session/set_config_option"
	methodPrompt          = "session/prompt"
	methodCancel          = "session/cancel"

	methodUpdate            = "session/update"
	methodRequestPermission = "session/request_permission"

	methodCancelRequest = "$/cancel_request"
)

// JSON-RPC's error codes, as the bridge answers with them.
const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// refusal is why the bridge answers a request itself, with an error.
type refusal struct {
	code    int
	message string
	// data is what an agent's own error says besides, where it is one
	// that is passed on.
	data any
}

// bridge is one ACP Bridge at work: the folder both sides have, the two
// ways out, and the requests of each side that the other has not
// answered yet, each under its id with its method.
type bridge struct {
	folder   folder
	toClient *lineWriter
	toAgent  *lineWriter

	mu          sync.Mutex
	clientAsked map[string]string
	agentAsked  map[string]string
}

func newBridge(clientOut, workstation io.Writer, sharedFolder string) (*bridge, error) {
	f, err := newFolder(sharedFolder)
	if err != nil {
		return nil, err
	}
	return &bridge{
		folder:      f,
		toClient:    &lineWriter{w: clientOut},
		toAgent:     &lineWriter{w: workstation},
		clientAsked: map[string]string{},
		agentAsked:  map[string]string{},
	}, nil
}

// Serve passes the protocol's messages between an ACP client, read
// from client and written to clientOut, and the agent whose standard
// streams workstation is, until either stream ends: then it closes
// workstation, which ends the agent, and returns why it ended, or nil
// where a stream simply ended. sharedFolder is the sandbox's Shared
// Folder as its path on this Mac, which the agent has at
// sandbox.SharedFolderPath.
//
// What the client still sends after the agent's stream ended is not
// read to its end: the caller's process ends then.
func Serve(client io.Reader, clientOut io.Writer, workstation io.ReadWriteCloser, sharedFolder string) error {
	b, err := newBridge(clientOut, workstation, sharedFolder)
	if err != nil {
		workstation.Close()
		return err
	}
	ended := make(chan error, 2)
	go func() { ended <- b.readClient(client) }()
	go func() { ended <- b.readAgent(workstation) }()
	err = <-ended
	workstation.Close()
	return err
}

// readClient takes the client's messages until its stream ends. A line
// too long is dropped: the client is this Mac's user's own program, and
// its next message is taken.
func (b *bridge) readClient(r io.Reader) error {
	in := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := sandbox.ReadLine(in, sandbox.ACPMaxLine)
		if errors.Is(err, sandbox.ErrLineTooLong) {
			if err = sandbox.SkipLine(in); err == nil {
				continue
			}
		}
		if err != nil {
			return streamEnd(err)
		}
		if err := b.clientLine(line); err != nil {
			return err
		}
	}
}

// readAgent takes the agent's messages until its stream ends. A line
// too long ends the bridge: a workstation that sends one is given no
// more of this Mac's time to read it.
func (b *bridge) readAgent(r io.Reader) error {
	in := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := sandbox.ReadLine(in, sandbox.ACPMaxLine)
		if errors.Is(err, sandbox.ErrLineTooLong) {
			return fmt.Errorf("the agent sent %w", err)
		}
		if err != nil {
			return streamEnd(err)
		}
		if err := b.agentLine(line); err != nil {
			return err
		}
	}
}

// streamEnd is why a stream's reading ended, nil where the stream
// simply ended or was closed.
func streamEnd(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

// clientLine judges one line the client sent, and writes what follows
// from it: the message to the agent, or the bridge's own answer to the
// client. What is no message of the protocol is dropped. It returns an
// error only where nothing can be written any more.
func (b *bridge) clientLine(line []byte) error {
	m, ok := read(line)
	if !ok {
		return nil
	}
	switch m.kind {
	case isRequest:
		// A second request under an id that is still being answered is
		// refused, and not answered: an answer would be taken for the
		// first one's.
		if b.asked(b.clientAsked, m.key) {
			return nil
		}
		params, refused := b.requestToAgent(m.method, m.params)
		if refused != nil {
			return b.toClient.send(failure(m.id, *refused))
		}
		b.ask(b.clientAsked, m.key, m.method)
		return b.toAgent.send(request(m.id, m.method, params))
	case isNotification:
		// That a session's turn is cancelled, and that a request the
		// client made is withdrawn.
		var params object
		switch m.method {
		case methodCancel:
			params = object{}
			ok = needText(params, m.params, "sessionId")
		case methodCancelRequest:
			params, ok = b.withdrawal(m.params, b.clientAsked)
		default:
			ok = false
		}
		if ok {
			return b.toAgent.send(notification(m.method, params))
		}
	case isResult, isFailure:
		// The client answers what the agent asked it: a permission.
		if _, asked := b.answered(b.agentAsked, m.key); !asked {
			return nil
		}
		if m.kind == isFailure {
			return b.toAgent.send(failure(m.id, errorOf(m.failure)))
		}
		outcome, ok := permissionOutcome(m.result)
		if !ok {
			return b.toAgent.send(failure(m.id, refusal{code: codeInternalError, message: "the client's answer was not one the protocol has: ask again"}))
		}
		return b.toAgent.send(result(m.id, outcome))
	}
	return nil
}

// agentLine judges one line the agent sent, and writes what follows
// from it: the message to the client, or the bridge's own answer to the
// agent. What is no message of the protocol is dropped. It returns an
// error where nothing can be written any more, and where the agent
// keeps more requests waiting than it may.
func (b *bridge) agentLine(line []byte) error {
	m, ok := read(line)
	if !ok {
		return nil
	}
	switch m.kind {
	case isRequest:
		if b.asked(b.agentAsked, m.key) {
			return nil
		}
		// What an agent asks of its client is a permission, or is
		// answered here: the client never sees it.
		if m.method != methodRequestPermission {
			return b.toAgent.send(failure(m.id, notOffered(m.method)))
		}
		params, ok := b.permissionRequest(m.params)
		if !ok {
			return b.toAgent.send(failure(m.id, refusal{code: codeInvalidParams, message: "the request is not as version 1 of the protocol has it"}))
		}
		if b.waiting(b.agentAsked) >= maxAgentRequests {
			return fmt.Errorf("the agent keeps more than %d requests waiting for an answer", maxAgentRequests)
		}
		b.ask(b.agentAsked, m.key, m.method)
		return b.toClient.send(request(m.id, m.method, params))
	case isNotification:
		// What a session did, and that a request the agent made is
		// withdrawn.
		var params object
		switch m.method {
		case methodUpdate:
			params, ok = b.sessionUpdate(m.params)
		case methodCancelRequest:
			params, ok = b.withdrawal(m.params, b.agentAsked)
		default:
			ok = false
		}
		if ok {
			return b.toClient.send(notification(m.method, params))
		}
	case isResult, isFailure:
		method, asked := b.answered(b.clientAsked, m.key)
		if !asked {
			return nil
		}
		if m.kind == isFailure {
			return b.toClient.send(failure(m.id, errorToClient(m.failure)))
		}
		rebuilt, ok := b.resultToClient(method, m.result)
		if !ok {
			return b.toClient.send(failure(m.id, refusal{code: codeInternalError, message: fmt.Sprintf("zeolite's ACP Bridge: the agent answered %s with what version 1 of the Agent Client Protocol does not have", method)}))
		}
		return b.toClient.send(result(m.id, rebuilt))
	}
	return nil
}

// withdrawal rebuilds the notification that a request is withdrawn: it
// crosses while that request, of the side that sends it, waits for its
// answer.
func (b *bridge) withdrawal(params object, asked map[string]string) (object, bool) {
	id, key, ok := requestID(params["requestId"])
	if !ok || !b.asked(asked, key) {
		return nil, false
	}
	return object{"requestId": id}, true
}

func (b *bridge) asked(table map[string]string, key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, asked := table[key]
	return asked
}

func (b *bridge) ask(table map[string]string, key, method string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	table[key] = method
}

// answered takes a request off those that wait, and says what it asked.
func (b *bridge) answered(table map[string]string, key string) (method string, asked bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	method, asked = table[key]
	delete(table, key)
	return method, asked
}

func (b *bridge) waiting(table map[string]string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(table)
}

// lineWriter writes one message on a line, whole, whoever else writes.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) send(m object) error {
	line, err := write(m)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.Write(line)
	return err
}
