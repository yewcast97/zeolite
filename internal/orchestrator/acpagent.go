package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"zeolite/internal/sandbox"
	"zeolite/internal/sandboximage"
)

// ACPAgent is an agent that a sandbox's ACP Bridge had started in the
// sandbox's workstation: its standard streams, on which it speaks the
// Agent Client Protocol. What is written to it is the agent's standard
// input, and what is read from it the agent's standard output. The
// streams are carried by the Docker API: nothing listens for them on
// the Mac, and no port of the sandbox is published.
type ACPAgent struct {
	docker       *client.Client
	stream       *execStream
	sharedFolder string
}

// acpAgentCommand is the command that starts an agent in a workstation
// for an ACP Bridge: the supervisor's, with the command by which the
// agent speaks the protocol and then what its client added, each a
// word of its own.
func acpAgentCommand(agent sandbox.Agent, args []string) []string {
	command := append([]string{sandboximage.SupervisorPath, "acp"}, agent.ACP...)
	return append(command, args...)
}

// StartACPAgent starts an agent in the workstation of a running
// sandbox, for the sandbox's ACP Bridge: the one way the app starts an
// agent, and the agent is the one named, by its Agent Executable. args
// are added to the command by which the agent speaks the protocol.
//
// It asks the Docker daemon at endpoint, on a connection of its own,
// about the workstation as it runs, and takes nothing a saved config
// says of it: the sandbox was started with an ACP Bridge where its
// workstation has the label that says so, and its Shared Folder is
// where the workstation mounts it from. A sandbox that does not run,
// one that was started without an ACP Bridge, and an agent that is not
// known to speak the protocol are refused, in words for the user.
func StartACPAgent(ctx context.Context, endpoint, sandboxID, agent string, args []string) (*ACPAgent, error) {
	known, ok := sandbox.KnownAgent(agent)
	if !ok {
		var names []string
		for _, a := range sandbox.KnownAgents {
			if len(a.ACP) > 0 {
				names = append(names, a.Executable)
			}
		}
		return nil, fmt.Errorf("%q is no agent that can be started here: name one of %s", agent, strings.Join(names, ", "))
	}
	if len(known.ACP) == 0 {
		return nil, fmt.Errorf("%s is not known to speak the Agent Client Protocol", known.Title)
	}
	if !sandbox.ValidID(sandboxID) {
		return nil, fmt.Errorf("%q is no sandbox's ID", sandboxID)
	}
	docker, err := client.New(client.WithHost(endpoint))
	if err != nil {
		return nil, err
	}
	a, err := startACPAgent(ctx, docker, sandboxID, known, args)
	if err != nil {
		docker.Close()
		return nil, err
	}
	return a, nil
}

func startACPAgent(ctx context.Context, docker *client.Client, sandboxID string, agent sandbox.Agent, args []string) (*ACPAgent, error) {
	name := WorkstationContainer(sandboxID)
	inspected, err := docker.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case cerrdefs.IsNotFound(err):
		return nil, errors.New("the sandbox is not running: start it in zeolite")
	case err != nil:
		return nil, fmt.Errorf("the Docker daemon does not say whether the sandbox runs: %w", err)
	}
	workstation := inspected.Container
	if workstation.State == nil || !workstation.State.Running || workstation.Config == nil ||
		workstation.Config.Labels[labelSandbox] != sandboxID || workstation.Config.Labels[labelRole] != roleWorkstation {
		return nil, errors.New("the sandbox is not running: start it in zeolite")
	}
	if workstation.Config.Labels[labelACPBridge] != "true" {
		return nil, errors.New("the sandbox was not started with an ACP Bridge: give it one in its Setup, which starts it again")
	}
	sharedFolder := ""
	for _, m := range workstation.Mounts {
		if m.Destination == sandbox.SharedFolderPath && m.Type == mount.TypeBind {
			sharedFolder = m.Source
		}
	}
	if sharedFolder == "" {
		return nil, errors.New("the sandbox's workstation mounts no Shared Folder, in which an ACP Bridge's agent works")
	}
	// The agent starts in the Shared Folder, as the agent's user, with
	// the workstation's own environment and none of this Mac's. What the
	// supervisor says on its standard error is not read.
	stream, err := openExecStream(ctx, docker, workstation.ID, sandboximage.WorkstationUser, sandbox.SharedFolderPath, io.Discard, acpAgentCommand(agent, args)...)
	if err != nil {
		return nil, fmt.Errorf("starting %s in the sandbox's workstation: %w", agent.Title, err)
	}
	return &ACPAgent{docker: docker, stream: stream, sharedFolder: sharedFolder}, nil
}

func (a *ACPAgent) Read(p []byte) (int, error)  { return a.stream.Read(p) }
func (a *ACPAgent) Write(p []byte) (int, error) { return a.stream.Write(p) }

// Close ends the agent's standard input and the connection: the
// supervisor then ends every process of the agent's session.
func (a *ACPAgent) Close() error {
	a.stream.stream.CloseWrite()
	return a.stream.Close()
}

// SharedFolder is the sandbox's Shared Folder as its path on the Mac,
// as the running workstation mounts it: the folder the agent has at
// sandbox.SharedFolderPath.
func (a *ACPAgent) SharedFolder() string { return a.sharedFolder }

// Wait waits until the command that started the agent has ended, and
// returns its exit status: the agent's, or 127 where the workstation
// has no such program. It then closes the connection to Docker.
func (a *ACPAgent) Wait(ctx context.Context) (int, error) {
	defer a.docker.Close()
	for {
		inspected, err := a.docker.ExecInspect(ctx, a.stream.id, client.ExecInspectOptions{})
		if err != nil {
			return 0, err
		}
		if !inspected.Running {
			return inspected.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
