package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"zeolite/internal/sandbox"
	"zeolite/internal/sandboximage"
)

// TerminalSession is the connection of one Terminal Session: a login
// shell in a sandbox's workstation, on a TTY. What it prints is read
// from the connection and what is typed is written to it.
type TerminalSession struct {
	o         *Orchestrator
	pod       *pod
	container string
	id        string
	execID    string
	stream    client.HijackedResponse
	closeOnce sync.Once
}

// sessionCommand is the command a Terminal Session runs in the
// workstation. The supervisor records it as this session's, so that
// Close can end it: Docker would leave it running.
func sessionCommand(id string) []string {
	return []string{sandboximage.SupervisorPath, "shell", id}
}

// OpenTerminalSession starts a Terminal Session in the sandbox's
// workstation: a login shell, as the agent's user, in /workspace.
// Whatever else runs in it, its user starts there by typing.
func (o *Orchestrator) OpenTerminalSession(ctx context.Context, sandboxID string) (*TerminalSession, error) {
	o.mu.Lock()
	p := o.running[sandboxID]
	o.mu.Unlock()
	if p == nil || p.workstation == "" {
		return nil, fmt.Errorf("the sandbox is not running")
	}
	raw := make([]byte, 8)
	rand.Read(raw)
	id := hex.EncodeToString(raw)

	size := client.ConsoleSize{Height: 24, Width: 80}
	created, err := o.docker.ExecCreate(ctx, p.workstation, client.ExecCreateOptions{
		User:         sandboximage.WorkstationUser,
		TTY:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		ConsoleSize:  size,
		Env:          []string{"TERM=xterm-256color"},
		WorkingDir:   sandbox.HomePath,
		Cmd:          sessionCommand(id),
	})
	if err != nil {
		return nil, fmt.Errorf("starting a terminal session: %w", err)
	}
	attached, err := o.docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if err != nil {
		return nil, fmt.Errorf("starting a terminal session: %w", err)
	}
	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = map[string]bool{}
	}
	p.sessions[id] = true
	p.mu.Unlock()
	return &TerminalSession{o: o, pod: p, container: p.workstation, id: id, execID: created.ID, stream: attached.HijackedResponse}, nil
}

// HasSession reports whether a Terminal Session of this ID is open in a
// sandbox's workstation: one the app opened there, and has not closed.
// What a workstation says of a session is taken only for such a one.
func (o *Orchestrator) HasSession(sandboxID, sessionID string) bool {
	o.mu.Lock()
	p := o.running[sandboxID]
	o.mu.Unlock()
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[sessionID]
}

// ID is the session's ID in its workstation: the name under which the
// supervisor recorded it, and reports on it.
func (s *TerminalSession) ID() string { return s.id }

func (s *TerminalSession) Read(p []byte) (int, error)  { return s.stream.Reader.Read(p) }
func (s *TerminalSession) Write(p []byte) (int, error) { return s.stream.Conn.Write(p) }

// Resize tells the shell's TTY the size of the terminal that shows it.
func (s *TerminalSession) Resize(cols, rows int) error {
	_, err := s.o.docker.ExecResize(context.Background(), s.execID, client.ExecResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	})
	return err
}

// Close ends the session: the connection closes, and the shell and
// everything started from it are hung up.
func (s *TerminalSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.pod.mu.Lock()
		delete(s.pod.sessions, s.id)
		s.pod.mu.Unlock()
		s.stream.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _, err = s.o.exec(ctx, s.container, sandboximage.WorkstationUser, nil, sandboximage.SupervisorPath, "hangup", s.id)
	})
	return err
}
