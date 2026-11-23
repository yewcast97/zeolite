package main

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"zeolite/internal/sandbox"
)

// The supervisor says of each Terminal Session which agent runs in it,
// if one does. It reads the kernel's list of processes and nothing a
// session printed: a process is of a session by its session ID, and is
// an agent by the name it was started under.

// interpreters are the programs that run an agent written in their
// language: such a process is the agent whose script it was given.
var interpreters = map[string]bool{"node": true, "bun": true, "deno": true}

// scriptEndings are what a script's name ends with besides the agent's
// own name.
var scriptEndings = []string{".js", ".mjs", ".cjs", ".py"}

// agentOf is the Agent Executable of the agent a command line is, or
// empty: the name of the program it was started under, or, under an
// interpreter, the name of the script it was given. A shell is no
// interpreter here: a shell script named like an agent is not taken for
// one, and neither is a shell that was told an agent's name.
func agentOf(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	name := path.Base(argv[0])
	if _, known := sandbox.KnownAgent(name); known {
		return name
	}
	if !interpreters[name] && !strings.HasPrefix(name, "python") {
		return ""
	}
	for _, arg := range argv[1:] {
		// What the interpreter itself is told comes before its script.
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		script := path.Base(arg)
		for _, ending := range scriptEndings {
			script = strings.TrimSuffix(script, ending)
		}
		if _, known := sandbox.KnownAgent(script); known {
			return script
		}
		return ""
	}
	return ""
}

// commandLine is the command line of a process, as the kernel shows it.
func commandLine(pid int) []string {
	data, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return nil
	}
	parts := bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0})
	argv := make([]string, len(parts))
	for i, part := range parts {
		argv[i] = string(part)
	}
	return argv
}

// sessionStates says, for each Terminal Session that is recorded and
// has a process left, which agent runs in it: the agent among its
// processes, the lowest process ID first where there are several.
func sessionStates() []sandbox.SessionState {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil
	}
	var states []sandbox.SessionState
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(sessionDir, e.Name()))
		if err != nil {
			continue
		}
		leader, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || leader <= 1 {
			continue
		}
		members := sessionMembers(leader)
		if len(members) == 0 {
			continue
		}
		sort.Ints(members)
		state := sandbox.SessionState{ID: e.Name()}
		for _, pid := range members {
			if agent := agentOf(commandLine(pid)); agent != "" {
				state.Agent = agent
				break
			}
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return states
}
