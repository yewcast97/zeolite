//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"zeolite/internal/configstore"
	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// A sandbox's ACP Bridge is the app's own executable, started by an ACP
// client in an agent's place. These tests are that client: they build
// the executable as it is shipped, start it as an editor would, with a
// data directory of their own, and speak the Agent Client Protocol on
// its standard streams, against a sandbox the harness started.

// theApp is the app's executable, built once for the tests, as it is
// shipped: without the tag that links the automation in.
var theApp = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "zeolite-test-app-")
	if err != nil {
		return "", err
	}
	binary := filepath.Join(dir, "zeolite")
	if out, err := exec.Command("go", "build", "-o", binary, "..").CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the app: %v\n%s", err, out)
	}
	return binary, nil
})

// macMarker is a variable of the bridge's own environment, as an editor
// would leave one there: none of it may reach the workstation.
const macMarker = "ZEOLITE_TEST_OF_THIS_MAC"

// acpClient is a test that is an ACP client: it started the bridge, and
// holds its streams.
type acpClient struct {
	t     *testing.T
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string
	said  *bytes.Buffer
	// saw is every line the bridge wrote where the protocol is.
	mu  sync.Mutex
	saw []string
}

// startBridge starts the app's executable as the ACP Bridge of a
// sandbox, with the sandbox saved in a data directory of the test's
// own, and the words given after the command.
func startBridge(t *testing.T, cfg sandbox.Config, words ...string) *acpClient {
	t.Helper()
	binary, err := theApp()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := configstore.Open(dir).Save([]sandbox.Config{cfg}); err != nil {
		t.Fatal(err)
	}
	c := &acpClient{t: t, lines: make(chan string, 256), said: &bytes.Buffer{}}
	c.cmd = exec.Command(binary, append([]string{"acp-bridge"}, words...)...)
	c.cmd.Env = append(os.Environ(), "ZEOLITE_DATA_DIR="+dir, macMarker+"=/Users/someone/secret")
	c.cmd.Stderr = c.said
	if c.in, err = c.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.cmd.Process.Kill() })
	go func() {
		defer close(c.lines)
		reader := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				c.mu.Lock()
				c.saw = append(c.saw, line)
				c.mu.Unlock()
				c.lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}

// send writes one message to the bridge.
func (c *acpClient) send(message string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, message+"\n"); err != nil {
		c.t.Fatalf("writing to the bridge: %v; it said: %s", err, c.said)
	}
}

// message is a message of the protocol as a client reads it.
type acpMessage struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	raw string
}

// next is the next message the bridge writes, within a time.
func (c *acpClient) next(within time.Duration) acpMessage {
	c.t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			c.t.Fatalf("the bridge ended; it said: %s", c.said)
		}
		var m acpMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			c.t.Fatalf("the bridge wrote what is not JSON: %q", line)
		}
		m.raw = line
		return m
	case <-time.After(within):
		c.t.Fatalf("the bridge wrote nothing for %v; it said: %s", within, c.said)
	}
	return acpMessage{}
}

// answer waits for the answer to the request of an id, and returns it
// with the messages that came before it.
func (c *acpClient) answer(id any, within time.Duration) (acpMessage, []acpMessage) {
	c.t.Helper()
	var before []acpMessage
	deadline := time.Now().Add(within)
	for {
		m := c.next(time.Until(deadline))
		if m.Method == "" && fmt.Sprint(m.ID) == fmt.Sprint(id) {
			return m, before
		}
		before = append(before, m)
	}
}

// ended waits for the bridge to end, and returns its exit status.
func (c *acpClient) ended(within time.Duration) int {
	c.t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
		return c.cmd.ProcessState.ExitCode()
	case <-time.After(within):
		c.t.Fatalf("the bridge did not end within %v; it said: %s", within, c.said)
	}
	return 0
}

// everything is every line the bridge wrote where the protocol is.
func (c *acpClient) everything() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.saw, "")
}

// scriptedAgent is an agent that tries what a hostile one would: it asks
// its client for a file of this Mac's and for a terminal there, offers a
// way to log in that runs a command where its client runs, and reports
// files inside its Shared Folder and outside it. It writes down what it
// was given and what it was answered, in its Home, for the test to read
// through the sandbox. It starts a process of its own, which must not
// outlive it.
const scriptedAgent = `#!/usr/bin/env python3
import json, os, subprocess, sys

log = open("/workspace/acp-agent.log", "a")
def note(what, value):
    log.write(json.dumps({what: value}) + "\n")
    log.flush()
def send(message):
    sys.stdout.write(json.dumps(message) + "\n")
    sys.stdout.flush()

note("argv", sys.argv[1:])
note("cwd", os.getcwd())
note("environment", sorted(os.environ))
subprocess.Popen(["sleep", "600"])
asked, prompt = {}, None
for line in sys.stdin:
    try:
        m = json.loads(line)
    except ValueError:
        note("garbage", line)
        continue
    note("got", m)
    method = m.get("method")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": m["id"], "_meta": {"from": "the agent"}, "result": {
            "protocolVersion": 1,
            "agentCapabilities": {"mcpCapabilities": {"http": True, "sse": True}, "_meta": {"from": "the agent"}},
            "authMethods": [
                {"id": "plain", "name": "Log in"},
                {"type": "terminal", "id": "tui", "name": "Log in in a terminal", "args": ["login"], "env": {"X": "1"}},
                {"id": "old", "name": "Log in the old way", "_meta": {"terminal-auth": {"command": "/bin/sh", "args": ["-c", "touch /tmp/zeolite-acp-test-ran"], "label": "Log in"}}},
            ],
            "_meta": {"from": "the agent"}}})
    elif method == "session/new":
        send({"jsonrpc": "2.0", "id": m["id"], "result": {"sessionId": "s1"}})
    elif method == "session/prompt":
        prompt = m["id"]
        send({"jsonrpc": "2.0", "id": "a1", "method": "fs/read_text_file", "params": {"sessionId": "s1", "path": "/etc/passwd"}})
        send({"jsonrpc": "2.0", "id": "a2", "method": "fs/write_text_file", "params": {"sessionId": "s1", "path": "/tmp/zeolite-acp-test-wrote", "content": "written"}})
        send({"jsonrpc": "2.0", "id": "a3", "method": "terminal/create", "params": {"sessionId": "s1", "command": "touch", "args": ["/tmp/zeolite-acp-test-ran"]}})
        send({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "s1", "_meta": {"from": "the agent"}, "update": {
            "sessionUpdate": "tool_call", "toolCallId": "c1", "title": "Edit two files", "kind": "edit", "status": "completed",
            "locations": [{"path": "/workspace/shared/project/inside.txt", "line": 2}, {"path": "/workspace/outside.txt"}, {"path": "/etc/passwd"}, {"path": "/workspace/shared/../.bashrc"}],
            "content": [
                {"type": "diff", "path": "/workspace/shared/project/inside.txt", "oldText": "a", "newText": "b"},
                {"type": "diff", "path": "/etc/passwd", "newText": "root::0:0"},
                {"type": "content", "content": {"type": "resource_link", "uri": "file:///etc/passwd", "name": "passwd"}},
                {"type": "terminal", "terminalId": "t1"}]}}})
        send({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "s1", "update": {
            "sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "Both are edited."}}}})
    elif method is None and "id" in m:
        asked[m["id"]] = m
        if prompt is not None and len(asked) == 3:
            send({"jsonrpc": "2.0", "id": prompt, "result": {"stopReason": "end_turn"}})
            prompt = None
`

// scriptedAgentPath is where the scripted agent is put: under the name
// by which OpenCode is started, where a login shell finds what its user
// installed.
const scriptedAgentPath = sandbox.HomePath + "/.local/bin/opencode"

// agentNotes are what the scripted agent wrote down, each under what it
// is, as it wrote them.
func agentNotes(t *testing.T, sandboxID string) map[string][]json.RawMessage {
	t.Helper()
	out, code := inSandbox(sandboxID, "cat /workspace/acp-agent.log")
	if code != 0 {
		t.Fatalf("reading what the scripted agent wrote down: %s", out)
	}
	notes := map[string][]json.RawMessage{}
	for _, line := range strings.Split(trimmed(out), "\n") {
		var note map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &note); err != nil {
			t.Fatalf("the scripted agent wrote down %q", line)
		}
		for what, value := range note {
			notes[what] = append(notes[what], value)
		}
	}
	return notes
}

// sameJSON reports whether two JSON texts say the same value.
func sameJSON(got, want string) bool {
	var g, w any
	return json.Unmarshal([]byte(got), &g) == nil && json.Unmarshal([]byte(want), &w) == nil && reflect.DeepEqual(g, w)
}

// An ACP client on this Mac works with an agent in a sandbox through
// the sandbox's ACP Bridge, and the agent gets nothing of this Mac by
// it but its Shared Folder: what it asks of its client for a file or a
// terminal is answered by the bridge and never seen by the client, a
// login that runs a command is not offered to the client, a session
// outside the Shared Folder is refused, and of the files the agent
// reports the client sees those in the Shared Folder, by this Mac's
// path. The agent is started by the command its client named, in the
// Shared Folder, with nothing of this Mac's environment; and when its
// client leaves, is killed or its sandbox stops, no process of the
// agent's is left.
func TestACPBridgeKeepsAnAgentToItsSharedFolder(t *testing.T) {
	_, folder := aSharedFolder(t, "test-acp-bridge")
	cfg := lockedConfig("acp-bridge", "ACP")
	cfg.SharedFolder, cfg.ACPBridge = "test-acp-bridge", true
	startWith(t, cfg, nil, orchestrator.StartOptions{SharedFolder: folder})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	baseline := processes(t, cfg.ID)
	swept := func(after string) {
		t.Helper()
		waitFor(t, 15*time.Second, "the agent's processes to end after "+after, func() bool { return processes(t, cfg.ID) <= baseline })
	}

	// A sandbox that is none, and an agent that is none.
	c := startBridge(t, cfg, "no-such-sandbox", "opencode")
	if status := c.ended(20 * time.Second); status != 1 || !strings.Contains(c.said.String(), `no sandbox has the ID "no-such-sandbox"`) {
		t.Errorf("for a sandbox that is none the bridge ended with %d and said: %s", status, c.said)
	}
	c = startBridge(t, cfg, cfg.ID, "bash")
	if status := c.ended(20 * time.Second); status != 1 || !strings.Contains(c.said.String(), "is no agent that can be started here") {
		t.Errorf("for an agent that is none the bridge ended with %d and said: %s", status, c.said)
	}
	// An agent the workstation does not have.
	c = startBridge(t, cfg, cfg.ID, "opencode")
	if status := c.ended(30 * time.Second); status != 127 || !strings.Contains(c.said.String(), "its workstation has no program opencode") {
		t.Errorf("for an agent that is not installed the bridge ended with %d and said: %s", status, c.said)
	}
	if got := c.everything(); got != "" {
		t.Errorf("a bridge that started no agent wrote %q where the protocol is", got)
	}
	swept("an agent that was not there")

	// The scripted agent, installed as its user installs one: under the
	// home. A profile that prints is not heard where the protocol is.
	if out, code := dockerWith(nil, scriptedAgent, "exec", "-i", orchestrator.WorkstationContainer(cfg.ID), "sh", "-c",
		"mkdir -p "+filepath.Dir(scriptedAgentPath)+" && cat > "+scriptedAgentPath+" && chmod +x "+scriptedAgentPath+
			" && echo 'echo a profile that prints' >> /workspace/.bashrc && mkdir -p /workspace/shared/project"); code != 0 {
		t.Fatalf("installing the scripted agent: %s", out)
	}

	c = startBridge(t, cfg, cfg.ID, "opencode", "--added", "a b; touch /workspace/acp-ran-a-word")
	// The client says what it can do, which is everything; the agent
	// hears that it can do nothing.
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true,"auth":{"terminal":true},"elicitation":{"form":{},"url":{}},"_meta":{"terminal_output":true,"terminal-auth":true}},"clientInfo":{"name":"test","version":"1"}}}`)
	initialized, _ := c.answer(1, 30*time.Second)
	if want := `{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":false,"sse":false}},"authMethods":[{"id":"plain","name":"Log in"},{"id":"old","name":"Log in the old way"}]}`; !sameJSON(string(initialized.Result), want) {
		t.Errorf("the client is told of the agent:\n%s\nwant\n%s", initialized.Result, want)
	}

	// A session outside the Shared Folder is refused, and the agent
	// never hears of it.
	home, _ := os.UserHomeDir()
	for i, outside := range []string{home, filepath.Dir(folder), folder + "-other", folder + "/../" + filepath.Base(folder) + "-other"} {
		request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 10 + i, "method": "session/new", "params": map[string]any{"cwd": outside, "mcpServers": []any{}}})
		c.send(string(request))
		refused, _ := c.answer(10+i, 10*time.Second)
		if refused.Error == nil || !strings.Contains(refused.Error.Message, "works in its Shared Folder, "+folder) {
			t.Errorf("a session in %s was answered %s", outside, refused.raw)
		}
	}
	// One in it is made, with none of the client's MCP servers.
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 20, "method": "session/new", "params": map[string]any{
		"cwd":        folder + "/project",
		"mcpServers": []any{map[string]any{"name": "files", "command": "/usr/local/bin/files", "args": []string{home}, "env": []any{}}},
	}})
	c.send(string(request))
	if made, _ := c.answer(20, 10*time.Second); !sameJSON(string(made.Result), `{"sessionId":"s1"}`) {
		t.Fatalf("a session in the Shared Folder was answered %s", made.raw)
	}

	// A prompt, in which the agent asks for a file and a terminal of this
	// Mac's and reports files in and outside the Shared Folder.
	prompt, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 30, "method": "session/prompt", "params": map[string]any{
		"sessionId": "s1",
		"prompt": []any{
			map[string]any{"type": "text", "text": "Edit them."},
			map[string]any{"type": "resource_link", "uri": "file://" + folder + "/project/inside.txt", "name": "inside.txt"},
			map[string]any{"type": "resource_link", "uri": "file://" + home + "/Documents/private.txt", "name": "private.txt"},
		},
	}})
	c.send(string(prompt))
	done, before := c.answer(30, 30*time.Second)
	if !sameJSON(string(done.Result), `{"stopReason":"end_turn"}`) {
		t.Errorf("the prompt was answered %s", done.raw)
	}
	var told []string
	for _, m := range before {
		if m.Method != "session/update" {
			t.Errorf("during the prompt the client was sent %s", m.raw)
			continue
		}
		told = append(told, string(m.Params))
	}
	wantCall, _ := json.Marshal(map[string]any{"sessionId": "s1", "update": map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "Edit two files", "kind": "edit", "status": "completed",
		"locations": []any{map[string]any{"path": folder + "/project/inside.txt", "line": 2}},
		"content":   []any{map[string]any{"type": "diff", "path": folder + "/project/inside.txt", "oldText": "a", "newText": "b"}},
	}})
	if len(told) != 2 || !sameJSON(told[0], string(wantCall)) || !sameJSON(told[1], `{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Both are edited."}}}`) {
		t.Errorf("the client was told of the session:\n%s\nwant first\n%s", strings.Join(told, "\n"), wantCall)
	}
	// Nothing the client was sent asks it for a file or a terminal, has
	// a member the protocol leaves open, or names a path of the
	// workstation or a file outside the Shared Folder.
	for _, never := range []string{"fs/", "terminal", "_meta", "passwd", "/workspace", "outside.txt", ".bashrc", "zeolite-acp-test", "a profile that prints"} {
		if strings.Contains(c.everything(), never) {
			t.Errorf("what the client was sent holds %q:\n%s", never, c.everything())
		}
	}
	for _, file := range []string{"/tmp/zeolite-acp-test-ran", "/tmp/zeolite-acp-test-wrote"} {
		if _, err := os.Lstat(file); err == nil {
			t.Errorf("%s is on this Mac: something of the agent's was done here", file)
			os.Remove(file)
		}
	}

	// What the agent was given, as it wrote it down.
	notes := agentNotes(t, cfg.ID)
	if got := notes["argv"]; len(got) != 1 || !sameJSON(string(got[0]), `["acp","--added","a b; touch /workspace/acp-ran-a-word"]`) {
		t.Errorf("the agent was started with %s", got)
	}
	if _, code := inSandbox(cfg.ID, "test -e /workspace/acp-ran-a-word"); code == 0 {
		t.Error("a word after the agent's name was run by a shell")
	}
	if got := notes["cwd"]; len(got) != 1 || string(got[0]) != `"/workspace/shared"` {
		t.Errorf("the agent was started in %s", got)
	}
	if got := string(notes["environment"][0]); strings.Contains(got, macMarker) || strings.Contains(got, "ZEOLITE_DATA_DIR") || !strings.Contains(got, `"HOME"`) || !strings.Contains(got, `"TZ"`) {
		t.Errorf("the agent's environment has the variables %s", got)
	}
	var got []struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	for _, raw := range notes["got"] {
		var m struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		json.Unmarshal(raw, &m)
		got = append(got, m)
	}
	if len(got) != 6 {
		t.Fatalf("the agent was sent %d messages, want the client's three and three answers: %s", len(got), notes["got"])
	}
	if want := `{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false},"clientInfo":{"name":"test","version":"1"}}`; got[0].Method != "initialize" || !sameJSON(string(got[0].Params), want) {
		t.Errorf("the agent was told of its client:\n%s\nwant\n%s", got[0].Params, want)
	}
	if want := `{"cwd":"/workspace/shared/project","mcpServers":[]}`; got[1].Method != "session/new" || !sameJSON(string(got[1].Params), want) {
		t.Errorf("the agent was asked for a session with %s, want %s", got[1].Params, want)
	}
	if want := `{"sessionId":"s1","prompt":[{"type":"text","text":"Edit them."},{"type":"resource_link","uri":"file:///workspace/shared/project/inside.txt","name":"inside.txt"},{"type":"resource_link","uri":"private.txt","name":"private.txt"}]}`; got[2].Method != "session/prompt" || !sameJSON(string(got[2].Params), want) {
		t.Errorf("the agent was prompted with %s, want %s", got[2].Params, want)
	}
	for _, answer := range got[3:] {
		if answer.Error == nil || answer.Error.Code != -32601 {
			t.Errorf("what the agent asked of its client under %v was answered %+v, want an error of the bridge's", answer.ID, answer)
		}
	}
	if all := fmt.Sprint(notes["got"]); strings.Contains(all, home) || strings.Contains(all, folder) || strings.Contains(all, "/Users/") {
		t.Errorf("the agent was sent a path of this Mac: %s", all)
	}

	// The agent runs, with what it started; when its client leaves, all
	// of it ends.
	if n := processes(t, cfg.ID); n < baseline+3 {
		t.Fatalf("with the agent started the workstation has %d processes, and had %d before", n, baseline)
	}
	c.in.Close()
	if status := c.ended(20 * time.Second); status != 0 {
		t.Errorf("after its client left the bridge ended with %d; it said: %s", status, c.said)
	}
	swept("its client left")

	// A bridge that is killed leaves nothing either.
	c = startBridge(t, cfg, cfg.ID, "opencode")
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	c.answer(1, 30*time.Second)
	if n := processes(t, cfg.ID); n < baseline+3 {
		t.Fatalf("with the agent started again the workstation has %d processes, and had %d before", n, baseline)
	}
	c.cmd.Process.Kill()
	c.ended(10 * time.Second)
	swept("the bridge was killed")

	// When the sandbox stops, the bridge ends by itself.
	c = startBridge(t, cfg, cfg.ID, "opencode")
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	c.answer(1, 30*time.Second)
	orch.Stop(context.Background(), cfg.ID)
	c.ended(30 * time.Second)

	// A sandbox that does not run starts no agent; nor does one that was
	// started without an ACP Bridge, whatever its saved config says.
	c = startBridge(t, cfg, cfg.ID, "opencode")
	if status := c.ended(20 * time.Second); status != 1 || !strings.Contains(c.said.String(), "is not running") {
		t.Errorf("for a sandbox that does not run the bridge ended with %d and said: %s", status, c.said)
	}
	without := cfg
	without.ACPBridge = false
	startAgain(t, without, nil, orchestrator.StartOptions{SharedFolder: folder})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	c = startBridge(t, cfg, cfg.ID, "opencode")
	if status := c.ended(20 * time.Second); status != 1 || !strings.Contains(c.said.String(), "was not started with an ACP Bridge") {
		t.Errorf("for a sandbox started without an ACP Bridge the bridge ended with %d and said: %s", status, c.said)
	}
	if n := processes(t, cfg.ID); n > baseline {
		t.Errorf("a sandbox started without an ACP Bridge has %d processes, and one with had %d before any agent", n, baseline)
	}
}

// acpModelConfig is the opencode config of the ACP Bridge's experiment:
// the experiments' model, defined and chosen, since no command line
// names one to an agent that its client starts.
const acpModelConfig = `{
  "$schema": "https://opencode.ai/config.json",
  "autoupdate": false,
  "permission": "allow",
  "model": "` + agentModel + `",
  "provider": {"openrouter": {"models": {"` + agentModelID + `": {}}}}
}
`

// crossedPaths collects every value of a message that the protocol
// reads as a path: where a tool call is, and the file a diff changes.
func crossedPaths(v any, name string, into *[]string) {
	switch v := v.(type) {
	case map[string]any:
		for member, value := range v {
			if member == "rawInput" || member == "rawOutput" {
				continue // the agent's own account, which names what it likes
			}
			crossedPaths(value, member, into)
		}
	case []any:
		for _, value := range v {
			crossedPaths(value, name, into)
		}
	case string:
		if name == "path" || name == "cwd" {
			*into = append(*into, v)
		}
	}
}

// The experiment: two agents, each driven by a real model, are started
// in a sandbox by the sandbox's ACP Bridge and given a job by this
// test, which is their ACP client: OpenCode, which speaks the protocol
// itself, and Claude Code, which speaks it by a program of another
// name. The job is done where the client asked for it, in the Shared
// Folder; every path the client is told of is in that folder, as this
// Mac has it; the client is asked for no file and no terminal, and for
// a permission where the agent wants one; and when the client leaves,
// nothing of the agent is left. The verdicts are the file and what
// crossed, never what a model says of itself. Whether an agent started
// so finds the sandbox's own MCP servers, in its MCP Registry, is found
// out and said: the job asks for a hand to be raised.
func TestACPBridgeExperiment(t *testing.T) {
	key := openRouterKey(t)
	_, folder := aSharedFolder(t, "test-acp-experiment")
	cfg := openConfig(t, "acp-experiment")
	cfg.Resources.MemoryMB = 6144
	cfg.SharedFolder, cfg.ACPBridge = "test-acp-experiment", true
	startWith(t, cfg, nil, orchestrator.StartOptions{SharedFolder: folder, Link: serveLink(cfg.ID, nil, nil, nil)})
	noteLocks(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
	redact := func(text string) string {
		text = strings.ReplaceAll(text, key, "[the API key]")
		if physical, err := seenAt(); err == nil {
			text = strings.ReplaceAll(text, physical, "[this Mac's physical exit address]")
		}
		return text
	}

	workstation := orchestrator.WorkstationContainer(cfg.ID)
	installAgent(t, cfg.ID)
	if out, code := dockerWith(nil, acpModelConfig, "exec", "-i", workstation, "sh", "-c", "mkdir -p /workspace/.config/opencode && cat > /workspace/.config/opencode/opencode.json"); code != 0 {
		t.Fatalf("defining the model in the workstation: %s", out)
	}
	claude, _ := sandbox.KnownAgent("claude")
	claudeInstalled := installedByHand(t, cfg.ID, claude) != ""
	if claudeInstalled {
		if out, code := inLoginShell(cfg.ID, acpPrograms["claude"]+" 2>&1 | tail -5"); code != 0 {
			t.Errorf("installing the program by which Claude Code speaks the protocol: %s", out)
			claudeInstalled = false
		}
	}
	// The key, and what sends Claude Code to a model, are where the
	// agents' user puts them for an agent that an ACP client starts: in
	// ~/.bashrc, which the shell an agent is started through reads. They
	// cross on a command's standard input, and the Home is deleted with
	// the test's sandbox.
	lines := append([]string{"export " + keyVariable + "=" + shellQuote(key)}, agentJobs["claude"].environment...)
	if out, code := dockerWith(nil, strings.Join(lines, "\n")+"\n", "exec", "-i", workstation, "sh", "-c", "umask 077 && cat >> /workspace/.bashrc"); code != 0 {
		t.Fatalf("giving the agents their key: exit %d (%d bytes said)", code, len(out))
	}
	baseline := processes(t, cfg.ID)

	for _, agent := range []string{"opencode", "claude"} {
		if agent == "claude" && !claudeInstalled {
			continue
		}
		known, _ := sandbox.KnownAgent(agent)
		waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
		file := "from-" + agent + ".txt"
		calls := len(updates.mcpCallsOf(cfg.ID))

		c := startBridge(t, cfg, cfg.ID, agent)
		c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true},"clientInfo":{"name":"zeolite-test","version":"1"}}}`)
		initialized, _ := c.answer(1, 2*time.Minute)
		if initialized.Error != nil {
			t.Fatalf("%s answered initialize with %s; the bridge said: %s", known.Title, redact(initialized.raw), c.said)
		}
		request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": folder, "mcpServers": []any{}}})
		c.send(string(request))
		made, _ := c.answer(2, 2*time.Minute)
		var session struct {
			SessionID string `json:"sessionId"`
		}
		json.Unmarshal(made.Result, &session)
		if session.SessionID == "" {
			t.Fatalf("%s made no session: %s", known.Title, redact(made.raw))
		}

		prompt, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": map[string]any{
			"sessionId": session.SessionID,
			"prompt": []any{map[string]any{"type": "text", "text": "Create the file " + file + " in the current working directory containing exactly the word READY. " +
				"Then call the tool raise_hand of the MCP server sandbox once, and then stop."}},
		}})
		c.send(string(prompt))
		var paths []string
		kinds := map[string]int{}
		deadline := time.Now().Add(8 * time.Minute)
		for answered := false; !answered; {
			m := c.next(time.Until(deadline))
			switch {
			case m.Method == "" && fmt.Sprint(m.ID) == "3":
				answered = true
				if m.Error != nil {
					t.Errorf("%s answered the prompt with an error: %s", known.Title, redact(m.raw))
				}
			case m.Method == "session/request_permission":
				// A user who allows what is asked.
				var asked struct {
					Options []struct {
						OptionID string `json:"optionId"`
						Kind     string `json:"kind"`
					} `json:"options"`
				}
				json.Unmarshal(m.Params, &asked)
				choice := ""
				for _, option := range asked.Options {
					if strings.HasPrefix(option.Kind, "allow") && choice == "" {
						choice = option.OptionID
					}
				}
				id, _ := json.Marshal(m.ID)
				answer, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": choice}})
				c.send(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":` + string(answer) + `}`)
				kinds["a permission asked"]++
				crossedPaths(func() (v any) { json.Unmarshal(m.Params, &v); return }(), "", &paths)
			case m.Method == "session/update":
				var params struct {
					Update map[string]any `json:"update"`
				}
				json.Unmarshal(m.Params, &params)
				kinds[fmt.Sprint(params.Update["sessionUpdate"])]++
				crossedPaths(params.Update, "", &paths)
			default:
				t.Errorf("the client of %s was sent %s", known.Title, redact(m.raw))
			}
		}
		t.Logf("%s, started by %q: the client was told, by kind: %v; and of the paths %v", known.Title, known.ACP, kinds, paths)

		// The job is done, in the Shared Folder: read through the
		// sandbox, and seen on this Mac by its name and its size.
		if out, _ := inSandbox(cfg.ID, "cat /workspace/shared/"+file+" 2>/dev/null"); trimmed(out) != "READY" {
			t.Errorf("%s left %q in the file it was asked for", known.Title, trimmed(out))
		}
		if info, err := os.Lstat(filepath.Join(folder, file)); err != nil || info.Size() < 5 || info.Size() > 6 {
			t.Errorf("%s: on this Mac the Shared Folder has no such file of that size: %v", known.Title, err)
		}
		for _, p := range paths {
			if p != folder && !strings.HasPrefix(p, folder+"/") {
				t.Errorf("the client of %s was told of the path %q, which is not in the Shared Folder", known.Title, p)
			}
		}
		for _, never := range []string{`"method":"fs/`, `"method":"terminal/`, `"_meta"`, key} {
			if strings.Contains(c.everything(), never) {
				t.Errorf("what the client of %s was sent holds %s", known.Title, redact(never))
			}
		}
		// What is found out: whether the agent, started so, called the
		// sandbox's own server, which is in its MCP Registry and which no
		// client gave it; and what the server said to an agent that is
		// in no Terminal Session.
		raised, answer := 0, ""
		for _, call := range updates.mcpCallsOf(cfg.ID)[calls:] {
			if call.Server != sandbox.StateServer {
				continue
			}
			if call.FromAgent && call.Method == "tools/call" {
				raised++
			}
			if !call.FromAgent && raised > 0 && answer == "" {
				answer = call.Summary
			}
		}
		t.Logf("%s, started by %q, called the tool of the sandbox's own MCP server %d times; the server answered: %.200s", known.Title, known.ACP, raised, redact(answer))

		c.in.Close()
		if status := c.ended(30 * time.Second); status != 0 {
			t.Errorf("after the client of %s left the bridge ended with %d; it said: %s", known.Title, status, redact(c.said.String()))
		}
		waitFor(t, 20*time.Second, "the processes of "+known.Title+" to end after its client left", func() bool { return processes(t, cfg.ID) <= baseline })
	}
}

// acpPrograms are the command lines that install the program by which
// an agent speaks the Agent Client Protocol, where that is a program of
// another name than the agent's: as their repositories gave them on 27
// November 2026. They are here and nowhere in the app.
var acpPrograms = map[string]string{
	"claude": "npm install --global @agentclientprotocol/claude-agent-acp",
	"codex":  "npm install --global @agentclientprotocol/codex-acp",
}

// The experiment of the agents' commands: each agent the app knows is
// installed by its maker's installer, and with it the program by which
// it speaks the protocol where that is another; then the sandbox's ACP
// Bridge starts it, by the command written beside its name in the app,
// and it answers `initialize` as an agent of the protocol does. No
// model is asked anything. A command that does not answer is one the
// app must not name: this test is the one that says so.
func TestACPBridgeExperimentOfEveryKnownAgent(t *testing.T) {
	_, folder := aSharedFolder(t, "test-acp-agents")
	cfg := openConfig(t, "acp-agents")
	cfg.Resources.MemoryMB = 6144
	cfg.SharedFolder, cfg.ACPBridge = "test-acp-agents", true
	startWith(t, cfg, nil, orchestrator.StartOptions{SharedFolder: folder, Link: serveLink(cfg.ID, nil, nil, nil)})
	noteLocks(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)

	for _, agent := range sandbox.KnownAgents {
		if len(agent.ACP) == 0 {
			t.Logf("%s: the app names no command by which it speaks the protocol", agent.Title)
			continue
		}
		waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
		if where := installedByHand(t, cfg.ID, agent); where == "" {
			continue
		}
		if install, besides := acpPrograms[agent.Executable]; besides {
			if out, code := inLoginShell(cfg.ID, install+" 2>&1 | tail -5"); code != 0 {
				t.Errorf("%s: installing the program by which it speaks the protocol: %s", agent.Title, out)
				continue
			}
		}
		if where := whereInstalled(cfg.ID, agent.ACP[0]); !strings.HasPrefix(where, sandbox.HomePath+"/") {
			t.Errorf("%s: a login shell finds %s at %q", agent.Title, agent.ACP[0], where)
			continue
		}
		baseline := processes(t, cfg.ID)
		c := startBridge(t, cfg, cfg.ID, agent.Executable)
		c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true},"clientInfo":{"name":"zeolite-test","version":"1"}}}`)
		answered := make(chan acpMessage, 1)
		go func() {
			for line := range c.lines {
				var m acpMessage
				if json.Unmarshal([]byte(line), &m) == nil && m.Method == "" && fmt.Sprint(m.ID) == "1" {
					m.raw = line
					answered <- m
					return
				}
			}
			close(answered)
		}()
		select {
		case m, ok := <-answered:
			var result struct {
				ProtocolVersion int `json:"protocolVersion"`
			}
			json.Unmarshal(m.Result, &result)
			switch {
			case !ok:
				t.Errorf("%s: started by %q, it ended without an answer; the bridge said: %s", agent.Title, agent.ACP, c.said)
			case m.Error != nil || result.ProtocolVersion != 1:
				t.Errorf("%s: started by %q, it answered initialize with %s", agent.Title, agent.ACP, m.raw)
			default:
				t.Logf("%s, started by %q, says of itself: %s", agent.Title, agent.ACP, m.Result)
			}
		case <-time.After(2 * time.Minute):
			t.Errorf("%s: started by %q, it did not answer initialize; the bridge said: %s", agent.Title, agent.ACP, c.said)
		}
		c.in.Close()
		c.cmd.Process.Kill()
		c.cmd.Wait()
		waitFor(t, 30*time.Second, "the processes of "+agent.Title+" to end", func() bool { return processes(t, cfg.ID) <= baseline })
	}
}
