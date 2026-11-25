package dashboard

import (
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/viewport"
)

// browsing is a harness whose first sandbox, Research, runs with the
// test's browser ready, which lists the Browser Tabs given, in a window
// of a size.
func browsing(t *testing.T, width, height int, tabs ...viewport.BrowserTab) (*harness, *sandboxView, pack.Manifest) {
	t.Helper()
	browser := browserPack(t)
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg, gated("s2", "Plain")}, browser)
	h.tt.SetSize(width, height)
	h.backend.browse(tabs...)
	v := h.d.view("s1")
	v.running = cfg
	h.runs("s1")
	h.ready("s1", browser)
	return h, v, browser
}

// paneAt is where a Pane of Research stands in the window.
func (h *harness) paneAt(pane int) ui.Rect {
	h.t.Helper()
	at, ok := h.tt.Find(paneName(pane) + " of Research")
	if !ok {
		h.t.Fatalf("no %s in the window: %q", paneName(pane), h.tt.Texts())
	}
	return at
}

// another adds a Pane by a control of a strip, and waits for its shell.
func (h *harness) another(label string) {
	h.t.Helper()
	v := h.d.views[h.d.selected]
	n := len(v.panes)
	h.click(label)
	h.settle("another Pane's shell", func() bool {
		return len(v.panes) == n+1 && !slices.ContainsFunc(v.panes, func(p *pane) bool { return p.viewport == nil && p.session == nil })
	})
}

// says is whether the window shows this text, or something labelled so,
// and nothing longer.
func (h *harness) says(text string) bool { _, ok := h.tt.Find(text); return ok }

// just is whether a length is what a divider that stopped for it leaves:
// that much, and a hair more at most.
func just(length, want float32) bool { return length >= want && length <= want+0.5 }

var (
	homeTab = viewport.BrowserTab{ID: "t1", Title: "Example Domain", Address: "https://example.com/"}
	docsTab = viewport.BrowserTab{ID: "t2", Title: "Docs", Address: "https://example.org/docs"}
)

const showBrowser1 = "Show Web Test Browser in Pane 1"

// A Pane shows a browser in the place of its Terminal Session, by the
// control of its strip, and a terminal again by New Session. The session
// that loses the Pane ends if it is a plain shell's and is Hidden if it
// is an Agent Session; the control says which. Leaving a Viewport, by
// New Session, by a session dropped on its Pane or by Close Pane, closes
// its stream and nothing of the browser: the Browser Tab stays, and the
// next Viewport takes it up.
func TestAPaneShowsABrowserInTheSessionsPlace(t *testing.T) {
	h, v, _ := browsing(t, 1280, 900, homeTab)
	closed := func(s *fakeStream) bool {
		select {
		case <-s.closed:
			return true
		default:
			return false
		}
	}
	// kept is that the browser was asked to close no Browser Tab, and
	// lists what it listed.
	kept := func() {
		t.Helper()
		for _, s := range h.backend.opens() {
			if s.sawInput("close tab") {
				t.Fatalf("leaving a Viewport closed the Browser Tab %s", s.tab)
			}
		}
		h.backend.mu.Lock()
		defer h.backend.mu.Unlock()
		if len(h.backend.browserTabs) != 1 || h.backend.browserTabs[0].ID != "t1" || len(h.backend.links) != 0 {
			t.Fatalf("the browser lists %+v, and was asked for %q", h.backend.browserTabs, h.backend.links)
		}
	}

	// A plain shell: the control says that it ends, and it does.
	shell := h.connOf("s1", 0)
	if told := h.told(showBrowser1); !strings.Contains(told, "Show Web Test Browser in this Pane: its shell ends, with what runs in it") {
		t.Errorf("over a plain shell the control says %s", told)
	}
	vp := h.showBrowser(0)
	h.settle("the browser's Browser Tab in the Pane", func() bool { return vp.bitmap != nil && vp.address == homeTab.Address })
	if _, over := shell.state(); !over || len(v.sessions) != 0 || v.panes[0].session != nil {
		t.Fatalf("the plain shell of a Pane that shows a browser is closed: %v; the sandbox holds %d sessions", over, len(v.sessions))
	}
	first := h.backend.opens()[0]
	if first.tab != "t1" || first.sawInput("front") {
		t.Fatalf("the Pane shows the Browser Tab %q, brought to the front: %v", first.tab, first.sawInput("front"))
	}
	// The strip names the browser, offers a terminal again, and no longer
	// offers the browser the Pane shows. The Viewport stands under it.
	strip, frames := h.paneAt(0), ui.Rect{}
	if at, ok := h.tt.Find(viewport1); ok {
		frames = at
	}
	if !h.says("Web Test Browser") || !h.says("New Session in Pane 1") || h.says(showBrowser1) || frames.Y < strip.Y+stripHeight || frames.X < strip.X || frames.Y+frames.H > strip.Y+strip.H+1 {
		t.Fatalf("a Pane that shows a browser has its frames at %+v in a Pane at %+v: %q", frames, strip, h.tt.Texts())
	}
	if why := h.needs("s1"); why != "" {
		t.Errorf("a shell that gave way to a browser marks its sandbox: %q", why)
	}

	// New Session: a login shell in the browser's place. The stream is
	// closed, and the Browser Tab stays in the browser.
	shells := len(h.backend.opened())
	h.click("New Session in Pane 1")
	h.settle("a login shell in the browser's place", func() bool {
		return v.panes[0].viewport == nil && v.panes[0].session != nil && len(h.backend.opened()) == shells+1
	})
	if !closed(first) || h.says(viewport1) || !h.says(showBrowser1) {
		t.Fatalf("with a terminal again the stream is closed: %v: %q", closed(first), h.tt.Texts())
	}
	kept()

	// An Agent Session: the control says that it goes on, and it is
	// Hidden. The next Viewport takes the same Browser Tab up.
	agent := h.connOf("s1", 0)
	h.agentIn("s1", "claude")
	if told := h.told(showBrowser1); !strings.Contains(told, "Show Web Test Browser in this Pane: its session goes on, behind its glyph in the Header") {
		t.Errorf("over an Agent Session the control says %s", told)
	}
	vp = h.showBrowser(0)
	h.settle("the Browser Tab that stayed, in the Pane again", func() bool { return vp.bitmap != nil && vp.tab == "t1" && vp.stream != nil })
	if _, over := agent.state(); over || len(v.sessions) != 1 || !h.says("Claude Code, hidden") {
		t.Fatalf("the Agent Session of a Pane that shows a browser is closed: %v: %q", over, h.tt.Texts())
	}
	second := h.backend.opens()[1]

	// Its glyph dropped on the Pane shows the session there again: the
	// Viewport is closed, and its Browser Tab stays.
	h.drag("Claude Code, hidden", "Pane 1 of Research")
	h.settle("the session in its Pane again", func() bool { return v.panes[0].session != nil && v.panes[0].viewport == nil })
	if _, over := agent.state(); over || !closed(second) || h.says(viewport1) || !h.says("Claude Code, in Pane 1") {
		t.Fatalf("after its glyph was dropped on the browser's Pane the session is closed: %v, the stream: %v", over, closed(second))
	}
	kept()

	// A session that another Pane shows changes places with a Viewport
	// as with a session: the Viewport goes to the Pane the session left.
	h.another(beside(0))
	moved := h.showBrowser(1)
	h.settle("the browser in the second Pane", func() bool { return moved.stream != nil })
	third := h.backend.opens()[2]
	h.drag("Claude Code, in Pane 1", "Pane 2 of Research")
	if v.panes[1].session == nil || v.panes[0].viewport != moved || closed(third) || len(h.backend.opens()) != 3 {
		t.Fatalf("a session dragged onto the browser's Pane: the Panes show %v and %v, the stream closed: %v", v.panes[0].viewport != nil, v.panes[1].session != nil, closed(third))
	}

	// Close Pane closes the stream, and leaves the Browser Tab.
	h.click("Close Pane 1")
	h.settle("the Pane to close", func() bool { return len(v.panes) == 1 })
	if !closed(third) {
		t.Error("the stream of a Pane that closed stays open")
	}
	kept()
	// A sandbox that stops closes every stream.
	last := h.showBrowser(0)
	h.settle("the browser in the Pane that is left", func() bool { return last.stream != nil })
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	h.settle("the streams to close", func() bool { return closed(h.backend.opens()[3]) })
}

// Several Panes show the browser at once, each a Browser Tab of its own,
// which no other shows: the first of the browser's list that is free,
// and a new blank one where every one is shown. One listing of the
// browser serves them all. A tab chosen in one Pane's strip that another
// Pane shows makes the two change places.
func TestSeveralPanesShowTheBrowserAtOnce(t *testing.T) {
	// The browser lists the one last used first.
	h, v, _ := browsing(t, 1600, 1000, docsTab, homeTab)
	// Pane 1, and beside it Pane 2 over Pane 3: each has room for a
	// browser.
	h.another(beside(0))
	h.another(below(1))
	one := h.showBrowser(0)
	two := h.showBrowser(1)
	h.settle("both Panes to show a Browser Tab", func() bool {
		return one.bitmap != nil && two.bitmap != nil && one.stream != nil && two.stream != nil
	})
	if one.tab != "t2" || two.tab != "t1" || one.address != docsTab.Address || two.address != homeTab.Address {
		t.Fatalf("the Panes show the Browser Tabs %q and %q, want the browser's first and then its second", one.tab, two.tab)
	}
	// Both draw at once: each stream is asked for frames while the other
	// is.
	streams := h.backend.opens()
	if len(streams) != 2 {
		t.Fatalf("two Viewports opened %d streams", len(streams))
	}
	before := []int{streams[0].timesAsked(), streams[1].timesAsked()}
	h.settle("frames of both Browser Tabs", func() bool {
		return streams[0].timesAsked() >= before[0]+5 && streams[1].timesAsked() >= before[1]+5
	})
	// One list for both, with both tabs in each Pane's strip.
	if one.b != two.b || len(v.browsers) != 1 {
		t.Fatalf("the two Viewports list their browser apart")
	}
	for _, strip := range []string{"Browser Tabs of Pane 1", "Browser Tabs of Pane 2"} {
		if !h.says(strip) {
			t.Fatalf("no %s: %q", strip, h.tt.Texts())
		}
	}
	if h.count("Docs") != 2 || h.count("Example Domain") != 2 {
		t.Fatalf("the strips of two Panes name the two Browser Tabs %d and %d times: %q", h.count("Docs"), h.count("Example Domain"), h.tt.Texts())
	}

	// The tab the other Pane shows, chosen in the first Pane's strip:
	// the two change places, and no stream is opened or closed for it.
	h.click("Example Domain")
	h.settle("the Panes to change what they show", func() bool { return v.panes[0].viewport == two && v.panes[1].viewport == one })
	h.settle("a frame", func() bool { return true })
	if got := h.backend.opens(); len(got) != 2 || one.tab != "t2" || two.tab != "t1" || one.stream == nil || two.stream == nil {
		t.Fatalf("changing places opened %d streams; the Viewports show %q and %q", len(got), one.tab, two.tab)
	}
	if at, ok := h.tt.Find("Address of Pane 1"); !ok || v.panes[0].viewport.address != homeTab.Address || at.W <= 0 {
		t.Errorf("after they changed places the first Pane's Address Bar holds %q", v.panes[0].viewport.address)
	}

	// A third Pane: every Browser Tab is shown, so a blank one is opened
	// for it, and the keyboard is in its Address Bar.
	three := h.showBrowser(2)
	h.settle("a blank Browser Tab for the third Pane", func() bool {
		return three.tab == "link1" && three.stream != nil && h.tt.Focused("Address of Pane 3")
	})
	if got := h.backend.linksOpened(); len(got) != 1 || got[0] != "s1 "+blankAddress || three.address != "" {
		t.Fatalf("the browser was asked for %q, and the Address Bar holds %q", got, three.address)
	}
	h.tt.Type("example.net")
	h.tt.Frame()
	if three.address != "example.net" {
		t.Errorf("what was typed for the new Browser Tab is %q in its Address Bar", three.address)
	}

	// Close Browser Tab where every other tab is shown by another Pane:
	// a blank one is opened first, as for the browser's last.
	h.click(closeTabLabel + " in Pane 1")
	h.settle("a blank Browser Tab in the closed one's place", func() bool {
		closing := v.panes[0].viewport
		return len(h.backend.linksOpened()) == 2 && closing.tab == "link2" && closing.stream != nil
	})
	h.settle("the Browser Tab to be closed", func() bool {
		h.backend.mu.Lock()
		defer h.backend.mu.Unlock()
		return !slices.ContainsFunc(h.backend.browserTabs, func(tab viewport.BrowserTab) bool { return tab.ID == "t1" })
	})
	if one.tab != "t2" || three.tab != "link1" {
		t.Errorf("closing one Pane's Browser Tab changed what the others show: %q and %q", one.tab, three.tab)
	}
}

// The control that has a Pane show a browser is dimmed, does nothing,
// and says why, while the browser is not ready, while the sandbox has
// 85 hundredths of its memory in use or more, and on a Pane that is
// smaller than a Viewport takes. Memory that is not known dims nothing.
func TestTheBrowsersControlIsDimmedAndSaysWhy(t *testing.T) {
	browser := browserPack(t)
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg}, browser)
	h.tt.SetSize(1280, 900)
	h.backend.browse(homeTab)
	v := h.d.view("s1")
	v.running = cfg
	h.runs("s1")
	// refused is that a click on the control of a Pane does nothing, and
	// that it says why.
	refused := func(pane int, why string) {
		t.Helper()
		label := "Show Web Test Browser in " + paneName(pane)
		at, ok := h.tt.Find(label)
		if !ok {
			t.Fatalf("no %s: %q", label, h.tt.Texts())
		}
		h.tt.ClickAt(at.X+at.W/2, at.Y+at.H/2)
		for range 3 {
			h.settle("a frame", func() bool { return true })
		}
		if v.panes[pane].viewport != nil || v.panes[pane].session == nil || len(h.backend.opens()) != 0 {
			t.Fatalf("the dimmed control of %s made it show a browser", paneName(pane))
		}
		if told := strings.Split(h.told(label), "|"); !slices.Contains(told, why) {
			t.Errorf("the dimmed control of %s says %q, want %q", paneName(pane), told, why)
		}
		h.park()
	}
	offered := func(pane int) {
		t.Helper()
		label := "Show Web Test Browser in " + paneName(pane)
		if why := v.cannotShow(v.panes[pane], browser); why != "" {
			t.Fatalf("the control of %s is dimmed: %q", paneName(pane), why)
		}
		if told := h.told(label); !strings.Contains(told, "Show Web Test Browser in this Pane") {
			t.Errorf("the control of %s says %s", paneName(pane), told)
		}
		h.park()
	}
	memory := func(used, limit uint64) {
		t.Helper()
		status := sandbox.WorkstationStatus{Daemons: []sandbox.DaemonState{{Pack: browser.Name, Ready: true}}, MemoryUsed: used, MemoryLimit: limit}
		h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &status})
		h.settle("a frame", func() bool { return true })
	}

	// The browser is not ready yet.
	refused(0, "Web Test Browser is not ready yet")
	h.ready("s1", browser)
	offered(0)

	// Memory: at 85 hundredths and over, and not under; and not where
	// the workstation could not read it.
	const gigabyte = 1 << 30
	memory(85*gigabyte/10, 10*gigabyte)
	refused(0, memoryIsFull)
	memory(10*gigabyte, 10*gigabyte)
	refused(0, memoryIsFull)
	memory(84*gigabyte/10, 10*gigabyte)
	offered(0)
	memory(0, 0)
	offered(0)
	memory(9*gigabyte, 0)
	offered(0)
	for _, c := range []struct {
		used, limit uint64
		full        bool
	}{{85, 100, true}, {84, 100, false}, {849, 1000, false}, {850, 1000, true}, {100, 100, true}, {200, 100, true}, {5, 0, false}, {0, 0, false}, {0, 100, false}} {
		if got := nearlyFull(c.used, c.limit); got != c.full {
			t.Errorf("%d in use of %d is nearly full: %v, want %v", c.used, c.limit, got, c.full)
		}
	}

	// A Pane narrower than a Viewport takes: the page is 1059 wide, a
	// half of it wide enough, and a quarter not.
	h.another(beside(0))
	h.another(beside(1))
	if narrow := h.paneAt(1); narrow.W >= viewportMinWidth || h.paneAt(0).W < viewportMinWidth {
		t.Fatalf("the Panes are %v and %v wide", h.paneAt(0).W, narrow.W)
	}
	offered(0)
	refused(1, paneTooSmall)
	refused(2, paneTooSmall)
	// And one lower: a half of the page's height is enough, a quarter not.
	h.another(below(0))
	offered(0)
	h.another(below(0))
	if low := h.paneAt(0); low.H >= viewportMinHeight || low.W < viewportMinWidth {
		t.Fatalf("the first Pane is %v by %v", low.W, low.H)
	}
	refused(0, paneTooSmall)
	if !strings.Contains(paneTooSmall, "480 by 340") {
		t.Errorf("a Pane that is too small is told %q", paneTooSmall)
	}
}

// A Pane that shows a browser is never left less than a Viewport takes,
// 480 by 340: its own divider, a divider further out and the sidebar's
// each stop there, in every frame of a drag; the terminals beside it
// give way first; and a control that would add a Pane where there is no
// room left for one is dimmed, and says why.
func TestAPaneThatShowsABrowserKeepsItsSize(t *testing.T) {
	h, v, _ := browsing(t, 1280, 900, docsTab, homeTab)
	width := float32(1280)
	// Pane 1 is a terminal; beside it Pane 2, over Pane 3, shows the
	// browser.
	h.another(beside(0))
	h.another(below(1))
	vp := h.showBrowser(1)
	h.settle("the browser in the second Pane", func() bool { return vp.bitmap != nil })
	kept := func() {
		t.Helper()
		for i, p := range v.panes {
			at := h.paneAt(i)
			if p.viewport != nil && (at.W < viewportMinWidth || at.H < viewportMinHeight) {
				t.Fatalf("%s, which shows a browser, is %v by %v", paneName(i), at.W, at.H)
			}
			if at.W < paneMin || at.H < paneMin {
				t.Fatalf("%s is %v by %v", paneName(i), at.W, at.H)
			}
			if at.X < 0 || at.Y < 0 || at.X+at.W > width+0.5 || at.Y+at.H > 900.5 {
				t.Fatalf("%s is at %+v, not in the window", paneName(i), at)
			}
		}
	}
	kept()

	// The divider further out, between Pane 1 and the two beside it:
	// dragged right, it stops where the browser's Pane is 480 wide, and
	// dragged left, where the terminal has what a divider leaves it.
	one, two := h.paneAt(0), h.paneAt(1)
	h.dragDivider(one.X+one.W, 450, 1270, 450, kept)
	if two = h.paneAt(1); !just(two.W, viewportMinWidth) || !just(h.paneAt(2).W, viewportMinWidth) {
		t.Errorf("with the outer divider dragged to the right edge the browser's Pane is %v wide, want %v", two.W, float32(viewportMinWidth))
	}
	one = h.paneAt(0)
	h.dragDivider(one.X+one.W, 450, 2, 450, kept)
	if one = h.paneAt(0); !just(one.W, paneMin) {
		t.Errorf("with the outer divider dragged to the left edge the terminal's Pane is %v wide, want %v", one.W, float32(paneMin))
	}

	// Its own divider, under it: dragged up, it stops where the browser's
	// Pane is 340 high, and dragged down, where the terminal under it has
	// what a divider leaves it.
	two = h.paneAt(1)
	h.dragDivider(two.X+two.W/2, two.Y+two.H, two.X+two.W/2, 2, kept)
	if two = h.paneAt(1); !just(two.H, viewportMinHeight) {
		t.Errorf("with its own divider dragged to the top the browser's Pane is %v high, want %v", two.H, float32(viewportMinHeight))
	}
	h.dragDivider(two.X+two.W/2, two.Y+two.H, two.X+two.W/2, 898, kept)
	if three := h.paneAt(2); !just(three.H, paneMin) {
		t.Errorf("with the browser's divider dragged to the bottom the terminal under it is %v high, want %v", three.H, float32(paneMin))
	}
	// A double click puts a divider back in the middle, where both have
	// room.
	two = h.paneAt(1)
	h.tt.ClickAt(two.X+two.W/2, two.Y+two.H)
	h.tt.ClickAt(two.X+two.W/2, two.Y+two.H)
	h.settle("a frame", func() bool { return true })
	if two, three := h.paneAt(1), h.paneAt(2); two.H < three.H-1 || two.H > three.H+1 {
		t.Errorf("after a double click on their divider the Panes are %v and %v high", two.H, three.H)
	}
	kept()

	// The sidebar's divider: the page needs 40, a line and 480, and the
	// sidebar leaves it that, in a window narrow enough for that to be
	// less than its fifth.
	width = 640
	h.tt.SetSize(640, 900)
	h.settle("the narrower window", func() bool { return true })
	h.settle("a frame", func() bool { return true })
	needs, _ := v.root.needs(nil, false)
	if needs != paneMin+1+viewportMinWidth {
		t.Fatalf("the page needs %v wide", needs)
	}
	edge := func() float32 {
		t.Helper()
		at, ok := h.tt.Find("Divider")
		if !ok {
			t.Fatal("no divider")
		}
		return at.X
	}
	h.dragDivider(edge(), 450, 630, 450, kept)
	if edge() != 640-1-needs || edge() >= 640*sidebarShare {
		t.Errorf("dragged to the right edge, the sidebar ends at %v, want what leaves the page %v: %v", edge(), needs, 640-1-needs)
	}
	kept()
}

// Where a Pane shows a browser, a control that would add a Pane the page
// has no room for is dimmed, says why and adds none, and one that has
// room adds one.
func TestNoPaneIsAddedThatWouldPressABrowsersPane(t *testing.T) {
	// The page is 979 wide: two Panes of 489, each wide enough for a
	// browser, and a third has no room beside them.
	h, v, _ := browsing(t, 1200, 900, docsTab, homeTab)
	h.another(beside(0))
	one, two := h.showBrowser(0), h.showBrowser(1)
	h.settle("two browsers side by side", func() bool { return one.bitmap != nil && two.bitmap != nil })
	for i := range 2 {
		if why := v.cannotDivide(v.panes[i], false); why != tightAcross {
			t.Fatalf("Add Pane Beside on %s, beside two browsers in a page of %v, says %q", paneName(i), v.room.W, why)
		}
		at, _ := h.tt.Find(beside(i))
		h.tt.ClickAt(at.X+at.W/2, at.Y+at.H/2)
		h.settle("a frame", func() bool { return true })
		if told := strings.Split(h.told(beside(i)), "|"); !slices.Contains(told, tightAcross) {
			t.Errorf("the dimmed Add Pane Beside says %q", told)
		}
		h.park()
	}
	if len(v.panes) != 2 {
		t.Fatalf("a dimmed Add Pane Beside added a Pane: there are %d", len(v.panes))
	}
	// Below there is room: the new Pane has what the browser's leaves it.
	if why := v.cannotDivide(v.panes[0], true); why != "" {
		t.Fatalf("Add Pane Below says %q", why)
	}
	h.another(below(0))
	if over, under := h.paneAt(0), h.paneAt(1); over.H < viewportMinHeight || under.H < paneMin || over.W < viewportMinWidth {
		t.Fatalf("with a Pane added below it the browser's Pane is %v by %v, and the new one %v high", over.W, over.H, under.H)
	}
	// And a third below, and a fourth: then the browser's Pane has only
	// what it takes, and no more are added there.
	for len(v.panes) < 16 && v.cannotDivide(v.panes[0], true) == "" {
		h.another(below(0))
		if over := h.paneAt(0); over.H < viewportMinHeight {
			t.Fatalf("with %d Panes the browser's is %v high", len(v.panes), over.H)
		}
	}
	if why := v.cannotDivide(v.panes[0], true); why != fullDown && why != tightDown {
		t.Errorf("no more Panes are added below the browser's for %q", why)
	}
}

// The window is told the smallest it may be for the page it shows: its
// own least, and more where Panes that show a browser need more, as they
// come and go and as the page changes. It is told once for each change.
func TestTheWindowsLeastSizeFollowsThePanes(t *testing.T) {
	h, v, _ := browsing(t, 1600, 1000, docsTab, homeTab)
	least := [2]int{MinWindowWidth, MinWindowHeight}
	// told waits until the window was told a size, and that it is the
	// last thing it was told.
	told := func(what string, want [2]int) {
		t.Helper()
		h.settle(what, func() bool { return len(h.minimums) > 0 && h.minimums[len(h.minimums)-1] == want })
		for range 3 {
			h.settle("a frame", func() bool { return true })
		}
		if last := h.minimums[len(h.minimums)-1]; last != want {
			t.Fatalf("%s: the window was told %v", what, h.minimums)
		}
		for i := 1; i < len(h.minimums); i++ {
			if h.minimums[i] == h.minimums[i-1] {
				t.Fatalf("the window was told one size twice in a row: %v", h.minimums)
			}
		}
	}
	// One browser's Pane fits the window's own least: nothing is told.
	one := h.showBrowser(0)
	h.settle("the browser", func() bool { return one.bitmap != nil })
	for range 3 {
		h.settle("a frame", func() bool { return true })
	}
	if len(h.minimums) != 0 {
		t.Fatalf("with one browser's Pane the window was told %v", h.minimums)
	}

	// Two side by side need the sidebar, its line, and twice 480 with a
	// line between.
	h.another(beside(0))
	h.showBrowser(1)
	wide := [2]int{int(h.d.sidebarWidth) + 1 + 2*viewportMinWidth + 1, MinWindowHeight}
	told("two browsers side by side", wide)
	// A third under them needs the Header, its line, and twice 340 with
	// a line between.
	h.another(below(0))
	h.showBrowser(1)
	both := [2]int{wide[0], headerHeight + 1 + 2*viewportMinHeight + 1}
	told("a third browser under the first", both)

	// With one of them a terminal again, less.
	h.click("New Session in Pane 3")
	told("the Pane beside them a terminal again", [2]int{MinWindowWidth, both[1]})
	h.showBrowser(2)
	told("the browser beside them again", both)

	// The Setup, another sandbox, and the Panes again.
	h.click("Setup")
	told("the Setup in the Panes' place", least)
	h.click("Setup")
	told("the Panes again", both)
	h.click("Plain")
	told("another sandbox's page", least)
	h.click("Research")
	told("the sandbox's Panes again", both)

	// A Pane that closes, and a sandbox that stops.
	h.click("Close Pane 2")
	told("the lower browser's Pane closed", wide)
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	told("the sandbox stopped", least)
	if len(v.panes) != 0 {
		t.Fatalf("a stopped sandbox has %d Panes", len(v.panes))
	}
}

// A Browser Tab that stands behind another in the browser's window draws
// nothing: its Viewport keeps the frame it last drew and says so in a
// line, and a press on its frames brings the tab forward, and is no
// press in its page. A stream is never brought forward unasked.
func TestAViewportSaysThatItsBrowserTabIsBehind(t *testing.T) {
	h, _, _ := browsing(t, 1280, 900, homeTab)
	vp := h.showBrowser(0)
	h.settle("the browser", func() bool { return vp.bitmap != nil && vp.stream != nil })
	stream := h.backend.opens()[0]
	if h.tt.HasText(behindSays) || stream.sawInput("front") {
		t.Fatalf("a Browser Tab in front is said to be behind, or was brought forward: %q", h.tt.Texts())
	}

	h.backend.putBehind("t1", true)
	h.settle("the Viewport to say that its Browser Tab is behind", func() bool { return h.tt.HasText(behindSays) })
	if !h.says(viewport1) || vp.bitmap == nil || stream.sawInput("front") {
		t.Fatalf("behind another, the Viewport shows %q", h.tt.Texts())
	}
	// A press on its frames brings it forward, and the page gets none of
	// that press.
	inputs := len(stream.inputsSince(0))
	box, _ := h.tt.Find(viewport1)
	h.tt.ClickAt(box.X+box.W/2, box.Y+box.H/2)
	h.settle("the Browser Tab to be brought forward", func() bool { return stream.sawInput("front") && !h.tt.HasText(behindSays) })
	for _, input := range stream.inputsSince(inputs) {
		if strings.HasPrefix(input, "mousePressed") {
			t.Errorf("the press that brought the Browser Tab forward reached its page: %q", stream.inputsSince(inputs))
		}
	}
	asked := stream.timesAsked()
	h.settle("frames again", func() bool { return stream.timesAsked() >= asked+3 })
	// In front, a press is the page's.
	h.tt.ClickAt(box.X+box.W/2, box.Y+box.H/2)
	h.settle("a press to reach the page", func() bool {
		return slices.ContainsFunc(stream.inputsSince(inputs), func(in string) bool { return strings.HasPrefix(in, "mousePressed") })
	})
}
