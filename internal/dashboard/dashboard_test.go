package dashboard

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"image"
	"io"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/viewport"
)

// fakeBackend stands in for Docker and the rest of the app.
type fakeBackend struct {
	mu      sync.Mutex
	packs   []pack.Manifest
	saved   [][]sandbox.Config
	started []sandbox.Config
	stopped []string
	// folders are the Shared Folders on the disk, removed the ones that
	// were deleted, shown the ones shown in the Finder, and removeErr
	// what deleting one answers.
	folders   []string
	removed   []string
	shown     []string
	removeErr error
	// did is what was asked of the backend that changes something, in
	// order: "stop s1", "remove-home s1", "remove-scratch s1", "remove
	// research", "save".
	did      []string
	startErr error
	// homeErr is what deleting a sandbox's Home answers, and scratchErr
	// what deleting its Host Tool Scratch does.
	homeErr    error
	scratchErr error
	// tell, where a test sets it, is told that a sandbox stopped when
	// it is stopped, as the orchestrator tells the app: before Stop
	// returns.
	tell    func(sandbox.Update)
	runtime Runtime
	// conns are the Terminal Sessions that were opened, in order, and
	// openErr what opening one answers.
	conns   []*fakeConn
	openErr error
	streams []*fakeStream
	// browserTabs are the Browser Tabs the packs' browser lists, and
	// links the addresses a new one was opened at, in order; tabErr is
	// what opening one answers.
	browserTabs []viewport.BrowserTab
	links       []string
	tabErr      error
	// selected is what the browser's page says is selected in it, and
	// closeErr what closing a Browser Tab answers.
	selected string
	closeErr error
}

func (b *fakeBackend) Packs() []pack.Manifest { return b.packs }

// Interfaces are the interfaces of the fake Mac.
func (b *fakeBackend) Interfaces() []Interface {
	return []Interface{
		{Name: "en0", Title: "Wi-Fi"},
		{Name: "utun4"},
		{Name: "en7", Title: "USB 10/100/1000 LAN"},
	}
}

func (b *fakeBackend) Validate(cfg sandbox.Config) []sandbox.FieldError { return cfg.Validate() }

func (b *fakeBackend) Save(configs []sandbox.Config) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.saved = append(b.saved, configs)
	b.did = append(b.did, "save")
	return nil
}

func (b *fakeBackend) Start(cfg sandbox.Config) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = append(b.started, cfg)
	// A start makes the Shared Folder the sandbox is given.
	if name := cfg.SharedFolder; name != "" && b.startErr == nil && !slices.Contains(b.folders, name) {
		b.folders = append(b.folders, name)
		slices.Sort(b.folders)
	}
	return b.startErr
}

func (b *fakeBackend) Stop(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = append(b.stopped, id)
	b.did = append(b.did, "stop "+id)
	if b.tell != nil {
		b.tell(sandbox.Update{SandboxID: id, Status: &sandbox.Status{State: sandbox.StateStopped}})
	}
}

func (b *fakeBackend) RemoveHome(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.did = append(b.did, "remove-home "+id)
	return b.homeErr
}

func (b *fakeBackend) RemoveHostToolScratch(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.did = append(b.did, "remove-scratch "+id)
	return b.scratchErr
}

func (b *fakeBackend) SharedFolders() ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.folders...), nil
}

func (b *fakeBackend) RemoveSharedFolder(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.did = append(b.did, "remove "+name)
	if b.removeErr != nil {
		return b.removeErr
	}
	b.removed = append(b.removed, name)
	b.folders = slices.DeleteFunc(b.folders, func(other string) bool { return other == name })
	return nil
}

func (b *fakeBackend) ShowSharedFolder(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.shown = append(b.shown, name)
}

// wasShown and wasRemoved are the Shared Folders shown in the Finder
// and deleted, in order.
func (b *fakeBackend) wasShown() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.shown, " ")
}

func (b *fakeBackend) wasRemoved() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.removed, " ")
}

// done is what was asked of the backend after its first n askings.
func (b *fakeBackend) done(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.did[n:]...)
}

// OpenTerminalSession starts a Terminal Session whose shell is the
// test: the connection is one of the fake's, which names the sandbox it
// was opened in and has an ID of its own.
func (b *fakeBackend) OpenTerminalSession(sandboxID string) (TerminalConn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openErr != nil {
		return nil, b.openErr
	}
	c := newFakeConn()
	c.sandbox, c.id = sandboxID, "session-"+strconv.Itoa(len(b.conns)+1)
	b.conns = append(b.conns, c)
	return c, nil
}

// opened are the Terminal Sessions that were opened so far, in order.
func (b *fakeBackend) opened() []*fakeConn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*fakeConn(nil), b.conns...)
}

func (b *fakeBackend) OpenViewport(_ string, _ pack.Manifest, tab string) (ViewportStream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if tab == "" && len(b.browserTabs) > 0 {
		tab = b.browserTabs[0].ID
	}
	s := &fakeStream{closed: make(chan struct{}), tab: tab, backend: b}
	b.streams = append(b.streams, s)
	return s, nil
}

// OpenBrowserTab opens a Browser Tab in the fake browser, which lists
// it from then on, the one last opened first.
func (b *fakeBackend) OpenBrowserTab(sandboxID string, _ pack.Manifest, address string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.links = append(b.links, sandboxID+" "+address)
	if b.tabErr != nil {
		return "", b.tabErr
	}
	tab := viewport.BrowserTab{ID: "link" + strconv.Itoa(len(b.links)), Title: "Sign in", Address: address}
	b.browserTabs = append([]viewport.BrowserTab{tab}, b.browserTabs...)
	return tab.ID, nil
}

// linksOpened are the sandboxes and addresses a Browser Tab was opened
// at, in order.
func (b *fakeBackend) linksOpened() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.links...)
}

// opens are the Viewport Streams that were opened so far, in order.
func (b *fakeBackend) opens() []*fakeStream {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*fakeStream(nil), b.streams...)
}

// browse sets the Browser Tabs the browser lists.
func (b *fakeBackend) browse(tabs ...viewport.BrowserTab) {
	b.mu.Lock()
	b.browserTabs = tabs
	b.mu.Unlock()
}

func (b *fakeBackend) Runtime() Runtime { return b.runtime }

func (b *fakeBackend) lastSaved() []sandbox.Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.saved) == 0 {
		return nil
	}
	return b.saved[len(b.saved)-1]
}

// fakeConn is a Terminal Session whose shell is the test.
type fakeConn struct {
	// sandbox is the sandbox the session was opened in, and id its ID
	// there.
	sandbox, id string
	fromShell   *io.PipeReader
	shell       *io.PipeWriter
	mu          sync.Mutex
	typed       bytes.Buffer
	closed      bool
}

func newFakeConn() *fakeConn {
	r, w := io.Pipe()
	return &fakeConn{fromShell: r, shell: w}
}

func (c *fakeConn) ID() string { return c.id }

func (c *fakeConn) Read(p []byte) (int, error) { return c.fromShell.Read(p) }

func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.typed.Write(p)
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.shell.Close()
	return nil
}

func (c *fakeConn) Resize(int, int) error { return nil }

func (c *fakeConn) state() (typed string, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.typed.String(), c.closed
}

// fakeStream gives a frame each time one is asked for, and counts.
type fakeStream struct {
	mu     sync.Mutex
	asked  int
	listed int
	inputs []string
	closed chan struct{}
	once   sync.Once
	// tab is the Browser Tab it shows, of the backend's browser.
	tab     string
	backend *fakeBackend
}

func (s *fakeStream) Tab() string { return s.tab }

func (s *fakeStream) Tabs(context.Context) ([]viewport.BrowserTab, error) {
	s.mu.Lock()
	s.listed++
	s.mu.Unlock()
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	return append([]viewport.BrowserTab(nil), s.backend.browserTabs...), nil
}

func (s *fakeStream) Navigate(address string) error { return s.record("navigate " + address) }
func (s *fakeStream) Edit(command viewport.EditCommand) error {
	return s.record("edit " + string(command))
}

// Selection is what the fake browser's page says is selected in it.
func (s *fakeStream) Selection(context.Context) (string, error) {
	s.record("selection")
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	return s.backend.selected, nil
}

// CloseTab closes the stream's Browser Tab in the fake browser, which
// lists it no more, and ends the stream; or answers closeErr.
func (s *fakeStream) CloseTab(context.Context) error {
	s.record("close tab")
	s.backend.mu.Lock()
	err := s.backend.closeErr
	if err == nil {
		s.backend.browserTabs = slices.DeleteFunc(s.backend.browserTabs, func(t viewport.BrowserTab) bool { return t.ID == s.tab })
	}
	s.backend.mu.Unlock()
	if err == nil {
		s.Close()
	}
	return err
}
func (s *fakeStream) Back(context.Context) error    { return s.record("back") }
func (s *fakeStream) Forward(context.Context) error { return s.record("forward") }

func (s *fakeStream) timesListed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listed
}

func (s *fakeStream) Next(ctx context.Context) (viewport.Frame, error) {
	s.mu.Lock()
	s.asked++
	s.mu.Unlock()
	select {
	case <-s.closed:
		return viewport.Frame{}, errors.New("closed")
	case <-ctx.Done():
		return viewport.Frame{}, ctx.Err()
	default:
	}
	return viewport.Frame{Image: image.NewYCbCr(image.Rect(0, 0, 640, 360), image.YCbCrSubsampleRatio420), PageWidth: 1280, PageHeight: 720}, nil
}

func (s *fakeStream) record(what string) error {
	s.mu.Lock()
	s.inputs = append(s.inputs, what)
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) Pointer(e viewport.PointerEvent, x, y float64, button, clicks int, _ viewport.Modifiers) error {
	// To the nearest point of the page: the view's box may be a fraction
	// of a point wide.
	return s.record(strings.Join([]string{string(e), strconv.Itoa(int(math.Round(x))), strconv.Itoa(int(math.Round(y)))}, " "))
}
func (s *fakeStream) Scroll(x, y, dx, dy float64, _ viewport.Modifiers) error {
	return s.record("scroll")
}
func (s *fakeStream) Text(text string) error { return s.record("text " + text) }
func (s *fakeStream) Compose(text string, caret int) error {
	return s.record("compose " + text + " " + strconv.Itoa(caret))
}
func (s *fakeStream) PressKey(k viewport.Key, down bool, _ viewport.Modifiers) error {
	return s.record("key " + string(k))
}
func (s *fakeStream) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func (s *fakeStream) timesAsked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

// inputsSince are the inputs the stream got after its first n.
func (s *fakeStream) inputsSince(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.inputs[min(n, len(s.inputs)):]...)
}

func (s *fakeStream) sawInput(what string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range s.inputs {
		if in == what {
			return true
		}
	}
	return false
}

// harness runs a dashboard without a window.
type harness struct {
	t       *testing.T
	backend *fakeBackend
	d       *Dashboard
	tt      *ui.Tester
	queue   chan func()
}

func newHarness(t *testing.T, configs []sandbox.Config, packs ...pack.Manifest) *harness {
	h := &harness{t: t, backend: &fakeBackend{
		packs:   packs,
		runtime: Runtime{SharedFolderRoot: "/Users/Shared/zeolite/shared.noindex"},
	}, queue: make(chan func(), 256)}
	// What goroutines send to the main thread waits here until the test,
	// which is the main thread, takes it.
	h.d = New(h.backend, configs, func(fn func()) { h.queue <- fn })
	// Tall enough for the whole Setup: a tester clicks only
	// what is in view.
	h.tt = ui.NewTester(h.d.View, 1200, 2000)
	t.Cleanup(h.d.Close)
	return h
}

// settle runs what was sent to the main thread and builds frames, until
// ok or a deadline.
func (h *harness) settle(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for drained := false; !drained; {
			select {
			case fn := <-h.queue:
				fn()
			default:
				drained = true
			}
		}
		h.tt.Frame()
		h.rules()
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; the window shows %q", what, h.tt.Texts())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) click(label string) {
	h.t.Helper()
	if err := h.tt.Click(label); err != nil {
		h.t.Fatalf("clicking %q: %v; the window shows %q", label, err, h.tt.Texts())
	}
	h.tt.Frame()
}

// rules checks what holds of every sandbox's Panes and sessions at any
// moment: a sandbox that runs has one Pane to four, and one that does
// not has none, nor a session; a session is shown in one Pane at most,
// and one that is in none is an Agent Session.
func (h *harness) rules() {
	h.t.Helper()
	for id, v := range h.d.views {
		if !running(v.status.State) {
			if len(v.panes) != 0 || len(v.sessions) != 0 {
				h.t.Fatalf("%s does not run and has %d Panes and %d sessions", id, len(v.panes), len(v.sessions))
			}
			continue
		}
		if len(v.panes) < 1 || len(v.panes) > maxPanes {
			h.t.Fatalf("%s runs with %d Panes, want one to %d", id, len(v.panes), maxPanes)
		}
		shown := map[*session]int{}
		for _, p := range v.panes {
			if p.session == nil {
				continue
			}
			if shown[p.session]++; shown[p.session] > 1 {
				h.t.Fatalf("a session of %s is shown in two Panes", id)
			}
			if !slices.Contains(v.sessions, p.session) {
				h.t.Fatalf("a Pane of %s shows a session the sandbox does not hold", id)
			}
		}
		for _, s := range v.sessions {
			if shown[s] == 0 && s.agent == "" {
				h.t.Fatalf("a session of %s is Hidden, and is no Agent Session", id)
			}
		}
	}
}

// open is the status of a sandbox whose gate is open.
var open = sandbox.Status{State: sandbox.StateGateOpen, ExitAddress: "203.0.113.7"}

// runs has sandboxes run with their gates open, as a start leaves them,
// each with the login shell of its first Pane.
func (h *harness) runs(ids ...string) {
	h.t.Helper()
	for _, id := range ids {
		if h.d.view(id) == nil {
			h.t.Fatalf("there is no sandbox %s", id)
		}
		h.d.Apply(sandbox.Update{SandboxID: id, Status: &open})
	}
	h.settle("the sandboxes' first Panes", func() bool {
		for _, id := range ids {
			if v := h.d.view(id); len(v.panes) == 0 || v.panes[0].session == nil {
				return false
			}
		}
		return true
	})
}

// connOf is the connection of the session a Pane of a sandbox shows.
func (h *harness) connOf(id string, pane int) *fakeConn {
	h.t.Helper()
	v := h.d.view(id)
	if pane >= len(v.panes) || v.panes[pane].session == nil {
		h.t.Fatalf("Pane %d of %s shows no session", pane+1, id)
	}
	for _, c := range h.backend.opened() {
		if c.id == v.panes[pane].session.id {
			return c
		}
	}
	h.t.Fatalf("the session of Pane %d of %s was not opened by the backend", pane+1, id)
	return nil
}

// rest rests the pointer on what shows label, or is labelled so, until
// its tooltip shows. The pointer comes to it from elsewhere: a press on
// an element hides its tooltip until the pointer has left it.
func (h *harness) rest(label string) {
	h.t.Helper()
	h.tt.Move(1, 1)
	h.tt.Frame()
	at, ok := h.tt.Find(label)
	if !ok {
		h.t.Fatalf("nothing shows %q to rest the pointer on; the window shows %q", label, h.tt.Texts())
	}
	h.tt.Move(at.X+at.W/2, at.Y+at.H/2)
	h.tt.Frame()
	h.tt.Advance(700 * time.Millisecond)
	h.tt.Frame()
	h.tt.Frame()
}

// park takes the pointer off everything that has something to say:
// what it rested on says nothing more.
func (h *harness) park() {
	h.tt.Move(1, 1)
	h.tt.Frame()
}

// told is what the window shows while the pointer rests on what shows
// label: its tooltip among the rest. The pointer leaves again.
func (h *harness) told(label string) string {
	h.t.Helper()
	h.rest(label)
	texts := strings.Join(h.tt.Texts(), "|")
	h.park()
	return texts
}

// facts are what the Card of the sandbox of this name says of it to
// whoever rests the pointer on it: each fact's label, a bar, and its
// value, from its state on.
func (h *harness) facts(name string) string {
	h.t.Helper()
	texts := h.told(name)
	from := strings.LastIndex(texts, "|State|")
	if from < 0 {
		h.t.Fatalf("the Card of %s says no facts; the window shows %s", name, texts)
	}
	return texts[from+1:]
}

// replace replaces the text of the field with this label.
func (h *harness) replace(label, text string) {
	h.t.Helper()
	h.click(label)
	h.tt.Key(ui.Cmd, ui.KeyA)
	h.tt.Type(text)
	h.tt.Frame()
}

// find chooses, in the combobox with this label, the first of its
// choices that what is typed finds.
func (h *harness) find(label, typed string) {
	h.t.Helper()
	h.replace(label, typed)
	h.tt.Key(0, ui.KeyEnter)
	h.tt.Frame()
	h.tt.Frame()
}

// The ways out the fake Mac has, as the form names them, in the order it
// offers them.
const (
	wayWiFi   = "Wi-Fi · en0"
	wayTunnel = "utun4"
	wayUSB    = "USB 10/100/1000 LAN · en7"
)

var ways = []string{wayWiFi, wayTunnel, wayUSB}

// count is how many of the window's texts are exactly this one.
func (h *harness) count(text string) int {
	n := 0
	for _, other := range h.tt.Texts() {
		if other == text {
			n++
		}
	}
	return n
}

// choose makes a choice in the control labelled label: it opens with a
// click and the choice is made with the keys, from the first of the
// options down to the one wanted.
func (h *harness) choose(label string, options []string, want string) {
	h.t.Helper()
	h.click(label)
	h.settle("the choices of "+label, func() bool {
		for _, option := range options {
			if h.count(option) == 0 {
				return false
			}
		}
		return true
	})
	h.tt.Key(0, ui.KeyHome)
	for _, option := range options {
		if option == want {
			break
		}
		h.tt.Key(0, ui.KeyDown)
	}
	h.tt.Key(0, ui.KeyEnter)
	h.tt.Frame()
	h.tt.Frame()
}

// gated is a valid config.
func gated(id, name string) sandbox.Config {
	cfg := sandbox.NewConfig(id, name)
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7"}}}
	cfg.Gate.ProbeURLs = []string{"https://probe.example.com/ip"}
	return cfg
}

// browserWaits is what the viewport of browserPack's pack says until its
// daemon is ready.
const browserWaits = "Waiting for Web Test Browser to be ready…"

func browserPack(t *testing.T) pack.Manifest {
	m, err := pack.Parse([]byte("name: web-test-browser\ntitle: Web Test Browser\ndescription: Headless Chromium.\ndaemonProcess: {command: [chromium], readinessPort: 9222}\nviewportStream: {type: cdp_screencast, port: 9222}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEmptyDashboardOffersANewSandbox(t *testing.T) {
	h := newHarness(t, nil)
	if !h.tt.HasText("No sandboxes yet") {
		t.Fatalf("an empty dashboard shows %q", h.tt.Texts())
	}
	h.click("New Sandbox")
	h.settle("the new sandbox's Setup", func() bool { return h.tt.HasText("Leaves by") })
	if len(h.d.configs) != 1 || h.d.configs[0].Name != "Sandbox 1" || h.d.configs[0].Gate.Resolver != sandbox.DefaultResolver {
		t.Errorf("configs %+v", h.d.configs)
	}
	h.settle("the new sandbox to be saved", func() bool { return len(h.backend.lastSaved()) == 1 })
}

func TestRuntimeProblemIsShown(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.backend.runtime.Problem = "The Docker daemon mounts /Users: sandboxes could reach your files."
	h.tt.Frame()
	if !h.tt.HasText("The Docker daemon mounts /Users: sandboxes could reach your files.") {
		t.Errorf("the runtime's problem is not shown: %q", h.tt.Texts())
	}
}

func TestSetupShowsErrorsOnTheirFieldsAndRefusesToSave(t *testing.T) {
	// A timezone that was saved and is none: the form shows it as it is.
	cfg := gated("s1", "Research")
	cfg.Clock.Timezone = "Mars/Olympus"
	h := newHarness(t, []sandbox.Config{cfg})
	saves := len(h.backend.saved)

	h.replace("Resolver", "192.168.5.3")
	h.click("Save")
	for _, want := range []string{
		"enter the resolver as a public IPv4 address such as 1.1.1.1",
		`"Mars/Olympus" is not an IANA timezone name`,
		"Not saved: see the fields marked above.",
	} {
		if !h.tt.HasText(want) {
			t.Errorf("the form does not show %q; it shows %q", want, h.tt.Texts())
		}
	}
	if len(h.backend.saved) != saves || h.d.configs[0].Gate.Resolver != sandbox.DefaultResolver {
		t.Error("a config with errors was saved")
	}

	h.replace("Resolver", "9.9.9.9")
	h.find("Timezone", "berlin")
	h.replace("Name", "研究")
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+1 })
	got := h.backend.lastSaved()[0]
	if got.Gate.Resolver != "9.9.9.9" || got.Clock.Timezone != "Europe/Berlin" || got.Name != "研究" || got.ID != "s1" {
		t.Errorf("saved %+v", got)
	}
	if !h.tt.HasText("Saved.") || h.tt.HasText("Not saved: see the fields marked above.") {
		t.Errorf("after a good save the form shows %q", h.tt.Texts())
	}
}

// The exit addresses expected at a way out are typed, or taken from what
// the sandbox's own gate saw there: the field offers those, expected or
// not, and nothing the gate did not see, nor what a config cannot
// expect, nor what another way out was seen at.
func TestTheGatesAddressesAreOfferedAmongThoseExpected(t *testing.T) {
	cfg := gated("s1", "Research")
	cfg.AllowedInterfaces = append(cfg.AllowedInterfaces, sandbox.AllowedInterface{Name: "utun4", AnyExitAddress: true})
	h := newHarness(t, []sandbox.Config{cfg})
	v := h.d.view("s1")
	const exits = "Expected exit addresses of way out 1"
	offered := func() string {
		texts := h.tt.Texts()
		from := slices.Index(texts, seenByGate)
		if from < 0 {
			return ""
		}
		var addresses []string
		for _, text := range texts[from+1:] {
			if _, err := sandbox.ParsePublicIPv4(text); err != nil {
				break
			}
			addresses = append(addresses, text)
		}
		return strings.Join(addresses, " ")
	}
	open := func() {
		t.Helper()
		h.click("Name")
		h.click(exits)
		h.tt.Frame()
	}

	// Nothing was seen: the field is one to type in, and offers nothing.
	open()
	if h.tt.HasText(seenByGate) {
		t.Fatalf("before the gate saw anything the field offers %q", h.tt.Texts())
	}

	// The gate says what it sees: an address that is expected, one that
	// is not, for which it is locked, and what no config may expect.
	saw := func(iface, address string) {
		h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{Interface: iface, ExitAddress: address}})
	}
	saw("en0", "203.0.113.7")
	saw("en0", "198.51.100.9")
	saw("en0", "2001:db8::1")
	saw("en0", "10.0.0.1")
	saw("en0", "")
	saw("", "198.51.100.200")
	saw("utun4", "198.51.100.44")
	saw("en0", "198.51.100.9")
	open()
	if got := offered(); got != "198.51.100.9" {
		t.Fatalf("with 203.0.113.7 listed, the field offers %q: %q", got, h.tt.Texts())
	}
	// One that is taken stands after the ones listed, and is offered no
	// longer.
	h.tt.Key(0, ui.KeyDown)
	h.tt.Key(0, ui.KeyEnter)
	h.tt.Frame()
	if got := v.draft.interfaces[0].exits; got != "203.0.113.7, 198.51.100.9" {
		t.Fatalf("after the address seen was taken the row expects %q", got)
	}
	open()
	if h.tt.HasText(seenByGate) {
		t.Errorf("with every address seen listed the field offers %q", h.tt.Texts())
	}

	// While an address is typed, those that begin as it does are
	// offered, the latest first, and Enter takes the first in the place
	// of what was typed.
	saw("en0", "198.51.100.77")
	h.replace(exits, "203.0.113.7, 198")
	if got := offered(); got != "198.51.100.77 198.51.100.9" {
		t.Fatalf("with 198 typed the field offers %q: %q", got, h.tt.Texts())
	}
	h.tt.Key(0, ui.KeyEnter)
	h.tt.Frame()
	if got := v.draft.interfaces[0].exits; got != "203.0.113.7, 198.51.100.77" {
		t.Fatalf("after Enter the row expects %q", got)
	}
	// What is typed next stands after it.
	h.tt.Type(" 192.0.2.5")
	h.tt.Frame()
	if got := v.draft.interfaces[0].exits; got != "203.0.113.7, 198.51.100.77 192.0.2.5" {
		t.Errorf("typed after an address was taken, the row expects %q", got)
	}

	// What was seen is still offered once the sandbox stopped, and one is
	// taken with the pointer as with the keys. What was typed by hand is
	// the sandbox's as what was taken is.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	open()
	if got := offered(); got != "198.51.100.9" {
		t.Errorf("after a stop the field offers %q: %q", got, h.tt.Texts())
	}
	h.click("198.51.100.9")
	h.tt.Frame()
	if got := v.draft.interfaces[0].exits; got != "203.0.113.7, 198.51.100.77, 192.0.2.5, 198.51.100.9" || h.tt.HasText(seenByGate) {
		t.Fatalf("after a click on the address offered the row expects %q: %q", got, h.tt.Texts())
	}
	saves := len(h.backend.saved)
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+1 })
	if got := h.backend.lastSaved()[0].AllowedInterfaces[0].ExpectedExitAddresses; !slices.Equal(got, []string{"203.0.113.7", "198.51.100.77", "192.0.2.5", "198.51.100.9"}) {
		t.Errorf("saved the expected exit addresses %q", got)
	}

	// No more than the latest few are kept of a way out whose exit
	// address keeps changing.
	for i := range 20 {
		saw("utun4", "198.51.100."+strconv.Itoa(100+i))
	}
	if got := v.seenExits["utun4"]; len(got) != maxSeenExits || got[0] != "198.51.100.119" {
		t.Errorf("of 21 addresses seen at one way out, %q are kept", got)
	}
}

// Every field of a sandbox's resources that has something to know says
// it behind its glyph: /dev/shm what it is for, and that it is memory.
func TestSharedMemorySaysWhatItIs(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	told := h.told("About /dev/shm (MB)")
	for _, want := range []string{"Chromium and 3D engines crash with less than 2048 MB", "counts against the sandbox's memory"} {
		if !strings.Contains(told, want) {
			t.Errorf("/dev/shm does not say %q: %s", want, told)
		}
	}
}

// A sandbox's clock is this Mac's, and its Setup asks for nothing but
// the timezone it reads in: one of those offered, found by typing, or
// that of the exit address, which asks for its probe.
func TestTheTimezoneIsChosenAmongThoseOffered(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	v := h.d.view("s1")
	for _, gone := range []string{"Real clock", "Start at (UTC)", "Timezone probe"} {
		if h.tt.HasText(gone) {
			t.Errorf("the Setup of a new sandbox shows %q", gone)
		}
	}
	// Opened, it shows them all, that of the exit address first.
	h.click("Timezone")
	h.tt.Key(0, ui.KeyDown)
	h.settle("the timezones", func() bool { return h.count(timezoneOfExit) == 1 && h.count("Africa/Abidjan") == 1 })
	h.tt.Key(0, ui.KeyEscape)
	h.tt.Frame()

	h.find("Timezone", "tokyo")
	if v.draft.timezone != "Asia/Tokyo" {
		t.Fatalf("typing tokyo and taking the first chose %q", v.draft.timezone)
	}
	// What was typed and is no timezone is not kept.
	h.replace("Timezone", "Mars/Olympus")
	h.click("Name")
	h.tt.Frame()
	if v.draft.timezone != "Asia/Tokyo" {
		t.Errorf("typing a name that is no timezone's left %q", v.draft.timezone)
	}

	h.find("Timezone", "exit address")
	h.settle("the timezone probe's field", func() bool { return h.tt.HasText("Timezone probe") })
	h.click("Save")
	if !h.tt.HasText("enter an https:// URL") {
		t.Errorf("with no timezone probe the form shows %q", h.tt.Texts())
	}
	h.replace("Timezone probe URL", "https://probe.example.com/timezone")
	h.click("Save")
	h.settle("the config to be saved", func() bool {
		s := h.backend.lastSaved()
		return len(s) == 1 && s[0].Clock == sandbox.ClockPolicy{Timezone: sandbox.TimezoneAuto, TimezoneProbeURL: "https://probe.example.com/timezone"}
	})
	// Shown again, it is still that of the exit address.
	if again := newDraft(h.backend.lastSaved()[0], nil); again.timezone != timezoneOfExit {
		t.Errorf("a sandbox saved with the exit address's timezone shows %q", again.timezone)
	}
}

func TestNameTakesComposedText(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "")})
	h.click("Name")
	h.tt.Compose("yan'jiu", 7)
	h.tt.Frame()
	if got := h.d.view("s1").draft.name; got != "" {
		t.Errorf("the name is %q while composing", got)
	}
	h.tt.Type("研究")
	h.tt.Frame()
	if got := h.d.view("s1").draft.name; got != "研究" {
		t.Errorf("the name is %q after the composition was committed", got)
	}
	h.tt.Compose("shi", 3)
	h.tt.Compose("", 0)
	h.tt.Type("室")
	h.tt.Frame()
	if got := h.d.view("s1").draft.name; got != "研究室" {
		t.Errorf("the name is %q after a canceled and a committed composition", got)
	}
}

func TestCardsFollowTheSandboxes(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research"), gated("s2", "Scraper")})
	if n := strings.Count(strings.Join(h.tt.Texts(), "|"), "STOPPED"); n < 2 {
		t.Fatalf("two stopped sandboxes show %q", h.tt.Texts())
	}
	h.d.Apply(sandbox.Update{SandboxID: "s2", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockExitAddressMismatch, ExitAddress: "198.51.100.9"}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateOpen, ExitAddress: "203.0.113.7"}})
	h.tt.Frame()
	for _, want := range []string{"GATE OPEN", "GATE LOCKED", "GATE OPEN [203.0.113.7]"} {
		if !h.tt.HasText(want) {
			t.Errorf("the window lacks %q: %q", want, h.tt.Texts())
		}
	}
	// The other sandbox's page says why its gate is locked.
	h.click("Scraper")
	if !h.tt.HasText("GATE LOCKED [Exit Address Mismatch]") {
		t.Errorf("the locked sandbox's page shows %q", h.tt.Texts())
	}
	// News of a sandbox that no longer exists is ignored.
	h.d.Apply(sandbox.Update{SandboxID: "gone", Status: &sandbox.Status{State: sandbox.StateGateOpen}})
}

func TestStartStopCloneDestroy(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	// A sandbox that does not run shows its Setup, and its Header has
	// no control that would show it.
	if !h.tt.HasText(partNetwork) || h.tt.HasText("Pane 1") || h.tt.HasText("Setup") || h.tt.HasText("Stop") {
		t.Fatalf("a stopped sandbox shows %q", h.tt.Texts())
	}
	h.click("Start")
	if !h.tt.HasText("STARTING") || !h.tt.HasText("Starting…") || h.tt.HasText(partNetwork) || h.count("Start") != 0 {
		t.Errorf("after Start the window shows %q", h.tt.Texts())
	}
	h.settle("the backend to be asked to start", func() bool {
		h.backend.mu.Lock()
		defer h.backend.mu.Unlock()
		return len(h.backend.started) == 1 && h.backend.started[0].ID == "s1"
	})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &open})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{Open: true, ExitAddress: "203.0.113.7", ProbeLatency: 42 * time.Millisecond, PacketsPassed: 1234, PacketsRejected: 5}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Clock: "2026-11-10T09:00:00+09:00"}})
	// Running, it shows one Pane with a login shell, opened once: more
	// news of its gate opens no other.
	v := h.d.view("s1")
	h.settle("the first Pane's shell", func() bool { return h.tt.HasText("Pane 1") && len(v.panes) == 1 && v.panes[0].session != nil })
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &open})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockProbeUnanswered}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &open})
	h.settle("a frame", func() bool { return true })
	if conns := h.backend.opened(); len(conns) != 1 || conns[0].sandbox != "s1" || v.panes[0].session.id != conns[0].id {
		t.Errorf("the start opened %d sessions, want one login shell in the sandbox", len(conns))
	}
	// What its gate and its workstation do, its Card says.
	facts := h.facts("Research")
	for _, want := range []string{"State|GATE OPEN [203.0.113.7]", "Exit address|203.0.113.7", "Probe latency|42 ms", "Packets|1234 passed, 5 rejected", "Sandbox clock|2026-11-10T09:00:00+09:00"} {
		if !strings.Contains(facts, want) {
			t.Errorf("the Card's facts lack %q: %s", want, facts)
		}
	}
	if strings.Contains(h.told("Research"), "|Agents|") {
		t.Errorf("a Card names an agent, and none runs in its sandbox: %s", h.told("Research"))
	}

	h.click("Stop")
	h.settle("the sandbox to stop", func() bool { return h.tt.HasText(partNetwork) && !h.tt.HasText("Pane 1") })
	if len(h.backend.stopped) != 1 {
		t.Errorf("stopped %v", h.backend.stopped)
	}
	if _, closed := h.backend.opened()[0].state(); !closed {
		t.Error("the stop left the Pane's session open")
	}

	// A start that fails says why, and the sandbox's page is its Setup.
	h.backend.startErr = errors.New("the gate is not in place")
	h.click("Start")
	h.settle("the failure", func() bool { return h.tt.HasText("the gate is not in place") && h.tt.HasText(partNetwork) })
	h.backend.startErr = nil

	// A clone has a Home of its own.
	h.click("Clone")
	if len(h.d.configs) != 2 || h.d.configs[1].Name != "Research copy" || h.d.configs[1].ID == "s1" {
		t.Fatalf("after Clone the configs are %+v", h.d.configs)
	}
	h.settle("the clone to be shown", func() bool { return h.tt.HasText("Research copy") })

	h.click("Destroy")
	h.settle("the question", func() bool { return h.tt.HasText("Destroy “Research copy”?") })
	h.click("Cancel")
	if len(h.d.configs) != 2 {
		t.Fatal("Cancel destroyed the sandbox")
	}
	h.click("Destroy")
	h.settle("the question", func() bool { return h.tt.HasText("Destroy “Research copy”?") })
	if err := h.tt.Click("Destroy"); err != nil {
		// Two elements say Destroy now: the button under the alert and
		// the alert's own, which has the focus.
		h.tt.Key(0, ui.KeyEnter)
	}
	h.settle("the sandbox to be destroyed", func() bool { return len(h.d.configs) == 1 })
	if h.d.configs[0].ID != "s1" {
		t.Errorf("the wrong sandbox was destroyed: %+v", h.d.configs)
	}
}

// Start saves the Setup first, and starts nothing whose Setup has
// something wrong in it: the form says what, on its field.
func TestStartRefusesAnInvalidConfig(t *testing.T) {
	cfg := gated("s1", "Research")
	cfg.Gate.ProbeURLs = nil
	h := newHarness(t, []sandbox.Config{cfg})
	h.click("Start")
	if len(h.backend.started) != 0 || len(h.backend.saved) != 0 {
		t.Fatal("an invalid config was saved or started")
	}
	if !h.tt.HasText("enter a probe URL") || !h.tt.HasText("Not saved: see the fields marked above.") {
		t.Errorf("the window does not say what is wrong: %q", h.tt.Texts())
	}
	// Check starts nothing either.
	h.click("Check")
	if len(h.backend.started) != 0 {
		t.Fatal("Check started a sandbox whose Setup has something wrong in it")
	}
}

// No sandbox is paired with an agent, and the app starts none: a sandbox
// is started as it is, its page offers no agent, and its config names
// none. An agent is what its user types in a Pane.
func TestASandboxIsPairedWithNoAgent(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	for _, a := range sandbox.KnownAgents {
		if h.tt.HasText(a.Title) {
			t.Errorf("the Setup offers %s: %q", a.Title, h.tt.Texts())
		}
	}
	if h.count("Agent") != 0 {
		t.Errorf("the Setup speaks of an agent: %q", h.tt.Texts())
	}
	if told := h.told("Start"); !strings.Contains(told, "|Start|") && !strings.HasSuffix(told, "|Start") {
		t.Errorf("Start says %s", told)
	}
	h.park()
	h.click("Start")
	h.settle("the start", func() bool { return len(h.backend.started) == 1 })
	h.runs("s1")
	// The one thing the app opened there is a login shell.
	if conns := h.backend.opened(); len(conns) != 1 {
		t.Errorf("a start opened %d sessions", len(conns))
	}
	// What is saved of a sandbox names no agent.
	fields := reflect.TypeFor[sandbox.Config]()
	for i := range fields.NumField() {
		if name := fields.Field(i).Name; strings.Contains(name, "Agent") {
			t.Errorf("a sandbox's config has the field %s", name)
		}
	}
}

// A running sandbox's Terminal Sessions are login shells in its Panes:
// what is typed reaches the shell of the Pane that has the keyboard, a
// session keeps its screen while another sandbox is shown, and a
// sandbox that stops takes its sessions with it.
func TestSandboxTerminalSessions(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research"), gated("s2", "Scraper")})
	h.d.Apply(sandbox.Update{SandboxID: "s2", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockProbeUnanswered}})
	h.runs("s1")
	h.settle("the locked sandbox's first Pane", func() bool { v := h.d.view("s2"); return len(v.panes) == 1 && v.panes[0].session != nil })

	// The first Pane's shell has the keyboard as soon as it is there.
	first := h.connOf("s1", 0)
	first.shell.Write([]byte("research$ "))
	term := h.d.view("s1").panes[0].session.term
	h.settle("the shell's prompt", func() bool { return strings.Contains(term.Text(), "research$") })
	h.tt.Type("ls")
	h.settle("what was typed to reach the shell", func() bool { typed, _ := first.state(); return typed == "ls" })
	if got := h.d.view("s1").panes[0].session.id; got != first.id || got == "" {
		t.Errorf("the session is known by the ID %q, and its connection by %q", got, first.id)
	}

	// A session of another sandbox is another shell, and this one keeps
	// its screen meanwhile.
	h.click("Scraper")
	second := h.connOf("s2", 0)
	if second == first || second.sandbox != "s2" {
		t.Fatalf("the other sandbox's Pane shows the session %q of %q", second.id, second.sandbox)
	}
	if _, ok := h.tt.Find("Pane 1 of Scraper"); !ok {
		t.Errorf("the Pane does not say which sandbox it is of: %q", h.tt.Texts())
	}
	h.click("Terminal")
	h.tt.Type("pwd")
	h.settle("typing to reach the second shell only", func() bool {
		typed, _ := second.state()
		return typed == "pwd"
	})
	if typed, _ := first.state(); typed != "ls" {
		t.Errorf("the first shell got %q", typed)
	}
	h.click("Research")
	h.settle("the first session's screen again", func() bool { return strings.Contains(term.Text(), "research$") })

	// A sandbox that stops takes its sessions with it: the connection
	// closes, which hangs the shell up.
	h.d.Apply(sandbox.Update{SandboxID: "s2", Status: &sandbox.Status{State: sandbox.StateStopped}})
	h.settle("the stopped sandbox's session to close", func() bool {
		_, closed := second.state()
		return closed && len(h.d.view("s2").sessions) == 0
	})
	if _, closed := first.state(); closed {
		t.Error("one sandbox's stop closed another's session")
	}
}

// The MCP calls have a control in the Header once there is one to list:
// it shows them in the Panes' place, and the Panes again.
func TestMCPCallsAreListed(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.runs("s1")
	// With no call yet there is no control for them: the Pane is what is
	// shown.
	if h.tt.HasText(mcpCallsLabel) || !h.tt.HasText("Terminal") {
		t.Errorf("before any MCP call the window shows %q", h.tt.Texts())
	}
	h.d.Apply(sandbox.Update{SandboxID: "s1", MCPCall: &sandbox.MCPCall{Time: time.Now(), Server: "browser", FromAgent: true, Method: "tools/call", Summary: `{"name":"navigate"}`}})
	h.tt.Frame()
	// The control comes with the first call, in the Header, and what is
	// shown stays: the Pane, until the control is clicked.
	at, ok := h.tt.Find(mcpCallsLabel)
	if !ok || at.Y+at.H > headerHeight || !h.tt.HasText("Terminal") || h.tt.HasText(`tools/call {"name":"navigate"}`) {
		t.Fatalf("after the first MCP call the control is at %+v and the window shows %q", at, h.tt.Texts())
	}
	h.click(mcpCallsLabel)
	if !h.tt.HasText(`tools/call {"name":"navigate"}`) || !h.tt.HasText("browser") || h.tt.HasText("Terminal") || h.tt.HasText(addPaneLabel) {
		t.Errorf("the list shows %q", h.tt.Texts())
	}
	// The session behind the list goes on, and is not looked at: what it
	// asks for marks its sandbox.
	h.connOf("s1", 0).shell.Write([]byte("\a"))
	h.settle("the ring to be told", func() bool { return h.needs("s1") == "A session asks for you" })
	h.click(mcpCallsLabel)
	h.settle("the Pane again", func() bool { return h.tt.HasText("Terminal") && h.needs("s1") == "" })
	// A sandbox that stops has no list to show at its next start.
	h.click(mcpCallsLabel)
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	h.runs("s1")
	if !h.tt.HasText("Terminal") {
		t.Errorf("started again, the sandbox shows %q", h.tt.Texts())
	}
}

func TestViewportIsPacedByTheView(t *testing.T) {
	browser := browserPack(t)
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg}, browser)
	h.d.view("s1").running = cfg
	h.runs("s1")
	if !h.tt.HasText(browserWaits) || len(h.backend.streams) != 0 {
		t.Fatalf("before the daemon is ready the window shows %q", h.tt.Texts())
	}

	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Daemons: []sandbox.DaemonState{{Pack: browser.Name, Ready: true}}}})
	vp := h.d.view("s1").viewportOf(browser)
	h.settle("the first frame", func() bool { return vp.bitmap != nil })
	stream := h.backend.streams[0]
	h.settle("more frames while the view is built", func() bool { return stream.timesAsked() >= 5 })

	// Input goes to the stream, at the page's coordinates: the frame is
	// 640 wide for a page of 1280.
	box, ok := h.tt.Find("Web Test Browser viewport")
	if !ok {
		t.Fatalf("no viewport in the window: %q", h.tt.Texts())
	}
	h.tt.ClickAt(box.X+box.W/2, box.Y+box.H/2)
	h.settle("the click to reach the stream", func() bool {
		return stream.sawInput("mousePressed 640 360") && stream.sawInput("mouseReleased 640 360")
	})
	h.tt.Type("你好")
	h.settle("the text to reach the stream", func() bool { return stream.sawInput("text 你好") })
	h.tt.Key(0, ui.KeyEnter)
	h.settle("the key to reach the stream", func() bool { return stream.sawInput("key Enter") })

	// With the viewport out of sight nothing more is asked of the stream.
	h.click("Setup")
	h.settle("the Setup", func() bool { return h.tt.HasText("Leaves by") })
	time.Sleep(50 * time.Millisecond)
	asked := stream.timesAsked()
	for i := 0; i < 20; i++ {
		h.settle("a frame", func() bool { return true })
		time.Sleep(5 * time.Millisecond)
	}
	if after := stream.timesAsked(); after > asked+1 {
		t.Errorf("a hidden viewport asked for %d more frames", after-asked)
	}

	// And it resumes when it shows again: Setup gives the page back.
	h.click("Setup")
	h.settle("frames again", func() bool { return stream.timesAsked() >= asked+3 })

	// A sandbox that stops closes its stream.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	select {
	case <-stream.closed:
	case <-time.After(2 * time.Second):
		t.Error("the stream outlived its sandbox")
	}
}

func TestAPackWithoutAStreamHasNoTab(t *testing.T) {
	plain, err := pack.Parse([]byte("name: linter\ntitle: Linter\nmcpBridge: {name: lint, command: [x]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := gated("s1", "Research")
	cfg.Packs = []string{"linter"}
	h := newHarness(t, []sandbox.Config{cfg}, plain)
	h.runs("s1")
	if h.tt.HasText("Linter") || !h.tt.HasText("Pane 1") {
		t.Errorf("a pack without a stream shows %q", h.tt.Texts())
	}
}

// A running sandbox shows its Sandbox Terminal, which fills its page
// beside the sidebar: its Panes, right under the Header. Where a pack
// has a Viewport Stream, its viewport stands above the Panes.
func TestARunningSandboxShowsItsTerminal(t *testing.T) {
	browser := browserPack(t)
	plain, with := gated("s1", "Research"), gated("s2", "Scraper")
	with.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{plain, with}, browser)
	h.tt.SetSize(1280, 900)
	h.runs("s1", "s2")
	strip, ok := h.tt.Find("Pane 1")
	if !ok {
		t.Fatalf("a running sandbox shows %q", h.tt.Texts())
	}
	// Under the Header and nothing else, beside the sidebar, and as wide
	// as the page.
	if strip.Y < headerHeight || strip.Y > headerHeight+40 {
		t.Errorf("Pane 1 starts at %v, not right under a Header of %v", strip.Y, float32(headerHeight))
	}
	if h.tt.HasText(browserWaits) {
		t.Errorf("a sandbox without a Viewport Stream shows a viewport: %q", h.tt.Texts())
	}
	cards, ok1 := h.tt.Find(sidebarSandboxes)
	pane, ok2 := h.tt.Find("Pane 1 of Research")
	if !ok1 || !ok2 || cards.X >= pane.X || pane.X > h.d.sidebarWidth+8 || pane.X+pane.W < 1279 || pane.Y+pane.H < 899 {
		t.Errorf("the sidebar is at %+v and Pane 1 at %+v: %q", cards, pane, h.tt.Texts())
	}

	h.click("Scraper")
	over, ok := h.tt.Find(browserWaits)
	under, ok2 := h.tt.Find("Pane 1")
	if !ok || !ok2 || over.Y >= under.Y || over.Y < headerHeight {
		t.Fatalf("the viewport is at %+v and Pane 1 at %+v: %q", over, under, h.tt.Texts())
	}
	// One pack's viewport needs nothing to choose it by.
	if _, ok := h.tt.Find(viewportsLabel); ok {
		t.Errorf("the one viewport of a sandbox stands under a strip of tabs: %q", h.tt.Texts())
	}
}

// Where more than one of a sandbox's packs has a Viewport Stream, a strip
// over the viewport names each by its pack's title, and a click there
// shows that one.
func TestAStripChoosesAmongViewports(t *testing.T) {
	browser := browserPack(t)
	second, err := pack.Parse([]byte("name: second-browser\ntitle: Second Browser\ndescription: Headless Chromium.\ndaemonProcess: {command: [chromium], readinessPort: 9333}\nviewportStream: {type: cdp_screencast, port: 9333}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name, second.Name}
	h := newHarness(t, []sandbox.Config{cfg}, browser, second)
	h.d.view("s1").running = cfg
	h.runs("s1")
	strip, ok := h.tt.Find(viewportsLabel)
	first, ok2 := h.tt.Find(browserWaits)
	_, ok3 := h.tt.Find("Web Test Browser")
	if !ok || !ok2 || !ok3 || strip.Y >= first.Y {
		t.Fatalf("the strip is at %+v and the first viewport at %+v: %q", strip, first, h.tt.Texts())
	}
	h.click("Second Browser")
	if !h.tt.HasText("Waiting for Second Browser to be ready…") || h.tt.HasText(browserWaits) {
		t.Errorf("a click on the second viewport's tab shows %q", h.tt.Texts())
	}
}

// The window's own title bar is hidden: the Header and the sidebar make
// room for the window's controls, which sit over the sidebar.
func TestHeaderMakesRoomForTheWindowsControls(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.tt.SetSize(1280, 900)
	h.runs("s1")
	h.tt.SetTitleBar(ui.TitleBar{Left: 78, Height: 28, Bottom: 52})
	section, ok := h.tt.Find(sidebarSandboxes)
	if !ok {
		t.Fatalf("no sidebar: %q", h.tt.Texts())
	}
	if section.Y < 52 {
		t.Errorf("the sidebar starts at %v, under the window's controls, which end at 52", section.Y)
	}
	// The Header is at the top, as high as it says, and the page starts
	// under it.
	for _, control := range []string{addPaneLabel, "Stop", "Setup", "Clone", "Destroy"} {
		if at, ok := h.tt.Find(control); !ok || at.Y < 0 || at.Y+at.H > headerHeight {
			t.Errorf("%s is at %+v, not in a Header of %v", control, at, float32(headerHeight))
		}
	}
	if pane, ok := h.tt.Find("Pane 1 of Research"); !ok || pane.Y < headerHeight {
		t.Errorf("the Sandbox Terminal is at %+v, over a Header of %v", pane, float32(headerHeight))
	}
}

// A sandbox is given Host Tools in its Setup, under the Shared Folder
// they run in: with no Shared Folder it has none, and what is added to
// their Wall is asked only where there are some.
func TestHostToolsInTheSetup(t *testing.T) {
	cfg := gated("s1", "Research")
	d := newDraft(cfg, nil)
	if d.hostTools || d.hostGPU || d.hostMacName {
		t.Errorf("a new sandbox's form gives it Host Tools: %+v", d)
	}
	// Set while the sandbox is given no Shared Folder, they are not the
	// sandbox's.
	d.hostTools, d.hostGPU = true, true
	if back, errs := d.config(cfg); len(errs) != 0 || back.HostTools != (sandbox.HostTools{}) {
		t.Errorf("with no Shared Folder the sandbox is given %+v (%v)", back.HostTools, errs)
	}
	d.folderChoice = "research"
	back, errs := d.config(cfg)
	if len(errs) != 0 || back.HostTools != (sandbox.HostTools{Enabled: true, GPU: true}) || len(back.Validate()) != 0 {
		t.Errorf("in a Shared Folder the sandbox is given %+v (%v, %v)", back.HostTools, errs, back.Validate())
	}
	if again := newDraft(back, nil); !again.hostTools || !again.hostGPU || again.hostMacName {
		t.Errorf("a sandbox given Host Tools shows %+v", again)
	}
	// Without Host Tools nothing is added to a Wall.
	d.hostTools, d.hostMacName = false, true
	if back, _ := d.config(cfg); back.HostTools != (sandbox.HostTools{}) {
		t.Errorf("without Host Tools the sandbox is given %+v", back.HostTools)
	}

	h := newHarness(t, []sandbox.Config{cfg})
	if h.tt.HasText("Run this Mac's programs in the folder") {
		t.Fatalf("with no Shared Folder the Setup offers Host Tools: %q", h.tt.Texts())
	}
	h.giveFolder("research")
	if !h.tt.HasText("Run this Mac's programs in the folder") || h.tt.HasText("With this Mac's GPU") || h.tt.HasText("With this Mac's name") {
		t.Fatalf("with a Shared Folder the Setup shows %q", h.tt.Texts())
	}
	texts := h.told("About Host Tools")
	for _, want := range []string{
		"by the tool run of the MCP server host", "under the Host Tool Wall", "/Applications, Homebrew and the system's own",
		"none of your files, no network, no other program, no window", "which applications you have installed",
		"fills this Mac's disk, which nothing bounds", "do not open it as a program",
	} {
		if !strings.Contains(texts, want) {
			t.Errorf("what is said of Host Tools lacks %q: %s", want, texts)
		}
	}
	h.park()
	h.click("Run this Mac's programs in the folder")
	if !h.tt.HasText("With this Mac's GPU") || !h.tt.HasText("With this Mac's name") {
		t.Fatalf("with Host Tools the Setup shows %q", h.tt.Texts())
	}
	if texts := h.told("About The GPU") + h.told("About This Mac's name"); !strings.Contains(texts, "what Metal and VideoToolbox need") || !strings.Contains(texts, "The name often holds its owner's") {
		t.Errorf("what is said of the GPU and the name: %s", texts)
	}
	h.park()
	h.click("With this Mac's GPU")
	saves := len(h.backend.saved)
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+1 })
	if got := h.backend.lastSaved()[0]; got.SharedFolder != "research" || got.HostTools != (sandbox.HostTools{Enabled: true, GPU: true}) {
		t.Errorf("saved %+v", got)
	}

	// Given none again, the sandbox has no Host Tools, and their
	// switches are not shown.
	h.giveFolder(sharedFolderNone)
	if h.tt.HasText("Run this Mac's programs in the folder") {
		t.Errorf("with no Shared Folder the Setup still offers Host Tools: %q", h.tt.Texts())
	}
	h.click("Save")
	h.settle("the config to be saved again", func() bool { return len(h.backend.saved) == saves+2 })
	if got := h.backend.lastSaved()[0]; got.SharedFolder != "" || got.HostTools != (sandbox.HostTools{}) {
		t.Errorf("with no Shared Folder again, saved %+v", got)
	}
}

// A sandbox's Shared Folder is one choice of its Setup: none, one of the
// folders there are, or a new one, whose name is asked only then; with
// where the folder is and who else has it said beside it.
func TestASharedFolderInTheSetup(t *testing.T) {
	cfg := gated("s1", "Research")
	d := newDraft(cfg, nil)
	if d.folderChoice != sharedFolderNone || d.sharedFolder() != "" {
		t.Errorf("a new sandbox's form gives it the Shared Folder %q (%q)", d.sharedFolder(), d.folderChoice)
	}
	if back, errs := d.config(cfg); len(errs) != 0 || back.SharedFolder != "" {
		t.Errorf("with none chosen the sandbox is given %q (%v)", back.SharedFolder, errs)
	}
	d.folderChoice, d.newFolder = sharedFolderNew, " research "
	back, errs := d.config(cfg)
	if len(errs) != 0 || back.SharedFolder != "research" {
		t.Errorf("a new folder's name reads back as %q (%v)", back.SharedFolder, errs)
	}
	if again := newDraft(back, nil); again.folderChoice != "research" || again.newFolder != "" {
		t.Errorf("a sandbox given a Shared Folder shows the choice %q", again.folderChoice)
	}
	// A new folder with no name is not none: it is asked for.
	d.newFolder = "  "
	if _, errs := d.config(cfg); len(errs) != 1 || errs[0].Field != sandbox.FieldSharedFolder {
		t.Errorf("a new folder with no name is read with the errors %v", errs)
	}
	// What a name typed earlier was is nothing once another is chosen.
	d.folderChoice, d.newFolder = sharedFolderNone, "research"
	if back, errs := d.config(cfg); len(errs) != 0 || back.SharedFolder != "" {
		t.Errorf("with none chosen after a name was typed the sandbox is given %q (%v)", back.SharedFolder, errs)
	}

	h := newHarness(t, []sandbox.Config{cfg, gated("s2", "Builds")})
	h.onDisk("notes")
	v := h.d.view("s1")
	// The part is there, after the network, with one choice and no name
	// to type before a new folder is asked for.
	if !h.tt.HasText(partSharedFolder) || h.tt.HasText(newSharedFolderName) {
		t.Fatalf("the Setup shows %q", h.tt.Texts())
	}
	if at, ok := h.tt.Find(sharedFolderField); !ok {
		t.Fatalf("the Setup has no field for the Shared Folder: %q", h.tt.Texts())
	} else if network, _ := h.tt.Find("Resolver"); at.Y < network.Y {
		t.Errorf("the Shared Folder's field is at %v, above the network's last field at %v", at.Y, network.Y)
	}
	texts := h.told("About "+sharedFolderField) + h.told("About Memory (MB)")
	for _, want := range []string{
		"mounted at /workspace/shared, beside the sandbox's Home, /workspace, which is its own",
		"Choose one there is, or New folder… and name it",
		"/Users/Shared/zeolite/shared.noindex/<name>",
		"Sandboxes given the same folder mount the same one: each reads and writes all of it.",
		"The sandbox's Home, /workspace, is on the VM's disk: it is kept when the sandbox stops, and has no limit of its own.",
	} {
		if !strings.Contains(texts, want) {
			t.Errorf("the form does not say %q: %s", want, texts)
		}
	}
	h.park()

	// The choice offers none, the folders that are there and a new one,
	// with nothing typed: the sidebar names the folder once, and the
	// choice besides.
	named := func() int { return strings.Count("|"+strings.Join(h.tt.Texts(), "|")+"|", "|notes|") }
	if n := named(); n != 1 {
		t.Fatalf("before the choice is opened the folder on the disk shows %d times: %q", n, h.tt.Texts())
	}
	h.click(sharedFolderChoice)
	h.settle("the folder on the disk to be offered", func() bool {
		return named() == 2 && h.count(sharedFolderNew) == 1 && h.count(sharedFolderNone) >= 1
	})
	h.tt.Key(0, ui.KeyEscape)
	h.tt.Frame()

	// One that is there is chosen, and is the sandbox's.
	saves := len(h.backend.saved)
	h.giveFolder("notes")
	if h.tt.HasText(newSharedFolderName) {
		t.Errorf("with a folder chosen the Setup asks for a new one's name: %q", h.tt.Texts())
	}
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+1 })
	if got := h.backend.lastSaved(); got[0].SharedFolder != "notes" || got[1].SharedFolder != "" {
		t.Errorf("saved %+v", got)
	}

	// A new one is named under the choice. A name that is not one is an
	// error on the field, as no name is, and nothing is saved.
	h.giveFolder("My Folder")
	h.click("Save")
	if !h.tt.HasText("a Shared Folder's name is 1 to 32 lower-case letters, digits or hyphens, such as research") {
		t.Errorf("a bad name is not refused on its field: %q", h.tt.Texts())
	}
	h.replace(newSharedFolderName, "")
	h.click("Save")
	if !h.tt.HasText("name the new Shared Folder, or choose None") {
		t.Errorf("a new folder with no name is not refused on its field: %q", h.tt.Texts())
	}
	if len(h.backend.saved) != saves+1 || h.d.configs[0].SharedFolder != "notes" {
		t.Error("a config with a bad Shared Folder's name was saved")
	}

	h.replace(newSharedFolderName, "research")
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+2 })
	if got := h.backend.lastSaved(); got[0].SharedFolder != "research" || got[1].SharedFolder != "" {
		t.Errorf("saved %+v", got)
	}
	// Saved, the new folder is one of those there are: it is the choice,
	// and no name is asked for.
	h.tt.Frame()
	if v.draft.folderChoice != "research" || v.draft.newFolder != "" || h.tt.HasText(newSharedFolderName) {
		t.Errorf("after the new folder was saved the choice is %q, the name %q: %q", v.draft.folderChoice, v.draft.newFolder, h.tt.Texts())
	}

	// None: the sandbox is given no Shared Folder.
	h.giveFolder(sharedFolderNone)
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+3 })
	if got := h.backend.lastSaved(); got[0].SharedFolder != "" {
		t.Errorf("saved %+v", got)
	}
}

// giveFolder gives the sandbox shown a Shared Folder in its Setup, or
// none: one of the choices by choosing it, and any other name as a new
// folder's.
func (h *harness) giveFolder(name string) {
	h.t.Helper()
	choices := append(append([]string{sharedFolderNone}, h.d.sharedFolderNames()...), sharedFolderNew)
	if slices.Contains(choices, name) {
		h.choose(sharedFolderChoice, choices, name)
		return
	}
	h.choose(sharedFolderChoice, choices, sharedFolderNew)
	h.replace(newSharedFolderName, name)
}

// Sandboxes that name the same Shared Folder work in it together: a
// second one is given it as the first was, and a copy keeps the name.
func TestSeveralSandboxesAreGivenOneSharedFolder(t *testing.T) {
	first := gated("s1", "Research")
	first.SharedFolder = "research"
	h := newHarness(t, []sandbox.Config{first, gated("s2", "Builds")})
	h.click("Builds")
	saves := len(h.backend.saved)
	h.giveFolder("research")
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+1 })
	if got := h.backend.lastSaved(); got[0].SharedFolder != "research" || got[1].SharedFolder != "research" {
		t.Errorf("saved %+v", got)
	}
	if !h.tt.HasText("Saved.") {
		t.Errorf("a second sandbox was not given the Shared Folder: %q", h.tt.Texts())
	}

	h.d.clone("s1")
	if c := h.d.configs[2]; c.SharedFolder != "research" || c.ID == "s1" {
		t.Errorf("the copy is %+v", c)
	}
}

// onDisk puts Shared Folders on the fake Mac's disk, and has the
// dashboard list them, as it does when the window opens.
func (h *harness) onDisk(names ...string) {
	h.t.Helper()
	h.backend.mu.Lock()
	h.backend.folders = names
	h.backend.mu.Unlock()
	h.d.listSharedFolders()
	h.settle("the Shared Folders on the disk", func() bool { return slices.Equal(h.d.folders, names) })
}

// confirm chooses the button of an open alert that says label. Where
// the page has a button of that label too, the alert's own has the focus.
func (h *harness) confirm(label string) {
	h.t.Helper()
	if err := h.tt.Click(label); err != nil {
		h.tt.Key(0, ui.KeyEnter)
	}
	h.tt.Frame()
}

// A Shared Folder has a page: where it is, the sandboxes given it, and
// Delete, which asks first and is refused while a sandbox mounts it.
func TestSharedFolderPage(t *testing.T) {
	first, second := gated("s1", "Research"), gated("s2", "Builds")
	first.SharedFolder, second.SharedFolder = "research", "research"
	h := newHarness(t, []sandbox.Config{first, second, gated("s3", "Alone")})
	h.tt.SetSize(1280, 900)
	h.onDisk("old", "research")
	// Each Shared Folder has a row in the sidebar, under the sandboxes'
	// Cards, which stand as they were made whatever folder each is
	// given: no row is set in from another.
	if got := h.order(sidebarSandboxes, "Research", "Builds", "Alone", sidebarSharedFolders, "old", "research"); got != "Sandboxes Research Builds Alone Shared Folders old research" {
		t.Fatalf("the sidebar stands in the order %q: %q", got, h.tt.Texts())
	}
	given, _ := h.tt.Find("Builds")
	alone, _ := h.tt.Find("Alone")
	if given.X != alone.X {
		t.Errorf("a sandbox given a folder is at %v, and a sandbox given none at %v", given.X, alone.X)
	}
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateOpen}})

	h.click("research")
	const path = "/Users/Shared/zeolite/shared.noindex/research"
	texts := strings.Join(h.tt.Texts(), "|")
	for _, want := range []string{"Shared Folder|research", path, "GATE OPEN|Research", "STOPPED|Builds"} {
		if !strings.Contains(texts, want) {
			t.Errorf("the page lacks %q: %s", want, texts)
		}
	}
	// A sandbox that is not given the folder is in the sidebar, and not
	// on the page.
	if n := strings.Count(texts, "Alone"); n != 1 {
		t.Errorf("a sandbox that is not given the folder shows %d times: %s", n, texts)
	}

	h.click("Copy Path")
	if got := h.tt.Clipboard(); got != path {
		t.Errorf("Copy Path copied %q", got)
	}
	h.click("Show in Finder")
	h.settle("the folder to be shown in the Finder", func() bool { return h.backend.wasShown() == "research" })

	// Delete asks, and Cancel deletes nothing.
	h.click("Delete")
	h.settle("the question", func() bool { return h.tt.HasText("Delete the Shared Folder “research”?") })
	if !h.tt.HasText("Everything in it is deleted from this Mac. The sandboxes given it get an empty one at their next start.") {
		t.Errorf("the question does not say what goes: %q", h.tt.Texts())
	}
	h.click("Cancel")
	h.settle("the question to go", func() bool { return !h.tt.HasText("Delete the Shared Folder “research”?") })
	if got := h.backend.done(0); len(got) != 0 {
		t.Fatalf("Cancel asked the backend to %v", got)
	}

	// While a sandbox mounts the folder the backend refuses, and the
	// folder stays.
	h.backend.removeErr = errors.New("a running sandbox mounts it: stop the sandboxes given this Shared Folder first")
	h.click("Delete")
	h.settle("the question", func() bool { return h.tt.HasText("Delete the Shared Folder “research”?") })
	h.confirm("Delete")
	h.settle("the refusal", func() bool {
		return h.tt.HasText("The Shared Folder was not deleted: a running sandbox mounts it: stop the sandboxes given this Shared Folder first")
	})
	if h.backend.wasRemoved() != "" || !slices.Contains(h.d.folders, "research") {
		t.Fatalf("a folder that a sandbox mounts was deleted: %q", h.d.folders)
	}

	// Once nothing mounts it, it is deleted. Sandboxes still name it: its
	// page stays, and says that it is not on the disk.
	h.backend.removeErr = nil
	h.click("Delete")
	h.settle("the question", func() bool { return h.tt.HasText("Delete the Shared Folder “research”?") })
	h.confirm("Delete")
	h.settle("the folder to be deleted", func() bool { return h.backend.wasRemoved() == "research" && !slices.Contains(h.d.folders, "research") })
	if !strings.Contains(h.told("About this Shared Folder"), "Not on this Mac's disk yet: the app makes it when a sandbox given it starts.") {
		t.Errorf("the page of a folder that is not on the disk does not say so: %q", h.tt.Texts())
	}
	if h.tt.HasText("The Shared Folder was not deleted") || slices.Contains(h.d.folders, "research") {
		t.Errorf("after the folder was deleted the page shows %q", h.tt.Texts())
	}

	// A start makes it again.
	h.click("Research")
	h.click("Stop")
	h.settle("the sandbox to stop", func() bool { return h.d.view("s1").status.State == sandbox.StateStopped && h.tt.HasText(partNetwork) })
	h.click("Start")
	h.settle("the folder to be on the disk again", func() bool { return slices.Contains(h.d.folders, "research") })

	// A folder no sandbox is given says so, and is gone from the sidebar
	// once it is deleted.
	h.click("old")
	if !h.tt.HasText("No sandbox is given this folder: name it in a sandbox's Setup.") {
		t.Errorf("a folder no sandbox is given shows %q", h.tt.Texts())
	}
	h.click("Delete")
	h.settle("the question", func() bool { return h.tt.HasText("Delete the Shared Folder “old”?") })
	if h.tt.HasText("The sandboxes given it get an empty one at their next start.") {
		t.Error("the question speaks of sandboxes the folder is not given to")
	}
	h.confirm("Delete")
	h.settle("the folder to be gone", func() bool { return h.backend.wasRemoved() == "research old" && !slices.Contains(h.tt.Texts(), "old") })
	if _, ok := sharedFolderOf(h.d.selected); ok || h.d.folderViews["old"] != nil {
		t.Errorf("after its folder was deleted the page shows %q", h.d.selected)
	}
}

// Destroying a sandbox stops it, deletes its Home and forgets it. The
// Shared Folder it was given stays: it is deleted from its own page, and
// by nothing else.
func TestDestroyLeavesTheSharedFolder(t *testing.T) {
	shared := gated("s1", "Research")
	shared.SharedFolder = "research"
	h := newHarness(t, []sandbox.Config{shared, gated("s2", "Builds")})
	h.onDisk("research")

	h.click("Destroy")
	h.settle("the question", func() bool { return h.tt.HasText("Destroy “Research”?") })
	if !h.tt.HasText("Its Setup and its Home are deleted: everything in /workspace, the agents you installed there and their logins among it. Its Shared Folder, “research”, stays on this Mac.") {
		t.Errorf("the question does not say the Home goes and the Shared Folder stays: %q", h.tt.Texts())
	}
	h.click("Cancel")
	h.d.destroy("s1")
	h.settle("the sandbox to be forgotten", func() bool { return len(h.backend.lastSaved()) == 1 })
	if got := h.backend.done(0); strings.Join(got, ", ") != "stop s1, remove-home s1, remove-scratch s1, save" {
		t.Errorf("the backend was asked to %v", got)
	}

	// A sandbox without one has a question that does not speak of one.
	h.tt.Frame()
	h.click("Destroy")
	h.settle("the question", func() bool { return h.tt.HasText("Destroy “Builds”?") })
	if h.tt.HasText("stays on this Mac") {
		t.Error("the question speaks of a Shared Folder the sandbox is not given")
	}
	h.click("Cancel")
	h.d.destroy("s2")
	h.settle("the sandbox to be forgotten", func() bool { return len(h.backend.lastSaved()) == 0 && len(h.backend.done(0)) == 8 })

	// With no sandbox left, the folder is still there to be seen and
	// deleted.
	h.settle("the folder's page", func() bool {
		return strings.Contains(strings.Join(h.tt.Texts(), "|"), "Shared Folder|research|Delete")
	})
	if h.backend.wasRemoved() != "" || h.tt.HasText("No sandboxes yet") {
		t.Errorf("after every sandbox was destroyed the window shows %q; removed %q", h.tt.Texts(), h.backend.wasRemoved())
	}
}

// A sandbox whose Home could not be deleted is not forgotten: its Home
// is still in the VM, and the sandbox is what names it. The window says
// why, and nothing is saved. While it is being destroyed, and while it
// starts or stops, a sandbox cannot be destroyed again.
func TestASandboxWhoseHomeIsNotDeletedIsKept(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.backend.homeErr = errors.New("the runtime is not available")
	h.d.destroy("s1")
	// Under way: Destroy is not offered a second time.
	h.tt.Frame()
	if err := h.tt.Click("Destroy"); err == nil {
		h.tt.Frame()
		if h.d.destroying {
			t.Error("Destroy was offered while the sandbox was being destroyed")
		}
	}
	h.settle("the refusal", func() bool {
		return h.tt.HasText("Not destroyed: its Home could not be deleted: the runtime is not available")
	})
	if len(h.d.configs) != 1 || h.d.configs[0].ID != "s1" || h.d.selected != "s1" {
		t.Errorf("after a Home that could not be deleted the sandboxes are %+v, and %q is shown", h.d.configs, h.d.selected)
	}
	if got := strings.Join(h.backend.done(0), ", "); got != "stop s1, remove-home s1" {
		t.Errorf("the backend was asked to %s: nothing is saved of a sandbox that is kept", got)
	}

	// Nor is one whose Host Tool Scratch could not be deleted: it is on
	// this Mac's disk, and the sandbox is what names it.
	h.backend.homeErr, h.backend.scratchErr = nil, errors.New("the sandbox's Host Tools are running")
	h.d.destroy("s1")
	h.settle("the second refusal", func() bool {
		return h.tt.HasText("Not destroyed: its Host Tool Scratch could not be deleted: the sandbox's Host Tools are running")
	})
	if got := strings.Join(h.backend.done(2), ", "); len(h.d.configs) != 1 || got != "stop s1, remove-home s1, remove-scratch s1" {
		t.Errorf("after a Scratch that could not be deleted the sandboxes are %+v, and the backend was asked to %s", h.d.configs, got)
	}

	// Once both can be deleted, it is destroyed.
	h.backend.scratchErr = nil
	h.d.destroy("s1")
	h.settle("the sandbox to be forgotten", func() bool { return len(h.d.configs) == 0 })
	if got := strings.Join(h.backend.done(5), ", "); got != "stop s1, remove-home s1, remove-scratch s1, save" {
		t.Errorf("the backend was asked to %s", got)
	}

	// A sandbox that is starting is not destroyed under its start.
	h = newHarness(t, []sandbox.Config{gated("s2", "Builds")})
	h.d.view("s2").busy = true
	h.d.destroy("s2")
	h.tt.Frame()
	if len(h.backend.done(0)) != 0 || len(h.d.configs) != 1 {
		t.Errorf("a busy sandbox was destroyed: the backend was asked to %v", h.backend.done(0))
	}
}

// A sandbox's ID names its containers and its Home: it keeps its shape,
// which is also how a test's Home is told from the app's.
func TestASandboxsIDKeepsItsShape(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		id := newID()
		if len(id) != 11 || id[0] != 's' || strings.Trim(id[1:], "0123456789abcdef") != "" || !sandbox.ValidID(id) {
			t.Fatalf("a new sandbox's ID is %q, want s and ten hexadecimal digits", id)
		}
		if seen[id] {
			t.Fatalf("the ID %q was made twice", id)
		}
		seen[id] = true
	}
}

// The form's Allowed Interfaces are rows: an Interface each, with what
// the probe may see there, in the order of preference.
func TestAllowedInterfacesInTheForm(t *testing.T) {
	cfg := gated("s1", "Research")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{
		{Name: "utun4", ExpectedExitAddresses: []string{"203.0.113.7", "198.51.100.9"}},
		{Name: "en0", AnyExitAddress: true},
	}
	d := newDraft(cfg, nil)
	if len(d.interfaces) != 2 || d.interfaces[0].exits != "203.0.113.7, 198.51.100.9" || d.interfaces[0].expect != expectListed || d.interfaces[1].expect != expectAny {
		t.Fatalf("the rows are %+v, %+v", d.interfaces[0], d.interfaces[1])
	}
	back, errs := d.config(cfg)
	if len(errs) != 0 || len(back.AllowedInterfaces) != 2 || back.AllowedInterfaces[0].Name != "utun4" || len(back.AllowedInterfaces[0].ExpectedExitAddresses) != 2 || !back.AllowedInterfaces[1].AnyExitAddress {
		t.Errorf("the rows read back as %+v (%v)", back.AllowedInterfaces, errs)
	}
	// Each of the three expectations is a row's, and reads back as it was.
	three := gated("s3", "Three")
	three.AllowedInterfaces = []sandbox.AllowedInterface{
		{Name: "utun9", AnyExitAddressButPhysical: true},
		{Name: "utun4", ExpectedExitAddresses: []string{"203.0.113.7"}},
		{Name: "en0", AnyExitAddress: true},
	}
	rows := newDraft(three, nil)
	if got := rows.interfaces[0].expect + "|" + rows.interfaces[1].expect + "|" + rows.interfaces[2].expect; got != "any but physical|these addresses|any" {
		t.Fatalf("the rows expect %s", got)
	}
	if back, errs := rows.config(three); len(errs) != 0 || !reflect.DeepEqual(back.AllowedInterfaces, three.AllowedInterfaces) || len(back.Validate()) != 0 {
		t.Errorf("the three expectations read back as %+v (%v, %v)", back.AllowedInterfaces, errs, back.Validate())
	}
	// What was typed as addresses is kept in the row while it expects
	// something else, and is no part of the config then: a row never
	// says two things.
	rows.interfaces[1].expect = expectAnyButPhysical
	if back, _ := rows.config(three); len(back.AllowedInterfaces[1].ExpectedExitAddresses) != 0 || !back.AllowedInterfaces[1].AnyExitAddressButPhysical || back.AllowedInterfaces[1].AnyExitAddress || len(back.Validate()) != 0 {
		t.Errorf("a row with addresses typed that expects any but physical reads as %+v (%v)", back.AllowedInterfaces[1], back.Validate())
	}
	rows.interfaces[1].expect = expectListed
	if back, _ := rows.config(three); len(back.AllowedInterfaces[1].ExpectedExitAddresses) != 1 || back.AllowedInterfaces[1].AnyExitAddressButPhysical {
		t.Errorf("chosen back, the row reads as %+v", back.AllowedInterfaces[1])
	}
	// A new sandbox has one row to type in, which is no Interface until
	// something is typed.
	fresh := newDraft(sandbox.NewConfig("s9", "Fresh"), nil)
	if len(fresh.interfaces) != 1 {
		t.Fatalf("a new sandbox's form has %d rows", len(fresh.interfaces))
	}
	if back, _ := fresh.config(sandbox.NewConfig("s9", "Fresh")); len(back.AllowedInterfaces) != 0 {
		t.Errorf("an empty row was read as the Interface %+v", back.AllowedInterfaces)
	}

	h := newHarness(t, []sandbox.Config{cfg})
	v := h.d.view("s1")
	// A row names its way out as the system does, where the system has a
	// name for it, and the form has no other word for what it is.
	texts := strings.Join(h.tt.Texts(), "|")
	for _, want := range []string{"Leaves by", "Way out 1|" + wayTunnel, "Way out 2|" + wayWiFi, "Add a way out"} {
		if !strings.Contains(texts, want) {
			t.Errorf("the form does not say %q: %s", want, texts)
		}
	}
	if strings.Contains(strings.ToLower(texts), "interface") {
		t.Errorf("the form speaks of interfaces: %s", texts)
	}
	// What there is to know of the rows is said by their glyph.
	help := h.told("About Leaves by")
	for _, want := range []string{"most preferred first", "The list is what is up now.", "any but physical", "asks the probe URLs by Wi-Fi and Ethernet too"} {
		if !strings.Contains(help, want) {
			t.Errorf("what is said about the rows lacks %q: %s", want, help)
		}
	}
	if strings.Contains(strings.ToLower(help), "interface") {
		t.Errorf("what is said about the rows speaks of interfaces: %s", help)
	}

	// A row is added, which holds nothing until a way out is chosen for
	// it among this Mac's.
	h.click("Add a way out")
	if len(v.draft.interfaces) != 3 || v.draft.interfaces[2].name != "" || h.count(chooseWay) != 1 {
		t.Fatalf("after a row was added there are %d rows: %q", len(v.draft.interfaces), h.tt.Texts())
	}
	h.choose("Way out 3", ways, wayUSB)
	if got := v.draft.interfaces[2].name; got != "en7" {
		t.Fatalf("after %q was chosen the row names %q", wayUSB, got)
	}
	h.replace("Expected exit addresses of way out 3", "192.168.1.77")
	saves := len(h.backend.saved)
	// It is moved up twice: it keeps what was typed in it, and is the
	// most preferred.
	added := v.draft.interfaces[2]
	h.click("Move way out 3 up")
	h.click("Move way out 2 up")
	order := func() string {
		var names []string
		for _, row := range v.draft.interfaces {
			names = append(names, row.name)
		}
		return strings.Join(names, " ")
	}
	if order() != "en7 utun4 en0" || v.draft.interfaces[0] != added || added.exits != "192.168.1.77" {
		t.Fatalf("after moving the new row up twice the rows are %q; the row holds %+v", order(), added)
	}
	// An address that is not public is an error on the field, and nothing
	// is saved.
	h.click("Save")
	if !h.tt.HasText("Not saved: see the fields marked above.") || len(h.backend.saved) != saves {
		t.Fatalf("a row with an exit address that is not public was saved: %q", h.tt.Texts())
	}
	if texts := strings.Join(h.tt.Texts(), "|"); !strings.Contains(texts, `en7: "192.168.1.77": not a public address`) {
		t.Errorf("the error does not name the way out and the address it is about: %s", texts)
	}
	h.replace("Expected exit addresses of way out 1", "203.0.113.50")
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+1 })
	got := h.backend.lastSaved()[0].AllowedInterfaces
	if len(got) != 3 || got[0].Name != "en7" || len(got[0].ExpectedExitAddresses) != 1 || got[0].ExpectedExitAddresses[0] != "203.0.113.50" || got[0].AnyExitAddress || got[1].Name != "utun4" || got[2].Name != "en0" {
		t.Fatalf("saved the Interfaces %+v", got)
	}

	// A row is removed, and the others keep their order.
	h.click("Remove way out 2")
	if order() != "en7 en0" {
		t.Errorf("after the second row was removed the rows are %q", order())
	}

	// What a row expects is chosen from three. The addresses are asked
	// for only while it expects these addresses; what was typed stays in
	// the row, and is saved only then.
	choose := func(row int, expect string) {
		t.Helper()
		h.choose("Expected at way out "+strconv.Itoa(row), expectations, expect)
		if got := v.draft.interfaces[row-1].expect; got != expect {
			t.Fatalf("after choosing %q, way out %d expects %q", expect, row, got)
		}
	}
	choose(1, expectAnyButPhysical)
	if _, asked := h.tt.Find("Expected exit addresses of way out 1"); asked || v.draft.interfaces[0].exits != "203.0.113.50" {
		t.Errorf("a row that expects any but physical asks for addresses, or lost them: %q; %q", v.draft.interfaces[0].exits, h.tt.Texts())
	}
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+2 })
	got = h.backend.lastSaved()[0].AllowedInterfaces
	if len(got) != 2 || !got[0].AnyExitAddressButPhysical || got[0].AnyExitAddress || len(got[0].ExpectedExitAddresses) != 0 || !got[1].AnyExitAddress {
		t.Fatalf("saved the Interfaces %+v", got)
	}
	choose(1, expectAny)
	choose(2, expectAnyButPhysical)
	choose(1, expectListed)
	if _, asked := h.tt.Find("Expected exit addresses of way out 1"); !asked {
		t.Errorf("a row that expects these addresses again does not ask for them: %q", h.tt.Texts())
	}
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+3 })
	got = h.backend.lastSaved()[0].AllowedInterfaces
	if len(got) != 2 || got[0].Expectations() != 1 || len(got[0].ExpectedExitAddresses) != 1 || got[0].ExpectedExitAddresses[0] != "203.0.113.50" || !got[1].AnyExitAddressButPhysical || got[1].AnyExitAddress {
		t.Fatalf("saved the Interfaces %+v", got)
	}

	// A copy has rows of its own.
	h.d.clone("s1")
	copied := h.d.configs[1].AllowedInterfaces
	copied[0].ExpectedExitAddresses[0] = "198.51.100.200"
	if h.d.configs[0].AllowedInterfaces[0].ExpectedExitAddresses[0] == "198.51.100.200" {
		t.Error("a copy shares its Allowed Interfaces with the sandbox it was copied from")
	}
}

// A way out is chosen among this Mac's, each named as the system names
// it where it has a title for it, and what is saved is the interface's
// name. Before one is chosen the sandbox has none, and is not saved.
func TestAWayOutIsChosenByWhatTheSystemCallsIt(t *testing.T) {
	h := newHarness(t, []sandbox.Config{sandbox.NewConfig("s1", "Fresh")})
	v := h.d.view("s1")
	saves := len(h.backend.saved)
	if h.count(chooseWay) != 1 || len(v.draft.interfaces) != 1 {
		t.Fatalf("a new sandbox's form shows %q", h.tt.Texts())
	}
	h.click("Save")
	if !h.tt.HasText("choose what the sandbox leaves this Mac by") || len(h.backend.saved) != saves {
		t.Fatalf("a sandbox with no way out was saved: %q", h.tt.Texts())
	}

	// What is offered is what this Mac has now: the system's title and
	// the name of each, and no address of this Mac's.
	h.click("Way out 1")
	h.settle("this Mac's ways out to be offered", func() bool {
		return h.count(wayWiFi) == 1 && h.count(wayTunnel) == 1 && h.count(wayUSB) == 1
	})
	h.tt.Key(0, ui.KeyEscape)
	h.tt.Frame()
	h.choose("Way out 1", ways, wayWiFi)
	if got := v.draft.interfaces[0].name; got != "en0" || h.count(chooseWay) != 0 {
		t.Fatalf("after %q was chosen the row names %q: %q", wayWiFi, got, h.tt.Texts())
	}
	h.choose("Expected at way out 1", expectations, expectAny)
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+1 })
	if got := h.backend.lastSaved()[0].AllowedInterfaces; len(got) != 1 || got[0].Name != "en0" || !got[0].AnyExitAddress {
		t.Errorf("saved the Interfaces %+v", got)
	}

	// Another is chosen in its place.
	h.choose("Way out 1", ways, wayTunnel)
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+2 })
	if got := h.backend.lastSaved()[0].AllowedInterfaces; len(got) != 1 || got[0].Name != "utun4" {
		t.Errorf("saved the Interfaces %+v", got)
	}
}

// A Card says its sandbox's facts to whoever rests the pointer on it:
// its state alone while it does not run, and while it runs its gate and
// its workstation.
func TestTheCardSaysTheGatesFacts(t *testing.T) {
	browser := browserPack(t)
	gate := gated("s1", "Research")
	gate.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{gate, gated("s2", "Idle"), gated("s3", "Broken")}, browser)
	h.tt.SetSize(1280, 900)
	if facts := h.facts("Research"); facts != "State|STOPPED" {
		t.Errorf("a stopped sandbox's Card says %s", facts)
	}
	// One that was never shown says as much.
	if facts := h.facts("Idle"); facts != "State|STOPPED" || h.d.views["s2"] != nil {
		t.Errorf("the Card of a sandbox that was never shown says %s", facts)
	}

	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateOpen, ExitAddress: "203.0.113.7"}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{Open: true, Interface: "en0", InterfaceChange: 1, ExitAddress: "203.0.113.7", ProbeLatency: 83 * time.Millisecond, PacketsPassed: 4211, PacketsRejected: 3}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Clock: "2026-11-18T09:30:00Z", Daemons: []sandbox.DaemonState{{Pack: browser.Name, Error: "chromium exited"}}}})
	h.tt.Frame()
	const want = "State|GATE OPEN [203.0.113.7]|Leaves by|en0|Exit address|203.0.113.7|Expected|203.0.113.7|Probe latency|83 ms|Packets|4211 passed, 3 rejected|Sandbox clock|2026-11-18T09:30:00Z|web-test-browser|chromium exited"
	if facts := h.facts("Research"); facts != want {
		t.Errorf("a running sandbox's Card says\n%s\nwant\n%s", facts, want)
	}
	// The facts are beside the Card, in the page's column, and go when
	// the pointer does.
	h.rest("Research")
	card, _ := h.tt.Find("Research")
	fact, shown := h.tt.Find("Probe latency")
	if !shown || fact.X < h.d.sidebarWidth || fact.Y < card.Y-20 || fact.Y > card.Y+200 {
		t.Errorf("the Card is at %+v and its facts at %+v", card, fact)
	}
	h.park()
	if h.tt.HasText("Probe latency") {
		t.Errorf("with the pointer elsewhere the window still shows the facts: %q", h.tt.Texts())
	}

	h.d.Apply(sandbox.Update{SandboxID: "s3", Status: &sandbox.Status{State: sandbox.StateError, Error: "the image could not be built"}})
	h.tt.Frame()
	if facts := h.facts("Broken"); facts != "State|ERROR|Error|the image could not be built" {
		t.Errorf("the Card of a sandbox that failed says %s", facts)
	}
}

// The Card says which Interface is in use and what is expected there,
// and that there is none.
func TestTheInterfaceInUseIsShown(t *testing.T) {
	cfg := gated("s1", "Research")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{
		{Name: "utun4", ExpectedExitAddresses: []string{"203.0.113.7"}},
		{Name: "en0", AnyExitAddress: true},
	}
	h := newHarness(t, []sandbox.Config{cfg})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateOpen, ExitAddress: "203.0.113.7"}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{Open: true, Interface: "utun4", InterfaceChange: 1, ExitAddress: "203.0.113.7"}})
	h.tt.Frame()
	if facts := h.facts("Research"); !strings.Contains(facts, "Leaves by|utun4") || !strings.Contains(facts, "Expected|203.0.113.7") {
		t.Errorf("with the tunnel in use the Card says %s", facts)
	}
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{Open: true, Interface: "en0", InterfaceChange: 2, ExitAddress: "198.51.100.4"}})
	h.tt.Frame()
	if facts := h.facts("Research"); !strings.Contains(facts, "Leaves by|en0") || !strings.Contains(facts, "Expected|any exit address") {
		t.Errorf("with en0 in use the Card says %s", facts)
	}
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockNoInterface}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{LockReason: sandbox.LockNoInterface, InterfaceChange: 3}})
	h.tt.Frame()
	if facts := h.facts("Research"); !strings.Contains(facts, "State|GATE LOCKED [No Interface]") || !strings.Contains(facts, "Leaves by|nothing: none of its ways out is up") {
		t.Errorf("with no Interface in use the Card says %s", facts)
	}
	// The glyph of its state says the same to whoever does not see it.
	if h.count("GATE LOCKED [No Interface]") == 0 {
		t.Errorf("with no Interface in use the window shows %q", h.tt.Texts())
	}
}

// Where the Interface in use expects any exit address but a physical
// one, the Card says which those are, as the gatekeeper took note of
// them, and why the gate is locked while they are not known.
func TestThisMacsPhysicalExitAddressesAreShown(t *testing.T) {
	cfg := gated("s1", "Research")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: "utun9", AnyExitAddressButPhysical: true}}
	h := newHarness(t, []sandbox.Config{cfg, gated("s2", "Scraper")})
	h.tt.SetSize(1280, 900)
	shown := func(status sandbox.Status, gate sandbox.GateTelemetry) string {
		t.Helper()
		gate.Interface, gate.InterfaceChange = "utun9", 1
		h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &status})
		h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &gate})
		h.tt.Frame()
		return strings.Join(h.tt.Texts(), "|") + "|" + h.facts("Research")
	}

	// Not known: the Card says so in the app's own words.
	texts := shown(sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockPhysicalExitUnknown}, sandbox.GateTelemetry{LockReason: sandbox.LockPhysicalExitUnknown})
	for _, want := range []string{"GATE LOCKED [Physical Exit Address Unknown]", "Needs you|Gate locked: Physical Exit Address Unknown", "Expected|any but physical (not known)"} {
		if !strings.Contains(texts, want) {
			t.Errorf("with this Mac's physical exit addresses unknown the window does not say %q: %s", want, texts)
		}
	}
	if got := h.needs("s1"); got != "Gate locked: Physical Exit Address Unknown" {
		t.Errorf("the Card says %q", got)
	}
	// Known: named, one or several, or said to be none.
	open := sandbox.Status{State: sandbox.StateGateOpen, ExitAddress: "198.51.100.9"}
	for want, physical := range map[string][]string{
		"Expected|any but physical (203.0.113.9)":                   {"203.0.113.9"},
		"Expected|any but physical (203.0.113.10, 203.0.113.9)":     {"203.0.113.10", "203.0.113.9"},
		"Expected|any but physical (this Mac has no other way out)": nil,
	} {
		if texts := shown(open, sandbox.GateTelemetry{Open: true, ExitAddress: "198.51.100.9", PhysicalExitAddresses: physical, PhysicalExitKnown: true}); !strings.Contains(texts, want) {
			t.Errorf("with the physical exit addresses %v the Card does not say %q: %s", physical, want, texts)
		}
	}
	// At the window's smallest, with as much to say as there is, the
	// header keeps its controls, and the Card says every fact inside the
	// window.
	const width, height = 900, 600
	h.tt.SetSize(width, height)
	h.d.view("s1").running = cfg
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Clock: "2026-11-18T01:33:01Z"}})
	for _, what := range []string{"locked", "open"} {
		texts := shown(sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockPhysicalExitUnknown}, sandbox.GateTelemetry{LockReason: sandbox.LockPhysicalExitUnknown})
		expected := "any but physical (not known)"
		if what == "open" {
			texts = shown(sandbox.Status{State: sandbox.StateGateOpen, ExitAddress: "198.51.100.199"}, sandbox.GateTelemetry{Open: true, ExitAddress: "198.51.100.199",
				ProbeLatency: 1234 * time.Millisecond, PacketsPassed: 123456, PacketsRejected: 1234, PhysicalExitAddresses: []string{"203.0.113.119", "203.0.113.120"}, PhysicalExitKnown: true})
			expected = "any but physical (203.0.113.119, 203.0.113.120)"
		}
		if !strings.Contains(texts, "Expected|"+expected) {
			t.Fatalf("%s: the Card does not say %q: %s", what, expected, texts)
		}
		h.rest("Research")
		for _, fact := range []string{"Setup", "Stop", "Clone", "Destroy", "State", "Leaves by", "utun9", "Exit address", "Expected", expected, "Probe latency", "Packets", "Sandbox clock", "2026-11-18T01:33:01Z"} {
			box, ok := h.tt.Find(fact)
			if !ok {
				t.Fatalf("%s: at %dx%d the window does not show %q: %q", what, width, height, fact, h.tt.Texts())
			}
			if box.X < 0 || box.Y < 0 || box.X+box.W > width || box.Y+box.H > height || box.W <= 0 || box.H <= 0 {
				t.Errorf("%s: at %dx%d %q is at %+v, not inside the window", what, width, height, fact, box)
			}
		}
		h.park()
	}
	h.tt.SetSize(1280, 900)

	// A mismatch names the address, which is this Mac's
	// physical exit address.
	texts = shown(sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockExitAddressMismatch, ExitAddress: "203.0.113.9"},
		sandbox.GateTelemetry{LockReason: sandbox.LockExitAddressMismatch, ExitAddress: "203.0.113.9", PhysicalExitAddresses: []string{"203.0.113.9"}, PhysicalExitKnown: true})
	for _, want := range []string{"GATE LOCKED [Exit Address Mismatch]", "Exit address|203.0.113.9", "Expected|any but physical (203.0.113.9)"} {
		if !strings.Contains(texts, want) {
			t.Errorf("locked on this Mac's physical exit address the window does not say %q: %s", want, texts)
		}
	}
}

// A way out may expect this Mac's physical exit address, and no other: the
// form offers it, keeps it, and the Card says what is expected there, with
// this Mac's physical exit address as the gatekeeper has taken note of
// them.
func TestThisMacsPhysicalExitAddressIsExpected(t *testing.T) {
	cfg := gated("s1", "Research")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: "en0", PhysicalExitAddress: true}}
	rows := newDraft(cfg, nil)
	if got := rows.interfaces[0].expect; got != expectPhysical {
		t.Fatalf("a way out that expects this Mac's physical exit address shows %q", got)
	}
	back, errs := rows.config(cfg)
	if got := back.AllowedInterfaces; len(errs) != 0 || len(got) != 1 || !got[0].PhysicalExitAddress || got[0].Expectations() != 1 || len(back.Validate()) != 0 {
		t.Fatalf("the form gives back %+v (%v)", got, errs)
	}
	if expectations[len(expectations)-1] != expectPhysical || len(expectations) != 4 {
		t.Errorf("the form offers %q", expectations)
	}

	// Chosen in the form, it is what is saved.
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.choose("Expected at way out 1", expectations, expectPhysical)
	if h.tt.HasText("203.0.113.7") {
		t.Error("the addresses typed before are shown beside an expectation that takes none")
	}
	h.click("Save")
	h.settle("the save", func() bool { return len(h.backend.lastSaved()) == 1 })
	if got := h.backend.lastSaved()[0].AllowedInterfaces; len(got) != 1 || got[0].Name != "en0" || !got[0].PhysicalExitAddress || got[0].Expectations() != 1 {
		t.Fatalf("saved %+v", got)
	}
	if help := h.told("About Leaves by"); !strings.Contains(help, "or physical and no other") || strings.Contains(strings.ToLower(help), "interface") {
		t.Errorf("what is said about the rows: %s", help)
	}

	// What the Card says is expected.
	physical := sandbox.AllowedInterface{Name: "en0", PhysicalExitAddress: true}
	for want, gate := range map[string]sandbox.GateTelemetry{
		"physical (not known)":                {},
		"physical (there is none)":            {PhysicalExitKnown: true},
		"physical (203.0.113.7)":              {PhysicalExitKnown: true, PhysicalExitAddresses: []string{"203.0.113.7"}},
		"physical (203.0.113.7, 203.0.113.8)": {PhysicalExitKnown: true, PhysicalExitAddresses: []string{"203.0.113.7", "203.0.113.8"}},
	} {
		if got := expectedAt(physical, gate); got != want {
			t.Errorf("with %+v the Card says %q is expected, want %q", gate, got, want)
		}
	}
}

// At the window's smallest a row of the Allowed Interfaces is inside the
// form: its way out, Up and Remove on one line, and under them what is
// expected there, with the addresses beside it where addresses are, and
// nothing cut off.
func TestARowOfTheAllowedInterfacesFitsTheSmallestWindow(t *testing.T) {
	const width, height = 900, 600
	cfg := gated("s1", "Research")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{
		{Name: "utun9", AnyExitAddressButPhysical: true},
		{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7", "198.51.100.9", "198.51.100.10"}},
	}
	for _, size := range [][2]int{{width, height}, {1280, 860}} {
		h := newHarness(t, []sandbox.Config{cfg})
		h.tt.SetSize(size[0], size[1])
		inside := func(what string) ui.Rect {
			t.Helper()
			box, ok := h.tt.Find(what)
			if !ok {
				t.Fatalf("at %dx%d the form does not show %q: %q", size[0], size[1], what, h.tt.Texts())
			}
			if box.X < h.d.sidebarWidth || box.X+box.W > float32(size[0]) || box.W <= 0 || box.H <= 0 {
				t.Errorf("at %dx%d %q is at %+v, not inside the form", size[0], size[1], what, box)
			}
			return box
		}
		// A way out that is not up now is still the row's, and says so.
		if h.count("utun9 · not up now") != 1 || h.count(wayWiFi) != 1 {
			t.Errorf("at %dx%d the rows name their ways out as %q", size[0], size[1], h.tt.Texts())
		}
		for _, n := range []string{"1", "2"} {
			way, up, remove, expect := inside("Way out "+n), inside("Move way out "+n+" up"), inside("Remove way out "+n), inside("Expected at way out "+n)
			// One line, in this order, none over another, with room to
			// read the longest of this Mac's names.
			if !(way.X+way.W <= up.X && up.X+up.W <= remove.X) || way.W < 330 {
				t.Errorf("at %dx%d row %s: the way out is at %+v, Up at %+v and Remove at %+v", size[0], size[1], n, way, up, remove)
			}
			// Under it, from where the line begins.
			if expect.Y < way.Y+way.H || expect.X != way.X || expect.W < 190 {
				t.Errorf("at %dx%d row %s: what is expected is at %+v, want it under the line that begins at %v", size[0], size[1], n, expect, way.X)
			}
			if n == "1" {
				// With no address to ask for, it has the line.
				if _, asked := h.tt.Find("Expected exit addresses of way out " + n); asked || expect.X+expect.W != remove.X+remove.W {
					t.Errorf("at %dx%d row %s: what is expected is at %+v, want it to end with the line at %v", size[0], size[1], n, expect, remove.X+remove.W)
				}
				continue
			}
			// The addresses beside it, to the line's end, with room for
			// two of them.
			exits := inside("Expected exit addresses of way out " + n)
			if exits.Y != expect.Y || exits.X < expect.X+expect.W || exits.X+exits.W != remove.X+remove.W || exits.W < 240 {
				t.Errorf("at %dx%d row %s: the addresses are at %+v, want them beside %+v, to the line's end at %v", size[0], size[1], n, exits, expect, remove.X+remove.W)
			}
		}
	}
}

// A sandbox's Setup is its name, its network, its Shared Folder and the
// rest. Check starts the sandbox with its Setup still shown, and its
// gate says there whether the network is as required. The Header's
// Setup then gives the Panes, and the Setup again, without starting
// anything.
func TestCheckStartsTheSandboxWithItsSetupShown(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	v := h.d.view("s1")
	if got := h.order(partNetwork, sharedFolderField, "Clock Policy", "Resources"); got != partNetwork+" "+sharedFolderField+" Clock Policy Resources" {
		t.Errorf("the Setup's parts stand in the order %q", got)
	}
	if !h.tt.HasText("Not checked yet: Check starts the sandbox, and its gate says.") {
		t.Errorf("before the network was checked the Setup says %q", h.tt.Texts())
	}

	// Check saves the Setup and starts the sandbox: its page is still
	// its Setup.
	h.click("Check")
	h.settle("the backend to be asked to start", func() bool {
		h.backend.mu.Lock()
		defer h.backend.mu.Unlock()
		return len(h.backend.started) == 1 && h.backend.started[0].ID == "s1"
	})
	if len(h.backend.saved) != 1 || !v.showSetup || !h.tt.HasText(partNetwork) || !h.tt.HasText("Starting…") || h.tt.HasText("Pane 1") {
		t.Fatalf("after Check: %d saves, and the window shows %q", len(h.backend.saved), h.tt.Texts())
	}
	// A locked gate is said there. The sandbox runs: its first Pane has
	// its shell, behind the Setup.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockProbeUnanswered}})
	h.settle("the first Pane's shell", func() bool { return len(v.panes) == 1 && v.panes[0].session != nil })
	if !h.tt.HasText("Gate locked: Probe Unanswered") || h.tt.HasText("Terminal") || !h.tt.HasText("Running: Check starts it again where this changed; what is in its Panes ends with that.") {
		t.Errorf("with its gate locked the Setup says %q", h.tt.Texts())
	}
	// The gate opens: the line holds, and says what the gate does.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{Open: true, Interface: "en0", ExitAddress: "203.0.113.7"}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &open})
	h.tt.Frame()
	if !h.tt.HasText("Gate open · leaves by en0 · seen at 203.0.113.7") {
		t.Fatalf("with its gate open the Setup says %q", h.tt.Texts())
	}
	if _, holds := h.tt.Find("This holds"); !holds {
		t.Errorf("the open gate is not ticked: %q", h.tt.Texts())
	}

	// The Header's Setup gives the Panes. Nothing that is started
	// changed: the sandbox is not stopped and not started again, and
	// the shell is the one its start opened.
	h.click("Setup")
	if !h.tt.HasText("Pane 1") || !h.tt.HasText("Terminal") || h.tt.HasText(partNetwork) {
		t.Fatalf("the page of a running sandbox shows %q", h.tt.Texts())
	}
	if len(h.backend.started) != 1 || len(h.backend.stopped) != 0 || len(h.backend.opened()) != 1 {
		t.Errorf("showing the Panes started the sandbox %d times, stopped it %d times and opened %d sessions", len(h.backend.started), len(h.backend.stopped), len(h.backend.opened()))
	}
	// And the Setup is there to look at, behind its control.
	h.click("Setup")
	if !h.tt.HasText(partNetwork) || h.tt.HasText("Terminal") {
		t.Errorf("the Setup of a running sandbox shows %q", h.tt.Texts())
	}
	h.click("Setup")

	// A sandbox that stops by itself shows its Setup, and says why.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateError, Error: "the gatekeeper stopped"}})
	h.tt.Frame()
	if !h.tt.HasText(partNetwork) || !h.tt.HasText("the gatekeeper stopped") || h.tt.HasText("Pane 1") {
		t.Errorf("after the sandbox stopped by itself the window shows %q", h.tt.Texts())
	}
	// Start, from there, shows its Panes.
	h.click("Start")
	h.settle("the second start", func() bool { return len(h.backend.started) == 2 && !v.busy })
	h.runs("s1")
	if !h.tt.HasText("Terminal") || h.tt.HasText(partNetwork) {
		t.Errorf("after Start the window shows %q", h.tt.Texts())
	}
}

// Nothing of a running workstation is changed: what it mounts, what its
// gate expects. Check stops a sandbox that runs otherwise than its Setup
// now has it, and starts it again; one that runs as its Setup has it
// goes on, with its Panes.
func TestCheckStartsAgainWhatTheSetupChanged(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	// A stop is told as the orchestrator tells it: while the sandbox is
	// being stopped, before what stopped it goes on.
	h.backend.tell = func(u sandbox.Update) { h.queue <- func() { h.d.Apply(u) } }
	v := h.d.view("s1")
	starts := func() int {
		h.backend.mu.Lock()
		defer h.backend.mu.Unlock()
		return len(h.backend.started)
	}
	h.click("Check")
	h.settle("the start", func() bool { return starts() == 1 && !v.busy })
	h.runs("s1")
	first := h.connOf("s1", 0)

	// Check again, with nothing changed: it goes on running, and its
	// Pane's shell with it.
	h.click("Check")
	h.tt.Frame()
	if _, closed := first.state(); starts() != 1 || len(h.backend.stopped) != 0 || closed {
		t.Fatalf("Check with nothing changed started the sandbox %d times and stopped it %d times; its shell closed: %v", starts(), len(h.backend.stopped), closed)
	}
	// Another resolver: Check starts it again, its Setup still shown,
	// and what was in its Pane ended with the workstation.
	h.replace("Resolver", "9.9.9.9")
	h.click("Check")
	h.settle("the second start", func() bool { return starts() == 2 && !v.busy })
	if got := strings.Join(h.backend.done(0), ", "); !strings.HasSuffix(got, "save, stop s1") || !v.showSetup || h.backend.started[1].Gate.Resolver != "9.9.9.9" {
		t.Fatalf("after Check with another resolver the backend was asked to %s", got)
	}
	if _, closed := first.state(); !closed {
		t.Error("the shell of the workstation that was stopped is still open")
	}
	h.runs("s1")
	if second := h.connOf("s1", 0); second == first || len(h.backend.opened()) != 2 {
		t.Fatalf("started again, the sandbox shows the session %q, of %d opened", second.id, len(h.backend.opened()))
	}
	if !h.tt.HasText(partNetwork) || h.tt.HasText("Terminal") {
		t.Errorf("what the stop said took the Setup out of the page: %q", h.tt.Texts())
	}

	// Given a Shared Folder: Check stops the sandbox and starts it with
	// the folder.
	h.giveFolder("research")
	h.click("Check")
	h.settle("the third start", func() bool { return starts() == 3 && !v.busy })
	if got := h.backend.started[2]; got.SharedFolder != "research" || len(h.backend.stopped) != 2 {
		t.Fatalf("after Check with a Shared Folder: started with %q, stopped %d times", got.SharedFolder, len(h.backend.stopped))
	}
}

// terminalAt clicks in the terminal of a Pane of the sandbox shown,
// which gives it the keyboard.
func (h *harness) terminalAt(pane int) {
	h.t.Helper()
	cfg := h.d.config(h.d.selected)
	at, ok := h.tt.Find(paneName(pane) + " of " + cfg.Name)
	if !ok {
		h.t.Fatalf("no %s in the window: %q", paneName(pane), h.tt.Texts())
	}
	h.tt.ClickAt(at.X+at.W/2, at.Y+at.H/2+stripHeight/2)
	h.tt.Frame()
}

// A running sandbox's page is Panes of login shells: one at its start,
// and up to four, in two rows whose dividers are one line. Add Pane, in
// the Header, opens another; a Pane's strip closes it, and its shell
// with it; a shell that ends by itself leaves its Pane, which says so
// and offers another.
func TestPanes(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.tt.SetSize(1280, 900)
	h.runs("s1")
	v := h.d.view("s1")
	box := func(pane int) ui.Rect {
		t.Helper()
		at, ok := h.tt.Find(paneName(pane) + " of Research")
		if !ok {
			t.Fatalf("no %s in the window: %q", paneName(pane), h.tt.Texts())
		}
		return at
	}
	typed := func(pane int, text string) {
		t.Helper()
		conn := h.connOf("s1", pane)
		before, _ := conn.state()
		h.tt.Type(text)
		h.settle("what was typed to reach "+paneName(pane), func() bool { now, _ := conn.state(); return now == before+text })
	}
	add := func(want int) {
		t.Helper()
		h.click(addPaneLabel)
		h.settle("another Pane's shell", func() bool { return len(v.panes) == want && v.panes[want-1].session != nil })
		if conns := h.backend.opened(); len(conns) < want || conns[len(conns)-1].sandbox != "s1" {
			t.Fatalf("Add Pane opened %d sessions", len(conns))
		}
	}

	// One Pane fills the page, and is not to be closed: a running
	// sandbox has one.
	if one := box(0); one.X > h.d.sidebarWidth+8 || one.X+one.W < 1279 || one.Y+one.H < 899 || h.tt.HasText("Close Pane 1") {
		t.Fatalf("the first Pane is at %+v: %q", one, h.tt.Texts())
	}
	typed(0, "one")

	// A second stands beside it, and has the keyboard as it comes.
	add(2)
	one, two := box(0), box(1)
	if two.X < one.X+one.W || two.Y != one.Y || two.H != one.H || two.X+two.W < 1279 || !h.tt.HasText("Close Pane 1") || !h.tt.HasText("Close Pane 2") {
		t.Fatalf("with two Panes, the first is at %+v and the second at %+v: %q", one, two, h.tt.Texts())
	}
	typed(1, "two")
	// A click in a Pane's terminal gives it the keyboard.
	h.terminalAt(0)
	typed(0, "-again")

	// The third stands under them, as wide as the page; the fourth
	// beside it, under the second, their dividers one line. At four there
	// is no room for another.
	add(3)
	one, two, three := box(0), box(1), box(2)
	if three.Y < one.Y+one.H || three.X != one.X || three.X+three.W != two.X+two.W || three.Y+three.H < 899 {
		t.Fatalf("with three Panes they are at %+v, %+v and %+v", one, two, three)
	}
	add(4)
	one, two, three, four := box(0), box(1), box(2), box(3)
	if four.Y != three.Y || four.X != two.X || three.W != one.W || four.W != two.W || h.tt.HasText(addPaneLabel) {
		t.Fatalf("with four Panes they are at %+v, %+v, %+v and %+v: %q", one, two, three, four, h.tt.Texts())
	}
	typed(3, "four")

	// A Pane's strip closes it: its shell is hung up, the Panes after it
	// move up, and the others keep theirs.
	second, third, fourth := h.connOf("s1", 1), h.connOf("s1", 2), h.connOf("s1", 3)
	h.click("Close Pane 2")
	h.settle("the Pane to close", func() bool { _, closed := second.state(); return closed && len(v.panes) == 3 })
	if h.connOf("s1", 1) != third || h.connOf("s1", 2) != fourth || len(v.sessions) != 3 || !h.tt.HasText(addPaneLabel) {
		t.Fatalf("after Pane 2 closed the Panes show other sessions than the three that were left: %q", h.tt.Texts())
	}
	for i, conn := range []*fakeConn{h.connOf("s1", 0), third, fourth} {
		if _, closed := conn.state(); closed {
			t.Errorf("closing Pane 2 closed the session of what is now Pane %d", i+1)
		}
	}
	// Closing a Pane is no session that ended by itself.
	if why := h.needs("s1"); why != "" {
		t.Errorf("after a Pane was closed its sandbox needs its user for %q", why)
	}

	// A shell that ends by itself leaves its Pane, which says so and
	// offers another: none is started unasked.
	third.shell.Close()
	h.settle("the Pane to say that its shell ended", func() bool { return v.panes[1].session == nil && h.tt.HasText(paneEnded) })
	if len(v.panes) != 3 || len(v.sessions) != 2 || !h.tt.HasText("New Session in Pane 2") || h.tt.HasText("New Session in Pane 1") {
		t.Fatalf("after a shell ended the window shows %q", h.tt.Texts())
	}
	opened := len(h.backend.opened())
	for range 5 {
		h.settle("a frame", func() bool { return true })
	}
	if len(h.backend.opened()) != opened {
		t.Fatal("a shell was started unasked in the Pane whose shell ended")
	}
	h.click("New Session in Pane 2")
	h.settle("the Pane's new shell", func() bool { return v.panes[1].session != nil && len(h.backend.opened()) == opened+1 })
	if h.tt.HasText(paneEnded) || h.tt.HasText("New Session in Pane 2") {
		t.Errorf("with its new shell the Pane still says %q", h.tt.Texts())
	}
	typed(1, "anew")

	// A shell that cannot be started says why in its Pane, and is asked
	// for again there.
	h.backend.mu.Lock()
	h.backend.openErr = errors.New("the sandbox is not running")
	h.backend.mu.Unlock()
	h.click(addPaneLabel)
	h.settle("the Pane to say why it has no shell", func() bool { return len(v.panes) == 4 && h.tt.HasText("the sandbox is not running") })
	h.backend.mu.Lock()
	h.backend.openErr = nil
	h.backend.mu.Unlock()
	h.click("New Session in Pane 4")
	h.settle("the Pane's shell", func() bool { return v.panes[3].session != nil && !h.tt.HasText("the sandbox is not running") })

	// Down to one Pane, which stays.
	for len(v.panes) > 1 {
		n := len(v.panes)
		h.click("Close Pane " + strconv.Itoa(n))
		h.settle("the Pane to close", func() bool { return len(v.panes) == n-1 })
	}
	if h.tt.HasText("Close Pane 1") || len(v.sessions) != 1 {
		t.Errorf("with one Pane left the window shows %q, and the sandbox holds %d sessions", h.tt.Texts(), len(v.sessions))
	}

	// A sandbox that stops takes its Panes with it, and starts with one
	// again.
	last := h.connOf("s1", 0)
	h.click(addPaneLabel)
	h.settle("a second Pane", func() bool { return len(v.panes) == 2 && v.panes[1].session != nil })
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	h.settle("the sessions to close", func() bool { _, closed := last.state(); return closed && len(v.panes) == 0 })
	for _, conn := range h.backend.opened() {
		if _, closed := conn.state(); !closed {
			t.Errorf("the session %s outlived its sandbox", conn.id)
		}
	}
	h.runs("s1")
	if len(v.panes) != 1 {
		t.Errorf("started again, the sandbox has %d Panes", len(v.panes))
	}
}

// A shell that was being started when its sandbox stopped, or when its
// Pane was closed, is hung up when it comes, and shown nowhere.
func TestAShellThatComesTooLateIsHungUp(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	v := h.d.view("s1")
	// The sandbox runs, and stops before what the backend opened is
	// taken.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &open})
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(h.backend.opened()) < n {
			if time.Now().After(deadline) {
				t.Fatalf("the backend opened %d sessions, want %d", len(h.backend.opened()), n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor(1)
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	h.settle("the late shell to be hung up", func() bool { _, closed := h.backend.opened()[0].state(); return closed })
	if len(v.sessions) != 0 || len(v.panes) != 0 {
		t.Fatalf("a stopped sandbox holds %d sessions in %d Panes", len(v.sessions), len(v.panes))
	}

	// A Pane that is closed while its shell is being started.
	h.runs("s1")
	h.click(addPaneLabel)
	if len(v.panes) != 2 || !v.panes[1].opening {
		t.Fatalf("after Add Pane the sandbox has %d Panes", len(v.panes))
	}
	waitFor(3)
	h.click("Close Pane 2")
	h.settle("the late shell to be hung up", func() bool { _, closed := h.backend.opened()[2].state(); return closed })
	if len(v.sessions) != 1 || len(v.panes) != 1 {
		t.Errorf("after its Pane was closed the sandbox holds %d sessions in %d Panes", len(v.sessions), len(v.panes))
	}
}

// agentIn has the workstation of a sandbox say which agent runs in the
// session of each of its Panes, as the supervisor reports it: one name
// for each Pane, empty for a plain shell. The sessions that are Hidden
// are said to run what they ran.
func (h *harness) agentIn(id string, agents ...string) {
	h.t.Helper()
	v := h.d.view(id)
	status := sandbox.WorkstationStatus{Clock: "2026-11-20T09:00:00Z"}
	for _, s := range v.sessions {
		agent := s.agent
		for i, p := range v.panes {
			if p.session == s && i < len(agents) {
				agent = agents[i]
			}
		}
		if s.over {
			continue
		}
		status.Sessions = append(status.Sessions, sandbox.SessionState{ID: s.id, Agent: agent})
	}
	h.d.Apply(sandbox.Update{SandboxID: id, Workstation: &status})
	h.settle("a frame", func() bool { return true })
}

// drag drags what is labelled from onto what is labelled to, as a
// pointer does.
func (h *harness) drag(from, to string) {
	h.t.Helper()
	a, ok := h.tt.Find(from)
	b, ok2 := h.tt.Find(to)
	if !ok || !ok2 {
		h.t.Fatalf("dragging %q onto %q: the window shows %q", from, to, h.tt.Texts())
	}
	fromX, fromY, toX, toY := a.X+a.W/2, a.Y+a.H/2, b.X+b.W/2, b.Y+b.H/2
	h.tt.Press(fromX, fromY)
	for _, f := range []float32{0.2, 0.5, 0.8, 1} {
		h.tt.Move(fromX+(toX-fromX)*f, fromY+(toY-fromY)*f)
		h.tt.Frame()
	}
	h.tt.Release(toX, toY)
	h.settle("the drop", func() bool { return true })
}

// A session in which an agent runs is an Agent Session: its agent's
// glyph stands in the Header, and its Pane's strip names the agent. The
// app learns which agent it is from the workstation, which reads the
// names of the session's processes, and from nothing the session
// printed. It can be Hidden: its Pane gets a login shell, and the
// session goes on behind its glyph, which shows it again.
func TestAgentSessionsHaveGlyphsInTheHeader(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.tt.SetSize(1280, 900)
	h.runs("s1")
	v := h.d.view("s1")
	glyph := func(label string) ui.Rect {
		t.Helper()
		at, ok := h.tt.Find(label)
		if !ok || at.Y < 0 || at.Y+at.H > headerHeight {
			t.Fatalf("the glyph %q is at %+v, not in the Header: %q", label, at, h.tt.Texts())
		}
		return at
	}
	typed := func(conn *fakeConn, text string) {
		t.Helper()
		before, _ := conn.state()
		h.tt.Type(text)
		h.settle("what was typed to reach the session "+conn.id, func() bool { now, _ := conn.state(); return now == before+text })
	}
	open := func(conn *fakeConn) bool { _, closed := conn.state(); return !closed }

	// A plain shell has no glyph, and nothing to hide.
	if h.tt.HasText("Claude Code") || h.tt.HasText(hideLabel) || len(v.agentSessions()) != 0 {
		t.Fatalf("with a plain shell the window shows %q", h.tt.Texts())
	}
	// What the workstation says of a session the app does not hold, or
	// of none, changes nothing.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Sessions: []sandbox.SessionState{{ID: "not-mine", Agent: "codex"}}}})
	h.tt.Frame()
	if len(v.agentSessions()) != 0 {
		t.Fatal("a session the app did not open was taken for one of the sandbox's")
	}

	// Its user starts Claude Code in the Pane: the workstation says so,
	// and the session has its glyph, its Pane the agent's name and Hide.
	agent := h.connOf("s1", 0)
	h.agentIn("s1", "claude")
	at := glyph("Claude Code, in Pane 1")
	if at.W != sessionGlyph || at.H != sessionGlyph || h.count("Claude Code") != 1 || !h.tt.HasText("Hide the session of Pane 1") {
		t.Fatalf("with an agent in Pane 1 its glyph is at %+v and the window shows %q", at, h.tt.Texts())
	}
	if facts := h.facts("Research"); !strings.Contains(facts, "|Agents|Claude Code|") {
		t.Errorf("the Card says %s", facts)
	}
	if told := h.told("Claude Code, in Pane 1"); !strings.Contains(told, "Claude Code · in Pane 1: drag it onto another Pane to move it there") {
		t.Errorf("the glyph says %s", told)
	}
	h.park()

	// Hide: the session goes on, behind its glyph, and its Pane gets a
	// login shell of its own, which has the keyboard.
	h.click("Hide the session of Pane 1")
	h.settle("the Pane's own shell", func() bool { return v.panes[0].session != nil && v.panes[0].session.id != agent.id })
	shell := h.connOf("s1", 0)
	if !open(agent) || len(v.sessions) != 2 || h.tt.HasText(hideLabel+" the session") {
		t.Fatalf("after Hide the agent's session is open: %v, of %d sessions: %q", open(agent), len(v.sessions), h.tt.Texts())
	}
	glyph("Claude Code, hidden")
	if told := h.told("Claude Code, hidden"); !strings.Contains(told, "hidden: click to show it in a new Pane, or drag it onto a Pane") {
		t.Errorf("the hidden glyph says %s", told)
	}
	h.park()
	typed(shell, "ls")

	// A click on the glyph of a Hidden session shows it in a new Pane,
	// with the keyboard: no shell is started for it.
	h.click("Claude Code, hidden")
	h.settle("the session's Pane", func() bool {
		return len(v.panes) == 2 && v.panes[1].session != nil && v.panes[1].session.id == agent.id
	})
	if len(h.backend.opened()) != 2 {
		t.Fatalf("showing a Hidden session opened %d sessions in all", len(h.backend.opened()))
	}
	glyph("Claude Code, in Pane 2")
	typed(agent, "hello")

	// Dragged onto another Pane, a session that is shown changes places
	// with what that Pane shows: nothing ends.
	h.drag("Claude Code, in Pane 2", "Pane 1 of Research")
	if v.panes[0].session == nil || v.panes[0].session.id != agent.id || v.panes[1].session == nil || v.panes[1].session.id != shell.id || !open(agent) || !open(shell) {
		t.Fatalf("after the glyph was dropped on Pane 1 the Panes show other sessions than the two, changed over: %q", h.tt.Texts())
	}
	glyph("Claude Code, in Pane 1")
	typed(agent, " again")

	// Closing the Pane of an Agent Session hides the session; closing
	// that of a plain shell ends it.
	h.click("Close Pane 1")
	h.settle("the Pane to close", func() bool { return len(v.panes) == 1 })
	if !open(agent) || !open(shell) || len(v.sessions) != 2 {
		t.Fatalf("closing an Agent Session's Pane: the agent's session is open: %v", open(agent))
	}
	glyph("Claude Code, hidden")

	// Dropped on a Pane, a Hidden session takes it: the plain shell that
	// was there ends, since nothing stands for it in the Header.
	h.drag("Claude Code, hidden", "Pane 1 of Research")
	h.settle("the shell to end", func() bool { return !open(shell) && len(v.sessions) == 1 })
	if v.panes[0].session.id != agent.id || !open(agent) {
		t.Fatalf("a Hidden session dropped on a Pane: %q", h.tt.Texts())
	}

	// A second agent, in a second Pane. Dropped on its Pane, the first,
	// Hidden again, takes it, and the second is Hidden in its turn.
	h.click(addPaneLabel)
	h.settle("a second Pane's shell", func() bool { return len(v.panes) == 2 && v.panes[1].session != nil })
	other := h.connOf("s1", 1)
	h.agentIn("s1", "claude", "codex")
	glyph("Codex, in Pane 2")
	// The glyphs stand in the order their sessions were opened.
	if first, second := glyph("Claude Code, in Pane 1"), glyph("Codex, in Pane 2"); first.X >= second.X {
		t.Errorf("the glyphs are at %+v and %+v, want the order their sessions were opened in", first, second)
	}
	h.click("Hide the session of Pane 1")
	h.settle("the Pane's own shell", func() bool { return v.panes[0].session != nil && v.panes[0].session.id != agent.id })
	h.drag("Claude Code, hidden", "Pane 2 of Research")
	if v.panes[1].session.id != agent.id || v.paneOf(v.sessions[slices.IndexFunc(v.sessions, func(s *session) bool { return s.id == other.id })]) != nil || !open(other) || !open(agent) {
		t.Fatalf("a Hidden session dropped on another Agent Session's Pane: %q", h.tt.Texts())
	}
	glyph("Codex, hidden")
	glyph("Claude Code, in Pane 2")

	// An agent that quits in sight leaves a plain shell: its glyph goes,
	// and its session stays in its Pane.
	h.agentIn("s1", "", "")
	if h.tt.HasText("Claude Code") || !open(agent) || v.panes[1].session.id != agent.id || len(v.agentSessions()) != 1 {
		t.Fatalf("after the agent in sight quit the window shows %q", h.tt.Texts())
	}
	glyph("Codex, hidden")

	// With four Panes there is no room for a Hidden session to show
	// itself: its glyph says to drag it, and a click does nothing.
	for len(v.panes) < maxPanes {
		n := len(v.panes)
		h.click(addPaneLabel)
		h.settle("another Pane's shell", func() bool { return len(v.panes) == n+1 && v.panes[n].session != nil })
	}
	if told := h.told("Codex, hidden"); !strings.Contains(told, "Codex · hidden: drag it onto a Pane to show it there") {
		t.Errorf("with four Panes the hidden glyph says %s", told)
	}
	h.park()
	h.click("Codex, hidden")
	h.settle("a frame", func() bool { return true })
	if len(v.panes) != maxPanes {
		t.Fatalf("a click on a hidden glyph made a fifth Pane")
	}
	glyph("Codex, hidden")

	// A sandbox that stops takes every session with it, the Hidden ones
	// too.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	h.settle("the sessions to close", func() bool { return len(v.sessions) == 0 })
	for _, conn := range h.backend.opened() {
		if open(conn) {
			t.Errorf("the session %s outlived its sandbox", conn.id)
		}
	}
}

// A Hidden session that asks for its user, or whose agent ended, wears
// the Attention mark on its glyph, and its sandbox's Card wears it,
// until the session is looked at: shown in a Pane. An agent that ended
// out of sight keeps its glyph until then, since what it last said is
// on that screen.
func TestAHiddenSessionAsksThroughItsGlyph(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.tt.SetSize(1280, 900)
	h.runs("s1")
	v := h.d.view("s1")
	hidden := func(agent string) *fakeConn {
		t.Helper()
		conn := h.connOf("s1", 0)
		h.agentIn("s1", agent)
		h.click("Hide the session of Pane 1")
		h.settle("the Pane's own shell", func() bool { return v.panes[0].session != nil && v.panes[0].session.id != conn.id })
		return conn
	}

	// It rings out of sight: its glyph says so, and the sandbox needs
	// its user. Shown, it has been looked at.
	agent := hidden("opencode")
	agent.shell.Write([]byte("\a"))
	h.settle("the ring to be told", func() bool { return h.needs("s1") == "A session asks for you" })
	if _, ok := h.tt.Find("OpenCode, hidden, asks for you"); !ok || h.marks() != 1 || !h.markedAfter("Research") {
		t.Fatalf("a Hidden session rang: the window shows %q", h.tt.Texts())
	}
	h.click("OpenCode, hidden, asks for you")
	h.settle("the session to be looked at", func() bool { return h.needs("s1") == "" })
	if _, ok := h.tt.Find("OpenCode, in Pane 2"); !ok || h.marks() != 0 {
		t.Fatalf("after it was shown the window shows %q", h.tt.Texts())
	}
	h.click("Close Pane 1")
	h.settle("the Pane to close", func() bool { return len(v.panes) == 1 })

	// Its agent ends out of sight: the session keeps its glyph, with the
	// mark, and says so. Looked at, it is a plain shell's.
	h.click("Hide the session of Pane 1")
	h.settle("the Pane's own shell", func() bool { return v.panes[0].session != nil && v.panes[0].session.id != agent.id })
	h.agentIn("s1", "")
	// The workstation says of the Hidden session that no agent runs in
	// it any more.
	status := sandbox.WorkstationStatus{Sessions: []sandbox.SessionState{{ID: agent.id}, {ID: v.panes[0].session.id}}}
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &status})
	h.settle("the end to be told", func() bool { return h.needs("s1") == "An agent ended" })
	if _, ok := h.tt.Find("OpenCode, hidden, its agent ended"); !ok || h.marks() != 1 {
		t.Fatalf("an agent ended out of sight: the window shows %q", h.tt.Texts())
	}
	if facts := h.facts("Research"); !strings.Contains(facts, "Needs you|An agent ended") {
		t.Errorf("the Card says %s", facts)
	}
	// Said again, it is still so.
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &status})
	h.settle("a frame", func() bool { return true })
	h.click("OpenCode, hidden, its agent ended")
	h.settle("the session to be looked at", func() bool { return h.needs("s1") == "" && len(v.agentSessions()) == 0 })
	if h.tt.HasText("OpenCode") || len(v.panes) != 2 || v.panes[1].session.id != agent.id {
		t.Fatalf("after the session whose agent ended was shown the window shows %q", h.tt.Texts())
	}
	if _, closed := agent.state(); closed {
		t.Error("the session whose agent ended was closed when it was looked at")
	}
	h.click("Close Pane 1")
	h.settle("the Pane to close", func() bool { return len(v.panes) == 1 })

	// Its shell ends out of sight, with its agent: the session is kept,
	// for its last screen, until it is looked at; its Pane then offers a
	// new session in its place.
	h.agentIn("s1", "grok")
	h.click("Hide the session of Pane 1")
	h.settle("the Pane's own shell", func() bool { return v.panes[0].session != nil && v.panes[0].session.id != agent.id })
	agent.shell.Write([]byte("the agent's last words"))
	screen := v.sessions[0].term
	h.settle("its last words", func() bool { return strings.Contains(screen.Text(), "last words") })
	agent.shell.Close()
	h.settle("the end to be told", func() bool { return h.needs("s1") == "An agent ended" })
	if _, ok := h.tt.Find("Grok, hidden, its agent ended"); !ok || len(v.sessions) != 2 {
		t.Fatalf("a Hidden session's shell ended: the window shows %q", h.tt.Texts())
	}
	h.click("Grok, hidden, its agent ended")
	h.settle("the session to be looked at", func() bool { return h.needs("s1") == "" && len(v.panes) == 2 })
	if !strings.Contains(v.panes[1].session.term.Text(), "last words") || !h.tt.HasText("New Session in Pane 2") || h.tt.HasText("Grok") {
		t.Fatalf("the session that ended out of sight shows %q", h.tt.Texts())
	}
	opened := len(h.backend.opened())
	h.click("New Session in Pane 2")
	h.settle("the Pane's new shell", func() bool {
		return len(h.backend.opened()) == opened+1 && v.panes[1].session != nil && v.panes[1].session.id != agent.id && len(v.sessions) == 2
	})
}

// An agent raises its hand by the sandbox's Link, which names its
// Terminal Session: the session then asks for its user as one that rang
// does, on its sandbox's Card and, where it is Hidden, on its glyph,
// until it is looked at. It carries no words, and a hand of a session
// the sandbox does not hold marks nothing.
func TestARaisedHandMarksItsSession(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research"), gated("s2", "Scraper")})
	h.tt.SetSize(1280, 900)
	h.runs("s1", "s2")
	v := h.d.view("s1")
	agent := h.connOf("s1", 0)
	raise := func(sandboxID, session string) {
		h.d.Apply(sandbox.Update{SandboxID: sandboxID, Hand: &sandbox.Hand{Session: session}})
		h.settle("a frame", func() bool { return true })
	}

	// In sight, it is seen as it is raised.
	raise("s1", agent.id)
	if why := h.needs("s1"); why != "" || h.marks() != 0 {
		t.Fatalf("a hand raised in sight: %q, %d marks", why, h.marks())
	}
	// With another sandbox shown, it marks its sandbox's Card until the
	// session is looked at.
	h.click("Scraper")
	raise("s1", agent.id)
	if why := h.needs("s1"); why != "A session asks for you" || h.marks() != 1 || !h.markedAfter("Research") {
		t.Fatalf("a hand raised while another sandbox was shown: %q, %d marks", why, h.marks())
	}
	if why := h.needs("s2"); why != "" {
		t.Errorf("the sandbox that was shown needs its user for %q", why)
	}
	// A hand of a session the sandbox does not hold is nobody's: not
	// one of another sandbox's either.
	raise("s2", agent.id)
	raise("s2", "no-such-session")
	if why := h.needs("s2"); why != "" {
		t.Errorf("a hand of a session that is not the sandbox's marked it: %q", why)
	}
	h.click("Research")
	h.settle("the session to be looked at", func() bool { return h.needs("s1") == "" })

	// Hidden, the session wears the mark on its glyph.
	h.agentIn("s1", "claude")
	h.click("Hide the session of Pane 1")
	h.settle("the Pane's own shell", func() bool { return v.panes[0].session != nil && v.panes[0].session.id != agent.id })
	raise("s1", agent.id)
	if _, ok := h.tt.Find("Claude Code, hidden, asks for you"); !ok || h.marks() != 1 || h.needs("s1") != "A session asks for you" {
		t.Fatalf("a Hidden session's hand: the window shows %q", h.tt.Texts())
	}
	if facts := h.facts("Research"); !strings.Contains(facts, "Needs you|A session asks for you") {
		t.Errorf("the Card says %s", facts)
	}
	h.click("Claude Code, hidden, asks for you")
	h.settle("the session to be looked at", func() bool { return h.needs("s1") == "" && h.marks() == 0 })
}

// marks is how many Attention marks the window shows.
func (h *harness) marks() int {
	n := 0
	for _, text := range h.tt.Texts() {
		if text == attentionLabel {
			n++
		}
	}
	return n
}

// markedAfter is whether the Attention mark is what the window shows
// next after a text: the mark of a Card comes right after its name.
func (h *harness) markedAfter(text string) bool {
	texts := h.tt.Texts()
	for i, other := range texts {
		if other == text && i+1 < len(texts) && texts[i+1] == attentionLabel {
			return true
		}
	}
	return false
}

// needs is whether a sandbox needs its user, and why.
func (h *harness) needs(id string) string {
	if needs, why := h.d.view(id).needsUser(); needs {
		return why
	}
	return ""
}

// A Terminal Session that rings, or sends a notification, while it is
// not in sight marks its sandbox, until the user looks at it. The app
// learns that it did from the terminal, and shows nothing of what the
// notification said.
func TestASessionThatRingsMarksItsSandbox(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research"), gated("s2", "Scraper")})
	h.tt.SetSize(1280, 900)
	h.runs("s1", "s2")
	v := h.d.view("s1")
	shell, screen := h.connOf("s1", 0), v.panes[0].session.term
	said := 0
	// say has the session's program print something, and waits until the
	// terminal has told the app of it: by then a frame has been built
	// with the session in sight or out of it.
	say := func(what string) {
		t.Helper()
		said++
		tag := "said" + strconv.Itoa(said)
		shell.shell.Write([]byte(what + tag))
		h.settle("the terminal to take "+tag, func() bool { return strings.Contains(screen.Text(), tag) })
		// The terminal tells of a ring on a goroutine of its own.
		for range 20 {
			time.Sleep(2 * time.Millisecond)
			h.settle("a frame", func() bool { return true })
		}
	}

	// In sight, a ring is seen as it comes: nothing is marked.
	say("\a")
	if why := h.needs("s1"); why != "" || h.marks() != 0 {
		t.Fatalf("a session in sight rang: the sandbox needs its user for %q, and the window shows %d marks", why, h.marks())
	}

	// With another sandbox shown, it marks its sandbox's Card, until the
	// user looks at it again.
	h.click("Scraper")
	say("\a")
	if why := h.needs("s1"); why != "A session asks for you" || h.marks() != 1 || !h.markedAfter("Research") {
		t.Fatalf("a session rang while another sandbox was shown: its sandbox needs its user for %q, with %d marks", why, h.marks())
	}
	if why := h.needs("s2"); why != "" {
		t.Errorf("the sandbox that was shown needs its user for %q", why)
	}
	h.click("Research")
	if why := h.needs("s1"); why != "" || h.marks() != 0 {
		t.Errorf("after the user looked: %q, %d marks", why, h.marks())
	}

	// A notification marks as a ring does, and its words stay in the
	// terminal: OSC 9, and OSC 777.
	for _, notification := range []string{"\x1b]9;PWNED-BY-OSC9\x07", "\x1b]777;notify;PWNED-TITLE;PWNED-BODY\x07"} {
		h.click("Scraper")
		say(notification)
		if why := h.needs("s1"); why != "A session asks for you" {
			t.Fatalf("a session sent %q while another sandbox was shown: its sandbox needs its user for %q", notification, why)
		}
		if shown := strings.Join(h.tt.Texts(), "|"); strings.Contains(shown, "PWNED") {
			t.Fatalf("the window shows a notification's words: %q", shown)
		}
		h.click("Research")
		if why := h.needs("s1"); why != "" {
			t.Errorf("after the user looked: %q", why)
		}
	}

	// A session in any Pane that is shown is in sight.
	h.click(addPaneLabel)
	h.settle("a second Pane", func() bool { return len(v.panes) == 2 && v.panes[1].session != nil })
	say("\a")
	if why := h.needs("s1"); why != "" || h.marks() != 0 {
		t.Errorf("a session rang in a Pane beside the one with the keyboard: %q, %d marks", why, h.marks())
	}

	// Behind the Setup, nothing of the Sandbox Terminal is in sight.
	h.click("Setup")
	h.settle("the Setup", func() bool { return h.tt.HasText("Leaves by") })
	say("\a")
	if why := h.needs("s1"); why != "A session asks for you" || h.marks() != 1 {
		t.Errorf("a session rang behind the Setup: %q, %d marks", why, h.marks())
	}
	h.click("Setup")
	h.settle("a frame", func() bool { return true })
	if why := h.needs("s1"); why != "" {
		t.Errorf("after the Sandbox Terminal was shown again: %q", why)
	}

	// A locked gate is the weightier reason, and looking does not open it.
	h.click("Scraper")
	say("\a")
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockExitAddressMismatch}})
	if why := h.needs("s1"); why != "Gate locked: Exit Address Mismatch" {
		t.Errorf("a locked gate and a ring: %q", why)
	}
	h.click("Research")
	h.settle("a frame", func() bool { return true })
	if why := h.needs("s1"); why != "Gate locked: Exit Address Mismatch" {
		t.Errorf("a locked gate that was looked at: %q", why)
	}
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateOpen}})
	if why := h.needs("s1"); why != "" {
		t.Errorf("once the gate opened: %q", why)
	}
}

// No two sandboxes have one name: a Card is told from another by its
// name. A new sandbox and a clone get a name that is free.
func TestNamesTellSandboxesApart(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research"), gated("s2", "Writer")})
	h.click("Writer")
	saves := len(h.backend.saved)
	h.replace("Name", "research")
	h.click("Save")
	h.tt.Frame()
	if !strings.Contains(strings.Join(h.tt.Texts(), "|"), "another sandbox has this name") {
		t.Errorf("the form does not say that another sandbox has the name: %q", h.tt.Texts())
	}
	if len(h.backend.saved) != saves || h.d.configs[1].Name != "Writer" {
		t.Fatalf("a name somebody has was saved: %+v", h.d.configs[1])
	}
	h.replace("Name", "Second Writer")
	h.click("Save")
	h.settle("the name to be saved", func() bool { return len(h.backend.saved) == saves+1 && h.d.configs[1].Name == "Second Writer" })

	// A new sandbox and a clone take names nobody has.
	h.d.configs[0].Name = "Sandbox 3"
	h.d.create()
	h.d.clone("s2")
	h.d.clone("s2")
	var names []string
	for _, cfg := range h.d.configs[2:] {
		names = append(names, cfg.Name)
	}
	if got := strings.Join(names, "|"); got != "Sandbox 1|Second Writer copy|Second Writer copy 2" {
		t.Errorf("the new sandboxes are named %s", got)
	}
}

// A Terminal Session that ends by itself while its sandbox is not shown
// marks the sandbox, until it is shown. One whose Pane the user closed,
// or that its sandbox's stop ended, marks nothing.
func TestASessionThatEndsMarksItsSandbox(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research"), gated("s2", "Scraper")})
	h.tt.SetSize(1280, 900)
	h.runs("s1", "s2")
	v := h.d.view("s1")
	// another opens a second Pane, whose shell the test then ends.
	another := func() *fakeConn {
		t.Helper()
		for len(v.panes) > 1 {
			n := len(v.panes)
			h.click("Close Pane " + strconv.Itoa(n))
			h.settle("the Pane to close", func() bool { return len(v.panes) == n-1 })
		}
		h.click(addPaneLabel)
		h.settle("a second Pane's shell", func() bool { return len(v.panes) == 2 && v.panes[1].session != nil })
		return h.connOf("s1", 1)
	}

	// Its shell exits while its sandbox is shown: its Pane says so, and
	// there is nothing more to say.
	shell := another()
	shell.shell.Close()
	h.settle("the session to go", func() bool { return v.panes[1].session == nil })
	h.settle("a frame", func() bool { return true })
	if why := h.needs("s1"); why != "" || !h.tt.HasText(paneEnded) {
		t.Errorf("a session ended in sight: %q; the window shows %q", why, h.tt.Texts())
	}

	// While another sandbox is shown: the sandbox says so until it is.
	shell = another()
	h.click("Scraper")
	shell.shell.Close()
	h.settle("the session to go", func() bool { return v.panes[1].session == nil })
	if why := h.needs("s1"); why != "A session ended" {
		t.Fatalf("a session ended while another sandbox was shown: %q", why)
	}
	if why := h.needs("s2"); why != "" {
		t.Errorf("the sandbox that was shown needs its user for %q", why)
	}
	h.click("Research")
	h.settle("a frame", func() bool { return true })
	if why := h.needs("s1"); why != "" {
		t.Errorf("after the sandbox was shown: %q", why)
	}

	// A session whose Pane the user closed did not end by itself.
	another()
	h.click("Close Pane 2")
	h.settle("the Pane to close", func() bool { return len(v.panes) == 1 })
	h.click("Scraper")
	for range 10 {
		time.Sleep(2 * time.Millisecond)
		h.settle("a frame", func() bool { return true })
	}
	if why := h.needs("s1"); why != "" {
		t.Errorf("a session whose Pane the user closed: %q", why)
	}

	// Neither did one its sandbox's stop ended.
	h.click("Research")
	another()
	h.click("Scraper")
	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateStopped}})
	h.settle("the sessions to close", func() bool { return len(v.sessions) == 0 })
	for range 10 {
		time.Sleep(2 * time.Millisecond)
		h.settle("a frame", func() bool { return true })
	}
	if why := h.needs("s1"); why != "" {
		t.Errorf("a session that its sandbox's stop ended: %q", why)
	}
}

// The app reads nothing a Terminal Session prints. The dashboard shows a
// terminal, and takes from it that its program rang, sent a notification
// or ended, and the address of a link its user Command-clicked there,
// which one function takes: no screen, no history, no title, no folder,
// no link the pointer only rests on, and not what a notification says.
// Only the tests read a screen.
func TestTheDashboardReadsNoTerminal(t *testing.T) {
	// What a terminal gives of what its program printed, each a call
	// without arguments.
	reads := map[string]bool{
		"Text": true, "Snapshot": true, "History": true, "Title": true, "Dir": true,
		"NewMirror": true, "SearchStatus": true, "HoveredLink": true,
	}
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	files, shows, notified, links := 0, 0, 0, 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			files++
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "terminal" && sel.Sel.Name == "View" {
						shows++
					}
					if reads[sel.Sel.Name] && len(n.Args) == 0 {
						t.Errorf("%s calls %s(): the dashboard reads what a Terminal Session printed", name, sel.Sel.Name)
					}
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || i >= len(n.Rhs) {
							continue
						}
						switch sel.Sel.Name {
						case "OnTitle":
							t.Errorf("%s takes a terminal's title, which its program wrote", name)
						case "OpenLink":
							// The one taker of a link: openLink, which
							// TestALinkOpensInTheSandboxsBrowser holds.
							links++
							if fn, ok := n.Rhs[i].(*ast.SelectorExpr); !ok || fn.Sel.Name != "openLink" {
								t.Errorf("%s: a link that was clicked is taken by something other than openLink", name)
							}
						case "OnNotify":
							notified++
							fn, ok := n.Rhs[i].(*ast.FuncLit)
							if !ok {
								t.Errorf("%s: OnNotify is not a function written there, whose parameters could be seen", name)
								continue
							}
							for _, field := range fn.Type.Params.List {
								for _, param := range field.Names {
									if param.Name != "_" {
										t.Errorf("%s takes %q of a notification, which the terminal's program wrote", name, param.Name)
									}
								}
							}
						}
					}
				case *ast.KeyValueExpr:
					if key, ok := n.Key.(*ast.Ident); ok && (key.Name == "OnTitle" || key.Name == "OnNotify" || key.Name == "OpenLink") {
						t.Errorf("%s sets %s where the test does not look at it", name, key.Name)
					}
				}
				return true
			})
		}
	}
	if files < 10 || shows != 1 || notified != 1 || links != 1 {
		t.Errorf("read %d files, with %d views of a terminal, %d takers of a notification and %d of a link: the test is not reading the dashboard", files, shows, notified, links)
	}
}

// A link that a sandbox's user Command-clicks in a Pane opens in a new
// Browser Tab of the sandbox's own browser, which is shown: a web
// address, as it was printed, where the sandbox has a browser. Without
// the key nothing opens. Any other link, and every link of a sandbox
// that has no browser, the terminal opens with this Mac's. The address
// reaches the browser and is in the window only where the browser's own
// words are: the Address Bar.
func TestALinkOpensInTheSandboxsBrowser(t *testing.T) {
	browser := browserPack(t)
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg, gated("s2", "Plain")}, browser)
	h.tt.SetSize(1280, 900)
	home := viewport.BrowserTab{ID: "t1", Title: "Example Domain", Address: "https://example.com/"}
	h.backend.browse(home)
	h.d.view("s1").running = cfg
	h.runs("s1")
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Daemons: []sandbox.DaemonState{{Pack: browser.Name, Ready: true}}}})
	v := h.d.view("s1")
	vp := v.viewportOf(browser)
	h.settle("the browser's first Browser Tab", func() bool { return vp.address == home.Address && v.panes[0].session != nil })

	// prints fills the screen of the sandbox shown with one hyperlink,
	// whose words say nothing of where it leads, and click clicks in the
	// middle of it.
	prints := func(id, address string) {
		t.Helper()
		screen := h.d.view(id).panes[0].session.term
		h.connOf(id, 0).shell.Write([]byte("\x1b[2J\x1b[H\x1b]8;;" + address + "\x1b\\" + strings.Repeat("Click here to authenticate ", 400) + "\x1b]8;;\x1b\\"))
		h.settle("the link to be printed", func() bool { return strings.Contains(screen.Text(), "authenticate Click") })
	}
	click := func(mods ui.Modifiers, name string) {
		t.Helper()
		at, ok := h.tt.Find("Pane 1 of " + name)
		if !ok {
			t.Fatalf("no Pane in the window: %q", h.tt.Texts())
		}
		h.tt.ClickAtWith(mods, at.X+at.W/2, at.Y+at.H/2+stripHeight/2)
		h.tt.Frame()
	}

	// An address with escapes of its own, as one to sign in at has.
	link := "https://accounts.example.com/o/auth?redirect_uri=http%3A%2F%2Flocalhost%3A8085%2Fcallback&scope=a%20b+c#state"
	prints("s1", link)
	click(0, "Research")
	h.settle("a frame", func() bool { return true })
	if got := h.backend.linksOpened(); len(got) != 0 || len(h.tt.OpenedURLs()) != 0 {
		t.Fatalf("a click without Command opened %q in the browser and %q with this Mac's", got, h.tt.OpenedURLs())
	}
	click(ui.Super, "Research")
	h.settle("the link to open in the sandbox's browser", func() bool { return len(h.backend.linksOpened()) == 1 })
	if got := h.backend.linksOpened(); got[0] != "s1 "+link {
		t.Fatalf("the browser was asked for %q, want the link as it was printed", got)
	}
	h.settle("the new Browser Tab to be shown", func() bool {
		opens := h.backend.opens()
		return len(opens) == 2 && opens[1].tab == "link1" && vp.address == link && h.tt.HasText("Sign in")
	})
	if opened := h.tt.OpenedURLs(); len(opened) != 0 {
		t.Errorf("this Mac was asked to open %q", opened)
	}
	// The address is the Address Bar's, and nowhere else in the window.
	for _, text := range h.tt.Texts() {
		if strings.Contains(text, "accounts.example.com") && text != link {
			t.Errorf("the window says %q", text)
		}
	}

	// A link that is no web address is left to the terminal, which
	// opens it with this Mac's.
	prints("s1", "mailto:someone@example.com")
	click(ui.Super, "Research")
	h.settle("the link that is no web address to be opened with this Mac's", func() bool { return slices.Equal(h.tt.OpenedURLs(), []string{"mailto:someone@example.com"}) })
	if got := h.backend.linksOpened(); len(got) != 1 {
		t.Errorf("the browser was asked for %q", got)
	}

	// A browser that opens nothing: that is said, over its frames, and
	// trying again shows the browser as it was.
	h.backend.mu.Lock()
	h.backend.tabErr = errors.New("the browser does not answer")
	h.backend.mu.Unlock()
	prints("s1", link)
	click(ui.Super, "Research")
	h.settle("the failure to be said", func() bool { return h.tt.HasText("The Browser Tab was not opened: the browser does not answer") })
	h.click("Try Again")
	h.settle("the browser again", func() bool { _, ok := h.tt.Find("Web Test Browser viewport"); return ok })

	// A sandbox without a browser: its link is this Mac's to open.
	h.d.selected = "s2"
	h.runs("s2")
	h.settle("the other sandbox's shell", func() bool { return h.d.view("s2").panes[0].session != nil })
	prints("s2", link)
	click(ui.Super, "Plain")
	h.settle("the link of a sandbox without a browser to be opened with this Mac's", func() bool {
		return slices.Equal(h.tt.OpenedURLs(), []string{"mailto:someone@example.com", link})
	})
	if got := h.backend.linksOpened(); len(got) != 2 {
		t.Errorf("a sandbox without a browser had %q opened in one", got)
	}
}

// order is the order in which the window shows some texts, top to
// bottom, each once.
func (h *harness) order(texts ...string) string {
	h.t.Helper()
	type placed struct {
		text string
		y    float32
	}
	var found []placed
	for _, text := range texts {
		box, ok := h.tt.Find(text)
		if !ok {
			h.t.Fatalf("the window does not show %q: %q", text, h.tt.Texts())
		}
		found = append(found, placed{text, box.Y})
	}
	slices.SortStableFunc(found, func(a, b placed) int {
		switch {
		case a.y < b.y:
			return -1
		case a.y > b.y:
			return 1
		}
		return 0
	})
	var out []string
	for _, f := range found {
		out = append(out, f.text)
	}
	return strings.Join(out, " ")
}

// The Cards of the sandboxes that need their user come first, each with
// the Attention mark, and say why to whoever rests the pointer on them;
// the others keep the order they were made in, and what the page shows
// does not change as Cards move.
func TestCardsThatNeedYouComeFirst(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Alpha"), gated("s2", "Bravo"), gated("s3", "Charlie")})
	h.tt.SetSize(1280, 900)
	h.runs("s1", "s2", "s3")
	if got := h.order("Alpha", "Bravo", "Charlie"); got != "Alpha Bravo Charlie" || h.marks() != 0 {
		t.Fatalf("with nothing to say the Cards are %q, with %d marks", got, h.marks())
	}

	// A gate locks: its Card comes first, with the Attention mark and the
	// glyph of its state after its name, on one line, and says why.
	h.d.Apply(sandbox.Update{SandboxID: "s3", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockExitAddressMismatch}})
	h.tt.Frame()
	if got := h.order("Alpha", "Bravo", "Charlie"); got != "Charlie Alpha Bravo" {
		t.Fatalf("with Charlie's gate locked the Cards are %q", got)
	}
	name, _ := h.tt.Find("Charlie")
	state, stated := h.tt.Find("GATE LOCKED [Exit Address Mismatch]")
	mark, marked := h.tt.Find(attentionLabel)
	next, _ := h.tt.Find("Alpha")
	level := func(box ui.Rect) bool { return box.Y < name.Y+name.H && box.Y+box.H > name.Y && box.Y+box.H <= next.Y }
	if !stated || !marked || h.marks() != 1 || !level(mark) || !level(state) || name.X+name.W > mark.X || mark.X+mark.W > state.X || state.X+state.W > h.d.sidebarWidth {
		t.Fatalf("Charlie's Card: its name at %+v, the mark at %+v, its state at %+v, the next Card at %+v: %q", name, mark, state, next, h.tt.Texts())
	}
	if facts := h.facts("Charlie"); !strings.Contains(facts, "State|GATE LOCKED [Exit Address Mismatch]|Needs you|Gate locked: Exit Address Mismatch") {
		t.Errorf("Charlie's Card says %s", facts)
	}
	if facts := h.facts("Bravo"); strings.Contains(facts, "Needs you") {
		t.Errorf("the Card of a sandbox that does not need its user says %s", facts)
	}
	// The page still shows the sandbox it showed.
	if _, ok := h.tt.Find("Pane 1 of Alpha"); h.d.selected != "s1" || !ok {
		t.Errorf("the page shows %q after a Card moved", h.d.selected)
	}

	// A session of Bravo's rings while Alpha is shown: Bravo's Card joins
	// Charlie's, in the order the sandboxes were made.
	// The pointer leaves the Cards, which move under it: the one that
	// comes under it would say its facts.
	h.park()
	h.connOf("s2", 0).shell.Write([]byte("\a"))
	h.settle("the ring to be told", func() bool { return h.needs("s2") != "" })
	if got := h.order("Alpha", "Bravo", "Charlie"); got != "Bravo Charlie Alpha" || h.marks() != 2 {
		t.Fatalf("with Bravo ringing and Charlie locked the Cards are %q, with %d marks: %q", got, h.marks(), h.tt.Texts())
	}
	// Resting the pointer on a Card is not looking at its sandbox.
	if facts := h.facts("Bravo"); !strings.Contains(facts, "Needs you|A session asks for you") || h.needs("s2") == "" {
		t.Fatalf("Bravo's Card says %s", facts)
	}

	// Clicking Bravo's Card shows Bravo, where the session is in sight:
	// its Card goes back to its place.
	h.click("Bravo")
	h.settle("Bravo's page", func() bool { _, ok := h.tt.Find("Pane 1 of Bravo"); return h.d.selected == "s2" && ok })
	h.park()
	if got := h.order("Alpha", "Bravo", "Charlie"); got != "Charlie Alpha Bravo" || h.marks() != 1 {
		t.Errorf("after Bravo was looked at the Cards are %q, with %d marks", got, h.marks())
	}

	// A session of Alpha's ends while Bravo is shown.
	h.connOf("s1", 0).shell.Close()
	h.settle("the end to be told", func() bool { return h.needs("s1") == "A session ended" })
	if got := h.order("Alpha", "Bravo", "Charlie"); got != "Alpha Charlie Bravo" {
		t.Errorf("with Alpha's session ended and Charlie locked the Cards are %q: %q", got, h.tt.Texts())
	}
	if facts := h.facts("Alpha"); !strings.Contains(facts, "Needs you|A session ended") {
		t.Errorf("Alpha's Card says %s", facts)
	}

	// The gate opens and Alpha is looked at: the order they were made in.
	h.d.Apply(sandbox.Update{SandboxID: "s3", Status: &sandbox.Status{State: sandbox.StateGateOpen}})
	h.click("Alpha")
	h.settle("a frame", func() bool { return true })
	if got := h.order("Alpha", "Bravo", "Charlie"); got != "Alpha Bravo Charlie" || h.marks() != 0 {
		t.Errorf("with nothing left to say the Cards are %q, with %d marks", got, h.marks())
	}
	// The Shared Folders stay under the sandboxes.
	h.onDisk("research")
	h.settle("the Shared Folder", func() bool { return h.tt.HasText("research") })
	if got := h.order(sidebarSandboxes, "Charlie", sidebarSharedFolders, "research"); got != "Sandboxes Charlie Shared Folders research" {
		t.Errorf("the sidebar stands in the order %q", got)
	}
}

// The sidebar is two lists: the sandboxes' Cards, those that need their
// user first, whatever Shared Folder each is given; and under them the
// Shared Folders, in their order, each a row that shows its page. The
// arrows walk the rows in that order.
func TestTheSidebarListsTheSandboxesAndThenTheSharedFolders(t *testing.T) {
	alpha, bravo, charlie, delta := gated("s1", "Alpha"), gated("s2", "Bravo"), gated("s3", "Charlie"), gated("s4", "Delta")
	bravo.SharedFolder, charlie.SharedFolder, delta.SharedFolder = "research", "research", "docs"
	h := newHarness(t, []sandbox.Config{alpha, bravo, charlie, delta})
	h.tt.SetSize(1280, 900)
	h.onDisk("old")
	rows := []string{sidebarSandboxes, "Alpha", "Bravo", "Charlie", "Delta", sidebarSharedFolders, "docs", "old", "research"}
	if got := h.order(rows...); got != strings.Join(rows, " ") {
		t.Fatalf("the sidebar stands in the order %q", got)
	}
	// A folder no sandbox is given has its row, to show its page; and
	// each row says to whoever rests the pointer on it who is given it.
	h.click("old")
	if !h.tt.HasText("No sandbox is given this folder: name it in a sandbox's Setup.") {
		t.Errorf("the page of a folder no sandbox is given shows %q", h.tt.Texts())
	}
	if said := h.told("research") + h.told("old"); !strings.Contains(said, "The Shared Folder research: given to Bravo, Charlie") || !strings.Contains(said, "The Shared Folder old: no sandbox is given it") {
		t.Errorf("the folders' rows say %s", said)
	}
	// A folder's page names the sandboxes given it, with their states.
	h.click("research")
	page := strings.Join(h.tt.Texts(), "|")
	if !strings.Contains(page, "STOPPED|Bravo|STOPPED|Charlie") {
		t.Errorf("the page of a folder two sandboxes are given shows %s", page)
	}

	// A sandbox that needs its user comes first among the sandboxes,
	// whatever folder it is given, and the folders stand where they
	// stood.
	h.d.Apply(sandbox.Update{SandboxID: "s3", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockExitAddressMismatch}})
	h.tt.Frame()
	moved := []string{sidebarSandboxes, "Charlie", "Alpha", "Bravo", "Delta", sidebarSharedFolders, "docs", "old", "research"}
	if got := h.order(moved...); got != strings.Join(moved, " ") || h.marks() != 1 || !h.markedAfter("Charlie") {
		t.Fatalf("with Charlie's gate locked the sidebar stands in the order %q, with %d marks", got, h.marks())
	}

	// The arrows walk the rows in the order shown, the folders' among
	// them, and stop at the last.
	h.click("Alpha")
	for i, want := range []string{"s2", "s4", sharedFolderKey("docs"), sharedFolderKey("old"), sharedFolderKey("research"), sharedFolderKey("research")} {
		h.tt.Key(0, ui.KeyDown)
		h.tt.Frame()
		if h.d.selected != want {
			t.Fatalf("Down, %d times: %q is chosen, want %q", i+1, h.d.selected, want)
		}
	}

	// A sandbox given another folder, or none, stands where it stood:
	// the folders' rows follow what the sandboxes are given.
	h.d.configs[1].SharedFolder = "docs"
	h.d.configs[2].SharedFolder = ""
	h.tt.Frame()
	again := []string{sidebarSandboxes, "Charlie", "Alpha", "Bravo", "Delta", sidebarSharedFolders, "docs", "old"}
	if got := h.order(again...); got != strings.Join(again, " ") || h.tt.HasText("research") {
		t.Errorf("after two sandboxes were given other folders the sidebar stands in the order %q: %q", got, h.tt.Texts())
	}
}

// A Card is a row of the sidebar: the arrows move through the
// Cards in the order shown, a letter goes to the Card whose name begins
// with it, and both choose what the page shows.
func TestCardsKeepTheSidebarsKeys(t *testing.T) {
	h := newHarness(t, []sandbox.Config{gated("s1", "Alpha"), gated("s2", "Bravo"), gated("s3", "Charlie")})
	h.tt.SetSize(1280, 900)
	h.onDisk("research")
	h.d.Apply(sandbox.Update{SandboxID: "s3", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockNoInterface}})
	h.settle("the Shared Folder", func() bool { return h.tt.HasText("research") })
	if got := h.order("Alpha", "Bravo", "Charlie"); got != "Charlie Alpha Bravo" {
		t.Fatalf("the Cards are %q", got)
	}

	h.click("Alpha")
	if h.d.selected != "s1" {
		t.Fatalf("a click on a Card chose %q", h.d.selected)
	}
	for _, step := range []struct {
		key  ui.Key
		want string
	}{
		{ui.KeyDown, "s2"}, {ui.KeyDown, sharedFolderKey("research")}, {ui.KeyUp, "s2"}, {ui.KeyUp, "s1"}, {ui.KeyUp, "s3"}, {ui.KeyUp, "s3"},
	} {
		h.tt.Key(0, step.key)
		h.tt.Frame()
		if h.d.selected != step.want {
			t.Fatalf("an arrow chose %q, want %q", h.d.selected, step.want)
		}
	}
	// A Card is chosen by a click on its mark and on the glyph of its
	// state too.
	h.click("Alpha")
	h.click("GATE LOCKED [No Interface]")
	if h.d.selected != "s3" {
		t.Errorf("a click on the glyph of a Card's state chose %q", h.d.selected)
	}
	h.click("Alpha")
	h.click(attentionLabel)
	if h.d.selected != "s3" {
		t.Errorf("a click on a Card's Attention mark chose %q", h.d.selected)
	}
	h.tt.TypeKey(0, ui.KeyB, "b")
	h.tt.Frame()
	if h.d.selected != "s2" {
		t.Errorf("typing b chose %q, want Bravo", h.d.selected)
	}
}

// At its smallest the window still holds everything: the Cards, the
// Header with its controls, the viewport, and four Panes, each
// with its strip and a terminal that has room.
func TestTheWindowAtItsSmallest(t *testing.T) {
	const width, height = 900, 600
	browser := browserPack(t)
	cfg := gated("s1", "A sandbox with quite a long name")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg, gated("s2", "Scraper")}, browser)
	h.tt.SetSize(width, height)
	v := h.d.view("s1")
	v.running = cfg
	h.runs("s1")
	h.d.Apply(sandbox.Update{SandboxID: "s2", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockExitAddressMismatch}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", MCPCall: &sandbox.MCPCall{Time: time.Now(), Server: "browser", FromAgent: true, Method: "tools/call"}})
	// The control of the MCP calls takes its place in the Header.
	h.tt.Frame()
	for n := 2; n <= 3; n++ {
		h.click(addPaneLabel)
		h.settle("another Pane's shell", func() bool { return len(v.panes) == n && v.panes[n-1].session != nil })
	}

	inside := func(what string) ui.Rect {
		t.Helper()
		box, ok := h.tt.Find(what)
		if !ok {
			t.Fatalf("at %dx%d the window does not show %q: %q", width, height, what, h.tt.Texts())
		}
		if box.X < 0 || box.Y < 0 || box.X+box.W > width || box.Y+box.H > height || box.W <= 0 || box.H <= 0 {
			t.Errorf("at %dx%d %q is at %+v, not inside the window", width, height, what, box)
		}
		return box
	}
	// With three Panes, the Header holds every control it has.
	for _, what := range []string{
		attentionLabel, "GATE LOCKED [Exit Address Mismatch]", "GATE OPEN [203.0.113.7]", "New Sandbox",
		"A sandbox with quite a long name", addPaneLabel, mcpCallsLabel, "Stop", "Setup", "Clone", "Destroy",
		browserWaits,
	} {
		inside(what)
	}
	h.click(addPaneLabel)
	h.settle("the fourth Pane's shell", func() bool { return len(v.panes) == 4 && v.panes[3].session != nil })
	for i := range 4 {
		n := strconv.Itoa(i + 1)
		inside("Pane " + n)
		inside("Close Pane " + n)
		inside("Pane " + n + " of A sandbox with quite a long name")
	}
	// Each of the four sessions has a terminal with room to work in.
	for i, p := range v.panes {
		if cols, rows := p.session.term.Size(); cols < 20 || rows < 3 {
			t.Errorf("at %dx%d the terminal of Pane %d is %d columns by %d rows", width, height, i+1, cols, rows)
		}
	}
	// The viewport stands over the Panes, which are two rows of two.
	over, one, two, three, four := inside(browserWaits), inside("Pane 1"), inside("Pane 2"), inside("Pane 3"), inside("Pane 4")
	if over.Y >= one.Y || two.X <= one.X || two.Y != one.Y || three.Y <= one.Y || three.X != one.X || four.X != two.X || four.Y != three.Y {
		t.Errorf("the viewport is at %+v and the Panes at %+v, %+v, %+v and %+v", over, one, two, three, four)
	}
}

// A Hold is one switch of the network in a sandbox's Setup: on in a new
// sandbox, off in one that was saved without, and either's to change,
// with what it costs said behind its glyph. The Card of a sandbox whose
// gate is locked while its Hold lasts says so, in the app's own words.
func TestAHoldInTheSetup(t *testing.T) {
	fresh := gated("s1", "Research")
	if !fresh.Gate.Hold || !newDraft(fresh, nil).hold {
		t.Error("a new sandbox's gate keeps no Hold, or its form shows none")
	}
	// A sandbox that was saved without a Hold keeps none until asked.
	cfg := fresh
	cfg.Gate.Hold = false
	d := newDraft(cfg, nil)
	if d.hold {
		t.Error("the form of a sandbox that was saved without a Hold shows one")
	}
	d.hold = true
	back, errs := d.config(cfg)
	if len(errs) != 0 || !back.Gate.Hold || len(back.Validate()) != 0 {
		t.Fatalf("a form with a Hold makes %+v (%v, %v)", back.Gate, errs, back.Validate())
	}
	if again := newDraft(back, nil); !again.hold {
		t.Error("a sandbox whose gate keeps a Hold shows none")
	}

	h := newHarness(t, []sandbox.Config{cfg})
	texts := h.told("About Hold")
	for _, want := range []string{
		"locked 5 seconds after the last probe", "With one it is locked sooner, about two seconds after that probe with three probe URLs, and cuts nothing",
		"are kept for 30 seconds from that probe", "What they receive still arrives",
		"What the sandbox sends on them waits on this Mac until a probe is answered again", "makes no new connection and asks no name",
		"which shows a new exit address if it changed meanwhile", "ends the Hold at once",
	} {
		if !strings.Contains(texts, want) {
			t.Errorf("what is said of a Hold lacks %q: %s", want, texts)
		}
	}
	h.park()
	h.click("Keep its connections while no probe answers")
	saves := len(h.backend.saved)
	h.click("Save")
	h.settle("the config to be saved", func() bool { return len(h.backend.saved) == saves+1 })
	if got := h.backend.lastSaved()[0]; !got.Gate.Hold {
		t.Errorf("saved %+v", got.Gate)
	}
	// And off again.
	h.click("Keep its connections while no probe answers")
	h.click("Save")
	h.settle("the config to be saved again", func() bool { return len(h.backend.saved) == saves+2 })
	if got := h.backend.lastSaved()[0]; got.Gate.Hold {
		t.Errorf("with the Hold switched off again, saved %+v", got.Gate)
	}

	h.d.Apply(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateLocked, LockReason: sandbox.LockProbeUnanswered}})
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{LockReason: sandbox.LockProbeUnanswered, Held: true, Interface: "en0", InterfaceChange: 1}})
	h.tt.Frame()
	if facts := h.facts("Research"); !strings.Contains(facts, "State|GATE LOCKED [Probe Unanswered]|") || !strings.Contains(facts, "Hold|its connections are kept, what it sends waits, and it makes no new one|Leaves by|en0") {
		t.Errorf("with its Hold lasting the Card says %s", facts)
	}
	h.d.Apply(sandbox.Update{SandboxID: "s1", Gate: &sandbox.GateTelemetry{LockReason: sandbox.LockProbeUnanswered, Interface: "en0", InterfaceChange: 1}})
	h.tt.Frame()
	if facts := h.facts("Research"); strings.Contains(facts, "Hold|") {
		t.Errorf("with its Hold over the Card says %s", facts)
	}
}

// The frames of a viewport take text, which the window gives only to
// what says that it does: what a key types, and what an input method
// composes, which shows in the page where the pointer was last pressed.
// Input reaches the stream in the order it came. Back and Forward stand
// before the Address Bar, and the keys that go back and forward in a
// history do the same while the frames have the keyboard.
func TestTheViewportTakesWhatIsTypedAndGoesBackAndForward(t *testing.T) {
	browser := browserPack(t)
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg}, browser)
	h.tt.SetSize(1280, 900)
	home := viewport.BrowserTab{ID: "t1", Title: "Example Domain", Address: "https://example.com/"}
	h.backend.browse(home)
	h.d.view("s1").running = cfg
	h.runs("s1")
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Daemons: []sandbox.DaemonState{{Pack: browser.Name, Ready: true}}}})
	vp := h.d.view("s1").viewportOf(browser)
	h.settle("the first frame", func() bool { return vp.bitmap != nil && vp.stream != nil })
	stream := h.backend.opens()[0]
	// since waits until the stream got n inputs after its first from,
	// and gives them.
	since := func(what string, from, n int) []string {
		t.Helper()
		h.settle(what, func() bool { return len(stream.inputsSince(from)) >= n })
		return stream.inputsSince(from)
	}
	count := func() int { return len(stream.inputsSince(0)) }

	box, ok := h.tt.Find("Web Test Browser viewport")
	if !ok {
		t.Fatalf("no viewport in the window: %q", h.tt.Texts())
	}
	x, y := box.X+box.W/3, box.Y+box.H/4
	h.tt.ClickAt(x, y)
	h.settle("the click to reach the stream", func() bool { return count() >= 2 })
	if active, _ := h.tt.TextInputActive(); !active {
		t.Fatal("with the keyboard, the frames take no text: the window types none into them")
	}
	if caret, composes := h.tt.TextCaret(); !composes || math.Abs(float64(caret.X-x)) > 1 || math.Abs(float64(caret.Y-y)) > 1 {
		t.Errorf("an input method composes at %+v (%v), and the pointer was pressed at %v, %v", caret, composes, x, y)
	}

	// Keys that type: each comes as its key, which the stream has no
	// name for, and then as its text. All of it arrives, in order.
	from := count()
	typed := "the quick brown fox jumps over the lazy dog, " + strings.Repeat("again and ", 20)
	var want []string
	for _, r := range typed {
		h.tt.TypeKey(0, ui.KeyA, string(r))
		want = append(want, "text "+string(r))
	}
	if got := since("what was typed to reach the stream", from, len(want)); !slices.Equal(got, want) {
		t.Fatalf("what was typed reached the stream as %q", got)
	}

	// What an input method composes, its end, and what it commits.
	from = count()
	h.tt.Compose("ni", 2)
	h.tt.Compose("ni hao", 6)
	h.tt.Compose("", 0)
	h.tt.Type("你好")
	want = []string{"compose ni 2", "compose ni hao 6", "compose  0", "text 你好"}
	if got := since("the composition to reach the stream", from, len(want)); !slices.Equal(got, want) {
		t.Fatalf("a composition reached the stream as %q, want %q", got, want)
	}

	// Back and Forward stand before the Address Bar, in its row.
	back, ok1 := h.tt.Find(backLabel)
	forward, ok2 := h.tt.Find(forwardLabel)
	field, ok3 := h.tt.Find("Address of Web Test Browser")
	if !ok1 || !ok2 || !ok3 || back.X >= forward.X || forward.X+forward.W > field.X || back.Y < field.Y-field.H || back.Y+back.H > box.Y+1 {
		t.Fatalf("Back is at %+v, Forward at %+v, the Address Bar at %+v and the frames at %+v", back, forward, field, box)
	}
	from = count()
	h.click(backLabel)
	h.click(forwardLabel)
	want = []string{"back", "forward"}
	if got := since("Back and Forward to reach the stream", from, len(want)); !slices.Equal(got, want) {
		t.Fatalf("Back and Forward reached the stream as %q", got)
	}

	// The keys: a mouse's side buttons and a swipe come as Back and
	// Forward, and Command with a bracket is the keyboard's. Each goes
	// once, on its press. A bracket alone types.
	h.tt.ClickAt(x, y)
	h.settle("the click to reach the stream", func() bool { return count() >= from+4 })
	from = count()
	h.tt.Key(0, ui.KeyBack)
	h.tt.Key(0, ui.KeyForward)
	h.tt.Key(ui.Super, ui.KeyBracketLeft)
	h.tt.Key(ui.Super, ui.KeyBracketRight)
	h.tt.TypeKey(0, ui.KeyBracketLeft, "[")
	want = []string{"back", "forward", "back", "forward", "text ["}
	if got := since("the keys of a history to reach the stream", from, len(want)); !slices.Equal(got, want) {
		t.Fatalf("the keys of a history reached the stream as %q, want %q", got, want)
	}

	// An address the Browser Tab was sent to is not what the Address
	// Bar holds once the tab goes back: it holds what the browser lists.
	h.replace("Address of Web Test Browser", "example.org/docs")
	h.tt.Key(0, ui.KeyEnter)
	h.settle("the address to reach the stream", func() bool { return stream.sawInput("navigate https://example.org/docs") })
	if vp.address != "https://example.org/docs" {
		t.Fatalf("once sent, the Address Bar holds %q", vp.address)
	}
	h.click(backLabel)
	h.settle("the Address Bar to hold what the browser lists", func() bool { return vp.address == home.Address })

	// With the keyboard in the Address Bar, what is typed is the bar's.
	from = count()
	h.replace("Address of Web Test Browser", "example.net")
	if got := stream.inputsSince(from); len(got) != 0 {
		t.Errorf("what was typed into the Address Bar reached the page: %q", got)
	}
}

// The Edit menu's commands are the page's while the frames have the
// keyboard: Select All, Undo and Redo are done there; Copy and Cut put
// what the page says is selected on this Mac's clipboard, and leave it
// alone where nothing is; Paste types what the clipboard holds. Close
// Browser Tab, after the Address Bar, closes the one shown: another is
// shown then, and the browser's last gives way to a blank one.
func TestTheViewportTakesTheEditMenuAndClosesABrowserTab(t *testing.T) {
	browser := browserPack(t)
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg}, browser)
	h.tt.SetSize(1280, 900)
	home := viewport.BrowserTab{ID: "t1", Title: "Example Domain", Address: "https://example.com/"}
	h.backend.browse(home)
	h.d.view("s1").running = cfg
	h.runs("s1")
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Daemons: []sandbox.DaemonState{{Pack: browser.Name, Ready: true}}}})
	vp := h.d.view("s1").viewportOf(browser)
	h.settle("the first frame", func() bool { return vp.bitmap != nil && vp.stream != nil && vp.address == home.Address })
	stream := h.backend.opens()[0]
	count := func() int { return len(stream.inputsSince(0)) }
	// does performs a command of the Edit menu and gives what reached
	// the stream for it, which is n inputs.
	does := func(command string, n int) []string {
		t.Helper()
		from := count()
		h.tt.Command(command)
		h.settle("the stream to get what "+command+" sends", func() bool { return count() >= from+n })
		h.settle("a frame", func() bool { return true })
		return stream.inputsSince(from)
	}
	selects := func(text string) {
		h.backend.mu.Lock()
		h.backend.selected = text
		h.backend.mu.Unlock()
	}
	box, ok := h.tt.Find("Web Test Browser viewport")
	if !ok {
		t.Fatalf("no viewport in the window: %q", h.tt.Texts())
	}
	h.tt.ClickAt(box.X+box.W/2, box.Y+box.H/2)
	h.settle("the click to reach the stream", func() bool { return count() >= 2 })

	for command, want := range map[string]string{"selectAll": "edit selectAll", "undo": "edit undo", "redo": "edit redo"} {
		if got := does(command, 1); !slices.Equal(got, []string{want}) {
			t.Errorf("%s reached the stream as %q", command, got)
		}
	}

	// Copy: what the page says is selected, on this Mac's clipboard.
	h.tt.SetClipboard("what was there")
	selects("你好, page")
	if got := does("copy", 1); !slices.Equal(got, []string{"selection"}) {
		t.Errorf("Copy reached the stream as %q", got)
	}
	h.settle("the selection to be on the clipboard", func() bool { return h.tt.Clipboard() == "你好, page" })
	// Nothing selected: the clipboard keeps what it held, and nothing
	// is cut.
	selects("")
	if got := does("cut", 1); !slices.Equal(got, []string{"selection"}) {
		t.Errorf("Cut with nothing selected reached the stream as %q", got)
	}
	if h.tt.Clipboard() != "你好, page" {
		t.Errorf("with nothing selected, the clipboard holds %q", h.tt.Clipboard())
	}
	// Cut: on the clipboard, and then cut in the page.
	selects("cut words")
	if got := does("cut", 2); !slices.Equal(got, []string{"selection", "edit cut"}) {
		t.Errorf("Cut reached the stream as %q", got)
	}
	h.settle("what was cut to be on the clipboard", func() bool { return h.tt.Clipboard() == "cut words" })

	// Paste types what the clipboard holds, and nothing where it holds
	// nothing.
	h.tt.SetClipboard("a pass phrase, pasted")
	if got := does("paste", 1); !slices.Equal(got, []string{"text a pass phrase, pasted"}) {
		t.Errorf("Paste reached the stream as %q", got)
	}
	h.tt.SetClipboard("")
	from := count()
	h.tt.Command("paste")
	h.settle("a frame", func() bool { return true })
	if got := stream.inputsSince(from); len(got) != 0 {
		t.Errorf("Paste of an empty clipboard reached the stream as %q", got)
	}

	// With the keyboard in the Address Bar, the commands are the bar's.
	h.tt.SetClipboard("example.net")
	h.replace("Address of Web Test Browser", "")
	from = count()
	h.tt.Command("paste")
	h.settle("the Address Bar to take what was pasted", func() bool { return vp.address == "example.net" })
	if got := stream.inputsSince(from); len(got) != 0 {
		t.Errorf("what was pasted into the Address Bar reached the page: %q", got)
	}
	h.tt.ClickAt(box.X+box.W/2, box.Y+box.H/2)

	// Close Browser Tab stands after the Address Bar, in its row, with
	// one Browser Tab too.
	closer, ok1 := h.tt.Find(closeTabLabel)
	field, ok2 := h.tt.Find("Address of Web Test Browser")
	if !ok1 || !ok2 || closer.X < field.X+field.W || closer.Y < field.Y-field.H || closer.Y > field.Y+field.H {
		t.Fatalf("Close Browser Tab is at %+v and the Address Bar at %+v: %q", closer, field, h.tt.Texts())
	}
	// With two, a browser that does not close the one shown: that is
	// said.
	docs := viewport.BrowserTab{ID: "t2", Title: "Docs", Address: "https://example.org/docs"}
	h.backend.browse(docs, home)
	h.settle("the strip of the Browser Tabs", func() bool { return h.tt.HasText("Docs") })
	fails := func(what *error, is bool) {
		h.backend.mu.Lock()
		defer h.backend.mu.Unlock()
		*what = nil
		if is {
			*what = errors.New("the browser does not answer")
		}
	}
	fails(&h.backend.closeErr, true)
	h.click(closeTabLabel)
	h.settle("the failure to be said", func() bool {
		return h.tt.HasText("The Browser Tab was not closed: the browser does not answer")
	})
	h.click("Try Again")
	fails(&h.backend.closeErr, false)
	h.settle("the browser again", func() bool { _, ok := h.tt.Find(closeTabLabel); return ok })

	// Closed: the other Browser Tab is shown, and nothing is said of an
	// error.
	h.click(closeTabLabel)
	h.settle("the Browser Tab that is left to be shown", func() bool {
		opens := h.backend.opens()
		return len(opens) == 2 && opens[1].tab == "t2" && vp.address == docs.Address
	})
	if !stream.sawInput("close tab") {
		t.Errorf("the stream of the Browser Tab shown was not asked to close it: %q", stream.inputsSince(0))
	}
	h.settle("the strip to go", func() bool { return !h.tt.HasText("Example Domain") })
	if h.tt.HasText("Try Again") {
		t.Errorf("with one Browser Tab left the viewport shows %q", h.tt.Texts())
	}

	// The browser's last Browser Tab gives way to a blank one, which is
	// opened first. A browser that opens none keeps the last: that is
	// said, and nothing is closed.
	last := h.backend.opens()[1]
	fails(&h.backend.tabErr, true)
	h.click(closeTabLabel)
	h.settle("the failure to be said", func() bool {
		return h.tt.HasText("The Browser Tab was not opened: the browser does not answer")
	})
	if last.sawInput("close tab") {
		t.Error("the browser's last Browser Tab was closed though the browser opened no other")
	}
	h.click("Try Again")
	fails(&h.backend.tabErr, false)
	h.settle("the browser again", func() bool { _, ok := h.tt.Find(closeTabLabel); return ok })

	// The blank one is shown, with the keyboard in the Address Bar,
	// emptied, as a new Browser Tab's is; and the last one is closed,
	// once: the browser lists the blank one alone.
	h.click(closeTabLabel)
	h.settle("the blank Browser Tab to be shown in the last one's place", func() bool {
		opens := h.backend.opens()
		return len(opens) == 3 && opens[2].tab == "link2" && last.sawInput("close tab") && !h.tt.HasText("Docs")
	})
	if got := h.backend.linksOpened(); !slices.Equal(got, []string{"s1 " + blankAddress, "s1 " + blankAddress}) {
		t.Errorf("the browser was asked for %q", got)
	}
	h.settle("the Address Bar to have the keyboard", func() bool { return !vp.toBar && vp.closing == nil })
	h.settle("a frame", func() bool { return true })
	if active, _ := h.tt.TextInputActive(); !active || vp.address != "" {
		t.Errorf("in the last Browser Tab's place the Address Bar holds %q, and the window takes text: %v", vp.address, active)
	}
	closes := 0
	for _, input := range last.inputsSince(0) {
		if input == "close tab" {
			closes++
		}
	}
	h.backend.mu.Lock()
	left := append([]viewport.BrowserTab(nil), h.backend.browserTabs...)
	h.backend.mu.Unlock()
	if closes != 1 || len(left) != 1 || left[0].ID != "link2" {
		t.Errorf("the last Browser Tab was asked to close %d times, and the browser lists %+v", closes, left)
	}
	if _, ok := h.tt.Find(closeTabLabel); !ok || h.tt.HasText("Try Again") {
		t.Errorf("with a blank Browser Tab in the last one's place the viewport shows %q", h.tt.Texts())
	}
}

// New Browser Tab, after the Address Bar, opens a blank Browser Tab,
// which is shown, with the keyboard in the Address Bar, emptied: what is
// typed next is where the tab goes. The bar keeps the keyboard though
// the strip of the Browser Tabs comes to stand over it.
func TestANewBrowserTabIsBlankAndAsksWhereToGo(t *testing.T) {
	browser := browserPack(t)
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg}, browser)
	h.tt.SetSize(1280, 900)
	home := viewport.BrowserTab{ID: "t1", Title: "Example Domain", Address: "https://example.com/"}
	h.backend.browse(home)
	h.d.view("s1").running = cfg
	h.runs("s1")
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Daemons: []sandbox.DaemonState{{Pack: browser.Name, Ready: true}}}})
	vp := h.d.view("s1").viewportOf(browser)
	h.settle("the first frame", func() bool { return vp.bitmap != nil && vp.stream != nil && vp.address == home.Address })

	// It stands after the Address Bar, in its row, with one Browser Tab
	// too.
	adder, ok1 := h.tt.Find(newTabLabel)
	field, ok2 := h.tt.Find("Address of Web Test Browser")
	if !ok1 || !ok2 || adder.X < field.X+field.W || adder.Y < field.Y-field.H || adder.Y > field.Y+field.H {
		t.Fatalf("New Browser Tab is at %+v and the Address Bar at %+v: %q", adder, field, h.tt.Texts())
	}
	h.click(newTabLabel)
	h.settle("the blank Browser Tab to be shown", func() bool {
		opens := h.backend.opens()
		return len(opens) == 2 && opens[1].tab == "link1" && h.tt.HasText("Sign in")
	})
	if got := h.backend.linksOpened(); !slices.Equal(got, []string{"s1 " + blankAddress}) {
		t.Fatalf("the browser was asked for %q", got)
	}
	// What is typed now is the Address Bar's, in the place of what it
	// held, and the new Browser Tab's stream is sent there.
	h.settle("the Address Bar to have the keyboard", func() bool { return !vp.toBar })
	h.settle("a frame", func() bool { return true })
	if active, _ := h.tt.TextInputActive(); !active || vp.address != "" {
		t.Fatalf("with a new Browser Tab the Address Bar holds %q, and the window takes text: %v", vp.address, active)
	}
	h.tt.Type("example.org/docs")
	h.tt.Key(0, ui.KeyEnter)
	blank := h.backend.opens()[1]
	h.settle("the new Browser Tab to be sent where was typed", func() bool { return blank.sawInput("navigate https://example.org/docs") })
	if first := h.backend.opens()[0]; first.sawInput("navigate https://example.org/docs") || first.sawInput("text example.org/docs") {
		t.Errorf("the Browser Tab that was shown before got %q", first.inputsSince(0))
	}
	// Both are in the strip now, and the new one can be closed.
	if _, ok := h.tt.Find(closeTabLabel); !ok {
		t.Errorf("with two Browser Tabs the viewport shows %q", h.tt.Texts())
	}
}

// The tests do not wait a second between two listings of the Browser
// Tabs.
func init() { tabsEvery = 5 * time.Millisecond }

// Over a browser's viewport stands the Address Bar, which holds the
// address of the Browser Tab shown and sends that tab to what is typed
// there; and over it, once the browser has more than one Browser Tab,
// the strip that chooses which is shown. Each Browser Tab keeps its
// place in the strip, and one that is closed gives way to another.
func TestTheViewportHasAnAddressBarAndBrowserTabs(t *testing.T) {
	browser := browserPack(t)
	cfg := gated("s1", "Research")
	cfg.Packs = []string{browser.Name}
	h := newHarness(t, []sandbox.Config{cfg}, browser)
	h.tt.SetSize(1280, 900)
	home := viewport.BrowserTab{ID: "t1", Title: "Example Domain", Address: "https://example.com/"}
	h.backend.browse(home)
	h.d.view("s1").running = cfg
	h.runs("s1")
	h.d.Apply(sandbox.Update{SandboxID: "s1", Workstation: &sandbox.WorkstationStatus{Daemons: []sandbox.DaemonState{{Pack: browser.Name, Ready: true}}}})
	vp := h.d.view("s1").viewportOf(browser)
	const bar = "Address of Web Test Browser"
	h.settle("the Address Bar to hold the address of the Browser Tab shown", func() bool { return vp.address == home.Address })
	field, ok1 := h.tt.Find(bar)
	frames, ok2 := h.tt.Find("Web Test Browser viewport")
	if !ok1 || !ok2 || field.Y >= frames.Y || field.Y+field.H > frames.Y+1 {
		t.Fatalf("the Address Bar is at %+v and the frames at %+v: %q", field, frames, h.tt.Texts())
	}
	// One Browser Tab needs no strip.
	if h.tt.HasText("Example Domain") {
		t.Errorf("with one Browser Tab the viewport shows a strip: %q", h.tt.Texts())
	}
	first := h.backend.opens()[0]
	if first.tab != "t1" {
		t.Fatalf("the first stream shows the Browser Tab %q", first.tab)
	}

	// An address typed there is sent to the Browser Tab shown, with its
	// scheme; what is typed stays while it is typed, whatever the
	// browser lists; and the keyboard is the frames' again afterwards.
	h.replace(bar, "example.org/docs")
	listed := first.timesListed()
	h.settle("the Browser Tabs to be listed while an address is typed", func() bool { return first.timesListed() >= listed+2 })
	if vp.address != "example.org/docs" {
		t.Fatalf("while it was typed the Address Bar came to hold %q", vp.address)
	}
	h.tt.Key(0, ui.KeyEnter)
	h.settle("the address to reach the stream", func() bool { return first.sawInput("navigate https://example.org/docs") })
	h.tt.Type("typed in the page")
	h.settle("what is typed after an address was sent to reach the page", func() bool { return first.sawInput("text typed in the page") })
	if vp.address != "https://example.org/docs" {
		t.Errorf("once sent, the Address Bar holds %q", vp.address)
	}

	// The browser has two: the strip shows both, each by its own title,
	// a long one cut, and the one shown is the one it was.
	long := strings.Repeat("A very long title ", 4)
	docs := viewport.BrowserTab{ID: "t2", Title: long, Address: "https://example.org/reference"}
	home.Title, home.Address = "Docs", "https://example.org/docs"
	// The browser lists the one last used first.
	h.backend.browse(docs, home)
	cut := string([]rune(long)[:tabTitle]) + "…"
	h.settle("the strip of the Browser Tabs", func() bool { return h.tt.HasText("Docs") && h.tt.HasText(cut) })
	if h.tt.HasText(long) {
		t.Errorf("a long title is shown whole: %q", h.tt.Texts())
	}
	one, _ := h.tt.Find("Docs")
	two, _ := h.tt.Find(cut)
	field, _ = h.tt.Find(bar)
	if one.X >= two.X || one.Y >= field.Y {
		t.Errorf("the Browser Tab that was there first is at %+v, the new one at %+v and the Address Bar at %+v", one, two, field)
	}
	if len(h.backend.opens()) != 1 || vp.address != "https://example.org/docs" {
		t.Fatalf("a second Browser Tab in the browser opened %d streams, and the Address Bar holds %q", len(h.backend.opens()), vp.address)
	}

	// The other is chosen: its stream is opened, the first one's closed,
	// and the Address Bar holds its address.
	h.click(cut)
	h.settle("the stream of the Browser Tab that was chosen", func() bool {
		opens := h.backend.opens()
		return len(opens) == 2 && opens[1].tab == "t2" && vp.address == docs.Address
	})
	select {
	case <-first.closed:
	case <-time.After(2 * time.Second):
		t.Error("the stream of the Browser Tab that is no longer shown stays open")
	}
	second := h.backend.opens()[1]
	h.settle("frames of the Browser Tab that was chosen", func() bool { return second.timesAsked() >= 3 })
	// The places in the strip are as they were, whatever the browser
	// lists first.
	h.backend.browse(home, docs)
	listed = second.timesListed()
	h.settle("the Browser Tabs to be listed again", func() bool { return second.timesListed() >= listed+2 })
	one, _ = h.tt.Find("Docs")
	two, _ = h.tt.Find(cut)
	if one.X >= two.X {
		t.Errorf("the Browser Tabs changed places: %+v and %+v", one, two)
	}

	// Out of sight, nothing is listed.
	h.click("Setup")
	h.settle("the Setup", func() bool { return h.tt.HasText("Leaves by") })
	time.Sleep(50 * time.Millisecond)
	listed = second.timesListed()
	for i := 0; i < 20; i++ {
		h.settle("a frame", func() bool { return true })
		time.Sleep(5 * time.Millisecond)
	}
	if after := second.timesListed(); after > listed+1 {
		t.Errorf("a hidden viewport listed its Browser Tabs %d more times", after-listed)
	}
	h.click("Setup")
	h.settle("the Browser Tabs to be listed once the viewport shows again", func() bool { return second.timesListed() >= listed+3 })

	// The Browser Tab shown is closed in the browser, and its stream
	// ends: the other is shown, and nothing is said of an error.
	h.backend.browse(home)
	second.Close()
	h.settle("the stream of the Browser Tab that is left", func() bool {
		opens := h.backend.opens()
		return len(opens) == 3 && opens[2].tab == "t1" && vp.address == home.Address
	})
	if h.tt.HasText("Try Again") || h.tt.HasText(cut) || h.tt.HasText("Docs") {
		t.Errorf("with one Browser Tab left the viewport shows %q", h.tt.Texts())
	}

	// The last one's stream ends too: that is said, and trying again
	// opens the browser's first.
	h.backend.opens()[2].Close()
	h.settle("the end of the stream to be said", func() bool { return h.tt.HasText("Try Again") })
	h.click("Try Again")
	h.settle("a stream of the browser's first Browser Tab", func() bool {
		opens := h.backend.opens()
		return len(opens) == 4 && opens[3].tab == "t1"
	})
}

// A way out may be said to answer names with stand-in addresses: each
// row has a switch of its own for it, which the form keeps with the row
// and saves with it, whatever the row expects of the exit address. What
// it means is said by the glyph beside it, and speaks of no interface.
func TestAWayOutMayAnswerNamesWithStandInAddresses(t *testing.T) {
	cfg := gated("s1", "Research")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{
		{Name: "utun4", AnyExitAddressButPhysical: true, StandInAddresses: true},
		{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7"}},
	}
	rows := newDraft(cfg, nil)
	if !rows.interfaces[0].standIns || rows.interfaces[1].standIns {
		t.Fatalf("the rows say %v and %v of stand-in addresses", rows.interfaces[0].standIns, rows.interfaces[1].standIns)
	}
	if back, errs := rows.config(cfg); len(errs) != 0 || !reflect.DeepEqual(back.AllowedInterfaces, cfg.AllowedInterfaces) || len(back.Validate()) != 0 {
		t.Fatalf("the form gives back %+v (%v, %v)", back.AllowedInterfaces, errs, back.Validate())
	}
	// A row nothing was chosen in is no Interface, switch or no switch.
	fresh := newDraft(sandbox.NewConfig("s9", "Fresh"), nil)
	fresh.interfaces[0].standIns = true
	if back, _ := fresh.config(sandbox.NewConfig("s9", "Fresh")); len(back.AllowedInterfaces) != 0 {
		t.Errorf("an empty row with the switch on was read as the Interface %+v", back.AllowedInterfaces)
	}

	h := newHarness(t, []sandbox.Config{cfg})
	v := h.d.view("s1")
	const first, second = standInsAbout + " at way out 1", standInsAbout + " at way out 2"
	if h.count(standInsLabel) != 2 {
		t.Fatalf("two ways out show %d switches for stand-in addresses: %q", h.count(standInsLabel), h.tt.Texts())
	}
	// Each row's switch is its own.
	h.click(second)
	if !v.draft.interfaces[0].standIns || !v.draft.interfaces[1].standIns {
		t.Fatalf("after the second row's switch was clicked the rows say %v and %v", v.draft.interfaces[0].standIns, v.draft.interfaces[1].standIns)
	}
	h.click(first)
	if v.draft.interfaces[0].standIns || !v.draft.interfaces[1].standIns {
		t.Fatalf("after the first row's switch was clicked the rows say %v and %v", v.draft.interfaces[0].standIns, v.draft.interfaces[1].standIns)
	}
	// A row keeps it when it is moved, and when what it expects changes.
	h.click("Move way out 2 up")
	h.choose("Expected at way out 1", expectations, expectAny)
	h.click("Save")
	h.settle("the save", func() bool { return len(h.backend.lastSaved()) == 1 })
	got := h.backend.lastSaved()[0].AllowedInterfaces
	if len(got) != 2 || got[0].Name != "en0" || !got[0].StandInAddresses || !got[0].AnyExitAddress || got[1].Name != "utun4" || got[1].StandInAddresses || !got[1].AnyExitAddressButPhysical {
		t.Fatalf("saved %+v", got)
	}

	help := h.told("About " + first)
	for _, want := range []string{"198.18.0.0/15", "its names resolve and nothing connects", "while it leaves by this way out and by no other", "decided where the way out leads, by the name it stands for", "reaches it", "This Mac's own address in the range stays out of reach"} {
		if !strings.Contains(help, want) {
			t.Errorf("what is said of stand-in addresses lacks %q: %s", want, help)
		}
	}
	if strings.Contains(strings.ToLower(standInsHelp+standInsLabel), "interface") {
		t.Error("what the form says of stand-in addresses speaks of interfaces")
	}
}
