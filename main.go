// Command zeolite is the zeolite desktop app: it creates, configures and
// monitors Docker sandboxes for AI agents.
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"zeolite/internal/configstore"
	"zeolite/internal/dashboard"
	"zeolite/internal/hosttool"
	"zeolite/internal/pack"
	"zeolite/internal/palette"
	"zeolite/internal/sandbox"
	"zeolite/internal/sharedfolder"
)

// dataDirPath is where the app's data directory is: ZEOLITE_DATA_DIR
// when it is set, so that a test run does not touch the user's
// sandboxes, and else the one configstore names. It makes nothing and
// changes nothing there.
func dataDirPath() (string, error) {
	if dir := os.Getenv("ZEOLITE_DATA_DIR"); dir != "" {
		return dir, nil
	}
	return configstore.Dir()
}

// dataDir is the app's data directory, made for the user alone. The UI
// toolkit is told the same directory and the same name, so that the
// app's data is in one place.
func dataDir() (string, error) {
	dir, err := dataDirPath()
	if err != nil {
		return "", err
	}
	mygo.App.SetName(configstore.DirName)
	mygo.App.SetPath(mygo.PathUserData, dir)
	return dir, configstore.Secure(dir)
}

// sharedFolderRoot is the folder that holds the Shared Folders: the one
// sharedfolder names, or one under ZEOLITE_DATA_DIR when that is set, so that
// a test run does not touch the user's.
func sharedFolderRoot() string {
	if dir := os.Getenv("ZEOLITE_DATA_DIR"); dir != "" {
		return filepath.Join(dir, filepath.Base(sharedfolder.DefaultRoot))
	}
	return sharedfolder.DefaultRoot
}

// hostToolRoot is the folder that holds the Host Tools' folders: the one
// hosttool names, or one under ZEOLITE_DATA_DIR when that is set, so that
// a test run does not touch the user's.
func hostToolRoot() string {
	if dir := os.Getenv("ZEOLITE_DATA_DIR"); dir != "" {
		return filepath.Join(dir, filepath.Base(hosttool.DefaultRoot))
	}
	return hosttool.DefaultRoot
}

// windowOptions are the options of the app's one window. Its look is the
// dashboard's: dark, with the window's controls set in the header the
// dashboard draws, and the palette's background until the first frame.
func windowOptions(content mygo.Content) mygo.WindowOptions {
	return mygo.WindowOptions{
		Title:           "zeolite",
		Width:           1280,
		Height:          860,
		MinWidth:        dashboard.MinWindowWidth,
		MinHeight:       dashboard.MinWindowHeight,
		StateKey:        "dashboard",
		TitleBarStyle:   mygo.TitleBarHiddenInset,
		BackgroundColor: palette.Background,
		Content:         content,
	}
}

// menu is the app's menu bar: the app's own menu, Edit and Window. There
// is no File menu: its Close Window takes Cmd+W, and closing the window
// puts every terminal out of sight while the sandboxes run on.
func menu() []*mygo.MenuItem {
	return []*mygo.MenuItem{
		{Role: mygo.RoleAppMenu},
		{Role: mygo.RoleEditMenu},
		{Role: mygo.RoleWindowMenu},
	}
}

// windowCloser answers a close of the app's one window. The sandboxes run
// through the app's process: their heartbeats, their Packets, their
// Carriers and their Links. So a close that is not a quit's hides the
// window and keeps the app, whose Dock icon brings the window back; a
// quit closes it, and stops every sandbox.
type windowCloser struct {
	// quitting is whether a quit is under way: it closes every window,
	// and asks each as a click on its close button does.
	quitting bool
	// hide puts the window out of sight.
	hide func()
}

// closing is asked when the window is about to close: it refuses, and
// hides the window, unless the app is quitting.
func (k *windowCloser) closing(e *mygo.CloseEvent) {
	if k.quitting {
		return
	}
	e.PreventDefault()
	k.hide()
}

// hideWindow puts the window out of sight. One in full screen leaves it
// first: hidden there, it would leave its user before an empty Space.
func hideWindow(win *mygo.Window) {
	if !win.IsFullScreen() {
		win.Hide()
		return
	}
	var off func()
	off = win.OnLeaveFullScreen(func() {
		off()
		win.Hide()
	})
	win.SetFullScreen(false)
}

// minimumSizer makes a window no smaller than a size: the least it may be
// from then on, and its size now where that is less. The system keeps a
// window from being made smaller than its least, and leaves one that is
// smaller already as it is.
type minimumSizer interface {
	SetMinimumSize(width, height int)
	Size() (width, height int)
	SetSize(width, height int)
}

// keepNoSmaller makes a window no smaller than a width and a height,
// which the dashboard says its page needs.
func keepNoSmaller(win minimumSizer, width, height int) {
	win.SetMinimumSize(width, height)
	if w, h := win.Size(); w < width || h < height {
		win.SetSize(max(w, width), max(h, height))
	}
}

func main() {
	// Started by an ACP client in an agent's place, the program is a
	// sandbox's ACP Bridge: it is that and nothing of the app, before
	// anything of the window is asked for.
	if len(os.Args) > 1 && os.Args[1] == acpBridgeCommand {
		os.Exit(acpBridge(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}

	// The window is dark whatever the system's appearance is: its colors
	// are the palette's.
	mygo.Theme.SetSource(mygo.ThemeDark)

	dir, err := dataDir()
	if err != nil {
		log.Fatal(err)
	}
	catalog, err := pack.NewCatalog()
	if err != nil {
		log.Fatal(err)
	}
	for _, err := range catalog.LoadDir(filepath.Join(dir, "packs")) {
		log.Println("a Capability Pack was left out:", err)
	}

	// Until the window exists there is nowhere to send news.
	var win *mygo.Window
	var board *dashboard.Dashboard
	onMain := func(fn func()) {
		if win != nil {
			win.Update(fn)
		}
	}
	// While a sandbox is starting or runs, this Mac does not sleep by
	// itself: its display may turn off, and its lid still puts it to
	// sleep.
	stay := newAwake(func() func() { return mygo.Power.KeepAwake("A sandbox is running", false) })
	go stay.run()
	b := newBackend(dir, sharedFolderRoot(), hostToolRoot(), catalog, func(u sandbox.Update) {
		stay.note(u)
		onMain(func() { board.Apply(u) })
	})
	configs, err := b.store.Load()
	if err != nil {
		log.Println("the saved sandboxes could not be read:", err)
	}
	board = dashboard.New(b, configs, onMain, func(width, height int) {
		if win != nil {
			keepNoSmaller(win, width, height)
		}
	})
	board.ShowOpening()

	mygo.App.WhenReady(func() {
		// Report a missing terminal library before any window shows.
		if err := terminal.Load(); err != nil {
			log.Println("the terminal's library did not load:", err)
		}
		mygo.App.SetMenu(mygo.NewMenu(menu()))
		win = mygo.NewWindow(windowOptions(ui.View(board.View)))
		// Closing the window hides it, and the sandboxes run on: only a
		// quit closes it. The Dock icon shows it again.
		closer := &windowCloser{hide: func() { hideWindow(win) }}
		mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { closer.quitting = true })
		mygo.App.OnQuitCanceled(func() { closer.quitting = false })
		win.OnClose(closer.closing)
		mygo.App.OnActivate(func(hasVisibleWindows bool) {
			if !hasVisibleWindows {
				win.Show()
			}
		})
		// Heartbeats must keep their pace while the window is covered or
		// minimized: macOS would otherwise slow the app's timers, and
		// every gate would lock for want of them.
		mygo.Power.KeepRunning("Keeping the sandboxes' gates open")
		// Lock every gate before the Mac sleeps, as it still does when
		// its lid is closed: it may wake on another network, before a
		// Host Condition is looked at again.
		mygo.Power.OnSuspend(func() { b.setSuspended(true) })
		mygo.Power.OnResume(func() { b.setSuspended(false) })
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			b.connect(ctx)
			win.Invalidate() // the runtime banner may have changed
		}()
	})
	err = mygo.App.Run()
	board.Close()
	b.close()
	if err != nil {
		log.Fatal(err)
	}
}
