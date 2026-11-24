//go:build integration

package integration

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// processes counts the processes in a sandbox's workstation, other than
// the ones the count itself takes.
func processes(t *testing.T, id string) int {
	t.Helper()
	out, code := inSandbox(id, "ls /proc | grep -c '^[0-9]'")
	if code != 0 {
		t.Fatalf("counting processes: %s", out)
	}
	n, _ := strconv.Atoi(trimmed(out))
	return n - 3 // sh, ls and grep
}

// shownTerminal drives a terminal view without a window.
type shownTerminal struct {
	t    *testing.T
	term *terminal.Terminal
	tt   *ui.Tester
}

func (s *shownTerminal) waitFor(what string, ok func(text string) bool) string {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s.tt.Frame()
		text := s.term.Text()
		if ok(text) {
			return text
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s; the terminal shows:\n%s", what, text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// showSession opens a Terminal Session in a sandbox and shows it in a
// terminal, as a Pane does: what runs there has a terminal that answers
// what it asks of one. onBell, where there is one, is told when the
// session's program rings. It returns once the shell is at its prompt;
// the session is closed when the test ends, if the test did not.
func showSession(t *testing.T, sandboxID string, onBell func()) (*shownTerminal, *orchestrator.TerminalSession) {
	t.Helper()
	session, err := orch.OpenTerminalSession(context.Background(), sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	term, err := terminal.New(terminal.Options{Conn: session, OnBell: onBell})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.Close(); session.Close() })
	tt := ui.NewTester(func(c *ui.Context) { terminal.View(c, term).Fill().AutoFocus() }, 1000, 480)
	shown := &shownTerminal{t, term, tt}
	shown.waitFor("the prompt", atPrompt)
	return shown, session
}

// until draws the terminal, which answers what its program asks of it,
// until ok or the time is up, and says whether it was ok.
func (s *shownTerminal) until(timeout time.Duration, ok func() bool) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(50 * time.Millisecond) {
		s.tt.Frame()
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func (s *shownTerminal) run(command string) {
	s.tt.Type(command)
	s.tt.Key(0, ui.KeyEnter)
}

// expectTTYSize checks that the shell's TTY has the terminal's size. The
// size reaches Docker a moment after the view changes, so it asks again
// until they agree.
func (s *shownTerminal) expectTTYSize(when string) {
	s.t.Helper()
	var cols, rows, c, r int
	for try := 0; try < 20; try++ {
		tag := fmt.Sprintf("size%d", time.Now().UnixNano()%1e9)
		s.run("echo " + tag + "=$(stty size | tr ' ' x)")
		re := regexp.MustCompile(tag + `=(\d+)x(\d+)`)
		m := re.FindStringSubmatch(s.waitFor("the TTY's size", re.MatchString))
		fmt.Sscan(m[1], &r)
		fmt.Sscan(m[2], &c)
		if cols, rows = s.term.Size(); c == cols && r == rows {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Errorf("%s the TTY is %dx%d and the terminal %dx%d", when, c, r, cols, rows)
}

// capture saves a picture of the terminal where ZEOLITE_SHOTS says.
func (s *shownTerminal) capture(name string) {
	dir := os.Getenv("ZEOLITE_SHOTS")
	if dir == "" {
		return
	}
	s.tt.Frame()
	file, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		s.t.Errorf("capture: %v", err)
		return
	}
	defer file.Close()
	if err := png.Encode(file, s.tt.Image()); err != nil {
		s.t.Errorf("capture: %v", err)
	}
}

func has(sub string) func(string) bool {
	return func(text string) bool { return strings.Contains(text, sub) }
}

func atPrompt(text string) bool { return strings.HasSuffix(strings.TrimSpace(text), "$") }

func TestSandboxTerminal(t *testing.T) {
	cfg := lockedConfig("terminal", "Terminal")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	baseline := processes(t, cfg.ID)

	open := func() (*terminal.Terminal, *orchestrator.TerminalSession) {
		t.Helper()
		session, err := orch.OpenTerminalSession(context.Background(), cfg.ID)
		if err != nil {
			t.Fatal(err)
		}
		term, err := terminal.New(terminal.Options{Conn: session})
		if err != nil {
			t.Fatal(err)
		}
		return term, session
	}
	first, firstSession := open()
	second, secondSession := open()
	// Each session has an ID of its own in the workstation, under which
	// the supervisor recorded it.
	if a, b := firstSession.ID(), secondSession.ID(); a == "" || a == b {
		t.Errorf("two sessions have the IDs %q and %q", a, b)
	}
	waitFor(t, 5*time.Second, "the supervisor to record both sessions under their IDs", func() bool {
		out, _ := inSandbox(cfg.ID, "ls /tmp/zeolite/sessions")
		return strings.Contains(out, firstSession.ID()) && strings.Contains(out, secondSession.ID())
	})
	defer first.Close()
	defer second.Close()

	// One view, showing whichever session is chosen, as a Pane shows
	// one session at a time.
	shown := first
	tt := ui.NewTester(func(c *ui.Context) { terminal.View(c, shown).Fill().AutoFocus() }, 800, 480)
	a := &shownTerminal{t, first, tt}
	b := &shownTerminal{t, second, tt}

	// The shell is the sandbox's: the agent's user, in /workspace.
	a.waitFor("the first prompt", atPrompt)
	a.run("echo who=$(id -u) where=$(pwd) term=$TERM")
	a.waitFor("the shell's identity", has("who=1000 where=/workspace term=xterm-256color"))

	// A prompt begins a line of its own, though the program before it
	// ended without ending its line.
	a.run("printf 'no-line-end'")
	a.waitFor("the prompt on a line of its own", func(text string) bool {
		return strings.Contains(text, "\nno-line-end\n") && atPrompt(text)
	})

	// The TTY in the workstation has the size of the view, and follows it.
	cols, rows := first.Size()
	a.expectTTYSize("at first")
	tt.SetSize(1200, 700)
	a.waitFor("the terminal to resize", func(string) bool { c, r := first.Size(); return c != cols || r != rows })
	cols, rows = first.Size()
	a.expectTTYSize("after a resize")

	// A full-screen editor: the alternate screen, keys, a resize inside it.
	a.run("vi /workspace/note.txt")
	a.waitFor("the editor", func(s string) bool { return strings.Contains(s, "note.txt") && strings.Contains(s, "~") })
	tt.Type("i")
	tt.Type("typed in the sandbox: 你好")
	tt.Key(0, ui.KeyEscape)
	tt.SetSize(900, 520)
	a.waitFor("a resize inside the editor", func(string) bool { c, r := first.Size(); return c != cols || r != rows })
	tt.Type(":wq")
	tt.Key(0, ui.KeyEnter)
	a.waitFor("the prompt after the editor", atPrompt)
	a.run("echo back=$(cat /workspace/note.txt)")
	text := a.waitFor("the file the editor wrote", has("back=typed in the sandbox: 你好"))
	if strings.Contains(text, "~\n~\n~") {
		t.Error("the editor's alternate screen leaked into the scrollback")
	}
	a.expectTTYSize("after a resize inside the editor")

	// A program that redraws by itself.
	a.run("top")
	a.waitFor("top", func(s string) bool { return strings.Contains(s, "PID") && strings.Contains(s, "load average") })
	tt.Type("q")
	a.waitFor("the prompt after top", atPrompt)

	// A second session is another shell, and the first keeps its screen
	// while the second is the one shown.
	a.run("echo marker-of-the-first-session")
	a.waitFor("the marker", has("\nmarker-of-the-first-session"))
	shown = second
	b.waitFor("the second session's prompt", atPrompt)
	b.run("echo $$ > /workspace/second.pid; echo in-the-second")
	if text := b.waitFor("the second session", has("\nin-the-second")); strings.Contains(text, "marker-of-the-first-session") {
		t.Error("the second session shows the first one's screen")
	}
	shown = first
	a.waitFor("the first session's screen again", has("\nmarker-of-the-first-session"))

	// Closing a session leaves nothing of it running: not the shell, not
	// a job it put in the background, not an editor left open.
	a.run("sleep 1000 &")
	a.run("vi")
	a.waitFor("the editor left open", func(s string) bool { return strings.Contains(s, "~") })
	if n := processes(t, cfg.ID); n < baseline+4 {
		t.Fatalf("%d processes with two sessions open, the baseline is %d", n, baseline)
	}
	if err := firstSession.Close(); err != nil {
		t.Errorf("closing the first session: %v", err)
	}
	waitFor(t, 5*time.Second, "the first session's processes to end", func() bool { return processes(t, cfg.ID) == baseline+1 })
	if out, _ := inSandbox(cfg.ID, "kill -0 $(cat /workspace/second.pid) && echo alive"); trimmed(out) != "alive" {
		t.Error("closing the first session ended the second")
	}
	secondSession.Close()
	waitFor(t, 5*time.Second, "the second session's processes to end", func() bool { return processes(t, cfg.ID) == baseline })

	// Stopping the sandbox ends a session that is still open.
	third, _ := open()
	defer third.Close()
	orch.Stop(context.Background(), cfg.ID)
	select {
	case <-third.Done():
	case <-time.After(10 * time.Second):
		t.Error("a session outlived its sandbox")
	}
}

// A Terminal Session is a login shell, and what runs in it its user
// starts there by typing. What the user put under the home by hand is
// started by its name, with no restart; when it quits, the shell is
// there again; and closing the session ends everything started in it.
// The test's agents are programs every workstation has, under agents'
// names.
func TestWhatIsInstalledByHandIsStartedByItsName(t *testing.T) {
	cfg := lockedConfig("agent-session", "Agent Session")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	baseline := processes(t, cfg.ID)

	// Where installers leave an executable, and where an installer wrote
	// its own place into ~/.bashrc.
	if out, code := inSandbox(cfg.ID, `mkdir -p ~/.local/bin ~/.opencode/bin ~/tools && for f in ~/.local/bin/claude ~/.opencode/bin/opencode ~/tools/kimi; do printf '#!/bin/sh\nexec top\n' > $f && chmod +x $f; done && echo 'export PATH="$HOME/tools:$PATH"' >> ~/.bashrc`); code != 0 {
		t.Fatalf("installing by hand: %s", out)
	}
	for executable, want := range map[string]string{"claude": "/workspace/.local/bin/claude", "opencode": "/workspace/.opencode/bin/opencode", "kimi": "/workspace/tools/kimi", "codex": ""} {
		if got := whereInstalled(cfg.ID, executable); got != want {
			t.Errorf("a login shell finds %s at %q, want %q", executable, got, want)
		}
	}

	// Typed by its name, what was installed by hand starts, as the
	// agent's user in /workspace; when it quits, the shell is there.
	for _, name := range []string{"claude", "kimi"} {
		shell, session := showSession(t, cfg.ID, nil)
		shell.run(name)
		shell.waitFor(name+", which is top", func(s string) bool { return strings.Contains(s, "PID") && strings.Contains(s, "load average") })
		if out, _ := inSandbox(cfg.ID, "for p in /proc/[0-9]*; do [ \"$(cat $p/comm 2>/dev/null)\" = top ] && echo $(stat -c %u $p) $(readlink $p/cwd); done"); trimmed(out) != "1000 /workspace" {
			t.Errorf("%s runs as %q, want the agent's user in /workspace", name, trimmed(out))
		}
		shell.tt.Type("q")
		shell.waitFor("the prompt after "+name, atPrompt)
		session.Close()
		waitFor(t, 5*time.Second, "the session's processes to end", func() bool { return processes(t, cfg.ID) == baseline })
	}

	// Closing a session ends what was started in it, and what that
	// started.
	vi, viSession := showSession(t, cfg.ID, nil)
	vi.run("vi")
	vi.waitFor("the editor", func(s string) bool { return strings.Contains(s, "~") })
	vi.tt.Type(":!sleep 1000 &")
	vi.tt.Key(0, ui.KeyEnter)
	waitFor(t, 5*time.Second, "what the editor started", func() bool { return processes(t, cfg.ID) >= baseline+3 })
	if err := viSession.Close(); err != nil {
		t.Errorf("closing the session: %v", err)
	}
	waitFor(t, 5*time.Second, "the session's processes to end", func() bool { return processes(t, cfg.ID) == baseline })
}

// raiseHandCalls are the lines an agent's MCP client sends to raise its
// hand: it meets the server, and calls its one tool.
const raiseHandCalls = `'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"raise_hand","arguments":{}}}'`

// An agent raises its hand by the MCP server sandbox, which every
// agent's registry holds: the server finds the Terminal Session it
// descends from, however far up, and the sandbox's Link carries the
// hand to the app, which takes it for that session and no other. It
// rings no terminal and carries no words. Outside every Terminal
// Session there is no hand to raise.
func TestAnAgentRaisesItsHand(t *testing.T) {
	cfg := lockedConfig("raise-hand", "Raise a hand")
	startWith(t, cfg, nil, orchestrator.StartOptions{Link: serveLink(cfg.ID, nil, nil, nil)})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)

	var rang atomic.Int32
	bell := func() { rang.Add(1) }
	agent, session := showSession(t, cfg.ID, bell)
	_, beside := showSession(t, cfg.ID, bell)

	// The server is in every agent's registry, and its bridge is how it
	// is started: from a tool of the agent's, in a session of its own,
	// with no terminal.
	if out, _ := inSandbox(cfg.ID, `cat "$OPENCODE_CONFIG" ~/.claude.json ~/.codex/config.toml ~/.grok/config.toml ~/.kimi-code/mcp.json ~/.omp/agent/mcp.json ~/.gemini/config/mcp_config.json | grep -c 'mcp-bridge.*sandbox'`); trimmed(out) != "7" {
		t.Errorf("the registries that hold the server sandbox: %q, want the seven", trimmed(out))
	}
	raise := "#!/bin/sh\n(printf '%s\\n' " + raiseHandCalls + "; sleep 2) | /usr/local/bin/supervisor mcp-bridge sandbox > /tmp/raised 2>&1\n"
	if out, code := dockerWith(nil, raise, "exec", "-i", orchestrator.WorkstationContainer(cfg.ID), "sh", "-c", "cat > /tmp/raise.sh && chmod +x /tmp/raise.sh"); code != 0 {
		t.Fatalf("writing the agent's tool: %s", out)
	}
	agent.run(`setsid -w sh -c 'setsid -w /tmp/raise.sh'; echo "done=[$(grep -c 'Your hand is raised' /tmp/raised)]"`)
	agent.waitFor("the hand to be raised", has("done=[1]"))
	waitFor(t, 5*time.Second, "the app to be told of the hand", func() bool { return len(updates.handsOf(cfg.ID)) == 1 })
	if hands := updates.handsOf(cfg.ID); hands[0] != session.ID() || hands[0] == beside.ID() {
		t.Errorf("the hand was raised for the session %q, want %q", hands[0], session.ID())
	}
	// It rang no terminal, and the server told its client its tool's
	// words, and nothing of where an agent works.
	if rang.Load() != 0 {
		t.Errorf("a raised hand rang %d bells", rang.Load())
	}
	if out, _ := inSandbox(cfg.ID, "cat /tmp/raised"); !strings.Contains(out, "The tool `raise_hand` of this server: Draw your user's attention to this session") || strings.Contains(out, "You work in") || strings.Contains(out, "/workspace") {
		t.Errorf("the server told its client:\n%s", out)
	}
	// The call was counted with the MCP calls, as a pack's are.
	waitFor(t, 5*time.Second, "the call to be reported", func() bool {
		for _, call := range updates.mcpCallsOf(cfg.ID) {
			if call.Server == sandbox.StateServer && call.FromAgent && call.Method == "tools/call" {
				return true
			}
		}
		return false
	})

	// Outside every Terminal Session there is no hand to raise, and the
	// app takes none for a session it did not open.
	out, _ := inSandbox(cfg.ID, "(printf '%s\\n' "+raiseHandCalls+"; sleep 1) | /usr/local/bin/supervisor state")
	if !strings.Contains(out, "not started from a terminal session") || strings.Contains(out, "Your hand is raised:") {
		t.Errorf("outside a Terminal Session the server said:\n%s", out)
	}
	if hands := updates.handsOf(cfg.ID); len(hands) != 1 {
		t.Errorf("the app was told of the hands %q", hands)
	}
	if got := raiseHand(cfg.ID, "0000000000000000"); !strings.Contains(got, "not a session your user has open") {
		t.Errorf("a hand of a session nobody opened was answered %q", got)
	}
	// No file in the Home tells agents any of this.
	if out, _ := inSandbox(cfg.ID, "ls /workspace/AGENTS.md /workspace/CLAUDE.md 2>/dev/null"); trimmed(out) != "" {
		t.Errorf("the Home holds %q", trimmed(out))
	}
}

// The workstation says which agent runs in each Terminal Session, from
// the names of the session's processes: a program started under an
// agent's name, natively or as a script of node or of python. A shell
// script named like an agent runs under a shell, and is not taken for
// one. The stand-ins here are programs every workstation has, under
// agents' names.
func TestTheWorkstationSaysWhichAgentRunsInASession(t *testing.T) {
	cfg := lockedConfig("session-facts", "Session facts")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	standIns := `mkdir -p ~/.local/bin && cd ~/.local/bin &&
printf '#!/usr/bin/env node\nsetInterval(() => {}, 1000)\n' > claude &&
printf '#!/usr/bin/env python3\nimport time\ntime.sleep(1000)\n' > kimi &&
cp /bin/sleep codex &&
printf '#!/bin/sh\nsleep 1000\n' > grok &&
chmod +x claude kimi codex grok`
	if out, code := inSandbox(cfg.ID, standIns); code != 0 {
		t.Fatalf("making the stand-ins: %s", out)
	}
	// said is what the workstation last said runs in a session.
	said := func(id string) (agent string, reported bool) {
		status, _ := updates.workstationOf(cfg.ID)
		for _, s := range status.Sessions {
			if s.ID == id {
				return s.Agent, true
			}
		}
		return "", false
	}

	// A session of a plain shell is reported, with no agent.
	shell, session := showSession(t, cfg.ID, nil)
	waitFor(t, 10*time.Second, "the workstation to report the session", func() bool {
		agent, reported := said(session.ID())
		return reported && agent == ""
	})
	for _, started := range []struct{ command, agent string }{
		{"claude", "claude"},    // a node script
		{"kimi", "kimi"},        // a python script
		{"codex 1000", "codex"}, // a program of its own
		{"grok", ""},            // a shell script: no agent
		{"vi", ""},              // what is no agent at all
	} {
		shell.run(started.command)
		if started.agent == "" {
			// Long enough for two of the supervisor's looks.
			time.Sleep(5 * time.Second)
			if agent, _ := said(session.ID()); agent != "" {
				t.Errorf("with %q typed, the session is said to run the agent %q", started.command, agent)
			}
		} else {
			waitFor(t, 10*time.Second, "the workstation to say that "+started.agent+" runs in the session", func() bool {
				agent, _ := said(session.ID())
				return agent == started.agent
			})
		}
		// It quits: the session is a shell's again.
		if started.command == "vi" {
			shell.tt.Type(":q!")
			shell.tt.Key(0, ui.KeyEnter)
		} else {
			shell.tt.Key(ui.Ctrl, ui.KeyC)
		}
		shell.waitFor("the prompt after "+started.command, atPrompt)
		waitFor(t, 10*time.Second, "the workstation to say that no agent runs in the session", func() bool {
			agent, reported := said(session.ID())
			return reported && agent == ""
		})
	}

	// Another session's agent says nothing of this one, and a session
	// that was closed is reported no more.
	other, otherSession := showSession(t, cfg.ID, nil)
	other.run("claude")
	waitFor(t, 10*time.Second, "the other session's agent", func() bool {
		agent, _ := said(otherSession.ID())
		return agent == "claude"
	})
	if agent, reported := said(session.ID()); !reported || agent != "" {
		t.Errorf("beside a session of an agent's, the plain one is said to run %q (reported: %v)", agent, reported)
	}
	otherSession.Close()
	session.Close()
	waitFor(t, 10*time.Second, "the closed sessions to be reported no more", func() bool {
		_, one := said(session.ID())
		_, two := said(otherSession.ID())
		return !one && !two
	})
}
