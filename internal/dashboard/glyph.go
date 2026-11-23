package dashboard

import (
	"math"

	"github.com/egoist/mygo/ui"
)

// A glyph is a small picture drawn from a grid of characters in the
// source, as the app's icon is: every line is a row of cells, a mark is
// a cell painted and a dot one left alone. The window's pictures are all
// glyphs: what a sandbox's state is, an agent, what a control does.
type glyph []string

// mark is the character of a glyph's art that is painted.
const mark = '#'

// columns is how many cells wide the glyph is.
func (g glyph) columns() int {
	n := 0
	for _, row := range g {
		n = max(n, len(row))
	}
	return n
}

// paint draws the glyph in the middle of a rectangle, each cell as wide
// as cell. A cell is a whole number of the screen's pixels, so that the
// marks join with no seam between them and every one is as large as the
// next.
func (g glyph) paint(color ui.Color, cell float32) func(p *ui.Painter, r ui.Rect) {
	return func(p *ui.Painter, r ui.Rect) {
		scale := p.Scale()
		if scale <= 0 {
			scale = 1
		}
		size := float32(math.Max(1, math.Round(float64(cell*scale)))) / scale
		left := r.X + (r.W-size*float32(g.columns()))/2
		top := r.Y + (r.H-size*float32(len(g)))/2
		// On the screen's pixels, so that the glyph is not between two.
		left = float32(math.Round(float64(left*scale))) / scale
		top = float32(math.Round(float64(top*scale))) / scale
		for y, row := range g {
			for x := 0; x < len(row); x++ {
				if row[x] != mark {
					continue
				}
				// A run of marks is one rectangle.
				end := x
				for end+1 < len(row) && row[end+1] == mark {
					end++
				}
				p.Fill(ui.Rect{X: left + float32(x)*size, Y: top + float32(y)*size, W: float32(end-x+1) * size, H: size}, color, 0)
				x = end
			}
		}
	}
}

// view shows the glyph in a color, each cell as wide as cell.
func (g glyph) view(c *ui.Context, color ui.Color, cell float32) ui.Element {
	return ui.Box(c).Size(cell*float32(g.columns()), cell*float32(len(g))).Shrink(0).Draw(g.paint(color, cell))
}

// glyphButton is a control that shows a glyph and no words. key is its
// identity among the controls beside it, and label says what it does,
// to whoever rests the pointer on it and to whoever does not see it.
func glyphButton(c *ui.Context, key string, g glyph, label string) ui.Element {
	return glyphButtonIn(c, key, g, label, label, c.Theme().Text)
}

// glyphButtonIn is a glyphButton whose glyph is in a color of its own,
// and whose tooltip may say more than its label.
func glyphButtonIn(c *ui.Context, key string, g glyph, label, tip string, color ui.Color) ui.Element {
	t := c.Theme()
	c.Key(key)
	b := ui.ButtonBase(c).Size(glyphButtonSize, glyphButtonSize).Shrink(0).Radius(t.Radius).Label(label).Tooltip(tip)
	switch {
	case b.Pressed():
		b.Background(t.SurfacePressed)
	case b.Hovered():
		b.Background(t.SurfaceHover)
	}
	return b.Draw(g.paint(color, glyphCell))
}

// info is the glyph that holds what would crowd the page: it shows the
// explanation to whoever rests the pointer on it, or comes to it with
// the keyboard.
func info(c *ui.Context, about, explanation string) ui.Element {
	t := c.Theme()
	return ui.Box(c).Size(glyphButtonSize, glyphButtonSize).Shrink(0).Focusable().Label("About " + about).Tooltip(explanation).
		Draw(glyphInfo.paint(t.TextMuted, glyphCell))
}
