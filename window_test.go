package main

import (
	"testing"

	"github.com/egoist/mygo"

	"zeolite/internal/dashboard"
	"zeolite/internal/palette"
)

// The window's own title bar is hidden: the dashboard draws its header
// under the window's controls, and the window is the palette's color
// before its first frame.
func TestWindowHidesItsTitleBar(t *testing.T) {
	opts := windowOptions(nil)
	if opts.TitleBarStyle != mygo.TitleBarHiddenInset {
		t.Errorf("the window's title bar style is %q", opts.TitleBarStyle)
	}
	if opts.BackgroundColor != palette.Background {
		t.Errorf("the window's background is %q, want the palette's %q", opts.BackgroundColor, palette.Background)
	}
	if opts.MinWidth != dashboard.MinWindowWidth || opts.MinHeight != dashboard.MinWindowHeight || opts.MinWidth < 900 || opts.MinHeight < 600 {
		t.Errorf("the window may shrink to %d × %d, not to the %d × %d the dashboard says is its least, in which sixteen Panes have room", opts.MinWidth, opts.MinHeight, dashboard.MinWindowWidth, dashboard.MinWindowHeight)
	}
}

// fakeWindow is a window of a size, which records what it is told.
type fakeWindow struct {
	width, height       int
	minWidth, minHeight int
	resized             int
}

func (w *fakeWindow) SetMinimumSize(width, height int) { w.minWidth, w.minHeight = width, height }
func (w *fakeWindow) Size() (int, int)                 { return w.width, w.height }
func (w *fakeWindow) SetSize(width, height int) {
	w.width, w.height, w.resized = width, height, w.resized+1
}

// The window is made no smaller than its page needs: that is its least
// from then on, and a window that is smaller already grows to it, by
// what it lacks and no more. One that is large enough keeps its size.
func TestTheWindowIsKeptNoSmallerThanItsPageNeeds(t *testing.T) {
	large := &fakeWindow{width: 1280, height: 860}
	keepNoSmaller(large, 1100, 700)
	if large.minWidth != 1100 || large.minHeight != 700 || large.resized != 0 || large.width != 1280 || large.height != 860 {
		t.Errorf("a window of 1280 × 860 told to be no smaller than 1100 × 700 is %+v", *large)
	}
	small := &fakeWindow{width: 1000, height: 860}
	keepNoSmaller(small, 1100, 700)
	if small.minWidth != 1100 || small.minHeight != 700 || small.width != 1100 || small.height != 860 {
		t.Errorf("a window of 1000 × 860 told to be no smaller than 1100 × 700 is %+v", *small)
	}
	keepNoSmaller(small, dashboard.MinWindowWidth, dashboard.MinWindowHeight)
	if small.minWidth != dashboard.MinWindowWidth || small.minHeight != dashboard.MinWindowHeight || small.width != 1100 || small.resized != 1 {
		t.Errorf("told of a smaller least, the window is %+v: it may be made smaller again, and is not by itself", *small)
	}
}

// Closing the window puts every terminal out of sight: no menu item gives
// that a key. The menu bar has no File menu and no Close.
func TestNoMenuItemClosesTheWindow(t *testing.T) {
	var walk func(items []*mygo.MenuItem)
	walk = func(items []*mygo.MenuItem) {
		for _, item := range items {
			switch item.Role {
			case mygo.RoleFileMenu, mygo.RoleClose:
				t.Errorf("the menu bar has an item of role %q", item.Role)
			}
			walk(item.Submenu)
		}
	}
	items := menu()
	walk(items)
	if len(items) != 3 || items[0].Role != mygo.RoleAppMenu || items[1].Role != mygo.RoleEditMenu || items[2].Role != mygo.RoleWindowMenu {
		t.Errorf("the menu bar is not the app's menu, Edit and Window")
	}
}

// Closing the window does not quit the app: the close is refused and the
// window hidden, so that the sandboxes run on. A quit's close goes
// through, and one whose quit was canceled is refused again.
func TestClosingTheWindowHidesIt(t *testing.T) {
	hidden := 0
	closer := &windowCloser{hide: func() { hidden++ }}

	e := &mygo.CloseEvent{}
	closer.closing(e)
	if !e.DefaultPrevented() || hidden != 1 {
		t.Errorf("a close of the window: refused %v, hidden %d times; want it refused and the window hidden once", e.DefaultPrevented(), hidden)
	}

	closer.quitting = true
	e = &mygo.CloseEvent{}
	closer.closing(e)
	if e.DefaultPrevented() || hidden != 1 {
		t.Errorf("a quit's close of the window: refused %v, hidden %d times; want it let through and nothing hidden", e.DefaultPrevented(), hidden-1)
	}

	closer.quitting = false
	e = &mygo.CloseEvent{}
	closer.closing(e)
	if !e.DefaultPrevented() || hidden != 2 {
		t.Errorf("a close after a quit that was canceled: refused %v, hidden %d times; want it refused and the window hidden", e.DefaultPrevented(), hidden-1)
	}
}
