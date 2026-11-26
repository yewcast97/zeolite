package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// appEnd is the app's end of a sandbox's Link, as a test holds it: it
// reads the asks the link sends, and answers them.
type appEnd struct {
	t        *testing.T
	requests *json.Decoder
	answers  *io.PipeWriter
	ended    chan error
	socket   string
}

// startLink starts a link on a socket of the test's.
func startLink(t *testing.T) *appEnd {
	t.Helper()
	// A socket's path is short: the test's own folder is too deep.
	dir, err := os.MkdirTemp("/tmp", "zeolite-link")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "link.sock")
	fromApp, answers := io.Pipe()
	requests, toApp := io.Pipe()
	a := &appEnd{t: t, requests: json.NewDecoder(requests), answers: answers, ended: make(chan error, 1), socket: socket}
	go func() { a.ended <- link(socket, fromApp, toApp) }()
	t.Cleanup(func() { answers.Close() })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(socket); err == nil {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatal("the link did not listen")
		}
	}
}

// ask is the next ask the link sent the app.
func (a *appEnd) ask() sandbox.LinkRequest {
	a.t.Helper()
	var r sandbox.LinkRequest
	if err := a.requests.Decode(&r); err != nil {
		a.t.Fatal(err)
	}
	return r
}

// request is the next ask the link sent, which is one for a Host Tool:
// its number, and the request.
func (a *appEnd) request() (uint64, sandbox.HostToolRequest) {
	a.t.Helper()
	r := a.ask()
	if r.HostTool == nil || r.Cancel {
		a.t.Fatalf("the app was sent %+v, want a Host Tool's request", r)
	}
	return r.ID, *r.HostTool
}

func (a *appEnd) answer(id uint64, result sandbox.HostToolResult) {
	a.t.Helper()
	if err := json.NewEncoder(a.answers).Encode(sandbox.LinkAnswer{ID: id, HostTool: &result}); err != nil {
		a.t.Fatal(err)
	}
}

// The link gives each ask a number of its own and carries each answer
// to whoever asked; an ask whose asker left is withdrawn.
func TestTheLinkCarriesAsksAndAnswers(t *testing.T) {
	a := startLink(t)
	type asked struct {
		result sandbox.HostToolResult
		err    error
	}
	run := func(r sandbox.HostToolRequest, stop chan struct{}) chan asked {
		done := make(chan asked, 1)
		go func() {
			result, err := askHostTool(a.socket, r, stop)
			done <- asked{result, err}
		}()
		return done
	}
	first := run(sandbox.HostToolRequest{Argv: []string{"one"}}, nil)
	oneID, one := a.request()
	second := run(sandbox.HostToolRequest{Argv: []string{"two"}, Dir: "sub", Seconds: 5}, nil)
	twoID, two := a.request()
	if oneID == twoID || one.Argv[0] != "one" || two.Argv[0] != "two" || two.Dir != "sub" || two.Seconds != 5 {
		t.Errorf("the app was sent %d: %+v and %d: %+v", oneID, one, twoID, two)
	}
	// Answers come back in any order, each to its asker.
	a.answer(twoID, sandbox.HostToolResult{Stdout: "second"})
	a.answer(oneID, sandbox.HostToolResult{Stdout: "first", ExitCode: 3})
	if got := <-first; got.err != nil || got.result.Stdout != "first" || got.result.ExitCode != 3 {
		t.Errorf("the first asker got %+v", got)
	}
	if got := <-second; got.err != nil || got.result.Stdout != "second" {
		t.Errorf("the second asker got %+v", got)
	}

	// An asker does not choose its ask's number, nor withdraw another's:
	// what it writes of either is not carried.
	forged := make(chan sandbox.LinkAnswer, 1)
	go func() {
		answer, _ := ask(a.socket, sandbox.LinkRequest{ID: oneID, Cancel: true, HostTool: &sandbox.HostToolRequest{Argv: []string{"forged"}}}, nil)
		forged <- answer
	}()
	if sent := a.ask(); sent.ID == oneID || sent.ID == twoID || sent.Cancel || sent.HostTool == nil || sent.HostTool.Argv[0] != "forged" {
		t.Errorf("an ask that named a number and a withdrawal was sent on as %+v", sent)
	} else {
		// What the app refuses is told as a refusal.
		json.NewEncoder(a.answers).Encode(sandbox.LinkAnswer{ID: sent.ID, Refused: "running programs on the Mac is not allowed here"})
	}
	if got := <-forged; got.Refused != "running programs on the Mac is not allowed here" {
		t.Errorf("an ask the app refused was answered %+v", got)
	}
	refused := run(sandbox.HostToolRequest{Argv: []string{"refused"}}, nil)
	id, _ := a.request()
	json.NewEncoder(a.answers).Encode(sandbox.LinkAnswer{ID: id, Refused: "running programs on the Mac is not allowed here"})
	if got := <-refused; got.err != nil || got.result.Refused != "running programs on the Mac is not allowed here" {
		t.Errorf("a Host Tool's request the app refused was answered %+v", got)
	}

	// An asker that stops waiting: its ask is withdrawn, by its number.
	stop := make(chan struct{})
	third := run(sandbox.HostToolRequest{Argv: []string{"three"}}, stop)
	threeID, _ := a.request()
	close(stop)
	if got := <-third; got.err == nil {
		t.Errorf("an asker that stopped waiting got %+v", got)
	}
	if withdrawn := a.ask(); !withdrawn.Cancel || withdrawn.ID != threeID {
		t.Errorf("after its asker left, the app was sent %+v", withdrawn)
	}
	// An answer for nobody is dropped, and the link goes on.
	a.answer(threeID, sandbox.HostToolResult{})
	fourth := run(sandbox.HostToolRequest{Argv: []string{"four"}}, nil)
	id, _ = a.request()
	a.answer(id, sandbox.HostToolResult{Stdout: "fourth"})
	if got := <-fourth; got.result.Stdout != "fourth" {
		t.Errorf("after an answer for nobody, an asker got %+v", got)
	}

	// The stream ends: the link does, and takes its socket with it.
	a.answers.Close()
	select {
	case err := <-a.ended:
		if err != nil {
			t.Errorf("the link ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the link went on after its stream ended")
	}
	if _, err := askHostTool(a.socket, sandbox.HostToolRequest{Argv: []string{"late"}}, nil); err == nil || !strings.Contains(err.Error(), "what answers this tool is not connected now") {
		t.Errorf("asking after the link ended: %v", err)
	}
}

// client is an MCP client's end of the server, as a test holds it.
type client struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Reader
}

func startServer(t *testing.T, socket string) *client {
	t.Helper()
	return serveClient(t, hostToolServer(socket, sandbox.HostToolsNotice{}))
}

// serveClient starts one of the supervisor's MCP servers for a client
// of the test's.
func serveClient(t *testing.T, server mcpServer) *client {
	t.Helper()
	fromClient, in := io.Pipe()
	out, toClient := io.Pipe()
	go server.serve(fromClient, toClient)
	t.Cleanup(func() { in.Close() })
	return &client{t: t, in: in, out: bufio.NewReader(out)}
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// reply reads the server's next message.
func (c *client) reply() (id string, result map[string]any, rpcErr map[string]any) {
	c.t.Helper()
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatal(err)
	}
	var msg struct {
		JSONRPC string
		ID      json.RawMessage
		Result  map[string]any
		Error   map[string]any
	}
	if err := json.Unmarshal(line, &msg); err != nil || msg.JSONRPC != "2.0" {
		c.t.Fatalf("the server wrote %s (%v)", line, err)
	}
	return string(msg.ID), msg.Result, msg.Error
}

// text is what a tool's result says, and whether it is an error.
func text(result map[string]any) (string, bool) {
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		return "", false
	}
	item, _ := content[0].(map[string]any)
	said, _ := item["text"].(string)
	failed, _ := result["isError"].(bool)
	return said, failed
}

// The server is an MCP server with one tool: it says so, hands a call to
// the link, and says what came of the run as an agent reads it.
func TestTheServerHasOneTool(t *testing.T) {
	a := startLink(t)
	c := startServer(t, a.socket)

	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	id, result, _ := c.reply()
	if info, _ := result["serverInfo"].(map[string]any); id != "1" || result["protocolVersion"] != "2025-06-18" || info["name"] != "macos" || result["capabilities"].(map[string]any)["tools"] == nil {
		t.Errorf("the server introduced itself as %v", result)
	}
	// What an agent is told of the server is told as it meets it.
	if told, _ := result["instructions"].(string); !strings.HasPrefix(told, "The tool `run` of this server: Run a program on a Mac, in a restricted macOS environment") {
		t.Errorf("the server's instructions are %q", told)
	}
	// A notification is answered by nothing: the next answer is the
	// list's.
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.send(`{"jsonrpc":"2.0","id":"list","method":"tools/list"}`)
	id, result, _ = c.reply()
	tools, _ := result["tools"].([]any)
	if id != `"list"` || len(tools) != 1 {
		t.Fatalf("the server lists %v", result)
	}
	tool := tools[0].(map[string]any)
	schema, _ := tool["inputSchema"].(map[string]any)
	if required, _ := schema["required"].([]any); tool["name"] != "run" || schema["type"] != "object" || len(required) != 1 || required[0] != "argv" || !strings.Contains(tool["description"].(string), "/workspace/shared") {
		t.Errorf("the tool is %v", tool)
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if id, result, rpcErr := c.reply(); id != "2" || result == nil || rpcErr != nil {
		t.Errorf("a ping was answered %v, %v", result, rpcErr)
	}
	c.send(`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)
	if _, _, rpcErr := c.reply(); rpcErr == nil || rpcErr["code"].(float64) != -32601 {
		t.Errorf("a method the server has not was answered %v", rpcErr)
	}
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"shell","arguments":{}}}`)
	if _, _, rpcErr := c.reply(); rpcErr == nil || !strings.Contains(rpcErr["message"].(string), "no tool shell") {
		t.Errorf("a tool the server has not was answered %v", rpcErr)
	}

	// A call that cannot be run is refused here, and nothing is sent.
	c.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"run","arguments":{"argv":["ls"],"dir":"../.."}}}`)
	_, result, _ = c.reply()
	if said, failed := text(result); !failed || !strings.Contains(said, "Not run") || !strings.Contains(said, "steps out") {
		t.Errorf("a run outside the Shared Folder was answered %q", said)
	}

	// A call is handed to the app as it was made, and its answer told.
	c.send(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"run","arguments":{"argv":["blender","-b"],"dir":"scene","env":{"A":"b"},"seconds":90}}}`)
	if id, r := a.request(); strings.Join(r.Argv, " ") != "blender -b" || r.Dir != "scene" || r.Env["A"] != "b" || r.Seconds != 90 {
		t.Errorf("the app was sent %+v", r)
	} else {
		a.answer(id, sandbox.HostToolResult{ExitCode: 2, Stdout: "rendered\n", Stderr: "a warning", Truncated: true})
	}
	id, result, _ = c.reply()
	said, failed := text(result)
	if id != "6" || failed || said != "Exit status 2.\nIt printed more than is shown: the first 256 KiB of each stream. Send long output to a file, and read it under /workspace/shared.\n--- stdout ---\nrendered\n--- stderr ---\na warning\n" {
		t.Errorf("the run was told as %q (error %v)", said, failed)
	}

	// A run that was refused, or ended for its time, is an error.
	for want, answer := range map[string]sandbox.HostToolResult{
		"Not run: 4 Host Tools":  {Refused: "4 Host Tools of this sandbox are running"},
		"Ended after 600 second": {TimedOut: true, ExitCode: -1, Stdout: "so far"},
	} {
		c.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"run","arguments":{"argv":["sleep","1000"]}}}`)
		id, _ := a.request()
		a.answer(id, answer)
		_, result, _ = c.reply()
		if said, failed := text(result); !failed || !strings.HasPrefix(said, want) {
			t.Errorf("want an error that begins %q, got %q (error %v)", want, said, failed)
		}
	}

	// A call the client cancels is withdrawn, and not answered: the next
	// answer is the ping's.
	c.send(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"run","arguments":{"argv":["sleep","1000"]}}}`)
	long, _ := a.request()
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":8}}`)
	if withdrawn := a.ask(); !withdrawn.Cancel || withdrawn.ID != long {
		t.Errorf("after the client cancelled, the app was sent %+v", withdrawn)
	}
	c.send(`{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if id, _, _ := c.reply(); id != "9" {
		t.Errorf("after a cancelled call the server answered %s", id)
	}
}

// Where the sandbox's Link is not open, nothing listens: the tool says
// so. And where the app refuses a request, as for a sandbox that is
// given no Host Tools, the tool says why, as the app answered it.
func TestTheServerSaysWhenThereAreNoHostTools(t *testing.T) {
	c := startServer(t, filepath.Join(t.TempDir(), "nothing-listens.sock"))
	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run","arguments":{"argv":["ls"]}}}`)
	_, result, _ := c.reply()
	if said, failed := text(result); !failed || !strings.Contains(said, "what answers this tool is not connected now") {
		t.Errorf("with no link the tool says %q", said)
	}
	a := startLink(t)
	c = startServer(t, a.socket)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run","arguments":{"argv":["ls"]}}}`)
	id, _ := a.request()
	json.NewEncoder(a.answers).Encode(sandbox.LinkAnswer{ID: id, Refused: "running programs on the Mac is not allowed here: ask your user to allow it"})
	_, result, _ = c.reply()
	if said, failed := text(result); !failed || said != "Not run: running programs on the Mac is not allowed here: ask your user to allow it" {
		t.Errorf("refused by the app, the tool says %q", said)
	}
}

// What an agent is told of Host Tools, it is told by the server: in its
// instructions, and whole in the description of its one tool, which a
// client may show its model alone. It says what the sandbox's Wall was
// given.
func TestTheServerTellsAnAgentOfHostTools(t *testing.T) {
	described := func(notice sandbox.HostToolsNotice) (instructions, description string) {
		t.Helper()
		c := serveClient(t, hostToolServer("/nowhere", notice))
		c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
		_, result, _ := c.reply()
		instructions, _ = result["instructions"].(string)
		c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		_, result, _ = c.reply()
		tools, _ := result["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("the server lists %v", result)
		}
		description, _ = tools[0].(map[string]any)["description"].(string)
		return instructions, description
	}
	instructions, description := described(sandbox.HostToolsNotice{})
	for _, want := range []string{
		"Run a program on a Mac, in a restricted macOS environment", `["bash", "-c", "..."]`, "named from /workspace/shared",
		"600 where you do not say, 1500 at most", "256 KiB of each stream", "It has no network",
		"Of your files the program sees /workspace/shared and nothing else", "no other file of the Mac",
		"fetch yourself, built for macOS, into /workspace/shared",
		"The Mac's GPU is not given", "The Mac's name is not given",
		"Homebrew under /opt/homebrew, the system's tools and its compilers, and nothing of /Applications: ask your user for an application you need.",
	} {
		if !strings.Contains(description, want) {
			t.Errorf("the tool's description does not say %q:\n%s", want, description)
		}
		if !strings.Contains(instructions, want) {
			t.Errorf("the server's instructions do not say %q:\n%s", want, instructions)
		}
	}
	for _, text := range []string{instructions, description} {
		if strings.Contains(text, "%!") || strings.Contains(text, "%d") || strings.Contains(text, "%s") {
			t.Errorf("something of what an agent is told was left to be filled in:\n%s", text)
		}
	}
	// It is said as the agent uses the tool: a Mac, and what a program
	// has there. Nothing says whose Mac it is or what else it does, and
	// nothing is named in the app's own words.
	if named := notAnAgentsWords.FindAllString(description, -1); len(named) != 0 {
		t.Errorf("the tool's description names %q:\n%s", named, description)
	}
	// With the GPU and the name: which programs have the GPU, the way
	// to the rest, and that its sh is bash.
	_, description = described(sandbox.HostToolsNotice{GPU: true, MacName: true})
	for _, want := range []string{
		"The Mac's GPU is given", "The Mac's name is given", "when bash or zsh starts it", "Homebrew's and started by its name",
		"Start it through bash", "`sh` as the program of `argv` is started as bash",
	} {
		if !strings.Contains(description, want) {
			t.Errorf("with the GPU and the name the tool's description does not say %q:\n%s", want, description)
		}
	}
	if strings.Contains(description, "not given") {
		t.Errorf("with the GPU and the name the tool's description says:\n%s", description)
	}
	// Of /Applications a program reads what the sandbox is given, which
	// the tool names by its paths, and nothing else there.
	instructions, description = described(sandbox.HostToolsNotice{Applications: []string{"Blender.app", "Xcode.app"}})
	for _, text := range []string{instructions, description} {
		if !strings.Contains(text, "and of /Applications these and nothing else: /Applications/Blender.app, /Applications/Xcode.app. Ask your user for another. It has no network") {
			t.Errorf("with two applications given, an agent is told:\n%s", text)
		}
		if strings.Contains(text, "nothing of /Applications") {
			t.Errorf("with two applications given, an agent is told it has none:\n%s", text)
		}
	}
	if named := notAnAgentsWords.FindAllString(description, -1); len(named) != 0 {
		t.Errorf("with two applications given, the tool's description names %q:\n%s", named, description)
	}
}

// The server by which an agent tells of its state has one tool, which
// raises its hand: the server says for which Terminal Session, and the
// Link carries it. What the server tells an agent is that tool's own
// words, and nothing of where the agent works.
func TestTheStateServerRaisesAHand(t *testing.T) {
	a := startLink(t)
	session := "aaa"
	var sessionErr error
	c := serveClient(t, stateServer(a.socket, func() (string, error) { return session, sessionErr }))

	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	_, result, _ := c.reply()
	told, _ := result["instructions"].(string)
	if info, _ := result["serverInfo"].(map[string]any); info["name"] != "sandbox" {
		t.Errorf("the server introduced itself as %v", result)
	}
	for _, want := range []string{"The tool `raise_hand` of this server", "only when you cannot go on without your user", "Do not call it to report progress"} {
		if !strings.Contains(told, want) {
			t.Errorf("the server's instructions do not say %q:\n%s", want, told)
		}
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	_, result, _ = c.reply()
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("the server lists %v", result)
	}
	tool := tools[0].(map[string]any)
	schema, _ := tool["inputSchema"].(map[string]any)
	description, _ := tool["description"].(string)
	if tool["name"] != "raise_hand" || schema["type"] != "object" || !strings.HasPrefix(description, "Draw your user's attention to this session, for them to step in.") || !strings.Contains(description, "carries no words") || !strings.Contains(description, "Do not call it to report progress") {
		t.Errorf("the tool is %v", tool)
	}
	// The instructions are the tool's words under its name, and no
	// sentence beside them: an agent is not told what it works in, what
	// hosts it or what its folders are.
	if told != "The tool `raise_hand` of this server: "+description {
		t.Errorf("the server's instructions say more than its tool does:\n%s", told)
	}
	if named := describesWhereAnAgentWorks.FindAllString(told, -1); len(named) != 0 {
		t.Errorf("the server's words name %q:\n%s", named, told)
	}
	// Nor is it said how the app shows a hand, which is nothing an agent
	// works with.
	if named := append(notAnAgentsWords.FindAllString(told, -1), howAHandIsShown.FindAllString(told, -1)...); len(named) != 0 {
		t.Errorf("the server's words name %q:\n%s", named, told)
	}

	// A call hands the session to the Link, and nothing else: no word
	// of the agent's crosses, whatever arguments it gives.
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"raise_hand","arguments":{"message":"please look"}}}`)
	asked := a.ask()
	if asked.Hand == nil || asked.Hand.Session != "aaa" || asked.HostTool != nil {
		t.Fatalf("the app was sent %+v", asked)
	}
	if raw, _ := json.Marshal(asked); strings.Contains(string(raw), "please") {
		t.Errorf("an agent's words crossed with its hand: %s", raw)
	}
	json.NewEncoder(a.answers).Encode(sandbox.LinkAnswer{ID: asked.ID})
	_, result, _ = c.reply()
	if said, failed := text(result); failed || !strings.HasPrefix(said, "Your hand is raised") {
		t.Errorf("a raised hand was answered %q (error %v)", said, failed)
	}
	// With no arguments at all, as the tool asks for.
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"raise_hand"}}`)
	asked = a.ask()
	json.NewEncoder(a.answers).Encode(sandbox.LinkAnswer{ID: asked.ID, Refused: "this is not a session your user has open"})
	_, result, _ = c.reply()
	if said, failed := text(result); !failed || !strings.Contains(said, "Your hand is not raised: this is not a session your user has open") {
		t.Errorf("a hand the app refused was answered %q (error %v)", said, failed)
	}
	// A server that descends from no Terminal Session asks nothing.
	sessionErr = errNoSession
	c.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"raise_hand","arguments":{}}}`)
	_, result, _ = c.reply()
	if said, failed := text(result); !failed || !strings.Contains(said, "not started from a terminal session: there is no session to call your user to") {
		t.Errorf("with no session the tool says %q (error %v)", said, failed)
	}
}

// describesWhereAnAgentWorks finds what the server sandbox does not
// name: what a sandbox is made of, what hosts it, and a path in it.
var describesWhereAnAgentWorks = regexp.MustCompile(`(?i)\b(mac|macos|darwin|apple|linux|debian|container|docker|colima|vm|virtual machine|host|hosts|hosted)\b|/workspace`)

// notAnAgentsWords finds what no tool's description says: the app's own
// names for things, how the app shows something to its user, and what
// hosts the agent.
var notAnAgentsWords = regexp.MustCompile(`(?i)\b(sandbox|card|hosts|hosted|container|docker|vm|shared folder|host tools?|wall|scratch|link)\b`)

// howAHandIsShown finds how the app shows a raised hand to its user.
var howAHandIsShown = regexp.MustCompile(`(?i)\b(window|mark|lights?|glyph)\b`)
