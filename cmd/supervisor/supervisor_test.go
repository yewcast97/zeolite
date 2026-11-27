package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"zeolite/internal/sandbox"
)

func TestDescribeMCPMessage(t *testing.T) {
	tests := []struct {
		line            string
		method, summary string
	}{
		{`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"navigate","arguments":{"url":"http://localhost:3000"}}}`,
			"tools/call", `{"name":"navigate","arguments":{"url":"http://localhost:3000"}}`},
		{`{"jsonrpc":"2.0","id":"a1","result":{"content":[]}}`, "", `{"content":[]}`},
		{`{"jsonrpc":"2.0","id":8,"error":{"code":-32601,"message":"no such tool"}}`, "", `error {"code":-32601,"message":"no such tool"}`},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "notifications/initialized", ""},
	}
	for _, tt := range tests {
		got := describeMCPMessage([]byte(tt.line))
		if got.Method != tt.method || got.Summary != tt.summary {
			t.Errorf("%s\n  described as %+v", tt.line, got)
		}
	}

	long := describeMCPMessage([]byte(`{"id":1,"result":"` + strings.Repeat("好", 500) + `"}`))
	if n := len([]rune(long.Summary)); n != summaryLimit+1 {
		t.Errorf("a long result is summarized in %d characters", n)
	}
	if got := describeMCPMessage([]byte("plain text\n")); !strings.HasPrefix(got.Summary, "not JSON-RPC") {
		t.Errorf("a line that is not JSON is described as %+v", got)
	}
}

func TestCopyLinesPassesEverythingThrough(t *testing.T) {
	in := "{\"id\":1}\n{\"id\":2}\nno newline at the end"
	var out bytes.Buffer
	var seen []string
	copyLines(&out, strings.NewReader(in), func(line []byte) { seen = append(seen, string(line)) })
	if out.String() != in {
		t.Errorf("copied %q", out.String())
	}
	if len(seen) != 3 {
		t.Errorf("saw %d lines", len(seen))
	}
}

func TestSessionIDs(t *testing.T) {
	if _, err := sessionFile("s1-a3f9"); err != nil {
		t.Error(err)
	}
	for _, id := range []string{"", "../etc/passwd", "a/b", "UPPER", strings.Repeat("x", 65)} {
		if _, err := sessionFile(id); err == nil {
			t.Errorf("%q is accepted as a session ID", id)
		}
	}
}

func TestStatSession(t *testing.T) {
	tests := []struct {
		stat string
		want int
		ok   bool
	}{
		{"61 (bash) S 0 61 61 34816 79 4194560 ...", 61, true},
		// A command name with spaces and parentheses.
		{"79 (my (odd) tool) R 61 79 61 34816 79 4194304 ...", 61, true},
		{"garbage", 0, false},
	}
	for _, tt := range tests {
		got, ok := statSession(tt.stat)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%q: session %d, %v", tt.stat, got, ok)
		}
	}
}

// A Terminal Session is a login shell and nothing else: its command line
// is one word, which no request can add to, and what starts it gives a
// shell no text to read and no program to start. The supervisor's one
// command that gives a shell a text is the ACP Bridge's, and that text
// is fixed.
func TestASessionIsALoginShell(t *testing.T) {
	if len(shellArgv) != 1 || shellArgv[0] != "-bash" {
		t.Errorf("a session's shell is %q, want a login shell alone", shellArgv)
	}
	source, err := os.ReadFile("session.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{`"-c"`, "--login", "os/exec"} {
		if strings.Contains(string(source), never) {
			t.Errorf("session.go holds %s: a session's shell is given nothing to read or to start", never)
		}
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), `len(args) == 2 && args[0] == "shell"`) {
		t.Error("the supervisor's command for a session is not a shell by the session's ID alone")
	}
}

// The server by which an agent tells of its state speaks for the
// Terminal Session it descends from, however far up that is: the
// session's shell started the agent, the agent its client's servers.
// It reads the kernel's list of processes, and no terminal.
func TestTheStateServerFindsItsSession(t *testing.T) {
	dir := t.TempDir()
	oldProc, oldSessions := procDir, sessionDir
	procDir, sessionDir = filepath.Join(dir, "proc"), filepath.Join(dir, "sessions")
	defer func() { procDir, sessionDir = oldProc, oldSessions }()

	// process puts a process where the kernel would show it.
	process := func(pid, parent int) {
		at := filepath.Join(procDir, strconv.Itoa(pid))
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (a (tool) of its own) S %d %d %d 34816 0 0", pid, parent, pid, pid)
		if err := os.WriteFile(filepath.Join(at, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	self := os.Getpid()

	// With no Terminal Session above it there is none to speak for.
	process(self, 70)
	process(70, 1)
	if id, err := ownSession(); err == nil || id != "" {
		t.Errorf("with no Terminal Session above the server it speaks for %q (%v)", id, err)
	}

	// The shell of the session aaa is 61, and another session, bbb, is
	// not the server's: the server is started by the bridge, 70, which
	// the agent, 65, started.
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sessionDir, "aaa"), []byte("61"), 0o600)
	os.WriteFile(filepath.Join(sessionDir, "bbb"), []byte("90"), 0o600)
	process(self, 70)
	process(70, 65)
	process(65, 61)
	process(61, 1)
	process(90, 1)
	if id, err := ownSession(); err != nil || id != "aaa" {
		t.Errorf("the server speaks for the session %q (%v), want aaa", id, err)
	}
	// One whose agent left it to the workstation's first process has no
	// session any more.
	process(70, 1)
	if id, err := ownSession(); err == nil {
		t.Errorf("a server that was left to the first process speaks for %q", id)
	}
	source, err := os.ReadFile("state.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{`"fd"`, "/dev/", "OpenFile", "os/exec", `"\a"`} {
		if strings.Contains(string(source), never) {
			t.Errorf("state.go names %s: a raised hand goes by the Link, and rings no terminal", never)
		}
	}
}

// What every start writes into the sandbox's Home does not stop the
// workstation when it cannot be written: one registry's failure is
// reported, and the others are written all the same. No file that tells
// agents how to work is written there.
func TestAHomeThatRefusesAFileIsStillPrepared(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes where a mode forbids it")
	}
	dir := t.TempDir()
	files := homeFiles{
		home:          dir,
		ownRegistries: filepath.Join(dir, "own"),
		sharedFolder:  filepath.Join(dir, "shared"),
	}
	// Claude Code's registry is a file that is no JSON.
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("// mine\n{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	errs := prepareHome(files, "/usr/local/bin/supervisor", sandbox.SupervisorConfig{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), ".claude.json") {
		t.Fatalf("preparing a Home with a registry that does not read: %v", errs)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".claude.json")); string(data) != "// mine\n{}" {
		t.Errorf("the registry that does not read now holds %q", data)
	}
	// Every sandbox has the server by which an agent tells of its
	// state: the other agents' registries hold it.
	for _, written := range []string{filepath.Join(files.ownRegistries, "opencode.json"), filepath.Join(dir, ".codex", "config.toml"), filepath.Join(dir, ".kimi-code", "mcp.json")} {
		if data, err := os.ReadFile(written); err != nil || !strings.Contains(string(data), "sandbox") {
			t.Errorf("%s was not written beside the one that failed: %v", filepath.Base(written), err)
		}
	}
	for _, never := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(dir, never)); err == nil {
			t.Errorf("%s was written into the Home", never)
		}
	}
}

// A sandbox that has no Shared Folder has no /workspace/shared: the
// empty folder a start with one left in the Home is removed, where it
// would look like a Shared Folder.
// Nothing that holds anything is removed, and nothing is where the
// sandbox has a Shared Folder.
func TestTheLeftoverSharedFolderIsRemovedFromAHome(t *testing.T) {
	prepare := func(t *testing.T, shared bool, make func(path string)) (string, []error) {
		t.Helper()
		dir := t.TempDir()
		files := homeFiles{
			home:          dir,
			ownRegistries: filepath.Join(dir, "own"),
			sharedFolder:  filepath.Join(dir, "shared"),
		}
		if make != nil {
			make(files.sharedFolder)
		}
		return files.sharedFolder, prepareHome(files, "/usr/local/bin/supervisor", sandbox.SupervisorConfig{SharedFolder: shared})
	}
	empty := func(path string) {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	full := func(path string) {
		empty(path)
		if err := os.WriteFile(filepath.Join(path, "notes"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file := func(path string) {
		if err := os.WriteFile(path, []byte("a file"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if path, errs := prepare(t, false, empty); len(errs) != 0 {
		t.Errorf("removing the empty folder: %v", errs)
	} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the empty folder a Shared Folder left is still there: %v", err)
	}
	if _, errs := prepare(t, false, nil); len(errs) != 0 {
		t.Errorf("with no folder to remove: %v", errs)
	}
	if path, errs := prepare(t, false, full); len(errs) != 0 {
		t.Errorf("with a folder that holds something: %v", errs)
	} else if data, _ := os.ReadFile(filepath.Join(path, "notes")); string(data) != "mine" {
		t.Error("a folder that holds something was removed")
	}
	if path, errs := prepare(t, false, file); len(errs) != 0 {
		t.Errorf("with a file of that name: %v", errs)
	} else if data, _ := os.ReadFile(path); string(data) != "a file" {
		t.Error("a file of that name was removed")
	}
	// With a Shared Folder, the folder is its mountpoint and stays.
	if path, errs := prepare(t, true, empty); len(errs) != 0 {
		t.Errorf("with a Shared Folder: %v", errs)
	} else if _, err := os.Lstat(path); err != nil {
		t.Errorf("the Shared Folder's place was removed: %v", err)
	}
}

// A process is an agent by the name it was started under: its own, or
// under an interpreter the name of its script, which is the first thing
// the interpreter is given that is no option. A shell script named like
// an agent runs under a shell, and is not taken for one.
func TestAgentOf(t *testing.T) {
	for want, lines := range map[string][][]string{
		"claude": {
			{"claude"}, {"/workspace/.local/bin/claude"}, {"claude", "--resume"},
			{"node", "/workspace/.npm-global/bin/claude"}, {"/usr/local/bin/node", "--no-warnings", "/workspace/.npm-global/bin/claude", "-p", "codex"},
		},
		"codex":    {{"codex"}, {"node", "/workspace/.npm-global/lib/node_modules/codex/bin/codex.js"}, {"node", "codex.mjs"}, {"deno", "--allow-all", "/workspace/bin/codex.js"}},
		"opencode": {{"/workspace/.opencode/bin/opencode"}, {"bun", "/workspace/.bun/bin/opencode"}},
		"kimi":     {{"python3", "/workspace/.kimi-code/bin/kimi"}, {"/usr/bin/python3.12", "-u", "kimi.py"}, {"python", "kimi"}},
		"omp":      {{"bun", "/workspace/.bun/bin/omp"}, {"omp"}},
		"agy":      {{"/workspace/.local/bin/agy"}},
		"grok":     {{"grok"}, {"node", "grok.cjs"}},
		"": {
			nil, {}, {""}, {"bash"}, {"-bash"}, {"vi", "claude"}, {"top"},
			// A shell script named like an agent, and a shell told its name.
			{"/bin/sh", "/workspace/.local/bin/claude"}, {"bash", "/workspace/bin/codex"}, {"sh", "-c", "claude"}, {"bash", "claude"},
			// An interpreter whose script is something else, or that has none.
			{"node"}, {"node", "/workspace/app/server.js", "claude"}, {"python3", "-m", "http.server", "claude"}, {"node", "--version"},
			{"node", "/workspace/claude/index.js"}, {"python3", "/workspace/tools/claude.txt"},
			// What is only named like one.
			{"claude-code"}, {"Claude"}, {"/workspace/claude/bin/run"}, {"notclaude"}, {"ruby", "claude"},
		},
	} {
		for _, argv := range lines {
			if got := agentOf(argv); got != want {
				t.Errorf("%q is taken for the agent %q, want %q", argv, got, want)
			}
		}
	}
	for _, a := range sandbox.KnownAgents {
		if agentOf([]string{a.Executable}) != a.Executable {
			t.Errorf("%s is not known by its executable", a.Title)
		}
	}
}

// What the supervisor says of the sessions comes from the kernel's list
// of processes: each recorded session that has a process left, with the
// agent among its processes. A session whose processes are gone is not
// reported, and a process of another session says nothing of this one.
func TestSessionStates(t *testing.T) {
	dir := t.TempDir()
	oldProc, oldSessions := procDir, sessionDir
	procDir, sessionDir = filepath.Join(dir, "proc"), filepath.Join(dir, "sessions")
	defer func() { procDir, sessionDir = oldProc, oldSessions }()
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	process := func(pid, session int, argv ...string) {
		at := filepath.Join(procDir, strconv.Itoa(pid))
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (some (odd) name) S 1 %d %d 34816 0 0", pid, pid, session)
		if err := os.WriteFile(filepath.Join(at, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		cmdline := strings.Join(argv, "\x00")
		if len(argv) > 0 {
			cmdline += "\x00"
		}
		if err := os.WriteFile(filepath.Join(at, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	record := func(id string, leader int) {
		if err := os.WriteFile(filepath.Join(sessionDir, id), []byte(strconv.Itoa(leader)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// What is no process: the kernel lists other things too.
	os.MkdirAll(filepath.Join(procDir, "self"), 0o755)

	// A shell alone; a shell with an agent under node, and its tools; a
	// shell whose agent's tool, in a session of its own, is named like
	// another agent; a session whose shell is gone; and one that was
	// never started.
	record("aaa", 100)
	process(100, 100, "-bash")
	record("bbb", 200)
	process(200, 200, "-bash")
	process(210, 200, "node", "/workspace/.npm-global/bin/claude")
	process(215, 200, "/bin/sh", "-c", "sleep 100")
	record("ccc", 300)
	process(300, 300, "-bash")
	process(310, 300, "bash", "/workspace/bin/launcher")
	process(320, 300, "/workspace/.opencode/bin/opencode")
	process(330, 330, "codex", "exec")
	record("ddd", 400)
	record("eee", 0)
	os.WriteFile(filepath.Join(sessionDir, "fff"), []byte("not a number"), 0o600)

	got := sessionStates()
	want := []sandbox.SessionState{{ID: "aaa"}, {ID: "bbb", Agent: "claude"}, {ID: "ccc", Agent: "opencode"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the sessions are said to be %+v, want %+v", got, want)
	}

	// Two agents in one session: the one that was there first.
	process(205, 200, "codex")
	if got := sessionStates(); len(got) != 3 || got[1].Agent != "codex" {
		t.Errorf("with two agents in a session it is said to be %+v", got)
	}
	// The agent quits: its session is a shell's again.
	os.RemoveAll(filepath.Join(procDir, "205"))
	os.RemoveAll(filepath.Join(procDir, "210"))
	if got := sessionStates(); len(got) != 3 || got[1].Agent != "" {
		t.Errorf("after its agent quit the session is said to be %+v", got)
	}
	// With no session recorded there is nothing to say, and nothing is
	// read of a session's terminal: the source opens no such file.
	source, err := os.ReadFile("sessionfacts.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{`"fd"`, "/dev/", "pts", "OpenFile"} {
		if strings.Contains(string(source), never) {
			t.Errorf("sessionfacts.go names %s: what a session is said to run comes from its processes' names alone", never)
		}
	}
}
