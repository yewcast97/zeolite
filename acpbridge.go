package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"zeolite/internal/acpbridge"
	"zeolite/internal/configstore"
	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// A sandbox's ACP Bridge is this program, started by an ACP client on
// this Mac in an agent's place:
//
//	zeolite acp-bridge <sandbox ID> <agent executable> [arguments...]
//
// It shows no window and runs none of the app. It has the agent started
// in the sandbox's workstation and passes the messages of the Agent
// Client Protocol between its client, on its standard streams, and that
// agent, as internal/acpbridge judges them. The sandbox is named by its
// ID, which never changes: a client's settings outlast a new name. The
// agent is named by its Agent Executable, and what follows it is added
// to the command by which that agent speaks the protocol.

// acpBridgeCommand is the first word after the program's name by which
// it is a sandbox's ACP Bridge.
const acpBridgeCommand = "acp-bridge"

// acpBridgeArguments reads the words after the program's name and
// acpBridgeCommand: a sandbox's ID, an agent's executable, and what is
// added to the agent's command.
func acpBridgeArguments(words []string) (sandboxID, agent string, added []string, ok bool) {
	if len(words) < 2 || words[0] == "" || words[1] == "" {
		return "", "", nil, false
	}
	return words[0], words[1], words[2:], true
}

// acpBridgeCommandLine is the command line an ACP client is given to
// start in an agent's place, as one line: this program, by its path,
// and the words acpBridgeArguments reads. A word that a shell or a
// reader would split is quoted.
func acpBridgeCommandLine(executable, sandboxID, agent string) string {
	words := []string{executable, acpBridgeCommand, sandboxID, agent}
	for i, word := range words {
		words[i] = quoted(word)
	}
	return strings.Join(words, " ")
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// quoted writes a word as a shell reads one word: as it is where it
// holds nothing a shell reads, and between single quotes otherwise.
func quoted(word string) string {
	if plainWord.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// notesItsEnd is a stream that says whether it was read to its end.
type notesItsEnd struct {
	io.Reader
	ended atomic.Bool
}

func (n *notesItsEnd) Read(p []byte) (int, error) {
	count, err := n.Reader.Read(p)
	if err != nil {
		n.ended.Store(true)
	}
	return count, err
}

// acpBridge is a sandbox's ACP Bridge, from the words after
// acpBridgeCommand to the status the program exits with. Why it cannot
// start an agent, or why the agent ended, it says on said, in the app's
// own words, for its client to show its user.
func acpBridge(words []string, in io.Reader, out, said io.Writer) int {
	fail := func(format string, args ...any) {
		fmt.Fprintf(said, "zeolite acp-bridge: "+format+"\n", args...)
	}
	sandboxID, agent, added, ok := acpBridgeArguments(words)
	if !ok {
		fmt.Fprintln(said, "usage: zeolite acp-bridge <sandbox ID> <agent executable> [arguments...]")
		return 2
	}
	// The saved sandboxes are read, to say which sandbox it is. Nothing
	// is written there.
	dir, err := dataDirPath()
	if err != nil {
		fail("%v", err)
		return 1
	}
	configs, err := configstore.Open(dir).Load()
	if err != nil {
		fail("the saved sandboxes could not be read: %v", err)
		return 1
	}
	var cfg *sandbox.Config
	for i := range configs {
		if configs[i].ID == sandboxID {
			cfg = &configs[i]
		}
	}
	if cfg == nil {
		fail("no sandbox has the ID %q: copy the command again from the sandbox's Setup", sandboxID)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	started, err := orchestrator.StartACPAgent(ctx, orchestrator.ColimaEndpoint(), sandboxID, agent, added)
	cancel()
	if err != nil {
		fail("%s: %v", cfg.Name, err)
		return 1
	}
	client := &notesItsEnd{Reader: in}
	err = acpbridge.Serve(client, out, started, started.SharedFolder())
	if err != nil {
		fail("%s: %v", cfg.Name, err)
	}
	// The client left: the agent was ended for it, and how is nothing
	// to say.
	if client.ended.Load() {
		return 0
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, waitErr := started.Wait(ctx)
	known, _ := sandbox.KnownAgent(agent)
	switch {
	case err != nil:
		return 1
	case waitErr != nil:
		fail("%s: how %s ended is not known: %v", cfg.Name, known.Title, waitErr)
		return 1
	case status == 127:
		fail("%s: its workstation has no program %s, by which %s speaks the Agent Client Protocol: install it there, in a Terminal Session", cfg.Name, known.ACP[0], known.Title)
	case status == 126:
		fail("%s: its workstation cannot run %s, by which %s speaks the Agent Client Protocol", cfg.Name, known.ACP[0], known.Title)
	case status != 0:
		fail("%s: %s ended with status %d", cfg.Name, known.Title, status)
	}
	return status
}
