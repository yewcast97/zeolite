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
var errNoSession = errors.New("this server was not started from a Terminal Session of the sandbox: there is no session to speak for")

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
// instructions, and once more, whole, as the tool's description.
const raiseHandWords = "Call your user to this session: it lights a mark on the session, and beside this sandbox's name, in your user's window. " +
	"It takes no arguments and carries no words: first say what you need in your own output, then call this, then wait there for the answer. " +
	"Call it only when you cannot go on without your user: a decision that is theirs to make, a secret or a permission you lack. " +
	"Do not call it to report progress."

// What an agent is told of where it works, in the server's
// instructions: in the sandbox's Home, and of its Shared Folder where
// it is given one.
const (
	workInHome   = "You work in a sandbox, a container of your own. Your user watches it from a window on their Mac, and does not follow what you do line by line. " + sandbox.HomePath + ", where you start, is this sandbox's own folder: it is kept from one start to the next, and no other sandbox sees it."
	workInShared = sandbox.SharedFolderPath + " is a Shared Folder, kept on the Mac that hosts this sandbox: the agents of the other sandboxes that are given it read and write it too. Put there what they, or your user, are to find."
)

// stateServer is the MCP server sandbox, whose tool speaks for the
// Terminal Session session reports, through the Link at socket. shared
// is whether the sandbox is given a Shared Folder.
func stateServer(socket string, shared bool, session func() (string, error)) mcpServer {
	instructions := workInHome
	if shared {
		instructions += "\n\n" + workInShared
	}
	instructions += "\n\nThe tool `" + sandbox.RaiseHandTool + "` of this server: " + raiseHandWords
	return mcpServer{
		name:         sandbox.StateServer,
		instructions: instructions,
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
				return "Your hand is raised: your user sees a mark on this session. Wait here for their answer.", false, nil
			},
		}},
	}
}
