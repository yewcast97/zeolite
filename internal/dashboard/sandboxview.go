package dashboard

import (
	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// sandboxView is what the dashboard shows and holds for one sandbox.
type sandboxView struct {
	d  *Dashboard
	id string

	// showSetup is whether the Setup shows in the page's place while the
	// sandbox starts or runs: after Check, and when the Header's Setup
	// asks for it. A sandbox that does not run shows nothing else.
	showSetup bool
	// showCalls is whether the MCP calls show in the Panes' place.
	showCalls bool
	// busy is true while a start, a stop or a destroy is under way.
	busy bool

	// What the sandbox last reported.
	status      sandbox.Status
	gate        sandbox.GateTelemetry
	workstation sandbox.WorkstationStatus
	mcpCalls    []sandbox.MCPCall
	// running is the config the sandbox was started with, which says
	// what its page shows until it is started again.
	running sandbox.Config
	// seenExits are the exit addresses the sandbox's gate saw at each
	// Interface since the window opened, the latest first: what the Setup
	// offers among the addresses expected there.
	seenExits map[string][]string

	// The Setup's form.
	draft *draft
	errs  []sandbox.FieldError
	saved bool

	// The Sandbox Terminal: the Panes, under the tabs of the packs'
	// viewports where the sandbox has any.
	// viewportsShare is the percentage of the page's height that the
	// viewports over the Panes take.
	viewportsShare float32
	viewportTab    int
	viewports      map[string]*viewportView
	// sessions are the sandbox's Terminal Sessions, in the order they
	// were opened. root is the page's one part while the sandbox runs: a
	// Pane, or what a Pane was divided into. panes are its Panes in the
	// order they stand, listed again whenever the parts change: one while
	// the sandbox runs, and up to sixteen. paneSeq numbers the Panes as
	// they are made, and partSeq the parts that are divided.
	sessions []*session
	root     *part
	panes    []*pane
	paneSeq  int
	partSeq  int
	// keyboard is the Pane the keyboard was in when the Panes were last
	// built, nil where it was in none.
	keyboard *pane
	// lived counts the times the sandbox's sessions were closed with
	// it: a shell that was being started meanwhile is not shown.
	lived int
	// ended is whether a Terminal Session of the sandbox ended by itself
	// since the user last looked at the sandbox.
	ended bool
}

func newSandboxView(d *Dashboard, cfg sandbox.Config) *sandboxView {
	return &sandboxView{
		d:              d,
		id:             cfg.ID,
		status:         sandbox.Status{State: sandbox.StateStopped},
		running:        cfg,
		seenExits:      map[string][]string{},
		draft:          newDraft(cfg, d.backend.Packs()),
		viewportsShare: 55,
		viewports:      map[string]*viewportView{},
	}
}

// live is whether the sandbox has containers, or is about to: it runs,
// or it is starting.
func (v *sandboxView) live() bool {
	return running(v.status.State) || v.status.State == sandbox.StateStarting
}

// panesShown is whether the page shows the sandbox's Panes: it runs,
// and neither its Setup nor its MCP calls stand in their place.
func (v *sandboxView) panesShown() bool {
	return running(v.status.State) && !v.showSetup && !v.showCalls
}

// needsUser is whether the sandbox needs its user, and why, in the
// app's own words, the weightiest reason first: its gate is locked; a
// Terminal Session rang or sent a notification and has not been looked
// at since; an agent ended in a session that was Hidden; a Terminal
// Session ended by itself while the sandbox was not shown.
func (v *sandboxView) needsUser() (bool, string) {
	switch {
	case v.status.State == sandbox.StateGateLocked:
		if v.status.LockReason == "" {
			return true, "Gate locked"
		}
		return true, "Gate locked: " + string(v.status.LockReason)
	}
	switch rang, agentEnded := v.asks(); {
	case rang:
		return true, "A session asks for you"
	case agentEnded:
		return true, "An agent ended"
	case v.ended:
		return true, "A session ended"
	}
	return false, ""
}

// see takes note of what the user looks at while the sandbox's page is
// the one shown: that a session ended is news no longer, and neither is
// what the sessions in sight asked for. A session is in sight while the
// Panes are: not behind the Setup, and not behind the MCP calls.
func (v *sandboxView) see() {
	v.ended = false
	if !v.panesShown() {
		return
	}
	for _, p := range v.panes {
		s := p.session
		if s == nil {
			continue
		}
		s.attention = false
		// An agent that ended out of sight has been looked at now: its
		// session is a plain shell's from here on.
		if s.agentEnded {
			s.agent, s.agentEnded = "", false
		}
	}
}

// closeLive ends what the sandbox's workstation was showing, as when it
// stops: its Terminal Sessions, with their Panes, and its Viewport
// Streams.
func (v *sandboxView) closeLive() {
	v.closeSessions()
	v.showCalls = false
	v.ended = false
	for _, vp := range v.viewports {
		vp.close()
	}
	v.viewports = map[string]*viewportView{}
	v.viewportTab = 0
}

// mcpCallsLabel is what the Header's control that lists the MCP calls is
// called.
const mcpCallsLabel = "MCP Calls"

func (v *sandboxView) view(c *ui.Context) {
	cfg := v.d.config(v.id)
	if cfg == nil {
		return
	}
	t := c.Theme()
	ui.Column(c).Fill().Children(func() {
		// The Header is the window's title bar here: it drags the window.
		// It holds the sandbox's name, the glyphs of its Agent Sessions and
		// what can be done with it, each a glyph: its state is its Card's
		// to show.
		ui.Row(c).Height(headerHeight).Shrink(0).Gap(t.Space(1)).Padding(0, t.Space(3), 0, t.Space(4)).AlignItems(ui.Center).
			Font(chromeFont).Background(panelTheme.Background).DragWindow().Children(func() {
			ui.Text(c, cfg.Name).FontSize(textTitle).Bold().MaxLines(1).Shrink(1)
			// The Agent Sessions' glyphs, in the order the sessions were
			// opened: the row scrolls where they do not fit.
			if running(v.status.State) {
				v.glyphsView(c)
			} else {
				ui.Spacer(c)
			}
			live := v.live()
			// The MCP calls, in the Panes' place, once there is one to
			// list.
			if running(v.status.State) && !v.showSetup && len(v.mcpCalls) > 0 {
				if toggle(c, "mcp-calls", glyphCalls, mcpCallsLabel, v.showCalls).Clicked() {
					v.showCalls = !v.showCalls
				}
			}
			if !live {
				if glyphButtonIn(c, "start", glyphStart, "Start", "Start", t.Accent).Disabled(v.busy).Clicked() {
					v.d.start(v.id)
				}
			} else {
				if glyphButton(c, "stop", glyphStop, "Stop").Disabled(v.busy).Clicked() {
					v.d.stop(v.id)
				}
				// Setup shows the Setup in the page's place while the
				// sandbox starts or runs, and the page again.
				if toggle(c, "setup", glyphConfigure, "Setup", v.showSetup).Clicked() {
					v.showSetup = !v.showSetup
				}
			}
			if glyphButton(c, "clone", glyphClone, "Clone").Clicked() {
				v.d.clone(v.id)
			}
			// A start under way would race the deletion of the Home.
			if glyphButton(c, "destroy", glyphDestroy, "Destroy").Disabled(v.busy).Clicked() {
				v.d.destroying = true
			}
		})
		if v.status.State == sandbox.StateError && v.status.Error != "" {
			banner(c, v.status.Error)
		}
		v.d.runtimeBanner(c)

		if v.live() && !v.showSetup {
			v.terminalView(c)
		} else {
			v.setupView(c)
		}
	})
}

// toggle is a control of the Header that shows something in the page's
// place, and the page again: its glyph is in the accent color, on a
// ground of its own, while what it shows is shown.
func toggle(c *ui.Context, key string, g glyph, label string, on bool) ui.Element {
	t := c.Theme()
	color := t.Text
	if on {
		color = t.Accent
	}
	b := glyphButtonIn(c, key, g, label, label, color)
	if on {
		b.Background(t.SurfacePressed)
	}
	return b
}
