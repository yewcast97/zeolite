package dashboard

import (
	"context"
	"image"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/pack"
	"zeolite/internal/viewport"
)

// daemonReady reports whether a pack's daemon accepts connections.
func (v *sandboxView) daemonReady(name string) bool {
	for _, d := range v.workstation.Daemons {
		if d.Pack == name {
			return d.Ready
		}
	}
	return false
}

func (v *sandboxView) viewportOf(m pack.Manifest) *viewportView {
	vp := v.viewports[m.Name]
	if vp == nil {
		vp = &viewportView{sv: v, pack: m, shown: make(chan struct{}, 1), listed: make(chan struct{}, 1)}
		v.viewports[m.Name] = vp
	}
	return vp
}

// tabsEvery is how long after a browser's Browser Tabs were listed, and
// the listing was built into a frame of the window, they are listed
// again; browserPatience bounds one question to the browser, a listing
// or where Back and Forward lead; tabTitle is how many characters of a
// Browser Tab's title its tab shows; and inputBacklog is how much input
// waits for a stream that takes none before more is dropped.
var tabsEvery = time.Second

const (
	browserPatience = 10 * time.Second
	tabTitle        = 28
	inputBacklog    = 256
)

// viewportView shows one Viewport Stream and sends it the pointer and
// the keyboard. Over a browser's it has the Address Bar, with Back and
// Forward beside it, and over that the strip of the browser's Browser
// Tabs where it has more than one.
type viewportView struct {
	sv   *sandboxView
	pack pack.Manifest

	opening bool
	stream  ViewportStream
	cancel  context.CancelFunc
	err     string
	// inputs is what the stream is still to be sent, in the order it
	// came: one goroutine sends it, so that the main thread never waits
	// on the sandbox and no input overtakes another.
	inputs chan func()

	bitmap     *ui.Bitmap
	frameW     int
	frameH     int
	pageW      float64
	pageH      float64
	boxW, boxH float32
	// shown is signalled by each build of the view: the stream is asked
	// for its next frame only after the one before was built into a
	// frame of the window, so a viewport nobody sees asks for nothing.
	// listed is the same for the listing of the Browser Tabs.
	shown  chan struct{}
	listed chan struct{}

	// tabs are the browser's Browser Tabs, each where the viewport first
	// saw it; tab is the ID of the one shown, empty for the browser's
	// first; and chosen is the strip's chosen tab, which is another than
	// the one shown once its user chose it.
	tabs   []viewport.BrowserTab
	tab    string
	chosen int
	// address is what the Address Bar holds: the address of the Browser
	// Tab shown, or what is typed there while the bar has the keyboard.
	// bar is the bar's field, and frames the box that shows the frames,
	// which has the keyboard again once an address is sent.
	address string
	bar     ui.Handle
	frames  ui.Handle
	// sent is the address the Browser Tab shown was last sent to, and
	// sentFrom the address the browser listed for it then: until it
	// lists another, the tab is on its way, and the bar holds sent.
	sent, sentFrom string
	// toBar is whether the Address Bar is to have the keyboard at the
	// next build: a new Browser Tab's.
	toBar bool
	// services are the window's, by which the Edit menu's commands
	// reach this Mac's clipboard.
	services ui.Services
	// caretX and caretY are where the pointer was last pressed on the
	// frames, in their box: what an input method composes shows there,
	// the page's own caret being the page's to know.
	caretX, caretY float32
}

func (vp *viewportView) close() {
	if vp.cancel != nil {
		vp.cancel()
	}
	if vp.stream != nil {
		vp.stream.Close()
	}
	vp.stream, vp.cancel, vp.opening = nil, nil, false
}

// open opens the stream, on the Browser Tab that is to be shown, and
// feeds its frames and the browser's Browser Tabs to the view.
func (vp *viewportView) open() {
	vp.opening, vp.err = true, ""
	d, id, tab := vp.sv.d, vp.sv.id, vp.tab
	ctx, cancel := context.WithCancel(context.Background())
	vp.cancel = cancel
	inputs := make(chan func(), inputBacklog)
	vp.inputs = inputs
	go func() {
		stream, err := d.backend.OpenViewport(id, vp.pack, tab)
		if err != nil {
			d.update(func() {
				// Closed since, it has nothing to say. Otherwise the
				// browser's first Browser Tab is the next to try.
				if ctx.Err() == nil {
					vp.opening, vp.err, vp.tab = false, err.Error(), ""
					vp.chosen = vp.shownIndex()
				}
			})
			return
		}
		d.update(func() {
			if ctx.Err() != nil {
				stream.Close()
				return
			}
			vp.stream, vp.tab = stream, stream.Tab()
			vp.chosen = vp.shownIndex()
		})
		go vp.listTabs(ctx, stream)
		go func() {
			for {
				select {
				case input := <-inputs:
					input()
				case <-ctx.Done():
					return
				}
			}
		}()
		for {
			frame, err := stream.Next(ctx)
			if err != nil {
				d.update(func() {
					if ctx.Err() != nil {
						return
					}
					vp.close()
					// A Browser Tab that was closed takes its stream with
					// it: where the browser has another, that one is
					// shown.
					for _, other := range vp.tabs {
						if other.ID != vp.tab {
							vp.tabs = []viewport.BrowserTab{other}
							vp.tab, vp.chosen = other.ID, 0
							return
						}
					}
					vp.err = "The stream ended: " + err.Error()
				})
				return
			}
			// Forget builds from before this frame, then wait for the
			// one that shows it.
			select {
			case <-vp.shown:
			default:
			}
			d.update(func() { vp.show(frame) })
			select {
			case <-vp.shown:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// listTabs lists the browser's Browser Tabs for the view until ctx
// ends: again a moment after each listing was built into a frame of the
// window, so that a viewport nobody sees asks for nothing.
func (vp *viewportView) listTabs(ctx context.Context, stream ViewportStream) {
	d := vp.sv.d
	for {
		listing, cancel := context.WithTimeout(ctx, browserPatience)
		tabs, err := stream.Tabs(listing)
		cancel()
		if ctx.Err() != nil {
			return
		}
		// Forget builds from before this listing, then wait for the one
		// that shows it.
		select {
		case <-vp.listed:
		default:
		}
		d.update(func() {
			// A listing that failed leaves the one before: the stream
			// says so itself when the browser is gone.
			if err == nil && vp.stream == stream {
				vp.list(tabs)
			}
		})
		select {
		case <-vp.listed:
		case <-ctx.Done():
			return
		}
		select {
		case <-time.After(tabsEvery):
		case <-ctx.Done():
			return
		}
	}
}

// list takes the browser's Browser Tabs as it lists them now. Each
// keeps the place the viewport first saw it at, and a new one comes
// last: the browser itself lists the one last used first. A tab its
// user chose and the view has not acted on yet stays chosen.
func (vp *viewportView) list(tabs []viewport.BrowserTab) {
	wanted := ""
	if vp.chosen != vp.shownIndex() && vp.chosen >= 0 && vp.chosen < len(vp.tabs) {
		wanted = vp.tabs[vp.chosen].ID
	}
	now := make(map[string]viewport.BrowserTab, len(tabs))
	for _, tab := range tabs {
		now[tab.ID] = tab
	}
	var kept []viewport.BrowserTab
	for _, tab := range vp.tabs {
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
	vp.tabs = kept
	vp.chosen = vp.shownIndex()
	for i, tab := range vp.tabs {
		if tab.ID == wanted {
			vp.chosen = i
		}
	}
}

// shownIndex is where the Browser Tab shown stands among the Browser
// Tabs: first, while it is not among them.
func (vp *viewportView) shownIndex() int {
	for i, tab := range vp.tabs {
		if tab.ID == vp.tab {
			return i
		}
	}
	return 0
}

// shownAddress is the address of the Browser Tab shown: where it was
// sent, while the browser still lists the address it was sent from, and
// otherwise what the browser last listed.
func (vp *viewportView) shownAddress() string {
	listed := ""
	for _, tab := range vp.tabs {
		if tab.ID == vp.tab {
			listed = tab.Address
		}
	}
	if vp.sent != "" && listed == vp.sentFrom {
		return vp.sent
	}
	vp.sent = ""
	return listed
}

// openLink takes a link that the sandbox's user Command-clicked in a
// Pane, and reports whether it did: a web address, where the sandbox has
// a browser, which opens it in a new Browser Tab, shown at once. So an
// agent's link to sign in is opened where its answer is waited for. The
// address is the session's word, the one thing a click takes of what a
// session printed: it goes to that browser and is kept nowhere. A link
// that is left, the terminal opens with this Mac's.
func (v *sandboxView) openLink(link string) bool {
	if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
		return false
	}
	for i, m := range v.streamPacks() {
		if m.ViewportStream.Type == pack.StreamCDP {
			v.viewportTab = i
			v.viewportOf(m).openTab(link, false)
			return true
		}
	}
	return false
}

// blankAddress is where a new Browser Tab is that was sent nowhere yet.
const blankAddress = "about:blank"

// openTab opens a new Browser Tab at an address, off the main thread,
// and shows it. One its user asked for is blank, and the keyboard goes
// to the Address Bar, which is emptied for where the tab is to go.
func (vp *viewportView) openTab(address string, typing bool) {
	sv := vp.sv
	d, id := sv.d, sv.id
	go func() {
		tab, err := d.backend.OpenBrowserTab(id, vp.pack, address)
		d.update(func() {
			// The sandbox stopped meanwhile: its viewports are gone.
			if sv.viewports[vp.pack.Name] != vp {
				return
			}
			if err != nil {
				vp.err = "The link was not opened: " + err.Error()
				return
			}
			vp.err = ""
			vp.switchTo(tab)
			if typing {
				vp.toBar, vp.address = true, ""
			}
		})
	}()
}

// switchTo shows another Browser Tab: the stream of the one shown is
// closed, and the next build opens the other's. The frame it showed
// stays until the other's first.
func (vp *viewportView) switchTo(tab string) {
	vp.close()
	vp.tab, vp.sent = tab, ""
	vp.chosen = vp.shownIndex()
}

// navigate sends the Browser Tab shown to what the Address Bar holds,
// and gives the keyboard back to the frames. The bar holds the address
// it sent until the browser lists another than it was sent from.
func (vp *viewportView) navigate(c *ui.Context) {
	stream, address := vp.stream, viewport.Address(vp.address)
	if stream == nil || address == "" {
		return
	}
	vp.sent = ""
	vp.sentFrom = vp.shownAddress()
	vp.sent, vp.address = address, address
	vp.frames.Focus(c)
	vp.send(func() { stream.Navigate(address) })
}

// send has the stream sent an input after those that came before it.
// What comes while inputBacklog of them wait is dropped: the sandbox
// takes none then.
func (vp *viewportView) send(input func()) {
	select {
	case vp.inputs <- input:
	default:
	}
}

// along sends the Browser Tab shown along its own history, by Back or
// by Forward of its stream. The Address Bar holds what the browser
// lists from then on, not an address the tab was sent to before.
func (vp *viewportView) along(step func(ViewportStream, context.Context) error) {
	stream := vp.stream
	if stream == nil {
		return
	}
	vp.sent = ""
	vp.send(func() { asked(func(ctx context.Context) { step(stream, ctx) }) })
}

// backLabel, forwardLabel, newTabLabel and closeTabLabel are what the
// controls beside the Address Bar say they do.
const (
	backLabel     = "Back"
	forwardLabel  = "Forward"
	newTabLabel   = "New Browser Tab"
	closeTabLabel = "Close Browser Tab"
)

// asked runs a question to the browser for no longer than
// browserPatience.
func asked(question func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.Background(), browserPatience)
	defer cancel()
	question(ctx)
}

// copy puts the text selected in the page on this Mac's clipboard, as
// its user asked by Copy or Cut, and with Cut has the page cut it. It
// is the page's word, and goes nowhere else. Where the page says that
// nothing is selected, the clipboard stays as it is.
func (vp *viewportView) copy(cut bool) {
	stream, d, services := vp.stream, vp.sv.d, vp.services
	vp.send(func() {
		asked(func(ctx context.Context) {
			text, err := stream.Selection(ctx)
			if err != nil || text == "" {
				return
			}
			d.update(func() { services.WriteClipboard(text) })
			if cut {
				stream.Edit(viewport.EditCut)
			}
		})
	})
}

// closeTab closes the Browser Tab shown. Its stream ends with it, and
// the view shows another, which there is: the control is offered only
// where the browser has more than one.
func (vp *viewportView) closeTab() {
	stream, d := vp.stream, vp.sv.d
	if stream == nil {
		return
	}
	vp.send(func() {
		asked(func(ctx context.Context) {
			if err := stream.CloseTab(ctx); err != nil {
				d.update(func() {
					if vp.stream == stream {
						vp.err = "The Browser Tab was not closed: " + err.Error()
					}
				})
			}
		})
	})
}

// tabTitles are what the strip's tabs say: each Browser Tab's own
// title, or its address where it has none, cut to a tab's length.
func (vp *viewportView) tabTitles() []string {
	titles := make([]string, len(vp.tabs))
	for i, tab := range vp.tabs {
		title := tab.Title
		if title == "" {
			title = tab.Address
		}
		if title == "" {
			title = newTabLabel
		}
		if runes := []rune(title); len(runes) > tabTitle {
			title = string(runes[:tabTitle]) + "…"
		}
		titles[i] = title
	}
	return titles
}

// show puts a frame in the bitmap. A frame as JPEG decodes it is given
// to the GPU as it is; anything else as RGBA.
func (vp *viewportView) show(f viewport.Frame) {
	b := f.Image.Bounds()
	if vp.bitmap == nil || b.Dx() != vp.frameW || b.Dy() != vp.frameH {
		vp.frameW, vp.frameH = b.Dx(), b.Dy()
		vp.bitmap = ui.NewBitmap(image.NewRGBA(image.Rect(0, 0, vp.frameW, vp.frameH)))
	}
	vp.pageW, vp.pageH = f.PageWidth, f.PageHeight
	if ycc, ok := f.Image.(*image.YCbCr); ok && ycc.SubsampleRatio == image.YCbCrSubsampleRatio420 {
		vp.bitmap.UpdateYUV(&ui.YUVImage{
			Format:    ui.I420,
			Width:     b.Dx(),
			Height:    b.Dy(),
			Planes:    [3][]byte{ycc.Y, ycc.Cb, ycc.Cr},
			Strides:   [3]int{ycc.YStride, ycc.CStride, ycc.CStride},
			Matrix:    ui.BT601,
			FullRange: true,
		})
		return
	}
	vp.bitmap.Update(f.Image)
}

// toPage maps a point of the view's box to the page the frame shows,
// which is fitted inside the box with its proportions kept.
func (vp *viewportView) toPage(x, y float32) (float64, float64) {
	if vp.boxW <= 0 || vp.boxH <= 0 || vp.frameW == 0 || vp.frameH == 0 {
		return 0, 0
	}
	scale := min(vp.boxW/float32(vp.frameW), vp.boxH/float32(vp.frameH))
	offX := (vp.boxW - float32(vp.frameW)*scale) / 2
	offY := (vp.boxH - float32(vp.frameH)*scale) / 2
	fx := float64((x-offX)/scale) / float64(vp.frameW)
	fy := float64((y-offY)/scale) / float64(vp.frameH)
	fx, fy = max(0, min(1, fx)), max(0, min(1, fy))
	return fx * vp.pageW, fy * vp.pageH
}

// editCommands are the commands of the Edit menu that the page does by
// itself, as the stream names them. Copy, Cut and Paste are the view's,
// by this Mac's clipboard.
var editCommands = map[string]viewport.EditCommand{
	"selectAll": viewport.EditSelectAll, "undo": viewport.EditUndo, "redo": viewport.EditRedo,
}

// viewportKeys are the keys that type no text, as the stream names them.
var viewportKeys = map[ui.Key]viewport.Key{
	ui.KeyEnter: viewport.KeyEnter, ui.KeyTab: viewport.KeyTab, ui.KeyBackspace: viewport.KeyBackspace,
	ui.KeyDelete: viewport.KeyDelete, ui.KeyEscape: viewport.KeyEscape,
	ui.KeyLeft: viewport.KeyLeft, ui.KeyRight: viewport.KeyRight, ui.KeyUp: viewport.KeyUp, ui.KeyDown: viewport.KeyDown,
	ui.KeyHome: viewport.KeyHome, ui.KeyEnd: viewport.KeyEnd, ui.KeyPageUp: viewport.KeyPageUp, ui.KeyPageDown: viewport.KeyPageDown,
}

func viewportMods(m ui.Modifiers) viewport.Modifiers {
	var out viewport.Modifiers
	if m&ui.Shift != 0 {
		out |= viewport.Shift
	}
	if m&ui.Ctrl != 0 {
		out |= viewport.Control
	}
	if m&ui.Alt != 0 {
		out |= viewport.Alt
	}
	if m&ui.Super != 0 {
		out |= viewport.Meta
	}
	return out
}

// handle forwards input to the stream, in the order it comes. A key
// that goes back or forward in a history does so in the Browser Tab's:
// a mouse's side buttons and a swipe between pages, which come as keys,
// and Command with a bracket. The Edit menu's commands are the page's:
// Copy and Cut put what is selected there on this Mac's clipboard, and
// Paste types what the clipboard holds.
func (vp *viewportView) handle(ev ui.InputEvent) bool {
	stream := vp.stream
	if stream == nil {
		return false
	}
	x, y := vp.toPage(ev.X, ev.Y)
	mods := viewportMods(ev.Mods)
	var send func()
	switch ev.Kind {
	case ui.InputPointerMove:
		send = func() { stream.Pointer(viewport.PointerMoved, x, y, ev.Button, 0, mods) }
	case ui.InputPointerDown:
		vp.caretX, vp.caretY = ev.X, ev.Y
		send = func() { stream.Pointer(viewport.PointerPressed, x, y, ev.Button, max(1, ev.Clicks), mods) }
	case ui.InputPointerUp:
		send = func() { stream.Pointer(viewport.PointerReleased, x, y, ev.Button, max(1, ev.Clicks), mods) }
	case ui.InputScroll:
		send = func() { stream.Scroll(x, y, float64(ev.DX), float64(ev.DY), mods) }
	case ui.InputText:
		send = func() { stream.Text(ev.Text) }
	case ui.InputCompose:
		send = func() { stream.Compose(ev.Text, ev.Caret) }
	case ui.InputCommand:
		switch command, edits := editCommands[ev.Text]; {
		case edits:
			send = func() { stream.Edit(command) }
		case ev.Text == "copy" || ev.Text == "cut":
			vp.copy(ev.Text == "cut")
			return true
		case ev.Text == "paste":
			text := vp.services.ReadClipboard()
			if text == "" {
				return true
			}
			send = func() { stream.Text(text) }
		default:
			return false
		}
	case ui.InputKeyDown, ui.InputKeyUp:
		down := ev.Kind == ui.InputKeyDown
		back := ev.Key == ui.KeyBack || ev.Mods == ui.Super && ev.Key == ui.KeyBracketLeft
		forward := ev.Key == ui.KeyForward || ev.Mods == ui.Super && ev.Key == ui.KeyBracketRight
		if back || forward {
			step := ViewportStream.Back
			if forward {
				step = ViewportStream.Forward
			}
			if down {
				vp.along(step)
			}
			return true
		}
		key, ok := viewportKeys[ev.Key]
		if !ok {
			return false
		}
		send = func() { stream.PressKey(key, down, mods) }
	default:
		return false
	}
	vp.send(send)
	return true
}

func (vp *viewportView) view(c *ui.Context) {
	t := c.Theme()
	vp.services = c.Services()
	// This build shows whatever frame the bitmap holds, and whatever
	// Browser Tabs were last listed.
	for _, built := range []chan struct{}{vp.shown, vp.listed} {
		select {
		case built <- struct{}{}:
		default:
		}
	}
	if vp.err != "" {
		ui.Column(c).Grow(1).Center().Gap(t.Space(2)).Children(func() {
			ui.Text(c, vp.err).TextColor(t.Danger)
			if ui.Button(c, "Try Again").Key("viewport-retry").Clicked() {
				vp.err = ""
			}
		})
		return
	}
	// The strip's tab that its user chose is the Browser Tab to show.
	if vp.chosen != vp.shownIndex() && vp.chosen >= 0 && vp.chosen < len(vp.tabs) {
		vp.switchTo(vp.tabs[vp.chosen].ID)
	}
	waiting := vp.stream == nil && !vp.opening
	if waiting && vp.sv.daemonReady(vp.pack.Name) {
		vp.open()
	}
	switch {
	case vp.bitmap != nil:
	case waiting:
		vp.sv.note(c, "Waiting for "+vp.pack.Title+" to be ready…")
		return
	default:
		vp.sv.note(c, "Connecting to "+vp.pack.Title+"…")
		return
	}
	if len(vp.tabs) > 1 {
		tabStrip(c, "Browser Tabs of "+vp.pack.Title, &vp.chosen, vp.tabTitles())
	}
	// The Address Bar follows the Browser Tab shown, but while something
	// is typed there.
	if !vp.bar.Focused(c) {
		vp.address = vp.shownAddress()
	}
	// The row is the same one whether or not a strip stands over it: the
	// Address Bar keeps the keyboard when the browser's second Browser
	// Tab comes.
	ui.Row(c).Key("viewport-bar").Shrink(0).FillWidth().MinHeight(stripHeight).AlignItems(ui.Center).Gap(t.Space(1)).Padding(t.Space(1), t.Space(2)).Font(chromeFont).
		Background(panelTheme.Background).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
		if glyphButton(c, "viewport-back-"+vp.pack.Name, glyphBack, backLabel).Clicked() {
			vp.along(ViewportStream.Back)
		}
		if glyphButton(c, "viewport-forward-"+vp.pack.Name, glyphForward, forwardLabel).Clicked() {
			vp.along(ViewportStream.Forward)
		}
		c.Key("viewport-address-" + vp.pack.Name)
		ui.TextInput(c, &vp.address).Label("Address of " + vp.pack.Title).Placeholder("Address").FontSize(textSmall).Grow(1).
			Bind(&vp.bar).OnSubmit(func() { vp.navigate(c) })
		if vp.toBar {
			vp.toBar = false
			vp.bar.Focus(c)
		}
		if glyphButton(c, "viewport-new-tab-"+vp.pack.Name, glyphAdd, newTabLabel).Clicked() {
			vp.openTab(blankAddress, true)
		}
		// The browser's last Browser Tab stays, as a sandbox's last Pane
		// does: a browser with none shows nothing.
		if len(vp.tabs) > 1 && glyphButton(c, "viewport-close-tab-"+vp.pack.Name, glyphClose, closeTabLabel).Clicked() {
			vp.closeTab()
		}
	})
	// The frames take text: the window gives what is typed, and what an
	// input method composes, only to what says that it does.
	ui.Box(c).Grow(1).FillWidth().Key("viewport").Label(vp.pack.Title + " viewport").Focusable().Bind(&vp.frames).
		Background(t.Background).
		Draw(func(_ *ui.Painter, r ui.Rect) { vp.boxW, vp.boxH = r.W, r.H }).
		HandleInput(vp.handle).
		TextCaret(ui.Rect{X: vp.caretX, Y: vp.caretY, W: 1, H: textBody}).
		Children(func() {
			ui.Image(c, vp.bitmap).Fit(ui.Contain).Fill()
		})
}
