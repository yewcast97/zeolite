package dashboard

import (
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// capture saves a picture of the window where ZEOLITE_SHOTS says, at twice
// the size, as a Retina screen shows it.
func (h *harness) capture(name string) {
	h.t.Helper()
	dir := os.Getenv("ZEOLITE_SHOTS")
	if dir == "" {
		return
	}
	h.tt.Frame()
	file, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, h.tt.Image()); err != nil {
		h.t.Fatal(err)
	}
}

// The window's pictures, for whoever changes how it looks: they are
// saved where ZEOLITE_SHOTS says, and looked at. Nothing is asserted of
// them but that each page builds.
func TestPictures(t *testing.T) {
	if os.Getenv("ZEOLITE_SHOTS") == "" {
		t.Skip("ZEOLITE_SHOTS names no folder to save the pictures in")
	}
	browser := browserPack(t)
	research, builder, scraper, broken := gated("s1", "Research"), gated("s2", "Builder"), gated("s3", "Scraper"), gated("s5", "Broken")
	research.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{research, builder, scraper, broken, gated("s6", "New")}, browser)
	h.tt.SetSize(1280, 820)
	h.tt.SetScale(2)
	frame := func() { h.settle("a frame", func() bool { return true }) }

	// A new sandbox: its Setup, with nothing checked.
	h.d.selected = "s6"
	h.tt.SetSize(1280, 1500)
	frame()
	h.capture("1-setup-new")
	h.tt.SetSize(1280, 820)

	// Checked: its gate is open, and its Setup says so.
	v6 := h.d.view("s6")
	h.click("Check")
	h.settle("the start", func() bool { return !v6.busy })
	h.d.Apply(sandbox.Update{SandboxID: "s6", Gate: &sandbox.GateTelemetry{Open: true, Interface: "en0", ExitAddress: "203.0.113.7"}})
	h.runs("s6")
	h.capture("2-setup-gate-open")

	// The others: running, locked, starting, in error.
	h.d.view("s1").running = research
	h.runs("s1")
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{Interface: "en0", ExitAddress: "203.0.113.7", ProbeLatency: 83 * time.Millisecond, PacketsPassed: 4211, PacketsRejected: 3}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Clock: "2026-11-18T09:30:00Z"}})
	h.d.Apply(sandbox.Update{SandboxID: "s2", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockExitAddressMismatch, ExitAddress: "198.51.100.9"}})
	h.d.Apply(sandbox.Update{SandboxID: "s3", Status: &sandbox.Status{State: sandbox.StateStarting}})
	h.d.Apply(sandbox.Update{SandboxID: "s5", Status: &sandbox.Status{State: sandbox.StateError, Error: "the image could not be built"}})
	v := h.d.view("s1")
	h.d.selected = "s1"
	frame()
	h.connOf("s1", 0).shell.Write([]byte("agent@sandbox:~$ "))
	frame()
	h.capture("3-one-pane")

	// The Card's facts.
	if card, ok := h.tt.Find("Research"); ok {
		h.tt.Move(card.X+card.W/2, card.Y+card.H/2)
		h.tt.Advance(700 * time.Millisecond)
		frame()
		h.capture("4-card-facts")
		h.tt.Move(700, 500)
	}

	// Two Panes, three, and four, each with its login shell; and the MCP
	// calls in their place.
	for n := 2; n <= 4; n++ {
		h.click(addPaneLabel)
		h.settle("another Pane's shell", func() bool { return len(v.panes) == n && v.panes[n-1].session != nil })
		h.connOf("s1", n-1).shell.Write([]byte("agent@sandbox:~$ "))
		frame()
		h.capture("5-panes-" + strconv.Itoa(n))
	}
	// Agents in three of the Panes: their glyphs in the Header. One is
	// Hidden, rings there, and another is dragged over a Pane.
	h.agentIn("s1", "claude", "codex", "", "opencode")
	h.capture("5a-agents")
	h.click("Hide the session of Pane 2")
	h.settle("the Pane's own shell", func() bool { return v.panes[1].session != nil && v.panes[1].session.agent == "" })
	for _, s := range v.sessions {
		if s.agent == "codex" {
			s.attention = true
		}
	}
	frame()
	h.capture("5a-hidden-asks")
	if from, ok := h.tt.Find("Claude Code, in Pane 1"); ok {
		if to, ok := h.tt.Find("Pane 3 of Research"); ok {
			h.tt.Press(from.X+from.W/2, from.Y+from.H/2)
			h.tt.Move(from.X+from.W/2+20, from.Y+from.H/2+20)
			h.tt.Frame()
			h.tt.Move(to.X+to.W/2, to.Y+to.H/2)
			frame()
			h.capture("5a-dragging")
			h.tt.Release(to.X+to.W/2, to.Y+to.H/2)
			frame()
		}
	}
	h.d.Apply(sandbox.Update{SandboxID: "s1", MCPCall: &sandbox.MCPCall{Time: time.Now(), Server: "browser", FromAgent: true, Method: "tools/call", Summary: `{"name":"navigate"}`}})
	frame()
	h.click(mcpCallsLabel)
	frame()
	h.capture("5b-mcp-calls")
	h.click(mcpCallsLabel)
	// A Pane whose shell ended.
	h.connOf("s1", 3).shell.Close()
	h.settle("the shell to end", func() bool { return v.panes[3].session == nil })
	h.capture("5c-shell-ended")

	// The Setup of a running sandbox.
	h.click("Setup")
	h.tt.SetSize(1280, 1500)
	frame()
	h.capture("6-setup-running")
	h.tt.SetSize(1280, 820)
	h.click("Setup")

	// A sandbox behind a locked gate, and one that starts.
	h.d.selected = "s2"
	frame()
	h.capture("7-stopped-by-a-locked-gate")
	h.d.selected = "s3"
	frame()
	h.capture("7b-starting")

	// Two sandboxes are given one Shared Folder, a third another. The
	// folder's page.
	h.backend.folders = []string{"research"}
	h.d.configs[0].SharedFolder, h.d.configs[1].SharedFolder, h.d.configs[4].SharedFolder = "research", "research", "docs"
	h.d.listSharedFolders()
	h.settle("the Shared Folders to be listed", func() bool { return len(h.d.folders) == 1 })
	h.d.selected = sharedFolderKey("research")
	frame()
	h.capture("8-shared-folder")

	h.tt.SetSize(900, 600)
	h.d.selected = "s1"
	frame()
	h.capture("9-smallest")
	h.d.selected = "s6"
	h.click("Setup")
	frame()
	h.capture("9b-smallest-setup")
}
