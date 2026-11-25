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

// What a control that adds a Pane says while it can add none.
const (
	fullAcross = "No more than four Panes stand side by side"
	fullDown   = "No more than four Panes stand one over another"
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
	*leaf = part{first: &part{pane: p}, second: &part{pane: added}, stacked: stacked, share: evenShare, key: v.partSeq}
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
	*parent = *other
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
	c.Key(fmt.Sprintf("part-%d", t.key))
	if t.stacked {
		ui.SplitVerticalPercent(c, &t.share, first, second).SplitReset(evenShare).Fill()
	} else {
		ui.SplitPercent(c, &t.share, first, second).SplitReset(evenShare).Fill()
	}
}
