package dashboard

import (
	"context"
	"io"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/viewport"
)

// Backend is everything the dashboard asks of the rest of the app. The
// main package implements it over Docker; tests implement it in memory.
//
// Its methods may be slow and are called off the main thread, except
// Packs, Interfaces, Validate and Runtime, which must answer at once.
type Backend interface {
	// Packs lists the Capability Packs a sandbox can enable.
	Packs() []pack.Manifest
	// Interfaces lists the interfaces of this Mac a sandbox could leave
	// by now, for the form to offer among the Allowed Interfaces.
	Interfaces() []Interface
	// Validate returns what is wrong with a config.
	Validate(cfg sandbox.Config) []sandbox.FieldError
	// Save replaces the saved configs.
	Save(configs []sandbox.Config) error
	// Start starts a sandbox and returns once its workstation runs.
	// What happens to it afterwards comes in Updates.
	Start(cfg sandbox.Config) error
	// Stop stops a sandbox.
	Stop(sandboxID string)
	// RemoveHome deletes a sandbox's Home, and everything in it, from
	// the VM. It refuses while the sandbox is starting or running, and
	// it is no error for there to be none. Where the runtime does not
	// answer it is an error: the Home may be there.
	RemoveHome(sandboxID string) error
	// RemoveHostToolScratch deletes a sandbox's Host Tool Scratch from
	// this Mac's disk. It refuses while the sandbox's Host Tools run, and
	// it is no error for there to be none.
	RemoveHostToolScratch(sandboxID string) error
	// Applications lists the folders of this Mac's /Applications, by
	// name, for the form to offer as a sandbox's Allowed Applications.
	Applications() []string
	// SharedFolders lists the Shared Folders on this Mac's disk, by
	// name.
	SharedFolders() ([]string, error)
	// RemoveSharedFolder deletes a Shared Folder, and everything in it,
	// from this Mac's disk. It refuses while a running sandbox mounts
	// it, and it is no error for there to be none.
	RemoveSharedFolder(name string) error
	// ShowSharedFolder shows a Shared Folder in the Finder.
	ShowSharedFolder(name string)
	// OpenTerminalSession starts a Terminal Session in a sandbox's
	// workstation: a login shell. What runs in it from there, its user
	// types: the app installs no agent, and starts none in a session.
	OpenTerminalSession(sandboxID string) (TerminalConn, error)
	// OpenViewport opens the Viewport Stream of a pack in a sandbox, on
	// the Browser Tab of an ID, or on the browser's first where the ID
	// is empty. It brings no Browser Tab to the front.
	OpenViewport(sandboxID string, p pack.Manifest, tab string) (ViewportStream, error)
	// OpenBrowserTab opens a new Browser Tab at an address in the
	// browser of a pack in a sandbox, in a window of its own, so that it
	// draws whatever the browser's other tabs do, and returns its ID.
	OpenBrowserTab(sandboxID string, p pack.Manifest, address string) (string, error)
	// BrowserTabs lists the Browser Tabs of the browser of a pack in a
	// sandbox as they are now, in the browser's order: the one last used
	// first.
	BrowserTabs(sandboxID string, p pack.Manifest) ([]viewport.BrowserTab, error)
	// Runtime says what, if anything, is wrong where sandboxes run, and
	// where the Shared Folders are.
	Runtime() Runtime
}

// Interface is one of this Mac's interfaces that a sandbox could leave
// by now, as the form offers it.
type Interface struct {
	// Name is what an Allowed Interface holds: en0.
	Name string
	// Title is what the system calls its port, Wi-Fi, or empty where it
	// has none, as a tunnel.
	Title string
}

// Runtime is the state of what sandboxes run on.
type Runtime struct {
	// Problem is why sandboxes cannot be trusted to run there, or empty.
	Problem string
	// SharedFolderRoot is the folder of this Mac that holds the Shared
	// Folders, each in a folder of its name.
	SharedFolderRoot string
}

// TerminalConn is the connection of a Terminal Session in a workstation:
// what its program prints is read from it, what is typed is written to
// it, and Resize follows the terminal that shows it.
type TerminalConn interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	// ID is the session's ID in its workstation, by which what the
	// workstation says of the session is known for this one's.
	ID() string
}

// ViewportStream is an open Viewport Stream.
type ViewportStream interface {
	// Next returns the next frame, and says by being called that the
	// one before was shown.
	Next(ctx context.Context) (viewport.Frame, error)
	Pointer(event viewport.PointerEvent, x, y float64, button, clicks int, mods viewport.Modifiers) error
	Scroll(x, y, dx, dy float64, mods viewport.Modifiers) error
	Text(text string) error
	// Compose shows text in the page as what an input method composes,
	// its caret after caret of its characters; an empty text ends it.
	Compose(text string, caret int) error
	PressKey(key viewport.Key, down bool, mods viewport.Modifiers) error
	// Edit has the page do a command of an Edit menu, and Selection is
	// the text selected in it: the page's word.
	Edit(command viewport.EditCommand) error
	Selection(ctx context.Context) (string, error)
	// Tab is the ID of the Browser Tab the stream shows. Behind is
	// whether that tab stands behind another in its window, where it
	// draws nothing and the stream gives no frame, and Front brings it
	// to the front of that window.
	Tab() string
	Behind() bool
	Front() error
	// Navigate sends the Browser Tab the stream shows to an address,
	// Back to the one before where it is in its own history, and
	// Forward to the one after: nowhere, where there is none.
	Navigate(address string) error
	Back(ctx context.Context) error
	Forward(ctx context.Context) error
	// CloseTab closes the Browser Tab the stream shows, which ends the
	// stream.
	CloseTab(ctx context.Context) error
	Close() error
}
