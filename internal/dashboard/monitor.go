package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
)

// terminalView is the Sandbox Terminal, the page of a running sandbox
// under its Header: its Panes, or its MCP calls in their place, under
// the tabs of its packs' viewports where it has any. While the sandbox
// starts it shows the glyph of its state, and says in a line what it
// waits for.
func (v *sandboxView) terminalView(c *ui.Context) {
	t := c.Theme()
	if !running(v.status.State) {
		// The page is a live sandbox's: one that does not run yet starts.
		ui.Column(c).Grow(1).Center().Gap(t.Space(4)).Font(chromeFont).Children(func() {
			stateGlyphs[sandbox.StateStarting].view(c, stateColor(t, sandbox.StateStarting), stateCell)
			ui.Row(c).Gap(t.Space(1)).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "Starting…").TextColor(t.TextMuted)
				info(c, "starting", "The first start of a set of Capability Packs builds their image, which takes minutes.")
			})
		})
		return
	}
	body := func() {
		if v.showCalls {
			v.mcpCallsView(c)
		} else {
			v.panesView(c)
		}
	}
	streams := v.streamPacks()
	page := ui.Column(c).Grow(1).FillWidth().BorderWidth(1, 0, 0, 0).BorderColor(t.Border)
	if len(streams) == 0 {
		page.Children(body)
		return
	}
	page.Children(func() {
		ui.SplitVerticalPercent(c, &v.viewportsShare, func() { v.viewportsView(c, streams) }, body).Fill()
	})
}

// mcpCallsView lists the MCP calls that crossed the sandbox's bridges.
func (v *sandboxView) mcpCallsView(c *ui.Context) {
	t := c.Theme()
	ui.List(c, nil, len(v.mcpCalls), func(i int) {
		call := v.mcpCalls[i]
		direction := "→ agent"
		if call.FromAgent {
			direction = "agent →"
		}
		what := call.Method
		if what == "" {
			what = "reply"
		}
		ui.Row(c).Gap(t.Space(2.5)).Padding(t.Space(1), t.Space(2)).Font(chromeFont).Children(func() {
			ui.Text(c, call.Time.Local().Format("15:04:05")).FontSize(textSmall).TextColor(t.TextMuted)
			ui.Text(c, call.Server).FontSize(textSmall).Bold()
			ui.Text(c, direction).FontSize(textSmall).TextColor(t.TextMuted)
			ui.Text(c, what+" "+call.Summary).FontSize(textSmall)
		})
	}).Grow(1).FillWidth().Label(mcpCallsLabel)
}

// fact is one line of what a Card says of its sandbox: what it is, and
// its value.
type fact struct{ label, value string }

// facts are what a Card says of its sandbox to whoever rests the pointer
// on it: its state, the agents that run in it, why it needs its user,
// and, while it runs, its gate and its workstation. Every word of it is
// the app's own.
func (v *sandboxView) facts() []fact {
	facts := []fact{{"State", v.status.String()}}
	if agents := v.agentSessions(); len(agents) > 0 {
		titles := make([]string, len(agents))
		for i, s := range agents {
			titles[i] = agentTitle(s.agent)
		}
		facts = append(facts, fact{"Agents", strings.Join(titles, ", ")})
	}
	if needs, why := v.needsUser(); needs {
		facts = append(facts, fact{"Needs you", why})
	}
	if v.status.State == sandbox.StateError && v.status.Error != "" {
		facts = append(facts, fact{"Error", v.status.Error})
	}
	if !running(v.status.State) {
		return facts
	}
	exit := v.gate.ExitAddress
	if exit == "" {
		exit = "no answer"
	}
	// The Interface in use, as the gatekeeper has taken note of it,
	// and what is expected there.
	inUse, expected := v.gate.Interface, "nothing"
	if inUse == "" {
		inUse = "nothing: none of its ways out is up"
	}
	for _, ifc := range v.running.AllowedInterfaces {
		if ifc.Name == v.gate.Interface {
			expected = expectedAt(ifc, v.gate)
		}
	}
	if v.gate.Held {
		facts = append(facts, fact{"Hold", heldSays})
	}
	facts = append(facts,
		fact{"Leaves by", inUse},
		fact{"Exit address", exit},
		fact{"Expected", expected},
		fact{"Probe latency", latency(v.gate.ProbeLatency)},
		fact{"Packets", fmt.Sprintf("%d passed, %d rejected", v.gate.PacketsPassed, v.gate.PacketsRejected)},
	)
	clock := v.workstation.Clock
	if clock == "" {
		clock = "…"
	}
	facts = append(facts, fact{"Sandbox clock", clock})
	for _, d := range v.workstation.Daemons {
		state := "ready"
		if !d.Ready {
			state = d.Error
		}
		facts = append(facts, fact{d.Pack, state})
	}
	return facts
}

// expectedAt says what is expected of the exit address at the Interface
// in use: the addresses listed, any, any but physical, or physical,
// with those as the gatekeeper has last taken note of them.
func expectedAt(ifc sandbox.AllowedInterface, gate sandbox.GateTelemetry) string {
	switch {
	case ifc.PhysicalExitAddress && !gate.PhysicalExitKnown:
		return "physical (not known)"
	case ifc.PhysicalExitAddress && len(gate.PhysicalExitAddresses) == 0:
		return "physical (there is none)"
	case ifc.PhysicalExitAddress:
		return "physical (" + strings.Join(gate.PhysicalExitAddresses, ", ") + ")"
	case ifc.AnyExitAddressButPhysical && !gate.PhysicalExitKnown:
		return "any but physical (not known)"
	case ifc.AnyExitAddressButPhysical && len(gate.PhysicalExitAddresses) == 0:
		return "any but physical (this Mac has no other way out)"
	case ifc.AnyExitAddressButPhysical:
		return "any but physical (" + strings.Join(gate.PhysicalExitAddresses, ", ") + ")"
	case ifc.AnyExitAddress:
		return "any exit address"
	}
	return strings.Join(ifc.ExpectedExitAddresses, ", ")
}

func latency(d time.Duration) string {
	if d <= 0 {
		return "…"
	}
	return fmt.Sprintf("%.0f ms", float64(d)/float64(time.Millisecond))
}

// streamPacks are the packs the sandbox was started with that have a
// Viewport Stream to show.
func (v *sandboxView) streamPacks() []pack.Manifest {
	var out []pack.Manifest
	for _, name := range v.running.Packs {
		for _, m := range v.d.backend.Packs() {
			if m.Name == name && m.ViewportStream.Type != pack.StreamNone {
				out = append(out, m)
			}
		}
	}
	return out
}

// viewportsView is the strip of tabs over what they show: the Viewport
// Stream of each pack that has one.
func (v *sandboxView) viewportsView(c *ui.Context, streams []pack.Manifest) {
	if v.viewportTab >= len(streams) {
		v.viewportTab = len(streams) - 1
	}
	titles := make([]string, len(streams))
	for i, m := range streams {
		titles[i] = m.Title
	}
	ui.Column(c).Fill().Children(func() {
		tabStrip(c, "Viewports", &v.viewportTab, titles)
		v.viewportOf(streams[v.viewportTab]).view(c)
	})
}

func (v *sandboxView) note(c *ui.Context, text string) {
	ui.Column(c).Grow(1).Center().Children(func() {
		ui.Text(c, text).TextColor(c.Theme().TextMuted)
	})
}
