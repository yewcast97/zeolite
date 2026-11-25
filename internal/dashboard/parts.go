package dashboard

import (
	"fmt"
	"slices"

	"github.com/egoist/mygo/ui"
)

// The most Panes that stand side by side on a running sandbox's page, and
// the most that stand one over another: sixteen Panes at most.
const (
	maxPanesAcross = 4
	maxPanesDown   = 4
)

// part is a part of a running sandbox's page: a Pane, or two parts that
// share its room, side by side or one over the other, with a divider
// between them that is dragged by itself. The page is one part, and a
// Pane that is divided becomes two.
type part struct {
	// pane is the Pane, where the part is one.
	pane *pane
	// first and second are the two parts of one that is no Pane: second
	// stands right of first, or under it where the part is stacked. share
	// is the percentage of the part's room that first takes, and key
	// tells its divider from the others of its sandbox for as long as it
	// is there.
	first, second *part
	stacked       bool
	share         float32
	key           int
	// box is the part as the window lays it out, and room where it stood
	// when it was last built: the room its two parts share, which says
	// how far its divider may go.
	box  ui.Handle
	room ui.Rect
}

// evenShare is the share of a part's room its first part takes until its
// divider is dragged, and again after a double click on it.
const evenShare = 50

// panes are the part's Panes in reading order: of two parts, the first's
// come before the second's.
func (t *part) panes(into []*pane) []*pane {
	if t == nil {
		return into
	}
	if t.pane != nil {
		return append(into, t.pane)
	}
	return t.second.panes(t.first.panes(into))
}

// extent is how many Panes stand side by side in the part, and how many
// one over another, were the Pane divided two: stacked, or side by side.
// With no Pane divided it is what stands there now.
func (t *part) extent(divided *pane, stacked bool) (across, down int) {
	switch {
	case t == nil:
		return 0, 0
	case t.pane != nil && t.pane == divided && stacked:
		return 1, 2
	case t.pane != nil && t.pane == divided:
		return 2, 1
	case t.pane != nil:
		return 1, 1
	}
	a1, d1 := t.first.extent(divided, stacked)
	a2, d2 := t.second.extent(divided, stacked)
	if t.stacked {
		return max(a1, a2), d1 + d2
	}
	return a1 + a2, max(d1, d2)
}

// needs is the least room the part takes, were the Pane divided two,
// stacked or side by side: as much as a Viewport takes for a Pane that
// shows one, what a divider leaves either side of itself for any other,
// and for two parts what both need and the line between them. With no
// Pane divided it is what the part needs as it stands.
func (t *part) needs(divided *pane, stacked bool) (width, height float32) {
	if t == nil {
		return 0, 0
	}
	if t.pane != nil {
		width, height = paneMin, paneMin
		if t.pane.viewport != nil {
			width, height = viewportMinWidth, viewportMinHeight
		}
		switch {
		case t.pane != divided:
		case stacked:
			height += paneMin + 1
		default:
			width += paneMin + 1
		}
		return width, height
	}
	w1, h1 := t.first.needs(divided, stacked)
	w2, h2 := t.second.needs(divided, stacked)
	if t.stacked {
		return max(w1, w2), h1 + h2 + 1
	}
	return w1 + w2 + 1, max(h1, h2)
}

// fitShare is the share of a room of a length that the first of two
// parts takes, of the share its divider was dragged to: neither part has
// less than it needs. In a room too small for both, each has of it in
// proportion to what it needs. A room whose length is not known yet
// leaves the share as it is.
func fitShare(share, length, first, second float32) float32 {
	// The line between the parts takes one of the room's length.
	length--
	if length <= 0 {
		return share
	}
	if first+second+2*fitSlack > length {
		return first * 100 / (first + second)
	}
	// A hair more than each needs, so that what the layout makes of the
	// share is no hair less.
	return min(max(share, (first+fitSlack)*100/length), 100-(second+fitSlack)*100/length)
}

// fitSlack is how much more than it needs a part is left by a divider
// that stops for it: less than a pixel of any screen.
const fitSlack = 0.01

// fit keeps the part's divider where both its parts have what they need,
// wherever it was dragged to: the share is written back, so that the
// divider stops there.
func (t *part) fit() {
	w1, h1 := t.first.needs(nil, false)
	w2, h2 := t.second.needs(nil, false)
	if t.stacked {
		t.share = fitShare(t.share, t.room.H, h1, h2)
	} else {
		t.share = fitShare(t.share, t.room.W, w1, w2)
	}
}

// of is the part that is a Pane, and the part it is one of two in, nil
// for the page's own.
func (t *part) of(p *pane) (leaf, parent *part) {
	if t == nil {
		return nil, nil
	}
	if t.pane != nil {
		if t.pane == p {
			return t, nil
		}
		return nil, nil
	}
	for _, child := range []*part{t.first, t.second} {
		if leaf, parent := child.of(p); leaf != nil {
			if parent == nil {
				parent = t
			}
			return leaf, parent
		}
	}
	return nil, nil
}

// arrange takes note that the page's parts changed: the Panes are listed
// again in the order they stand, by which each is named. The Pane that
// had the keyboard keeps it: its terminal is another element where it
// stands now.
func (v *sandboxView) arrange() {
	v.panes = v.root.panes(nil)
	if k := v.keyboard; k != nil && v.panesShown() && slices.Contains(v.panes, k) {
		v.giveKeyboard(k)
	}
}

// What a control that adds a Pane says while it can add none: four
// stand that way already, or the page has no room for another beside
// the Panes that show a browser, which keep their size.
const (
	fullAcross  = "No more than four Panes stand side by side"
	fullDown    = "No more than four Panes stand one over another"
	tightAcross = "No room for another Pane beside this one: a Pane that shows a browser keeps its width"
	tightDown   = "No room for another Pane below this one: a Pane that shows a browser keeps its height"
)

// cannotDivide says why a Pane cannot be divided now, stacked or side by
// side, in the app's own words: empty where it can be.
func (v *sandboxView) cannotDivide(p *pane, stacked bool) string {
	across, down := v.root.extent(p, stacked)
	switch {
	case across > maxPanesAcross:
		return fullAcross
	case down > maxPanesDown:
		return fullDown
	}
	// The page as it was last laid out holds what its Panes would need
	// then; one that was not laid out yet refuses nothing.
	width, height := v.root.needs(p, stacked)
	switch {
	case !stacked && v.room.W > 0 && width > v.room.W:
		return tightAcross
	case stacked && v.room.H > 0 && height > v.room.H:
		return tightDown
	}
	return ""
}

// divide halves a Pane and returns the new Pane that takes the other
// half, right of it or, stacked, under it: nil where the Pane cannot be
// divided so. The new Pane shows nothing yet.
func (v *sandboxView) divide(p *pane, stacked bool) *pane {
	leaf, _ := v.root.of(p)
	if leaf == nil || !running(v.status.State) || v.cannotDivide(p, stacked) != "" {
		return nil
	}
	added := v.newPane()
	v.partSeq++
	// The two share the room the Pane had, in halves, but for what the
	// Pane needs where it shows a browser.
	*leaf = part{first: &part{pane: p}, second: &part{pane: added}, stacked: stacked, share: evenShare, key: v.partSeq, room: p.size}
	leaf.fit()
	v.arrange()
	return added
}

// roomForPane is whether any Pane can still be divided.
func (v *sandboxView) roomForPane() bool {
	for _, p := range v.panes {
		if v.cannotDivide(p, false) == "" || v.cannotDivide(p, true) == "" {
			return true
		}
	}
	return false
}

// newPane is a Pane that stands nowhere yet.
func (v *sandboxView) newPane() *pane {
	v.paneSeq++
	return &pane{key: v.paneSeq}
}

// remove takes a Pane out of the page's parts, where it is not the last:
// the part it shared its room with takes all of it. It reports whether
// it did.
func (v *sandboxView) remove(p *pane) bool {
	leaf, parent := v.root.of(p)
	if leaf == nil || parent == nil {
		return false
	}
	other := parent.first
	if other == leaf {
		other = parent.second
	}
	// The part that stays has the room the two shared.
	room := parent.room
	*parent = *other
	parent.room = room
	if parent.pane != nil {
		parent.pane.size = room
	}
	v.arrange()
	return true
}

// partView builds a part: its Pane, or its two parts either side of
// their divider, which a double click puts back in the middle. What a
// Pane's controls ask is kept in asked, to be done once every Pane is
// built.
func (v *sandboxView) partView(c *ui.Context, t *part, asked *func()) {
	if t.pane != nil {
		if ask := v.paneView(c, t.pane); ask != nil {
			*asked = ask
		}
		return
	}
	first := func() { v.partView(c, t.first, asked) }
	second := func() { v.partView(c, t.second, asked) }
	// Where the part stood in the last frame says how far its divider
	// may go: a drag that took it further is brought back here, before
	// anything is laid out by it.
	if room := t.box.Bounds(c); room.W > 0 && room.H > 0 {
		t.room = room
	}
	t.fit()
	c.Key(fmt.Sprintf("part-%d", t.key))
	if t.stacked {
		ui.SplitVerticalPercent(c, &t.share, first, second).SplitReset(evenShare).Fill().Bind(&t.box)
	} else {
		ui.SplitPercent(c, &t.share, first, second).SplitReset(evenShare).Fill().Bind(&t.box)
	}
}
