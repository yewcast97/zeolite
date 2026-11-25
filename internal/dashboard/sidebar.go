package dashboard

import (
	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// card is what a sandbox's Card shows: its name, its status, whether it
// needs its user, and its facts, for whoever rests the pointer on it.
type card struct {
	id, name string
	status   sandbox.Status
	needs    bool
	facts    []fact
}

// cards are the sandboxes' Cards: those that need their user first, and
// among those, and among the others, in the order the sandboxes were
// made.
func (d *Dashboard) cards() []card {
	var first, rest []card
	for _, cfg := range d.configs {
		one := card{id: cfg.ID, name: cfg.Name, status: sandbox.Status{State: sandbox.StateStopped}}
		if v := d.views[cfg.ID]; v != nil {
			one.status = v.status
			one.needs, _ = v.needsUser()
			one.facts = v.facts()
		} else {
			one.facts = []fact{{"State", one.status.String()}}
		}
		if one.needs {
			first = append(first, one)
		} else {
			rest = append(rest, one)
		}
	}
	return append(first, rest...)
}

// sidebarLeast is the least width the sidebar has under the window's
// controls, which end this far from the window's left edge: its edge
// stands as far right of the last control as the first stands from the
// window's edge. A window that shows no controls, as in full screen,
// asks for nothing.
func sidebarLeast(controls float32) float32 {
	return controls + max(controls-windowControlsSpan, 0)
}

// fitSidebar is the width the sidebar has, of the width its user dragged
// it to, in a window this wide: no wider than sidebarShare of the
// window, and no narrower than least, which keeps its edge clear of the
// window's controls. Where the two cannot both hold, the controls are
// kept clear.
func fitSidebar(width, window, least float32) float32 {
	return max(min(width, window*sidebarShare), least)
}

// The sidebar's two parts, as it names them.
const (
	sidebarSandboxes     = "Sandboxes"
	sidebarSharedFolders = "Shared Folders"
)

// sidebarView is the sidebar, where a row chooses what the page shows:
// the Cards of the sandboxes, those that need their user first, and
// then a row for each Shared Folder, which shows the folder's page.
func (d *Dashboard) sidebarView(c *ui.Context, folders []string) {
	// The window's controls sit over the sidebar, which starts below them
	// and drags the window where it is empty above its list.
	bar := c.TitleBar()
	ui.Themed(c, panelTheme).Fill().Font(chromeFont).Children(func() {
		t := c.Theme()
		ui.Box(c).Height(max(bar.Bottom, headerHeight-t.Space(4))).FillWidth().Shrink(0).DragWindow()
		cards := d.cards()
		ui.Sidebar(c, &d.selected, func() {
			if len(cards) > 0 {
				ui.SidebarSection(c, sidebarSandboxes, nil, func() {
					for _, one := range cards {
						d.cardView(c, one)
					}
				})
			}
			if len(folders) > 0 {
				ui.SidebarSection(c, sidebarSharedFolders, nil, func() {
					for _, folder := range folders {
						folderRow(c, sharedFolderKey(folder), folder).
							Tooltip("The Shared Folder " + folder + ": " + d.givenLine(folder))
					}
				})
			}
		}).Grow(1).FillWidth().Background(ui.Transparent)
		ui.Row(c).Padding(t.Space(2)).Children(func() {
			if glyphButton(c, "new-sandbox", glyphAdd, "New Sandbox").Clicked() {
				d.create()
			}
		})
	})
}

// cardView is a sandbox's Card: a row of the sidebar with the sandbox's
// name, then the Attention mark while it needs its user, and the glyph
// of its state. The rest it says to whoever rests the pointer on it, in
// the app's own words.
func (d *Dashboard) cardView(c *ui.Context, one card) {
	t := c.Theme()
	item := ui.SidebarItem(c, one.id, nil, one.name).Padding(t.Space(1), t.Space(2)).MinHeight(cardGlyph + t.Space(2))
	item.Children(func() {
		if one.needs {
			attentionMark(c)
		}
		stateGlyph(c, one.status)
	})
	factsTip(c, item, one.facts)
}

// folderRow is a Shared Folder's row of the sidebar, which leads with
// the glyph of a folder: the row leaves room for it before the folder's
// name, and paints it there. The toolkit's own place for a row's picture
// takes a vector drawing, and a glyph is a grid.
func folderRow(c *ui.Context, key, name string) ui.Element {
	t := c.Theme()
	lead := t.Space(2)
	return ui.SidebarItem(c, key, nil, name).
		Padding(t.Space(1), t.Space(2), t.Space(1), lead+cardGlyph+t.Space(2)).MinHeight(cardGlyph + t.Space(2)).
		Draw(func(p *ui.Painter, r ui.Rect) {
			glyphFolder.paint(t.TextMuted, glyphCell)(p, ui.Rect{X: r.X + lead, Y: r.Y, W: cardGlyph, H: r.H})
		})
}

// stateColor is the color a sandbox's state is shown in.
func stateColor(t *ui.Theme, state sandbox.State) ui.Color {
	switch state {
	case sandbox.StateGateOpen:
		return t.Success
	case sandbox.StateGateLocked, sandbox.StateError:
		return t.Danger
	case sandbox.StateStarting:
		return t.Warning
	}
	return t.TextMuted
}

// stateGlyph shows a sandbox's status as the glyph of its state, in the
// state's color. Whoever does not see it is told the status in words.
func stateGlyph(c *ui.Context, s sandbox.Status) ui.Element {
	if s.State == "" {
		s.State = sandbox.StateStopped
	}
	g, ok := stateGlyphs[s.State]
	if !ok {
		g = stateGlyphs[sandbox.StateStopped]
	}
	return g.view(c, stateColor(c.Theme(), s.State), glyphCell).Label(s.String())
}

// factsTip shows facts beside an element while the pointer rests on it:
// what each is, and its value, in two columns.
func factsTip(c *ui.Context, anchor ui.Element, facts []fact) {
	t := c.Theme()
	ui.TooltipBase(c, anchor, func(tip ui.Element) {
		tip.MaxWidth(tipWidth).Padding(t.Space(2), t.Space(2.5)).Radius(t.Radius).Font(chromeFont).
			Background(windowTheme.Surface).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).
			AttachTo(anchor, ui.AnchorTopRight, ui.AnchorTopLeft).Margin(0, 0, 0, t.Space(2))
		tip.Children(func() {
			ui.Column(c).Gap(t.Space(1)).Children(func() {
				for _, f := range facts {
					ui.Row(c).Gap(t.Space(3)).Children(func() {
						ui.Text(c, f.label).FontSize(textSmall).TextColor(windowTheme.TextMuted).Width(t.Space(24)).Shrink(0)
						ui.Text(c, f.value).FontSize(textSmall).TextColor(windowTheme.Text).Shrink(1)
					})
				}
			})
		})
	})
}

// attentionLabel is what the Attention mark is called for whoever does
// not see it.
const attentionLabel = "Needs you"

// attentionMark is the Attention mark: a raised hand in the palette's
// Attention color, beside what needs the user.
func attentionMark(c *ui.Context) ui.Element {
	return glyphHand.view(c, attention, glyphCell).Label(attentionLabel)
}
