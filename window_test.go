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

// Closing the window quits the app, which stops every sandbox: no menu
// item gives that a key. The menu bar has no File menu and no Close.
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
