// Package dashboard is the app's window: MyGo views and the state they
// show. It is the only package, with the main package, that imports MyGo.
package dashboard

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// Dashboard is the state of the window. It belongs to the main thread:
// whatever else changes it does so inside the function given to update.
type Dashboard struct {
	backend Backend
	// update runs a function on the main thread and draws a new frame,
	// as mygo.Window.Update does.
	update func(func())

	configs []sandbox.Config
	views   map[string]*sandboxView
	// selected is what the page shows: the ID of a sandbox, or the key
	// of a Shared Folder.
	selected string

	// folders are the Shared Folders on this Mac's disk, by name, as the
	// backend last listed them, and folderViews what the dashboard holds
	// for each Shared Folder it has shown.
	folders     []string
	folderViews map[string]*sharedFolderView
	// newTerminal makes a session's terminal: terminal.New. A test puts
	// another in its place.
	newTerminal func(terminal.Options) (*terminal.Terminal, error)
	// foldersAsked is whether the backend was asked for them yet: it is
	// asked once the window builds its first frame, when there is
	// somewhere for the answer to go.
	foldersAsked bool

	sidebarWidth float32
	// problem is the last thing that went wrong outside any sandbox.
	problem string
	// destroying is whether the alert that asks before a sandbox is
	// destroyed is open.
	destroying bool
	// opening is the Opening while the window shows it, and nil once it
	// gave way to the page.
	opening *opening
}

// New returns a dashboard of the saved configs. update is how goroutines
// reach the main thread: mygo.Window.Update, once the window exists.
func New(backend Backend, configs []sandbox.Config, update func(func())) *Dashboard {
	d := &Dashboard{
		backend:      backend,
		update:       update,
		configs:      configs,
		views:        map[string]*sandboxView{},
		folderViews:  map[string]*sharedFolderView{},
		newTerminal:  terminal.New,
		sidebarWidth: sidebarWidth,
	}
	if len(configs) > 0 {
		d.selected = configs[0].ID
	}
	return d
}

// Apply takes news of a sandbox. It must be called on the main thread.
func (d *Dashboard) Apply(u sandbox.Update) {
	v := d.view(u.SandboxID)
	if v == nil {
		return
	}
	switch {
	case u.Status != nil:
		was := running(v.status.State)
		v.status = *u.Status
		switch {
		case !running(v.status.State):
			v.closeLive()
			// A sandbox that stopped shows its Setup, and its Panes at
			// its next start. One that is being started again shows what
			// whoever starts it said.
			if !v.busy && v.status.State != sandbox.StateStarting {
				v.showSetup = false
			}
		case !was:
			// A workstation that just started gets its first Pane: a
			// login shell.
			v.openFirstPane()
		}
	case u.Gate != nil:
		v.gate = *u.Gate
		v.sawExit(u.Gate.Interface, u.Gate.ExitAddress)
	case u.Workstation != nil:
		v.workstation = *u.Workstation
		v.sessionFacts(u.Workstation.Sessions)
	case u.Hand != nil:
		v.raised(u.Hand.Session)
	case u.MCPCall != nil:
		if v.mcpCalls = append(v.mcpCalls, *u.MCPCall); len(v.mcpCalls) > maxMCPCalls {
			v.mcpCalls = v.mcpCalls[len(v.mcpCalls)-maxMCPCalls:]
		}
	}
}

// Close ends what the dashboard holds open: Terminal Sessions and
// Viewport Streams.
func (d *Dashboard) Close() {
	for _, v := range d.views {
		v.closeLive()
	}
}

// maxMCPCalls is how many MCP calls of a sandbox are kept to show.
const maxMCPCalls = 2000

// running reports whether a sandbox in this state has a workstation.
func running(s sandbox.State) bool {
	switch s {
	case sandbox.StateGateOpen, sandbox.StateGateLocked:
		return true
	}
	return false
}

func (d *Dashboard) config(id string) *sandbox.Config {
	for i := range d.configs {
		if d.configs[i].ID == id {
			return &d.configs[i]
		}
	}
	return nil
}

// view returns the view of a sandbox, making it when the sandbox exists
// and has none yet.
func (d *Dashboard) view(id string) *sandboxView {
	if v, ok := d.views[id]; ok {
		return v
	}
	cfg := d.config(id)
	if cfg == nil {
		return nil
	}
	v := newSandboxView(d, *cfg)
	d.views[id] = v
	return v
}

func newID() string {
	raw := make([]byte, 5)
	rand.Read(raw)
	return "s" + hex.EncodeToString(raw)
}

// save writes the configs, and shows what went wrong if it could not.
func (d *Dashboard) save() {
	configs := append([]sandbox.Config(nil), d.configs...)
	go func() {
		if err := d.backend.Save(configs); err != nil {
			d.update(func() { d.problem = "Saving the sandboxes failed: " + err.Error() })
		}
	}()
}

// named is whether a sandbox other than the one with the ID except has
// this name, whatever its letters' case: two Cards with one name could
// not be told apart.
func (d *Dashboard) named(name, except string) bool {
	for _, cfg := range d.configs {
		if cfg.ID != except && strings.EqualFold(strings.TrimSpace(cfg.Name), strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

// freeName is a name no sandbox has: the first of name(1), name(2), …
func (d *Dashboard) freeName(name func(n int) string) string {
	for n := 1; ; n++ {
		if candidate := name(n); !d.named(candidate, "") {
			return candidate
		}
	}
}

// nameErrors is what is wrong with a sandbox's name beside the others:
// another sandbox has it.
func (d *Dashboard) nameErrors(cfg sandbox.Config) []sandbox.FieldError {
	if d.named(cfg.Name, cfg.ID) {
		return []sandbox.FieldError{{Field: sandbox.FieldName, Message: "another sandbox has this name"}}
	}
	return nil
}

// create adds a sandbox with the defaults and shows it: its Setup.
func (d *Dashboard) create() {
	cfg := sandbox.NewConfig(newID(), d.freeName(func(n int) string { return fmt.Sprintf("Sandbox %d", n) }))
	d.configs = append(d.configs, cfg)
	d.selected = cfg.ID
	d.save()
}

// clone adds a copy of a sandbox, stopped, under another name.
func (d *Dashboard) clone(id string) {
	src := d.config(id)
	if src == nil {
		return
	}
	cfg := *src
	cfg.ID = newID()
	cfg.Name = d.freeName(func(n int) string {
		if n == 1 {
			return src.Name + " copy"
		}
		return fmt.Sprintf("%s copy %d", src.Name, n)
	})
	cfg.AllowedInterfaces = nil
	for _, ifc := range src.AllowedInterfaces {
		ifc.ExpectedExitAddresses = append([]string(nil), ifc.ExpectedExitAddresses...)
		cfg.AllowedInterfaces = append(cfg.AllowedInterfaces, ifc)
	}
	cfg.Gate.ProbeURLs = append([]string(nil), src.Gate.ProbeURLs...)
	cfg.HostConditions = append([]sandbox.HostCondition(nil), src.HostConditions...)
	cfg.Packs = append([]string(nil), src.Packs...)
	d.configs = append(d.configs, cfg)
	d.selected = cfg.ID
	d.save()
}

// destroy stops a sandbox, deletes its Home and its Host Tool Scratch,
// and forgets it. A sandbox whose Home could not be deleted is not
// forgotten: its Home would be left in the VM, the agents' logins in
// it, with nothing to find it by. Nor is one whose Scratch could not be.
// A Shared Folder it was given stays on this Mac: it is deleted from its
// own page, and by nothing else.
func (d *Dashboard) destroy(id string) {
	v := d.view(id)
	if v == nil || v.busy {
		return
	}
	v.busy = true
	v.closeLive()
	go func() {
		d.backend.Stop(id)
		what := "Home"
		err := d.backend.RemoveHome(id)
		if err == nil {
			what = "Host Tool Scratch"
			err = d.backend.RemoveHostToolScratch(id)
		}
		d.update(func() {
			v.busy = false
			if err != nil {
				v.status = sandbox.Status{State: sandbox.StateError, Error: "Not destroyed: its " + what + " could not be deleted: " + err.Error()}
				return
			}
			delete(d.views, id)
			for i := range d.configs {
				if d.configs[i].ID == id {
					d.configs = append(d.configs[:i], d.configs[i+1:]...)
					break
				}
			}
			if d.selected == id {
				d.selected = ""
			}
			d.save()
		})
	}()
}

// check saves a sandbox's Setup and starts the sandbox with its Setup
// still shown: its gate then says there whether the network is as
// required. A sandbox that runs already as its Setup has it goes on
// running.
func (d *Dashboard) check(id string) {
	v := d.view(id)
	if v == nil || v.busy || !v.save() {
		return
	}
	v.showSetup = true
	d.run(id)
}

// start saves a sandbox's Setup and starts the sandbox, whose page then
// shows its Panes.
func (d *Dashboard) start(id string) {
	v := d.view(id)
	if v == nil || v.busy || !v.save() {
		return
	}
	v.showSetup = false
	d.run(id)
}

// sameStart is whether two configs start the same sandbox.
func sameStart(a, b sandbox.Config) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && bytes.Equal(x, y)
}

// run has a sandbox running as its saved config has it. One that runs
// as that config has it already is not started again. One that runs
// otherwise is stopped first, since nothing of a running workstation is
// changed: not what it mounts, not what its gate expects.
func (d *Dashboard) run(id string) {
	v, cfg := d.view(id), d.config(id)
	if v == nil || cfg == nil || v.busy {
		return
	}
	if errs := d.backend.Validate(*cfg); len(errs) > 0 {
		v.status = sandbox.Status{State: sandbox.StateError, Error: "The Setup needs attention: " + errs[0].Message}
		v.errs = errs
		return
	}
	live := running(v.status.State)
	if live && sameStart(v.running, *cfg) {
		return
	}
	v.busy = true
	v.closeLive()
	v.status = sandbox.Status{State: sandbox.StateStarting}
	v.gate, v.workstation, v.mcpCalls = sandbox.GateTelemetry{}, sandbox.WorkstationStatus{}, nil
	v.running = *cfg
	started := *cfg
	go func() {
		if live {
			d.backend.Stop(id)
		}
		err := d.backend.Start(started)
		d.update(func() {
			v.busy = false
			if err != nil {
				v.status = sandbox.Status{State: sandbox.StateError, Error: err.Error()}
				v.showSetup = false
			}
		})
		if started.SharedFolder != "" {
			// The start made the folder, where it was not there.
			d.listSharedFolders()
		}
	}()
}

// stop stops a sandbox.
func (d *Dashboard) stop(id string) {
	v := d.view(id)
	if v == nil || v.busy {
		return
	}
	v.busy = true
	v.closeLive()
	go func() {
		d.backend.Stop(id)
		d.update(func() {
			v.busy = false
			v.showSetup = false
			v.status = sandbox.Status{State: sandbox.StateStopped}
		})
	}()
}

// View builds the window's interface from the state.
func (d *Dashboard) View(c *ui.Context) {
	c.SetTheme(windowTheme)
	if !d.foldersAsked {
		d.foldersAsked = true
		d.listSharedFolders()
	}
	if d.openingView(c) {
		return
	}
	folders := d.sharedFolderNames()
	if len(d.configs) == 0 && len(folders) == 0 {
		d.emptyView(c)
		return
	}
	d.selectSomething(folders)
	d.see()
	selected := d.view(d.selected)
	// The sidebar keeps its bounds, whatever its edge was dragged to and
	// however the window was resized since: the width is written back, so
	// that the divider stops where the sidebar does.
	window, _ := c.Size()
	d.sidebarWidth = fitSidebar(d.sidebarWidth, window, sidebarLeast(c.TitleBar().Left))
	ui.Split(c, &d.sidebarWidth, func() { d.sidebarView(c, folders) }, func() {
		if name, ok := sharedFolderOf(d.selected); ok {
			d.folderView(name).view(c)
		} else if selected != nil {
			selected.view(c)
		}
	}).Fill()

	if cfg := d.config(d.selected); cfg != nil {
		question := fmt.Sprintf("Destroy “%s”?", cfg.Name)
		consequence := "Its Setup and its Home are deleted: everything in /workspace, the agents you installed there and their logins among it."
		if cfg.HostTools.Enabled {
			consequence += " So is its Host Tool Scratch, on this Mac."
		}
		if cfg.SharedFolder != "" {
			consequence += fmt.Sprintf(" Its Shared Folder, “%s”, stays on this Mac.", cfg.SharedFolder)
		}
		if ui.AlertDialog(c, &d.destroying, question, consequence, "Cancel", "Destroy") == 1 {
			d.destroy(cfg.ID)
		}
	}
}

// see takes note of what the user looks at now, before anything of this
// frame is built: what is in sight needs them no longer.
func (d *Dashboard) see() {
	if v := d.views[d.selected]; v != nil {
		v.see()
	}
}

// selectSomething makes the page show something that is there: the
// first sandbox, or else the first Shared Folder, where what was shown
// is gone.
func (d *Dashboard) selectSomething(folders []string) {
	if d.config(d.selected) != nil {
		return
	}
	if name, ok := sharedFolderOf(d.selected); ok && slices.Contains(folders, name) {
		return
	}
	switch {
	case len(d.configs) > 0:
		d.selected = d.configs[0].ID
	case len(folders) > 0:
		d.selected = sharedFolderKey(folders[0])
	}
}

func (d *Dashboard) emptyView(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Fill().Center().Gap(t.Space(2.5)).DragWindow().Children(func() {
		ui.Text(c, "No sandboxes yet").FontSize(textNotice).Bold().Font(chromeFont)
		ui.Text(c, "A sandbox is a workstation container for an agent, behind a gate you configure.").
			TextColor(t.TextMuted)
		d.runtimeBanner(c)
		if ui.PrimaryButton(c, "New Sandbox").Key("new-sandbox").Clicked() {
			d.create()
		}
	})
}

// banner says loudly that something is wrong.
func banner(c *ui.Context, text string) {
	t := c.Theme()
	ui.Row(c).Shrink(0).Padding(t.Space(2), t.Space(3)).Background(t.Danger).Children(func() {
		ui.Text(c, text).TextColor(onStatus)
	})
}

// runtimeBanner says, loudly, what is wrong where sandboxes run, and
// the last thing that went wrong outside any sandbox.
func (d *Dashboard) runtimeBanner(c *ui.Context) {
	if problem := d.backend.Runtime().Problem; problem != "" {
		banner(c, problem)
	}
	if d.problem != "" {
		banner(c, d.problem)
	}
}
