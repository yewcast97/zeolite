package dashboard

import (
	"context"
	"fmt"
	"slices"
	"time"

	"zeolite/internal/pack"
	"zeolite/internal/viewport"
)

// browser is what the dashboard holds of the browser of one pack in a
// running sandbox: the Browser Tabs it has open, listed once for every
// Viewport that shows one of them. No two Viewports show one Browser
// Tab.
type browser struct {
	sv   *sandboxView
	pack pack.Manifest

	// tabs are the browser's Browser Tabs, each where the dashboard first
	// saw it, and order their IDs as the browser last listed them: the
	// one last used first. listings counts the times it was listed.
	tabs     []viewport.BrowserTab
	order    []string
	listings int
	// cancel ends the listing, nil while the browser is not listed.
	cancel context.CancelFunc
	// shown is signalled by each build of a Viewport of the browser: its
	// Browser Tabs are listed again only after the listing before was
	// built into a frame of the window, so a browser nobody sees is asked
	// nothing. soon cuts short the wait between two listings.
	shown chan struct{}
	soon  chan struct{}
}

// browserOf is what the dashboard holds of a pack's browser in the
// sandbox, from the first time it is asked for until the sandbox stops.
func (v *sandboxView) browserOf(m pack.Manifest) *browser {
	b := v.browsers[m.Name]
	if b == nil {
		b = &browser{sv: v, pack: m, shown: make(chan struct{}, 1), soon: make(chan struct{}, 1)}
		v.browsers[m.Name] = b
	}
	return b
}

// linkBrowser is the browser a link clicked in the sandbox opens in: its
// first pack's that has one, nil where it has none.
func (v *sandboxView) linkBrowser() *browser {
	for _, m := range v.streamPacks() {
		if m.ViewportStream.Type == pack.StreamCDP {
			return v.browserOf(m)
		}
	}
	return nil
}

// views are the Viewports of the browser, in the order their Panes
// stand.
func (b *browser) views() []*viewportView {
	var out []*viewportView
	for _, p := range b.sv.panes {
		if p.viewport != nil && p.viewport.b == b {
			out = append(out, p.viewport)
		}
	}
	return out
}

// shownBy is the Viewport that shows a Browser Tab, other than one,
// or nil.
func (b *browser) shownBy(tab string, except *viewportView) *viewportView {
	for _, vp := range b.views() {
		if vp != except && vp.tab == tab && tab != "" {
			return vp
		}
	}
	return nil
}

// free is the first of the browser's Browser Tabs, in the browser's own
// order, that no Viewport shows and that is not this one: empty where
// every one is shown.
func (b *browser) free(except string) string {
	for _, id := range b.order {
		if id != except && b.shownBy(id, nil) == nil {
			return id
		}
	}
	return ""
}

// seen is told by each build of a Viewport of the browser: this build
// shows whatever Browser Tabs were last listed, and the browser is
// listed from the first build on, once it is ready.
func (b *browser) seen() {
	select {
	case b.shown <- struct{}{}:
	default:
	}
	if b.cancel == nil && b.sv.daemonReady(b.pack.Name) {
		ctx, cancel := context.WithCancel(context.Background())
		b.cancel = cancel
		go b.list(ctx)
	}
}

// hurry has the browser listed now, not a moment from now.
func (b *browser) hurry() {
	select {
	case b.soon <- struct{}{}:
	default:
	}
}

// close ends the listing.
func (b *browser) close() {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
}

// list lists the browser's Browser Tabs until ctx ends: again a moment
// after each listing was built into a frame of the window.
func (b *browser) list(ctx context.Context) {
	d, id := b.sv.d, b.sv.id
	for {
		tabs, err := d.backend.BrowserTabs(id, b.pack)
		if ctx.Err() != nil {
			return
		}
		// Forget builds from before this listing, then wait for the one
		// that shows it.
		select {
		case <-b.shown:
		default:
		}
		d.update(func() {
			// A listing that failed leaves the one before: a stream says
			// so itself when the browser is gone.
			if err == nil && ctx.Err() == nil {
				b.take(tabs)
			}
		})
		select {
		case <-b.shown:
		case <-ctx.Done():
			return
		}
		select {
		case <-time.After(tabsEvery):
		case <-b.soon:
		case <-ctx.Done():
			return
		}
	}
}

// take takes the browser's Browser Tabs as it lists them now. Each keeps
// the place the dashboard first saw it at, and a new one comes last. A
// tab that a Viewport's user chose in its strip, and the Viewport has
// not acted on yet, stays chosen there; and a Viewport that waits for a
// Browser Tab to show takes one.
func (b *browser) take(tabs []viewport.BrowserTab) {
	views := b.views()
	wanted := make([]string, len(views))
	for i, vp := range views {
		if vp.chosen != vp.shownIndex() && vp.chosen >= 0 && vp.chosen < len(b.tabs) {
			wanted[i] = b.tabs[vp.chosen].ID
		}
	}
	now := make(map[string]viewport.BrowserTab, len(tabs))
	b.order = b.order[:0]
	for _, tab := range tabs {
		now[tab.ID] = tab
		b.order = append(b.order, tab.ID)
	}
	var kept []viewport.BrowserTab
	for _, tab := range b.tabs {
		if latest, there := now[tab.ID]; there {
			kept = append(kept, latest)
			delete(now, tab.ID)
		}
	}
	for _, tab := range tabs {
		if _, unseen := now[tab.ID]; unseen {
			kept = append(kept, tab)
		}
	}
	b.tabs = kept
	b.listings++
	for i, vp := range views {
		if vp.choosing && b.listings > vp.since {
			vp.pick()
		}
		vp.chosen = vp.shownIndex()
		if at := slices.IndexFunc(b.tabs, func(t viewport.BrowserTab) bool { return t.ID == wanted[i] }); at >= 0 {
			vp.chosen = at
		}
	}
}

// memoryIsFull is what the control that has a Pane show a browser says
// while the sandbox has too much of its memory in use for another.
const memoryIsFull = "The sandbox's memory is nearly all in use"

// paneTooSmall is what that control says on a Pane that is smaller than
// a Viewport may be.
var paneTooSmall = fmt.Sprintf("This Pane is too small to show a browser, which takes %d by %d", viewportMinWidth, viewportMinHeight)

// memoryIsNearlyFull is whether the sandbox has so much of its memory in
// use that no Pane of it is made to show a browser. Where its
// workstation could not say how much it has in use, it is not.
func (v *sandboxView) memoryIsNearlyFull() bool {
	return nearlyFull(v.workstation.MemoryUsed, v.workstation.MemoryLimit)
}

// nearlyFull is whether used is memoryFull hundredths of limit, or more:
// never, of a limit that is not known.
func nearlyFull(used, limit uint64) bool {
	return limit > 0 && used*100 >= limit*memoryFull
}

// showLabel is what the control that has a Pane show a pack's browser
// is called.
func showLabel(m pack.Manifest) string { return "Show " + m.Title }

// cannotShow says why a Pane cannot be made to show a pack's browser
// now, in the app's own words: empty where it can be.
func (v *sandboxView) cannotShow(p *pane, m pack.Manifest) string {
	switch {
	case !v.daemonReady(m.Name):
		return m.Title + " is not ready yet"
	case v.memoryIsNearlyFull():
		return memoryIsFull
	case p.size.W > 0 && (p.size.W < viewportMinWidth || p.size.H < viewportMinHeight):
		return paneTooSmall
	}
	return ""
}

// showSays is what the control that has a Pane show a pack's browser
// says to whoever rests the pointer on it: what it does, and what
// becomes of the session the Pane shows.
func showSays(p *pane, m pack.Manifest) string {
	says := showLabel(m) + " in this Pane"
	switch s := p.session; {
	case s == nil || s.over:
	case s.agent != "":
		says += ": its session goes on, behind its glyph in the Header"
	default:
		says += ": its shell ends, with what runs in it"
	}
	return says
}

// showViewport has a Pane show a pack's browser: a Viewport, on the
// first of the browser's Browser Tabs that no other Pane shows, or on a
// new one where every one is shown. The session the Pane showed loses
// it.
func (v *sandboxView) showViewport(p *pane, m pack.Manifest) {
	if !slices.Contains(v.panes, p) || v.cannotShow(p, m) != "" {
		return
	}
	if p.viewport != nil && p.viewport.pack.Name == m.Name {
		return
	}
	vp := v.newViewport(p, m)
	vp.choose()
}

// newViewport puts a Viewport of a pack's browser in a Pane, which
// shows nothing else from then on: the session it showed loses it, and
// a Viewport it showed is closed. The Viewport shows no Browser Tab yet.
func (v *sandboxView) newViewport(p *pane, m pack.Manifest) *viewportView {
	was := p.session
	p.session, p.note, p.failed = nil, "", false
	v.lose(was)
	if p.viewport != nil {
		p.viewport.close()
	}
	p.viewport = &viewportView{sv: v, b: v.browserOf(m), pack: m, shown: make(chan struct{}, 1)}
	return p.viewport
}

// leaveViewport closes the Viewport a Pane shows, where it shows one:
// its stream, and nothing of the browser, whose Browser Tab stays for
// the next Viewport to take up.
func (p *pane) leaveViewport() {
	if p.viewport != nil {
		p.viewport.close()
		p.viewport = nil
	}
}

// paneOfViewport is the Pane that shows a Viewport, nil once none does.
func (v *sandboxView) paneOfViewport(vp *viewportView) *pane {
	for _, p := range v.panes {
		if p.viewport == vp {
			return p
		}
	}
	return nil
}

// changePlaces has two Viewports change Panes: each Browser Tab is then
// shown where the other was, by the stream that showed it.
func (v *sandboxView) changePlaces(a, b *viewportView) {
	pa, pb := v.paneOfViewport(a), v.paneOfViewport(b)
	if pa == nil || pb == nil {
		return
	}
	pa.viewport, pb.viewport = b, a
}

// copiedFor is how long a Pane's strip says that a link was copied.
var copiedFor = 2 * time.Second

// copiedSays is what a Pane's strip says while a link clicked in it was
// copied to this Mac's clipboard: the app's own word, and nothing of
// the link.
const copiedSays = "Copied"

// copyLink puts the address of a link clicked in a Pane on this Mac's
// clipboard, by its user's click, and has the Pane's strip say so for a
// moment. Nothing on this Mac opens it.
func (v *sandboxView) copyLink(p *pane, link string) {
	d := v.d
	d.services.WriteClipboard(link)
	if p == nil {
		return
	}
	p.copied++
	said := p.copied
	time.AfterFunc(copiedFor, func() {
		d.update(func() {
			if p.copied == said {
				p.copied = 0
			}
		})
	})
}
