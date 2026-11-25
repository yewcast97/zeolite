package main

import (
	"testing"

	"github.com/egoist/mygo"

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
	if opts.MinWidth < 900 || opts.MinHeight < 600 {
		t.Errorf("the window may shrink to %d × %d, too small for a viewport over the Sandbox Terminal", opts.MinWidth, opts.MinHeight)
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
