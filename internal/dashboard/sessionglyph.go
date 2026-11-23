package dashboard

import (
	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// agentTitle is what the app calls the agent of an Agent Executable.
func agentTitle(executable string) string {
	if a, known := sandbox.KnownAgent(executable); known {
		return a.Title
	}
	return executable
}

// glyphsView is the row of the Agent Sessions' glyphs in the Header,
// which takes the room between the sandbox's name and its controls and
// scrolls where the glyphs do not fit.
func (v *sandboxView) glyphsView(c *ui.Context) {
	t := c.Theme()
	var chosen *session
	ui.ScrollHorizontal(c).Grow(1).Shrink(1).MinWidth(0).Height(headerHeight).Label("Agent Sessions").Children(func() {
		ui.Row(c).Height(headerHeight).Gap(t.Space(1)).Padding(0, t.Space(3)).AlignItems(ui.Center).Children(func() {
			for _, s := range v.agentSessions() {
				if v.sessionGlyphView(c, s).Clicked() {
					chosen = s
				}
			}
		})
	})
	if chosen != nil {
		v.choose(chosen)
	}
}

// sessionGlyphSays is what an Agent Session's glyph says of its session,
// in the app's own words: which agent it is, where it shows, and why it
// asks for its user.
func (v *sandboxView) sessionGlyphSays(s *session) (label, tip string) {
	title := agentTitle(s.agent)
	where := "hidden"
	tip = title + " · hidden: drag it onto a Pane to show it there"
	if len(v.panes) < maxPanes {
		tip = title + " · hidden: click to show it in a new Pane, or drag it onto a Pane"
	}
	for i, p := range v.panes {
		if p.session == s {
			where = "in " + paneName(i)
			tip = title + " · " + where + ": drag it onto another Pane to move it there"
		}
	}
	label = title + ", " + where
	switch {
	case s.agentEnded:
		label += ", its agent ended"
		tip += " · its agent ended"
	case s.attention:
		label += ", asks for you"
		tip += " · asks for you"
	}
	return label, tip
}

// sessionGlyphView is an Agent Session's glyph: its agent's, muted while
// the session is Hidden, with the Attention mark at its top right while
// the session asks for its user. It is dragged onto a Pane to show the
// session there.
func (v *sandboxView) sessionGlyphView(c *ui.Context, s *session) ui.Element {
	t := c.Theme()
	label, tip := v.sessionGlyphSays(s)
	c.Key("session-glyph-" + s.id)
	b := ui.ButtonBase(c).Size(sessionGlyph, sessionGlyph).Shrink(0).Radius(t.Radius).Label(label).Tooltip(tip).Drag(s)
	switch {
	case b.Pressed():
		b.Background(t.SurfacePressed)
	case b.Hovered():
		b.Background(t.SurfaceHover)
	}
	g, known := agentGlyphs[s.agent]
	if !known {
		g = glyphInfo
	}
	color := t.Text
	if v.paneOf(s) == nil {
		color = t.TextMuted
	}
	asks := s.asks()
	return b.Draw(func(p *ui.Painter, r ui.Rect) {
		g.paint(color, glyphCell)(p, r)
		if asks {
			side := float32(len(glyphHand)) * markCell
			glyphHand.paint(attention, markCell)(p, ui.Rect{X: r.X + r.W - side, Y: r.Y, W: side, H: side})
		}
	})
}
