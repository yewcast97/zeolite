//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"zeolite/internal/hosttool"
	"zeolite/internal/link"
	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// A sandbox that is given Host Tools runs programs of this Mac from its
// workstation, by the MCP server the supervisor registers: the call
// crosses on a stream the Docker API carries, the app runs the program
// under the Host Tool Wall, in the sandbox's Shared Folder, and the
// answer comes back. What the Wall refuses is tested where the Wall is,
// in internal/hosttool; here it is the way there and back.

// callHostTool makes one call of the tool run as an agent's MCP client
// does, through the bridge, and returns what the tool said and whether
// it was an error. The client stays for a while after it asked: one that
// leaves withdraws its call.
func callHostTool(t *testing.T, sandboxID, arguments string, stays int) (string, bool) {
	t.Helper()
	return callHostToolBy(t, sandboxID, arguments, stays, "/usr/local/bin/supervisor mcp-bridge macos")
}

// callHostToolBy is callHostTool with the server's command line said:
// the bridge, as an agent's registry has it, or the server itself, as
// an agent that looked for it might start it.
func callHostToolBy(t *testing.T, sandboxID, arguments string, stays int, server string) (string, bool) {
	t.Helper()
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run","arguments":` + arguments + `}}`,
	}
	script := "(printf '%s\\n'"
	for _, line := range lines {
		if strings.Contains(line, "'") {
			t.Fatalf("a call with a quote in it: %s", line)
		}
		script += " '" + line + "'"
	}
	script += fmt.Sprintf("; sleep %d) | %s", stays, server)
	out, _ := inSandbox(sandboxID, script)
	for _, line := range strings.Split(out, "\n") {
		var msg struct {
			ID     int
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		if json.Unmarshal([]byte(line), &msg) == nil && msg.ID == 2 && len(msg.Result.Content) == 1 {
			return msg.Result.Content[0].Text, msg.Result.IsError
		}
	}
	t.Fatalf("the server did not answer the call:\n%s", out)
	return "", false
}

// raiseHand takes a raised hand as the app does: for a session the app
// opened in that sandbox, which the tests' recorder is then told of,
// and for no other.
func raiseHand(sandboxID, sessionID string) string {
	if !orch.HasSession(sandboxID, sessionID) {
		return "this is not a session your user has open"
	}
	updates.listen(sandbox.Update{SandboxID: sandboxID, Hand: &sandbox.Hand{Session: sessionID}})
	return ""
}

// serveLink serves a sandbox's Link as the app does: it takes raised
// hands, and runs its Host Tools, for as long as the Link is open, where
// it is given some, and none where it is not. It returns the Link's serving for a
// sandbox's StartOptions, and tells ended how the Link ended.
func serveLink(sandboxID string, tools *hosttool.Tools, settings *hosttool.Settings, ended chan<- error) func(io.ReadWriteCloser) {
	return func(stream io.ReadWriteCloser) {
		handlers := link.Handlers{Hand: func(r sandbox.HandRequest) string { return raiseHand(sandboxID, r.Session) }}
		if settings != nil {
			served, err := tools.Serve(*settings)
			if err != nil {
				stream.Close()
				if ended != nil {
					ended <- err
				}
				return
			}
			defer served.Close()
			handlers.HostTool = served.Run
		}
		err := link.Serve(stream, handlers)
		if ended != nil {
			ended <- err
		}
	}
}

func TestHostTools(t *testing.T) {
	const name = "test-host-tools"
	_, folder := aSharedFolder(t, name)
	tools, err := hosttool.Install(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := lockedConfig("host-tools", "Host Tools")
	cfg.SharedFolder = name
	cfg.HostTools.Enabled = true
	served := make(chan error, 1)
	startWith(t, cfg, nil, orchestrator.StartOptions{
		SharedFolder: folder,
		Link:         serveLink(cfg.ID, tools, &hosttool.Settings{SandboxID: cfg.ID, SharedFolder: folder}, served),
	})
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)

	// The agent finds the server in its registry, behind the bridge, and
	// is told how Host Tools work.
	// Every agent the app knows: each has the server in its own
	// registry, and OpenCode is sent to the one that is the app's own.
	if out, _ := inSandbox(cfg.ID, `cat "$OPENCODE_CONFIG"; echo; echo "$OPENCODE_CONFIG"`); !strings.Contains(out, `"macos"`) || !strings.Contains(out, `"mcp-bridge"`) || !strings.HasSuffix(trimmed(out), "/tmp/zeolite/opencode.json") {
		t.Errorf("OpenCode's registry is %s", out)
	}
	for _, a := range sandbox.KnownAgents {
		if a.Registry.Variable != "" {
			continue
		}
		if out, code := inSandbox(cfg.ID, "cat ~/"+a.Registry.File); code != 0 || !strings.Contains(out, "macos") || !strings.Contains(out, "/usr/local/bin/supervisor") || !strings.Contains(out, "mcp-bridge") {
			t.Errorf("the registry of %s, ~/%s, is (exit %d) %s", a.Title, a.Registry.File, code, out)
		}
	}
	// What the supervisor was told is where the bridge finds it, and is
	// the agent's user's alone.
	if out, _ := inSandbox(cfg.ID, "stat -c '%a %u' /tmp/zeolite/config.json"); trimmed(out) != "600 1000" {
		t.Errorf("what the supervisor was told is kept as %q", trimmed(out))
	}
	// What an agent is told of Host Tools, the server tells it as it
	// meets it, and in its tool's description: no file in the Home does.
	met, _ := inSandbox(cfg.ID, `printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | /usr/local/bin/supervisor mcp-bridge macos; ls -A /workspace`)
	if strings.Count(met, "Run a program on a Mac, in a restricted macOS environment") != 2 || !strings.Contains(met, "The Mac's GPU is not given") || !strings.Contains(met, "nothing of /Applications") || strings.Contains(met, "AGENTS.md") || strings.Contains(met, "CLAUDE.md") {
		t.Errorf("the agent is told %s", met)
	}

	// A program of this Mac, run from the workstation: it says what it
	// runs on, starts in the Shared Folder as this Mac has it, and what
	// it wrote there the workstation reads.
	said, failed := callHostTool(t, cfg.ID, `{"argv":["/bin/sh","-c","sw_vers -productName; pwd; sw_vers -productName > made-on-the-mac.txt"]}`, 5)
	if failed || !strings.HasPrefix(said, "Exit status 0.") || !strings.Contains(said, "macOS") || !strings.Contains(said, "/"+name+"\n") {
		t.Fatalf("a Host Tool said %q (error %v)", said, failed)
	}
	if out, _ := inSandbox(cfg.ID, "cat /workspace/shared/made-on-the-mac.txt"); trimmed(out) != "macOS" {
		t.Errorf("what the Host Tool wrote reads %q in the workstation", trimmed(out))
	}
	// What the agent wrote in the workstation, the program reads; in a
	// folder of the Shared Folder, named from it.
	if out, code := inSandbox(cfg.ID, "mkdir -p /workspace/shared/work && echo from-the-sandbox > /workspace/shared/work/in.txt"); code != 0 {
		t.Fatalf("writing in the Shared Folder: %s", out)
	}
	if said, failed := callHostTool(t, cfg.ID, `{"argv":["/bin/cat","in.txt"],"dir":"work"}`, 5); failed || !strings.Contains(said, "--- stdout ---\nfrom-the-sandbox\n") {
		t.Errorf("a Host Tool that reads the agent's file said %q (error %v)", said, failed)
	}
	// Its status and what it printed on each stream come back.
	if said, failed := callHostTool(t, cfg.ID, `{"argv":["/bin/sh","-c","echo out; echo err >&2; exit 3"]}`, 5); failed || !strings.HasPrefix(said, "Exit status 3.") || !strings.Contains(said, "--- stdout ---\nout\n--- stderr ---\nerr\n") {
		t.Errorf("a Host Tool that fails said %q (error %v)", said, failed)
	}
	// It is under the Wall: nothing of this Mac's users, and no network.
	if said, _ := callHostTool(t, cfg.ID, `{"argv":["/bin/ls","/Users"]}`, 5); strings.HasPrefix(said, "Exit status 0.") {
		t.Errorf("a Host Tool listed this Mac's users: %q", said)
	}
	// Nor what this Mac has in /Applications, of which this sandbox is
	// given nothing.
	if said, _ := callHostTool(t, cfg.ID, `{"argv":["/bin/ls","/Applications"]}`, 5); strings.HasPrefix(said, "Exit status 0.") {
		t.Errorf("a Host Tool listed this Mac's applications: %q", said)
	}
	if said, _ := callHostTool(t, cfg.ID, `{"argv":["/usr/bin/curl","-sS","-m","3","https://example.com"]}`, 8); strings.HasPrefix(said, "Exit status 0.") {
		t.Errorf("a Host Tool reached the network: %q", said)
	}
	// A call that names a folder outside the Shared Folder is refused in
	// the workstation, and one that takes too long is ended.
	if said, failed := callHostTool(t, cfg.ID, `{"argv":["/bin/ls"],"dir":"../other"}`, 2); !failed || !strings.Contains(said, "steps out") {
		t.Errorf("a run outside the Shared Folder said %q (error %v)", said, failed)
	}
	if said, failed := callHostTool(t, cfg.ID, `{"argv":["/bin/sleep","60"],"seconds":1}`, 6); !failed || !strings.HasPrefix(said, "Ended after 1 seconds") {
		t.Errorf("a run that takes too long said %q (error %v)", said, failed)
	}

	// The calls crossed the bridge, which reported each: they are what
	// the dashboard lists under MCP Calls.
	waitFor(t, 10*time.Second, "the calls to be reported", func() bool {
		asked, answered := 0, 0
		for _, call := range updates.mcpCallsOf(cfg.ID) {
			if call.Server != sandbox.HostToolServer {
				continue
			}
			if call.FromAgent && call.Method == "tools/call" {
				asked++
			} else if !call.FromAgent && strings.Contains(call.Summary, "Exit status") {
				answered++
			}
		}
		return asked >= 7 && answered >= 5
	})

	// A sandbox that is given the same folder and no Host Tools reaches
	// no program of this Mac: its agent is told of none, and the app
	// refuses what its Link carries of them.
	other := lockedConfig("host-tools-none", "No Host Tools")
	other.SharedFolder = name
	startWith(t, other, nil, orchestrator.StartOptions{SharedFolder: folder, Link: serveLink(other.ID, tools, nil, nil)})
	waitState(t, other.ID, sandbox.StateGateLocked, 20*time.Second)
	if out, _ := inSandbox(other.ID, `cat "$OPENCODE_CONFIG" ~/.claude.json ~/.codex/config.toml`); strings.Contains(out, `macos`) || strings.Count(out, "sandbox") < 3 {
		t.Errorf("the registries of a sandbox with no Host Tools hold %s", out)
	}
	// The bridge starts no server the sandbox has not.
	if out, code := inSandbox(other.ID, "/usr/local/bin/supervisor mcp-bridge macos < /dev/null 2>&1"); code == 0 || !strings.Contains(out, `no MCP server "macos"`) {
		t.Errorf("the bridge of a server the sandbox has not said %q (exit %d)", trimmed(out), code)
	}
	before, _ := os.ReadDir(folder)
	if said, failed := callHostToolBy(t, other.ID, `{"argv":["/usr/bin/touch","made-with-no-host-tools"]}`, 2, "/usr/local/bin/supervisor host-tools"); !failed || !strings.Contains(said, "running programs on the Mac is not allowed here") {
		t.Errorf("in a sandbox with no Host Tools the tool said %q (error %v)", said, failed)
	}
	if after, _ := os.ReadDir(folder); len(after) != len(before) {
		t.Errorf("a sandbox with no Host Tools had a program of this Mac run: the folder went from %d names to %d", len(before), len(after))
	}
	// And a sandbox whose Link is not open has nobody to ask.
	docker("exec", orchestrator.WorkstationContainer(other.ID), "pkill", "-x", "-f", "/usr/local/bin/supervisor link")
	waitFor(t, 10*time.Second, "the link to go", func() bool {
		_, code := docker("exec", orchestrator.WorkstationContainer(other.ID), "pgrep", "-x", "-f", "/usr/local/bin/supervisor link")
		return code != 0
	})
	if said, failed := callHostToolBy(t, other.ID, `{"argv":["/bin/ls"]}`, 2, "/usr/local/bin/supervisor host-tools"); !failed || !strings.Contains(said, "what answers this tool is not connected now") {
		t.Errorf("with its Link gone the tool said %q (error %v)", said, failed)
	}

	// A program that still runs when its sandbox stops is ended with it,
	// and the Link with it. The client is left in the workstation,
	// waiting.
	long := `{"argv":["/bin/sh","-c","echo $$ > running.txt; sleep 300"],"seconds":600}`
	docker("exec", "-d", orchestrator.WorkstationContainer(cfg.ID), "sh", "-c",
		`(printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run","arguments":`+long+`}}'; sleep 300) | /usr/local/bin/supervisor host-tools`)
	var pid int
	waitFor(t, 15*time.Second, "the long Host Tool to start", func() bool {
		out, _ := inSandbox(cfg.ID, "cat /workspace/shared/running.txt 2>/dev/null")
		pid, _ = strconv.Atoi(trimmed(out))
		return pid > 1
	})
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("the long Host Tool, process %d, does not run on this Mac", pid)
	}
	if err := tools.RemoveScratch(cfg.ID); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("deleting the Scratch of a sandbox whose Host Tools are served: %v", err)
	}
	orch.Stop(context.Background(), cfg.ID)
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("the Link of a sandbox that was stopped ended with %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the sandbox stopped and its Host Tools are still served")
	}
	waitFor(t, 5*time.Second, "the long Host Tool to end", func() bool { return syscall.Kill(pid, 0) != nil })
	// Once it is stopped, its Scratch is deleted, as its destruction does.
	if err := tools.RemoveScratch(cfg.ID); err != nil {
		t.Errorf("deleting the Scratch of a stopped sandbox: %v", err)
	}
}

// The Host Tool experiment: the model of the other experiment, run by
// opencode in a sandbox that leaves by the virtual Interface and is
// given Host Tools with the GPU, is asked for two things only this Mac
// can give: its system's version, and a video encoded by its hardware.
// Nobody tells it how: what it knows of Host Tools is what the server
// the supervisor registered tells its client. Every verdict comes from
// files.
func TestHostToolExperiment(t *testing.T) {
	iface := theVirtualInterface(t)
	key := openRouterKey(t)
	if _, err := os.Stat("/opt/homebrew/bin/ffmpeg"); err != nil {
		t.Skip("this Mac has no ffmpeg for the agent to encode with")
	}
	const name = "test-host-experiment"
	_, folder := aSharedFolder(t, name)
	tools, err := hosttool.Install(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cfg := virtualInterfaceConfig(iface, "host-experiment")
	cfg.Resources.MemoryMB = 6144
	cfg.SharedFolder = name
	cfg.HostTools = sandbox.HostTools{Enabled: true, GPU: true}
	e := &experiment{t: t, cfg: cfg, key: key, secrets: map[string]string{key: "the API key", iface.ExitAddress(): "the exit address"}}
	if physical, err := seenAt(); err == nil {
		e.secrets[physical] = "this Mac's physical exit address"
	}
	startWith(t, cfg, nil, orchestrator.StartOptions{
		SharedFolder: folder,
		Link:         serveLink(cfg.ID, tools, &hosttool.Settings{SandboxID: cfg.ID, SharedFolder: folder, GPU: true}, nil),
	})
	noteLocks(t, cfg.ID)
	capture := captureTUN(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
	installAgent(t, cfg.ID)
	defineModel(t, cfg.ID)

	// 1. What the sandbox cannot know: the system this Mac runs.
	version, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		t.Fatal(err)
	}
	e.ask("1. this Mac's system", "Find out which version of macOS the Mac that hosts this sandbox runs. "+
		"Write the version number, and nothing else, to /workspace/shared/mac-version.txt, then stop.")
	if got := trimmed(e.read("/workspace/shared/mac-version.txt")); got != trimmed(string(version)) {
		t.Errorf("the agent wrote %q for this Mac's system, which is %s", got, trimmed(string(version)))
	}

	// 2. What only this Mac's hardware does: a video its encoder made.
	e.ask("2. the hardware's encoder", "Make a two-second test video, 320 by 240 pixels, encoded by the hardware H.264 "+
		"encoder of the Mac that hosts this sandbox, at /workspace/shared/clip.mp4, then stop.")
	if out, _ := inSandbox(cfg.ID, "wc -c < /workspace/shared/clip.mp4; grep -c h264_videotoolbox /workspace/shared/clip.mp4"); !strings.HasSuffix(trimmed(out), "\n1") || strings.HasPrefix(trimmed(out), "0") {
		t.Errorf("the video the agent made: its size and whether the hardware's encoder signed it are %q", trimmed(out))
	}

	// The agent's work on this Mac was calls of the one tool, which the
	// bridge counted.
	calls := 0
	for _, call := range updates.mcpCallsOf(cfg.ID) {
		if call.Server == sandbox.HostToolServer && call.FromAgent && call.Method == "tools/call" {
			calls++
		}
	}
	if calls == 0 {
		t.Error("no call of the Host Tool server was reported")
	}
	t.Logf("the agent called the Host Tool server %d times", calls)
	// And its packets were what a sandbox's are: nothing of this went by
	// the network.
	capture.expectGated(t)
}
