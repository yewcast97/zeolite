//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
)

// automation is a connection to the automation plugin of a running app:
// one JSON object a line each way.
type automation struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

type node struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	Key    string `json:"key"`
	Label  string `json:"label"`
	Bounds struct{ X, Y, Width, Height float64 }
}

// ask sends a request and returns its result, or the app's error.
func (a *automation) ask(method string, params any) (json.RawMessage, string) {
	a.t.Helper()
	line, _ := json.Marshal(map[string]any{"id": 1, "method": method, "params": params})
	if _, err := a.conn.Write(append(line, '\n')); err != nil {
		a.t.Fatalf("%s: %v", method, err)
	}
	a.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	answer, err := a.r.ReadBytes('\n')
	if err != nil {
		a.t.Fatalf("%s: %v", method, err)
	}
	var res struct {
		Result json.RawMessage
		Error  *struct{ Code, Message string }
	}
	json.Unmarshal(answer, &res)
	if res.Error != nil {
		return nil, res.Error.Message
	}
	return res.Result, ""
}

func (a *automation) call(method string, params any) json.RawMessage {
	a.t.Helper()
	result, problem := a.ask(method, params)
	if problem != "" {
		a.t.Fatalf("%s %v: %s", method, params, problem)
	}
	return result
}

// nodes lists the elements of the window, none while it has no window.
func (a *automation) nodes() []node {
	a.t.Helper()
	var tree struct{ Nodes []node }
	if result, problem := a.ask("tree", map[string]any{}); problem == "" {
		json.Unmarshal(result, &tree)
	}
	return tree.Nodes
}

// waitNode waits for an element the window shows.
func (a *automation) waitNode(what string, timeout time.Duration, match func(node) bool) node {
	a.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		nodes := a.nodes()
		for _, n := range nodes {
			if match(n) {
				return n
			}
		}
		if time.Now().After(deadline) {
			var labels []string
			for _, n := range nodes {
				if n.Label != "" {
					labels = append(labels, n.Label)
				}
			}
			a.t.Fatalf("timed out waiting for %s; the window shows %q", what, labels)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func labelled(sub string) func(node) bool {
	return func(n node) bool { return strings.Contains(n.Label, sub) }
}

func (a *automation) act(key, action, text string) {
	a.t.Helper()
	params := map[string]any{"key": key, "action": action}
	if text != "" {
		params["text"] = text
	}
	a.call("act", params)
	a.call("idle", map[string]any{"timeout": 5000})
}

// click clicks an element where it is, as a pointer does.
func (a *automation) click(n node) {
	a.t.Helper()
	a.call("input", map[string]any{"events": []map[string]any{{"type": "click", "x": n.Bounds.X + n.Bounds.Width/2, "y": n.Bounds.Y + n.Bounds.Height/2}}})
	a.call("idle", map[string]any{"timeout": 5000})
}

// windowFocused reports whether the app's window is the one this Mac's
// keyboard goes to.
func (a *automation) windowFocused() bool {
	a.t.Helper()
	var listing struct{ Windows []struct{ Focused bool } }
	json.Unmarshal(a.call("windows", map[string]any{}), &listing)
	return len(listing.Windows) > 0 && listing.Windows[0].Focused
}

// typeText types text into what has the keyboard, and key presses a key.
func (a *automation) typeText(text string) {
	a.t.Helper()
	a.call("input", map[string]any{"events": []map[string]any{{"type": "text", "text": text}}})
}

func (a *automation) key(key string) {
	a.t.Helper()
	a.call("input", map[string]any{"events": []map[string]any{{"type": "key", "key": key}}})
}

// press presses an element that has no key, as assistive technology does.
func (a *automation) press(n node) {
	a.t.Helper()
	a.call("act", map[string]any{"id": n.ID, "action": "press"})
	a.call("idle", map[string]any{"timeout": 5000})
}

// start presses a control that starts the sandbox: Check, or Start.
// While the app is still preparing the runtime, it says so: again,
// until it has.
func (a *automation) start(key string) {
	a.t.Helper()
	a.act(key, "press", "")
	waitFor(a.t, 5*time.Minute, "the app to have prepared the runtime", func() bool {
		if !hasNode(a, labelled("the runtime is not available")) {
			return true
		}
		time.Sleep(2 * time.Second)
		a.act(key, "press", "")
		return false
	})
}

// capture saves a picture of the window where ZEOLITE_SHOTS says.
func (a *automation) capture(name string) {
	dir := os.Getenv("ZEOLITE_SHOTS")
	if dir == "" {
		return
	}
	var shot struct{ PNG string }
	json.Unmarshal(a.call("capture", map[string]any{}), &shot)
	data, _ := base64.StdEncoding.DecodeString(shot.PNG)
	os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

// TestBuiltApp drives the app itself, built as it ships but for the
// automation plugin: a sandbox is set up, its gate is seen to judge the
// exit address, its Panes hold login shells that are typed into, an
// agent is installed and started there by hand, and the sandbox is
// stopped, started on the Home it had, and destroyed.
func TestBuiltApp(t *testing.T) {
	exit := macExitAddress(t)
	physical, err := physicalInterface()
	if err != nil {
		t.Skipf("this Mac's physical way out could not be found: %v", err)
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "zeolite")
	build := exec.Command("go", "build", "-tags", "e2e", "-o", binary, "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the app: %v\n%s", err, out)
	}

	// A Unix socket's path is short: the system's temporary directory,
	// not the test's.
	socket := filepath.Join(os.TempDir(), "zeolite-test.sock")
	os.Remove(socket)
	app := exec.Command(binary)
	app.Env = append(os.Environ(), "ZEOLITE_DATA_DIR="+filepath.Join(dir, "data"), "MYGO_AUTOMATION="+socket)
	var log bytes.Buffer
	app.Stdout, app.Stderr = &log, &log
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	var sandboxID string
	t.Cleanup(func() {
		app.Process.Kill()
		app.Wait()
		// The app was killed, not quit: what it started is still there,
		// and so is the Home of the sandbox it made, unless the test
		// came to destroy it. The app gave the sandbox its ID: it is
		// the test's own all the same, made in a data directory of the
		// test's, and its Home is deleted by that ID. Its /tmp goes with
		// its workstation, as it does when the app removes one.
		if sandboxID != "" {
			docker("rm", "-f", "-v", orchestrator.WorkstationContainer(sandboxID), orchestrator.GatekeeperContainer(sandboxID))
			if err := removeHome(sandboxID); err != nil {
				t.Errorf("deleting the Home of the sandbox the test's app made: %v", err)
			}
		}
		if t.Failed() {
			t.Logf("the app's log:\n%s", log.String())
		}
	})

	var conn net.Conn
	waitFor(t, 20*time.Second, "the app's automation socket", func() bool {
		var err error
		conn, err = net.Dial("unix", socket)
		return err == nil
	})
	defer conn.Close()
	a := &automation{t: t, conn: conn, r: bufio.NewReaderSize(conn, 8<<20)}

	a.waitNode("the empty dashboard", 10*time.Second, labelled("No sandboxes yet"))
	a.capture("app-1-empty.png")
	a.act("new-sandbox", "press", "")
	item := a.waitNode("the new sandbox in the sidebar", 5*time.Second, func(n node) bool {
		return n.Role == "treeItem" && strings.HasPrefix(n.Label, "Sandbox 1")
	})
	sandboxID = item.Key

	// The network, as a user fills it: with an exit address that is not
	// the one this Mac's network leaves from.
	a.act("name", "setValue", "Research")
	// What the sandbox leaves by is chosen among this Mac's ways out, each
	// under what the system calls it: nobody types an interface's name.
	a.act("interface-name-1", "press", "")
	a.click(a.waitNode("this Mac's physical way out among the ones offered", 5*time.Second, func(n node) bool {
		return n.Role != "" && strings.Contains(" "+n.Label+" ", " "+physical+" ") && strings.Contains(n.Label, " · ")
	}))
	a.act("interface-exits-1", "setValue", elsewhere)
	a.act("probe", "setValue", publicProbeURL)
	a.capture("app-2-setup.png")

	// Check saves it and starts the sandbox, its Setup still shown. The
	// runtime is checked in the background after launch; a start needs
	// it.
	a.start("check")
	a.waitNode("the gate to be locked on a mismatch", 3*time.Minute, labelled("GATE LOCKED [Exit Address Mismatch]"))
	a.waitNode("the Setup to say so", 10*time.Second, labelled("Gate locked: Exit Address Mismatch"))
	a.capture("app-3-locked.png")
	if out, code := inSandbox(sandboxID, fetch(publicProbeURL)); code == 0 {
		t.Errorf("a request succeeded through the locked gate: %q", out)
	}

	// The expected exit address is put right: Check starts the sandbox
	// again as its Setup now has it, and its gate opens.
	a.act("interface-exits-1", "setValue", exit)
	key, _ := experimentKey()
	a.act("check", "press", "")
	a.waitNode("the gate to open", 2*time.Minute, labelled("GATE OPEN ["+exit+"]"))
	if out, code := inSandbox(sandboxID, fetch(publicProbeURL)); code != 0 || trimmed(out) != exit {
		t.Errorf("a request through the open gate: %q (exit %d)", out, code)
	}

	// The app put no agent in the workstation, and offers none.
	if out, _ := inLoginShell(sandboxID, "type -P opencode claude codex"); trimmed(out) != "" {
		t.Fatalf("the workstation holds %q, and nobody installed anything in it", trimmed(out))
	}

	// The Header's Setup gives the Panes: nothing that is started
	// changed, so the sandbox is not started again, and its first Pane
	// holds the login shell its start opened.
	const firstPane = "Pane 1 of Research"
	created, _ := docker("inspect", "-f", "{{.Created}}", orchestrator.WorkstationContainer(sandboxID))
	a.act("setup", "press", "")
	a.waitNode("the first Pane", time.Minute, labelled(firstPane))
	a.waitNode("its terminal", 10*time.Second, func(n node) bool { return n.Label == "Terminal" })
	if again, _ := docker("inspect", "-f", "{{.Created}}", orchestrator.WorkstationContainer(sandboxID)); trimmed(again) != trimmed(created) || trimmed(created) == "" {
		t.Errorf("showing the Panes after Check made another workstation: created %q, then %q", trimmed(created), trimmed(again))
	}
	time.Sleep(time.Second) // the shell prints its prompt
	a.call("idle", map[string]any{"timeout": 3000})
	a.capture("app-4-pane.png")

	// From here on the test types, and keys go to the window in front.
	// While this Mac's user types in another app, macOS keeps the app
	// behind it, and then there is nowhere to type.
	active := false
	for deadline := time.Now().Add(10 * time.Second); !active && time.Now().Before(deadline); {
		if active = a.windowFocused(); !active {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if active {
		// The Pane's shell, typed into as a user would: its terminal is
		// clicked where it is, as a pointer does.
		a.click(a.waitNode("the first Pane", 10*time.Second, labelled(firstPane)))
		a.typeText("mkdir -p project && cd project && echo typed-in-the-app > from-the-terminal")
		a.key("Enter")
		waitFor(t, 10*time.Second, "the command typed in the terminal to run in the sandbox", func() bool {
			out, _ := inSandbox(sandboxID, "cat /workspace/project/from-the-terminal 2>/dev/null")
			return trimmed(out) == "typed-in-the-app"
		})
		a.call("idle", map[string]any{"timeout": 3000})
		a.capture("app-5-typed.png")
	} else if out, code := inSandbox(sandboxID, "mkdir -p /workspace/project && echo typed-in-the-app > /workspace/project/from-the-terminal"); code != 0 {
		t.Fatalf("writing into the Home: %s", out)
	}

	// Add Pane, in the Header, opens another login shell beside the first.
	a.act("add-pane", "press", "")
	a.waitNode("the second Pane", 30*time.Second, labelled("Pane 2 of Research"))
	a.call("idle", map[string]any{"timeout": 3000})
	a.capture("app-6-two-panes.png")

	worked := agentAtWorkInTheBuiltApp(t, a, sandboxID, key, active)

	// Stopped, its page is its Setup again, and its workstation is gone;
	// its Home is not.
	a.act("stop", "press", "")
	a.waitNode("the sandbox to stop", 30*time.Second, labelled("STOPPED"))
	a.waitNode("its Setup", 10*time.Second, func(n node) bool { return n.Key == "check" })
	if _, code := docker("inspect", orchestrator.WorkstationContainer(sandboxID)); code == 0 {
		t.Error("Stop left the workstation")
	}
	if _, code := docker("volume", "inspect", orchestrator.HomeVolume(sandboxID)); code != 0 {
		t.Error("Stop deleted the sandbox's Home")
	}

	// Started again, with one click, on the Home it had: its page is one
	// Pane, and what was written and installed there is there.
	a.start("start")
	a.waitNode("the gate to open", 2*time.Minute, labelled("GATE OPEN ["+exit+"]"))
	a.waitNode("the first Pane", time.Minute, labelled(firstPane))
	if hasNode(a, labelled("Pane 2 of Research")) {
		t.Error("started again, the sandbox has the Panes of its last start")
	}
	if out, _ := inSandbox(sandboxID, "cat /workspace/project/from-the-terminal 2>/dev/null"); trimmed(out) != "typed-in-the-app" {
		t.Errorf("after a stop and a start the Home holds %q where the file was written", trimmed(out))
	}
	if worked {
		if out, _ := inLoginShell(sandboxID, "type -P opencode"); trimmed(out) == "" {
			t.Error("after a stop and a start the agent installed by hand is not in the Home")
		}
	}

	sharedFolderInTheBuiltApp(t, a, filepath.Join(dir, "data"), sandboxID)

	// Destroyed, through the window: the sandbox is gone from the
	// sidebar, and its containers and its Home from the VM.
	a.click(a.waitNode("the sandbox's Card", 10*time.Second, func(n node) bool { return n.Role == "treeItem" && n.Key == sandboxID }))
	a.act("destroy", "press", "")
	a.waitNode("the question", 10*time.Second, labelled("Destroy “Research”?"))
	a.press(a.waitNode("the alert's Destroy", 5*time.Second, func(n node) bool { return n.Label == "Destroy" && n.Key != "destroy" }))
	a.waitNode("the empty dashboard", time.Minute, labelled("No sandboxes yet"))
	for _, name := range []string{orchestrator.WorkstationContainer(sandboxID), orchestrator.GatekeeperContainer(sandboxID)} {
		if _, code := docker("inspect", name); code == 0 {
			t.Errorf("Destroy left %s", name)
		}
	}
	if _, code := docker("volume", "inspect", orchestrator.HomeVolume(sandboxID)); code == 0 {
		t.Error("Destroy left the sandbox's Home")
	}
	if !active {
		t.Skip("the app's window was not the active one, as while this Mac's user works in another app: typing in the Sandbox Terminal was not tested")
	}
}

// agentAtWorkInTheBuiltApp installs an agent in the sandbox by hand,
// starts it by typing its name in a Pane, and gives it work by a real
// model, where there is an API key and the keyboard: opencode writes a
// file and raises its hand, and the sandbox's Card wears the Attention
// mark. It says whether it did.
func agentAtWorkInTheBuiltApp(t *testing.T, a *automation, sandboxID, key string, active bool) bool {
	t.Helper()
	if key == "" || !active || !a.windowFocused() {
		t.Log("no agent was installed, started or given work in the built app: that needs an API key and the keyboard")
		return false
	}
	// As its user would: the agent installed in a login shell, its key
	// in the ~/.bashrc of the sandbox's Home, which the shell that
	// starts the agent reads, and its model in the agent's own config.
	// The app takes no key. The key crosses on a command's standard
	// input, and is on no command line and in no picture.
	installAgent(t, sandboxID)
	if out, code := dockerWith(nil, "export "+keyVariable+"="+shellQuote(key)+"\n", "exec", "-i", orchestrator.WorkstationContainer(sandboxID), "sh", "-c", "umask 077 && cat >> /workspace/.bashrc"); code != 0 {
		t.Fatalf("giving the agent its key: exit %d (%d bytes said)", code, len(out))
	}
	config := `{"$schema": "https://opencode.ai/config.json", "autoupdate": false, "permission": "allow", "model": "` + agentModel + `", "provider": {"openrouter": {"models": {"` + agentModelID + `": {}}}}}`
	if out, code := inSandbox(sandboxID, "mkdir -p /workspace/.config/opencode && cat > /workspace/.config/opencode/opencode.json <<'JSON'\n"+config+"\nJSON"); code != 0 {
		t.Fatalf("writing opencode's config: %s", out)
	}
	// A Pane opened now has a shell that read the key: the agent is
	// started there, by its name.
	a.act("add-pane", "press", "")
	pane := a.waitNode("the third Pane", 30*time.Second, labelled("Pane 3 of Research"))
	time.Sleep(time.Second) // the shell prints its prompt
	a.click(pane)
	a.typeText("opencode")
	a.key("Enter")
	time.Sleep(8 * time.Second) // the agent's screen comes up
	a.call("idle", map[string]any{"timeout": 3000})
	a.capture("app-7-agent.png")
	a.typeText("Create the file /workspace/from-opencode.txt containing exactly the word READY. Then wait 20 seconds, with the shell command sleep 20, and then call the tool raise_hand of the MCP server sandbox.")
	a.key("Enter")
	waitFor(t, 3*time.Minute, "opencode, driven by a real model, to do its work", func() bool {
		out, _ := inSandbox(sandboxID, "cat /workspace/from-opencode.txt 2>/dev/null")
		return strings.Contains(out, "READY")
	})
	a.call("idle", map[string]any{"timeout": 3000})
	a.capture("app-8-agent-at-work.png")
	// Its session is put out of sight, behind the Setup, and then its
	// hand is seen.
	a.act("setup", "press", "")
	a.waitNode("the Attention mark on the Card, once the agent raised its hand", 2*time.Minute, func(n node) bool {
		return n.Role == "treeItem" && strings.Contains(n.Label, "Needs you")
	})
	a.act("setup", "press", "")
	// Back on its session, which is looked at again: the mark is gone,
	// and the picture shows the agent's own screen.
	a.call("idle", map[string]any{"timeout": 3000})
	a.capture("app-9-after-the-hand.png")
	return true
}

// sharedFolderInTheBuiltApp gives the sandbox a Shared Folder, in its
// Setup: the folder has a row in the sidebar, under the sandboxes'
// Cards, and the row shows the folder's page. The app's data directory
// is the test's, and so is the folder's root: the VM does not mount it,
// and the sandbox, which runs, is not started with the folder, which is
// therefore not on this Mac's disk.
func sharedFolderInTheBuiltApp(t *testing.T, a *automation, dataDir, sandboxID string) {
	t.Helper()
	const folder = "test-in-the-app"
	row := func(key string) func(node) bool {
		return func(n node) bool { return n.Role == "treeItem" && n.Key == key }
	}
	a.act("setup", "press", "")
	a.act("shared-folder", "setValue", folder)
	a.act("save", "press", "")
	a.waitNode("the Setup to say it saved", 5*time.Second, labelled("Saved."))
	a.act("setup", "press", "")

	// The folder's row stands under the sandbox's Card, and is not set
	// in from it.
	head := a.waitNode("the Shared Folder's row", 5*time.Second, row("shared-folder/"+folder))
	card := a.waitNode("the sandbox's Card", 5*time.Second, row(sandboxID))
	if head.Bounds.Y <= card.Bounds.Y || card.Bounds.X != head.Bounds.X {
		t.Errorf("the sandbox's Card is at %+v and its folder's row at %+v, want the row under the Cards", card.Bounds, head.Bounds)
	}
	// Its page names the folder, which nothing has made yet.
	a.click(head)
	a.waitNode("the Shared Folder's page", 5*time.Second, func(n node) bool { return n.Key == "delete-shared-folder" })
	a.call("idle", map[string]any{"timeout": 3000})
	a.capture("app-10-shared-folder.png")
	if _, err := os.Lstat(filepath.Join(dataDir, "shared.noindex", folder)); !os.IsNotExist(err) {
		t.Errorf("a Shared Folder no sandbox was started with is on this Mac's disk: %v", err)
	}
}

func hasNode(a *automation, match func(node) bool) bool {
	for _, n := range a.nodes() {
		if match(n) {
			return true
		}
	}
	return false
}
