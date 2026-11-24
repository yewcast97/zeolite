package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"zeolite/internal/sandbox"
)

// The MCP server sandbox is how an agent tells its user of its state.
// `supervisor state` is that server: its tool hands what the agent says
// to the sandbox's Link, with the Terminal Session the agent runs in.
//
// An agent's client starts the server, so the server descends from the
// agent, and the agent from its session's shell: the session is found
// by going up from this process to the one the supervisor recorded as a
// session's. Nothing is read of a terminal, and nothing is written to
// one: a raised hand carries no words.

// procDir is where the kernel shows the workstation's processes.
var procDir = "/proc"

// sessionLeaders are the processes recorded as Terminal Sessions, each
// with its session's ID.
func sessionLeaders() (map[int]string, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	leaders := map[int]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(sessionDir, e.Name()))
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
			leaders[pid] = e.Name()
		}
	}
	return leaders, nil
}

// processParent is the process that started a process, or that it was
// left to.
func processParent(pid int) (int, bool) {
	stat, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return statField(string(stat), 1)
}

// errNoSession is why a tool of the server cannot say anything of its
// agent's session.
var errNoSession = errors.New("this server was not started from a terminal session: there is no session to call your user to")

// ownSession is the ID of the Terminal Session this process descends
// from.
func ownSession() (string, error) {
	leaders, err := sessionLeaders()
	if err != nil {
		return "", err
	}
	for pid := os.Getpid(); pid > 1; {
		if id, leads := leaders[pid]; leads {
			return id, nil
		}
		parent, ok := processParent(pid)
		if !ok {
			break
		}
		pid = parent
	}
	return "", errNoSession
}

// What an agent is told of raising its hand: once in the server's
// instructions, and once more, whole, as the tool's description. It is
// said as the agent uses the tool: what it is for, how it is called and
// when. How its user is shown the hand is the app's to know.
const raiseHandWords = "Draw your user's attention to this session, for them to step in. " +
	"It takes no arguments and carries no words: first say what you need in your own output, then call this, then wait there for the answer. " +
	"Call it only when you cannot go on without your user: a decision that is theirs to make, a secret or a permission you lack. " +
	"Do not call it to report progress."

// stateServer is the MCP server sandbox, whose tool speaks for the
// Terminal Session session reports, through the Link at socket.
//
// Its instructions are its tool's, and say nothing else: not what the
// agent works in, not what hosts it, not what its folders are. Those an
// agent finds out, as it would anywhere.
func stateServer(socket string, session func() (string, error)) mcpServer {
	return mcpServer{
		name:         sandbox.StateServer,
		instructions: "The tool `" + sandbox.RaiseHandTool + "` of this server: " + raiseHandWords,
		tools: []mcpTool{{
			name:        sandbox.RaiseHandTool,
			description: raiseHandWords,
			schema:      noArguments,
			call: func(_ json.RawMessage, stop <-chan struct{}) (string, bool, *rpcError) {
				id, err := session()
				if err != nil {
					return "Your hand is not raised: " + err.Error(), true, nil
				}
				answer, err := ask(socket, sandbox.LinkRequest{Hand: &sandbox.HandRequest{Session: id}}, stop)
				switch {
				case err != nil:
					return "Your hand is not raised: " + err.Error(), true, nil
				case answer.Refused != "":
					return "Your hand is not raised: " + answer.Refused, true, nil
				}
				return "Your hand is raised: your user is called to this session. Wait here for their answer.", false, nil
			},
		}},
	}
}
