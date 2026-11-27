package orchestrator

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"zeolite/internal/sandboximage"
)

// execStream is the standard streams of a command run in a container,
// as one connection: what is written to it is the command's standard
// input, and what is read from it the command's standard output. It is
// carried by the Docker API, so nothing listens for it and no port of a
// sandbox is published.
type execStream struct {
	// id is the exec's, by which Docker says whether the command ended
	// and how.
	id     string
	stream client.HijackedResponse
	out    *io.PipeReader
}

// openExecStream runs a command in a container, as a user and in a
// folder, the container's own where dir is empty, and returns its
// streams. What the command writes on standard error goes to errOut.
func openExecStream(ctx context.Context, docker *client.Client, container, user, dir string, errOut io.Writer, command ...string) (*execStream, error) {
	created, err := docker.ExecCreate(ctx, container, client.ExecCreateOptions{
		User:         user,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   dir,
		Cmd:          command,
	})
	if err != nil {
		return nil, err
	}
	attached, err := docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	// Without a TTY, Docker frames standard output and error together.
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, errOut, attached.Reader)
		pw.CloseWithError(err)
	}()
	return &execStream{id: created.ID, stream: attached.HijackedResponse, out: pr}, nil
}

func (s *execStream) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *execStream) Write(p []byte) (int, error) { return s.stream.Conn.Write(p) }

// Close ends the connection: the command's standard input ends, and
// what it still writes goes nowhere.
func (s *execStream) Close() error {
	s.stream.Close()
	return s.out.Close()
}

// DialWorkstation connects to a port on the 127.0.0.1 of a sandbox's
// workstation, as a Viewport Stream needs to reach a pack's daemon. The
// connection is the streams of a socat run in the workstation.
func (o *Orchestrator) DialWorkstation(ctx context.Context, sandboxID string, port int) (io.ReadWriteCloser, error) {
	o.mu.Lock()
	p := o.running[sandboxID]
	o.mu.Unlock()
	if p == nil || p.workstation == "" {
		return nil, fmt.Errorf("the sandbox is not running")
	}
	stream, err := openExecStream(ctx, o.docker, p.workstation, sandboximage.WorkstationUser, "", io.Discard, "socat", "-", "TCP4:127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return nil, fmt.Errorf("connecting to the workstation: %w", err)
	}
	return stream, nil
}
