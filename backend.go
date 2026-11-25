package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"

	"zeolite/internal/carrier"
	"zeolite/internal/configstore"
	"zeolite/internal/dashboard"
	"zeolite/internal/hosttool"
	"zeolite/internal/hostwatch"
	"zeolite/internal/link"
	"zeolite/internal/orchestrator"
	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/sharedfolder"
	"zeolite/internal/viewport"
)

// backend is what the dashboard runs on: the orchestrator on the Colima
// profile, the Pack Catalog, the config file, the Host Watchdog and the
// Shared Folders on this Mac's disk.
type backend struct {
	catalog  *pack.Catalog
	store    *configstore.Store
	watchdog *hostwatch.Watchdog
	folders  *sharedfolder.Folders
	// carrier is the Carrier's program on this Mac's disk, which is
	// started for each sandbox that has a network. noCarrier is why
	// there is none: no such sandbox starts without it.
	carrier   *carrier.Program
	noCarrier error
	// hostTools starts the Host Tools of the sandboxes that are given
	// some, each under the Host Tool Wall. noHostTools is why it cannot:
	// no such sandbox starts without it.
	hostTools   *hosttool.Tools
	noHostTools error
	// tell passes an Update on to the dashboard, from any goroutine.
	tell func(sandbox.Update)

	mu      sync.Mutex
	orch    *orchestrator.Orchestrator
	runtime dashboard.Runtime
	// noSharedFolder is why no sandbox can be started with a Shared
	// Folder, or empty: the VM does not mount the root that holds them,
	// or has not been asked yet.
	noSharedFolder string

	// folderMu makes seeing that no sandbox mounts a Shared Folder, and
	// deleting it, one step, which no start that is about to mount it
	// comes between. mounting counts, by a folder's name, the starts
	// under way that mount it: the orchestrator knows a sandbox only
	// once its start has begun.
	folderMu sync.Mutex
	mounting map[string]int
}

// howToStartColima is the command that starts the runtime as the app
// expects it: a VM of the size sandboxes are given by default, 8 GiB of
// memory among it, that mounts the Shared Folders' root and nothing
// else of the Mac. The README gives the same command.
func (b *backend) howToStartColima() string {
	return "colima start --profile zeolite --cpu 4 --memory 8 --disk 40 --vm-type vz --activate=false --mount " + b.folders.Root() + ":w"
}

// withoutUsersRoot takes the user's own Shared Folders' root out of what
// the VM exposes, for an app whose root is another: one run with a data
// directory of its own, as a test's is. That mount is for the user's app
// to judge, by a marker this one never wrote; this one neither uses it
// nor calls it exposed.
func withoutUsersRoot(exposed []string, root string) []string {
	if root == sharedfolder.DefaultRoot {
		return exposed
	}
	var others []string
	for _, folder := range exposed {
		if folder != sharedfolder.DefaultRoot {
			others = append(others, folder)
		}
	}
	return others
}

func newBackend(dir, sharedFolderRoot, hostToolRoot string, catalog *pack.Catalog, tell func(sandbox.Update)) *backend {
	folders := sharedfolder.New(sharedFolderRoot)
	hostTools, noHostTools := hosttool.Install(hostToolRoot)
	program, noCarrier := carrier.Install(dir)
	b := &backend{
		carrier:        program,
		noCarrier:      noCarrier,
		hostTools:      hostTools,
		noHostTools:    noHostTools,
		catalog:        catalog,
		store:          configstore.Open(dir),
		watchdog:       hostwatch.New(),
		folders:        folders,
		tell:           tell,
		runtime:        dashboard.Runtime{SharedFolderRoot: sharedFolderRoot},
		noSharedFolder: "The VM has not been asked yet whether it mounts the Shared Folders' root.",
		mounting:       map[string]int{},
	}
	// A physical exit address that was not known before goes to the
	// gatekeepers at once, not at the next heartbeat.
	b.watchdog.OnPhysicalExitChange(b.beatNow)
	return b
}

// connect reaches the Docker daemon, removes what an earlier run of the
// app left there, and checks what the VM mounts of the Mac: the Shared
// Folders' root, or nothing. It is slow the first time, when the
// gatekeeper's image is built.
func (b *backend) connect(ctx context.Context) {
	problem := ""
	noSharedFolder := "The runtime is not available."
	orch, err := orchestrator.New(ctx, orchestrator.ColimaEndpoint(), b.tell)
	switch {
	case err != nil:
		problem = "Sandboxes run in the Colima profile “zeolite”, which does not answer. Start it with: " + b.howToStartColima()
	default:
		// A root that could not be prepared has no marker, and no mount
		// is taken for it.
		marker, prepareErr := b.folders.Prepare()
		root := orchestrator.SharedFolderRoot{Path: b.folders.Root(), MarkerName: sharedfolder.MarkerName, MarkerToken: marker}
		// The Backstop is loaded whether or not an orphan stayed.
		orphans, backstop := orch.RemoveOrphans(ctx), orch.LoadBackstop(ctx)
		if orphans != nil {
			problem = "Removing what an earlier run left behind failed: " + orphans.Error()
		} else if backstop != nil {
			problem = "Loading the Backstop failed: " + backstop.Error()
		} else if exposed, mounted, err := orch.ExposedMacFolders(ctx, root); err != nil {
			problem = "Could not check what the VM sees of this Mac's files: " + err.Error()
		} else {
			exposed = withoutUsersRoot(exposed, b.folders.Root())
			if len(exposed) > 0 {
				problem = "The VM that runs sandboxes mounts " + strings.Join(exposed, ", ") + " of this Mac: anything that escaped a sandbox could read it. Recreate the Colima profile: " + b.howToStartColima()
			}
			switch {
			case prepareErr != nil:
				noSharedFolder = "The Shared Folders' root could not be prepared: " + prepareErr.Error()
			case !mounted:
				noSharedFolder = "The VM does not mount the Shared Folders' root, " + b.folders.Root() + ". Recreate the Colima profile: " + b.howToStartColima()
			default:
				noSharedFolder = ""
			}
		}
	}
	b.mu.Lock()
	b.orch = orch
	b.runtime.Problem = problem
	b.noSharedFolder = noSharedFolder
	b.mu.Unlock()
}

func (b *backend) orchestrator() (*orchestrator.Orchestrator, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.orch == nil {
		return nil, fmt.Errorf("the runtime is not available")
	}
	return b.orch, nil
}

// setSuspended tells the gates that the Mac is going to sleep, or woke.
// When it woke, the Host Watchdog first forgets what it read of this
// Mac, whether or not the runtime answers: it may have woken on another
// network. The heartbeats that resume then say that this Mac's physical
// exit addresses are not known, until they are read again.
func (b *backend) setSuspended(suspended bool) {
	if suspended {
		log.Print("this Mac is going to sleep: every gate is locked until it wakes")
	} else {
		b.watchdog.Forget()
		log.Print("this Mac woke: the Host Watchdog has forgotten what it read of it, and reads again")
	}
	if orch, err := b.orchestrator(); err == nil {
		orch.SetSuspended(suspended)
	}
}

// beatNow has every running sandbox's gatekeeper told at once what the
// Host Watchdog has just read.
func (b *backend) beatNow() {
	if orch, err := b.orchestrator(); err == nil {
		orch.BeatNow()
	}
}

func (b *backend) Packs() []pack.Manifest { return b.catalog.All() }

// Interfaces lists the interfaces of this Mac a sandbox could leave by
// now, each with what the system calls its port.
func (b *backend) Interfaces() []dashboard.Interface {
	var out []dashboard.Interface
	for _, ifc := range hostwatch.Interfaces() {
		out = append(out, dashboard.Interface{Name: ifc.Name, Title: ifc.Title})
	}
	return out
}

// carry starts a sandbox's Carrier on its Packet stream, under the
// Carrier Wall. It runs nothing of the agent's. What the agent asks to
// run on this Mac is a Host Tool, under the Host Tool Wall: those two
// are what the app starts for a sandbox.
func (b *backend) carry(name string) func(sandbox.CarrierConfig, io.ReadWriteCloser, func(sandbox.CarrierReport), func(error)) (orchestrator.Carrier, error) {
	return func(cfg sandbox.CarrierConfig, stream io.ReadWriteCloser, report func(sandbox.CarrierReport), ended func(error)) (orchestrator.Carrier, error) {
		if b.noCarrier != nil {
			return nil, b.noCarrier
		}
		return b.carrier.Start(cfg, stream, carrier.Events{
			Report: report,
			Said:   func(line string) { log.Printf("%s: %s", name, line) },
			Ended:  ended,
		})
	}
}

func (b *backend) Validate(cfg sandbox.Config) []sandbox.FieldError {
	return append(cfg.Validate(), b.catalog.Check(cfg)...)
}

func (b *backend) Save(configs []sandbox.Config) error { return b.store.Save(configs) }

func (b *backend) Runtime() dashboard.Runtime {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runtime
}

func (b *backend) Start(cfg sandbox.Config) error {
	orch, err := b.orchestrator()
	if err != nil {
		return err
	}
	packs := b.catalog.Enabled(cfg)
	conditions, gate := cfg.HostConditions, cfg.Gate
	opts := orchestrator.StartOptions{
		ConditionsMet: func() bool { return b.watchdog.Met(conditions) },
		// This Mac's physical exit addresses, as the sandbox's own probe URLs
		// see them: asked only while its Interface in use expects those,
		// or any exit address but those.
		PhysicalExits: func(beside string) ([]string, bool) { return b.watchdog.PhysicalExitAddresses(beside, gate) },
		Carry:         b.carry(cfg.Name),
	}
	if name := cfg.SharedFolder; name != "" {
		b.mu.Lock()
		why := b.noSharedFolder
		b.mu.Unlock()
		if why != "" {
			return errors.New(why)
		}
		b.folderMu.Lock()
		err := b.folders.Create(name)
		if err == nil {
			b.mounting[name]++
		}
		b.folderMu.Unlock()
		if err != nil {
			return err
		}
		// By the time the start returns, the orchestrator says whether
		// the sandbox mounts the folder.
		defer func() {
			b.folderMu.Lock()
			b.mounting[name]--
			b.folderMu.Unlock()
		}()
		opts.SharedFolder = b.folders.Path(name)
		if cfg.HostTools.Enabled && b.noHostTools != nil {
			return fmt.Errorf("the sandbox is given Host Tools, which cannot be run: %w", b.noHostTools)
		}
	}
	opts.Link = b.link(cfg, opts.SharedFolder)
	// The first start of a set of packs builds their image.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	return orch.Start(ctx, cfg, packs, opts)
}

// link serves a sandbox's Link: what its workstation asks of the app.
// An agent's raised hand is told to the dashboard. A sandbox that is
// given Host Tools has them run, each under the Host
// Tool Wall, for as long as the Link is open; one that is given none is
// refused. The app reads none of what a Host Tool printed: it passes it
// on.
func (b *backend) link(cfg sandbox.Config, sharedFolder string) func(io.ReadWriteCloser) {
	return func(stream io.ReadWriteCloser) {
		handlers := link.Handlers{Hand: func(r sandbox.HandRequest) string { return b.raiseHand(cfg.ID, r.Session) }}
		if cfg.HostTools.Enabled {
			settings := hosttool.Settings{SandboxID: cfg.ID, SharedFolder: sharedFolder, GPU: cfg.HostTools.GPU, MacName: cfg.HostTools.MacName}
			served, err := b.hostTools.Serve(settings)
			if err != nil {
				// Why is the app's to say to its user, here: an agent is
				// told what it can act on, and nothing of this Mac's.
				log.Printf("%s: its Host Tools cannot be run: %v", cfg.Name, err)
				refused := "no program can be run on the Mac now: your user has to look into it"
				handlers.HostTool = func(context.Context, sandbox.HostToolRequest) sandbox.HostToolResult {
					return sandbox.HostToolResult{Refused: refused}
				}
			} else {
				defer served.Close()
				handlers.HostTool = served.Run
			}
		}
		if err := link.Serve(stream, handlers); err != nil {
			log.Printf("%s: its Link ended: %v", cfg.Name, err)
		}
	}
}

// raiseHand takes note that the agent of a Terminal Session raised its
// hand, and tells the dashboard. The Link is trusted for nothing: a
// session the app did not open in that sandbox is refused, so that what
// runs in a workstation marks no session but one of its own sandbox's.
func (b *backend) raiseHand(sandboxID, sessionID string) (refused string) {
	orch, err := b.orchestrator()
	if err != nil || !orch.HasSession(sandboxID, sessionID) {
		return "this is not a session your user has open"
	}
	b.tell(sandbox.Update{SandboxID: sandboxID, Hand: &sandbox.Hand{Session: sessionID}})
	return ""
}

// SharedFolders lists the Shared Folders on this Mac's disk, by name.
func (b *backend) SharedFolders() ([]string, error) { return b.folders.Names() }

// RemoveSharedFolder deletes a Shared Folder from this Mac's disk. It
// is the one way one is deleted, and it refuses while a sandbox that is
// starting or running mounts the folder.
func (b *backend) RemoveSharedFolder(name string) error {
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	mounted := b.mounting[name] > 0
	if orch, err := b.orchestrator(); err == nil && orch.SharedFolderMounted(b.folders.Path(name)) {
		mounted = true
	}
	if mounted {
		return errors.New("a running sandbox mounts it: stop the sandboxes given this Shared Folder first")
	}
	return b.folders.Remove(name)
}

// ShowSharedFolder shows a Shared Folder in the Finder, in the folder
// that holds it.
func (b *backend) ShowSharedFolder(name string) {
	if sandbox.ValidSharedFolderName(name) {
		mygo.Shell.ShowItemInFolder(b.folders.Path(name))
	}
}

func (b *backend) Stop(sandboxID string) {
	if orch, err := b.orchestrator(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		orch.Stop(ctx, sandboxID)
		cancel()
	}
}

// RemoveHome deletes a sandbox's Home from the VM. A runtime that does
// not answer is an error: the Home may be there, and a sandbox that was
// forgotten would leave it with no name to find it by.
func (b *backend) RemoveHome(sandboxID string) error {
	orch, err := b.orchestrator()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return orch.RemoveHome(ctx, sandboxID)
}

// RemoveHostToolScratch deletes a sandbox's Host Tool Scratch from this
// Mac's disk. Where Host Tools cannot be run it is an error: a Scratch
// may be there from when they could.
func (b *backend) RemoveHostToolScratch(sandboxID string) error {
	if b.noHostTools != nil {
		return b.noHostTools
	}
	return b.hostTools.RemoveScratch(sandboxID)
}

// close stops every sandbox, as the app quits.
func (b *backend) close() {
	b.mu.Lock()
	orch := b.orch
	b.mu.Unlock()
	if orch != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		orch.Close(ctx)
		cancel()
	}
}

// OpenTerminalSession starts a login shell in a sandbox's workstation.
// The app installs no agent there and starts none: its user does both,
// in that shell.
func (b *backend) OpenTerminalSession(sandboxID string) (dashboard.TerminalConn, error) {
	orch, err := b.orchestrator()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return orch.OpenTerminalSession(ctx, sandboxID)
}

func (b *backend) OpenViewport(sandboxID string, p pack.Manifest, tab string) (dashboard.ViewportStream, error) {
	orch, err := b.orchestrator()
	if err != nil {
		return nil, err
	}
	port := p.ViewportStream.Port
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		return orch.DialWorkstation(ctx, sandboxID, port)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return viewport.OpenCDP(ctx, dial, port, tab)
}

func (b *backend) OpenBrowserTab(sandboxID string, p pack.Manifest, address string) (string, error) {
	orch, err := b.orchestrator()
	if err != nil {
		return "", err
	}
	port := p.ViewportStream.Port
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		return orch.DialWorkstation(ctx, sandboxID, port)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return viewport.NewTab(ctx, dial, port, address)
}

func (b *backend) BrowserTabs(sandboxID string, p pack.Manifest) ([]viewport.BrowserTab, error) {
	orch, err := b.orchestrator()
	if err != nil {
		return nil, err
	}
	port := p.ViewportStream.Port
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		return orch.DialWorkstation(ctx, sandboxID, port)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return viewport.Tabs(ctx, dial, port)
}
